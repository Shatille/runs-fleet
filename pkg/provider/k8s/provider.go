// Package k8s implements the provider interfaces for Kubernetes.
package k8s

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
)

// Runner state constants.
const (
	StateRunning    = "running"
	StatePending    = "pending"
	StateStopped    = "stopped"
	StateTerminated = "terminated"
	StateUnknown    = "unknown"
)

// dns1123LabelRegex validates DNS-1123 label compliance.
var dns1123LabelRegex = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizePodName ensures pod name is DNS-1123 compliant.
// Converts to lowercase, replaces invalid chars with dashes, truncates to 63 chars.
func sanitizePodName(runID string) string {
	name := "runner-" + strings.ToLower(runID)
	// Replace sequences of invalid chars with single dash
	name = dns1123LabelRegex.ReplaceAllString(name, "-")
	// Collapse consecutive dashes
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	// Remove leading/trailing dashes
	name = strings.Trim(name, "-")
	// Truncate to 63 chars (K8s label limit)
	if len(name) > 63 {
		name = name[:63]
		// Remove trailing dash after truncation
		name = strings.TrimRight(name, "-")
	}
	// Ensure non-empty
	if name == "" {
		name = "runner"
	}
	return name
}

// Provider implements provider.Provider for Kubernetes.
type Provider struct {
	clientset kubernetes.Interface
	config    *config.Config
}

// NewProvider creates a K8s provider with in-cluster or kubeconfig auth.
func NewProvider(appConfig *config.Config) (*Provider, error) {
	var restConfig *rest.Config
	var err error

	if appConfig.KubeConfig != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", appConfig.KubeConfig)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s clientset: %w", err)
	}

	return &Provider{
		clientset: clientset,
		config:    appConfig,
	}, nil
}

// NewProviderWithClient creates a Provider with an injected clientset for testing.
func NewProviderWithClient(clientset kubernetes.Interface, appConfig *config.Config) *Provider {
	return &Provider{
		clientset: clientset,
		config:    appConfig,
	}
}

// Clientset returns the Kubernetes clientset for use by PoolProvider.
func (p *Provider) Clientset() kubernetes.Interface {
	return p.clientset
}

// Name returns the provider identifier.
func (p *Provider) Name() string {
	return "k8s"
}

// CreateRunner creates a Kubernetes Pod for a runner.
// For private jobs, also creates a NetworkPolicy to restrict egress.
func (p *Provider) CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	podName := sanitizePodName(spec.RunID)
	namespace := p.config.KubeNamespace

	pod := p.buildPodSpec(podName, spec)

	created, err := p.clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create pod %s: %w", podName, err)
	}

	// Create NetworkPolicy for private jobs to restrict internal cluster access
	if spec.Private {
		netpol := p.buildNetworkPolicy(podName, spec)
		_, err := p.clientset.NetworkingV1().NetworkPolicies(namespace).Create(ctx, netpol, metav1.CreateOptions{})
		if err != nil {
			// Private jobs require NetworkPolicy for security - clean up pod and fail
			log.Printf("Failed to create NetworkPolicy for private pod %s: %v, cleaning up", podName, err)
			if delErr := p.clientset.CoreV1().Pods(namespace).Delete(ctx, created.Name, metav1.DeleteOptions{}); delErr != nil {
				log.Printf("Failed to cleanup pod %s after NetworkPolicy failure: %v", created.Name, delErr)
			}
			return nil, fmt.Errorf("failed to create NetworkPolicy for private job: %w", err)
		}
	}

	return &provider.RunnerResult{
		RunnerIDs: []string{created.Name},
		ProviderData: map[string]string{
			"namespace": namespace,
			"private":   fmt.Sprintf("%t", spec.Private),
		},
	}, nil
}

// TerminateRunner deletes a Kubernetes Pod and its associated NetworkPolicy.
// NotFound is treated as success for idempotency - termination is a desired end state.
func (p *Provider) TerminateRunner(ctx context.Context, runnerID string) error {
	namespace := p.config.KubeNamespace

	// Delete associated NetworkPolicy (best effort - may not exist for non-private jobs)
	netpolName := networkPolicyName(runnerID)
	err := p.clientset.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, netpolName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		log.Printf("Failed to delete NetworkPolicy %s: %v", netpolName, err)
	}

	err = p.clientset.CoreV1().Pods(namespace).Delete(ctx, runnerID, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod %s: %w", runnerID, err)
	}
	return nil
}

// DescribeRunner returns the current state of a Kubernetes Pod.
func (p *Provider) DescribeRunner(ctx context.Context, runnerID string) (*provider.RunnerState, error) {
	namespace := p.config.KubeNamespace

	pod, err := p.clientset.CoreV1().Pods(namespace).Get(ctx, runnerID, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("pod %s not found", runnerID)
		}
		return nil, fmt.Errorf("failed to get pod %s: %w", runnerID, err)
	}

	providerData := map[string]string{
		"namespace": namespace,
	}
	if pod.Spec.NodeName != "" {
		providerData["node"] = pod.Spec.NodeName
	}

	state := &provider.RunnerState{
		RunnerID:     runnerID,
		State:        mapPodPhase(pod.Status.Phase),
		InstanceType: getInstanceType(pod),
		ProviderData: providerData,
	}

	if pod.Status.StartTime != nil {
		state.LaunchTime = pod.Status.StartTime.Time
	} else {
		state.LaunchTime = pod.CreationTimestamp.Time
	}

	return state, nil
}

// buildPodSpec creates a Pod specification for a runner.
func (p *Provider) buildPodSpec(name string, spec *provider.RunnerSpec) *corev1.Pod {
	labels := map[string]string{
		"app":                         "runs-fleet-runner",
		"runs-fleet.io/run-id":        spec.RunID,
		"runs-fleet.io/pool":          spec.Pool,
		"runs-fleet.io/arch":          spec.Arch,
		"runs-fleet.io/os":            spec.OS,
		"runs-fleet.io/instance-type": spec.InstanceType,
	}

	// Add spot/private labels for NetworkPolicy and scheduling
	if spec.Spot {
		labels["runs-fleet.io/spot"] = "true"
	}
	if spec.Private {
		labels["runs-fleet.io/private"] = "true"
	}

	// Merge default node selector with config
	nodeSelector := make(map[string]string)
	for k, v := range p.config.KubeNodeSelector {
		nodeSelector[k] = v
	}
	// Add architecture selector
	switch spec.Arch {
	case "arm64":
		nodeSelector["kubernetes.io/arch"] = "arm64"
	case "x64":
		nodeSelector["kubernetes.io/arch"] = "amd64"
	}

	// Build tolerations for spot/preemptible nodes
	tolerations := p.buildTolerations(spec)

	// Resource requests from spec (Karpenter provisions nodes based on these)
	resources := p.getResourceRequirements(spec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.config.KubeNamespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: p.config.KubeServiceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			NodeSelector:       nodeSelector,
			Tolerations:        tolerations,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true),
				RunAsUser:    ptr.To(int64(1000)),
				RunAsGroup:   ptr.To(int64(1000)),
				FSGroup:      ptr.To(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "runner",
					Image:           p.config.KubeRunnerImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Resources:       resources,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						// ReadOnlyRootFilesystem false: GitHub Actions runners need writable
						// workspace at /runner/_work and temp directories for build artifacts.
						ReadOnlyRootFilesystem: ptr.To(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Env: []corev1.EnvVar{
						{Name: "RUNS_FLEET_RUN_ID", Value: spec.RunID},
						{Name: "RUNS_FLEET_JOB_ID", Value: spec.JobID},
						{Name: "RUNS_FLEET_REPO", Value: spec.Repo},
						{Name: "RUNS_FLEET_POOL", Value: spec.Pool},
					},
				},
			},
		},
	}

	return pod
}

// buildTolerations creates tolerations for spot/preemptible nodes.
// Supports common cloud provider taints for spot instances.
func (p *Provider) buildTolerations(spec *provider.RunnerSpec) []corev1.Toleration {
	var tolerations []corev1.Toleration

	if spec.Spot {
		// GKE preemptible/spot nodes
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "cloud.google.com/gke-preemptible",
			Operator: corev1.TolerationOpEqual,
			Value:    "true",
			Effect:   corev1.TaintEffectNoSchedule,
		})
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "cloud.google.com/gke-spot",
			Operator: corev1.TolerationOpEqual,
			Value:    "true",
			Effect:   corev1.TaintEffectNoSchedule,
		})

		// EKS spot nodes
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "eks.amazonaws.com/capacityType",
			Operator: corev1.TolerationOpEqual,
			Value:    "SPOT",
			Effect:   corev1.TaintEffectNoSchedule,
		})

		// AKS spot nodes
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "kubernetes.azure.com/scalesetpriority",
			Operator: corev1.TolerationOpEqual,
			Value:    "spot",
			Effect:   corev1.TaintEffectNoSchedule,
		})

		// GKE node provisioning (transient taint during node startup)
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "cloud.google.com/gke-provisioning",
			Operator: corev1.TolerationOpExists,
		})

		// Generic runs-fleet preemptible taint
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "runs-fleet.io/preemptible",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		})

		// Karpenter spot consolidation
		tolerations = append(tolerations, corev1.Toleration{
			Key:      "karpenter.sh/disruption",
			Operator: corev1.TolerationOpExists,
		})
	}

	return tolerations
}

// networkPolicyName returns the NetworkPolicy name for a runner.
func networkPolicyName(runnerID string) string {
	return runnerID + "-netpol"
}

// blockedCIDRs contains IP ranges that private jobs should not access.
// Includes RFC1918 private ranges and cloud metadata service ranges.
var blockedCIDRs = []string{
	"10.0.0.0/8",       // RFC1918 Class A private
	"172.16.0.0/12",    // RFC1918 Class B private
	"192.168.0.0/16",   // RFC1918 Class C private
	"169.254.0.0/16",   // Link-local (includes cloud metadata services)
	"100.64.0.0/10",    // Carrier-grade NAT (some cloud providers)
}

// buildNetworkPolicy creates a NetworkPolicy for private runners.
// Restricts egress to deny internal cluster IPs and cloud metadata services.
//
// Security trade-off: DNS egress (port 53) is unrestricted to any destination.
// This is intentional because: (1) kube-dns IP varies by cluster, making IP-based
// restrictions fragile, (2) namespace selectors vary across K8s distributions,
// (3) port 53 is protocol-constrained and cannot access non-DNS services like
// the metadata service (which uses HTTP on port 80). DNS tunneling risk is
// accepted as low for this threat model.
func (p *Provider) buildNetworkPolicy(podName string, spec *provider.RunnerSpec) *networkingv1.NetworkPolicy {
	netpolName := networkPolicyName(podName)
	dnsPort := intstr.FromInt32(53)
	httpsPort := intstr.FromInt32(443)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      netpolName,
			Namespace: p.config.KubeNamespace,
			Labels: map[string]string{
				"app":                  "runs-fleet-runner",
				"runs-fleet.io/run-id": spec.RunID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"runs-fleet.io/run-id":  spec.RunID,
					"runs-fleet.io/private": "true",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// Allow DNS to any destination (see function doc for security rationale)
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &dnsPort, Protocol: ptr.To(corev1.ProtocolUDP)},
						{Port: &dnsPort, Protocol: ptr.To(corev1.ProtocolTCP)},
					},
				},
				// Allow HTTPS to external IPs (deny internal and metadata ranges)
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &httpsPort, Protocol: ptr.To(corev1.ProtocolTCP)},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR:   "0.0.0.0/0",
								Except: blockedCIDRs,
							},
						},
					},
				},
				// Allow HTTP to external IPs (for package downloads)
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: ptr.To(intstr.FromInt32(80)), Protocol: ptr.To(corev1.ProtocolTCP)},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR:   "0.0.0.0/0",
								Except: blockedCIDRs,
							},
						},
					},
				},
			},
		},
	}
}

// Default resource values when spec doesn't provide them.
const (
	defaultCPUCores   = 2
	defaultMemoryGiB  = 4
	defaultStorageGiB = 30
)

// getResourceRequirements returns resource requests and limits from spec.
// Uses spec.CPUCores, spec.MemoryGiB, spec.StorageGiB directly for K8s-native
// resource management with Karpenter. Falls back to defaults if not specified.
func (p *Provider) getResourceRequirements(spec *provider.RunnerSpec) corev1.ResourceRequirements {
	cpu := spec.CPUCores
	if cpu <= 0 {
		cpu = defaultCPUCores
	}

	mem := spec.MemoryGiB
	if mem <= 0 {
		mem = defaultMemoryGiB
	}

	storage := spec.StorageGiB
	if storage <= 0 {
		storage = defaultStorageGiB
	}

	cpuQuantity := resource.MustParse(fmt.Sprintf("%d", cpu))
	// Use fractional format to preserve precision (e.g., 8.5Gi)
	memQuantity := resource.MustParse(fmt.Sprintf("%.1fGi", mem))
	storageQuantity := resource.MustParse(fmt.Sprintf("%dGi", storage))

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              cpuQuantity,
			corev1.ResourceMemory:           memQuantity,
			corev1.ResourceEphemeralStorage: storageQuantity,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              cpuQuantity,
			corev1.ResourceMemory:           memQuantity,
			corev1.ResourceEphemeralStorage: storageQuantity,
		},
	}
}

// mapPodPhase maps Kubernetes Pod phase to provider state.
func mapPodPhase(phase corev1.PodPhase) string {
	switch phase {
	case corev1.PodPending:
		return StatePending
	case corev1.PodRunning:
		return StateRunning
	case corev1.PodSucceeded, corev1.PodFailed:
		return StateTerminated
	default:
		return StateUnknown
	}
}

// getInstanceType extracts instance type from pod labels.
func getInstanceType(pod *corev1.Pod) string {
	if t, ok := pod.Labels["runs-fleet.io/instance-type"]; ok {
		return t
	}
	return ""
}

// WaitForPodRunning waits for a pod to reach Running state.
// Reserved for future pool warming implementation where we need to wait for pods to become ready.
func (p *Provider) WaitForPodRunning(ctx context.Context, podName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	namespace := p.config.KubeNamespace
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		pod, err := p.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if pod.Status.Phase == corev1.PodRunning {
			return nil
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("pod %s terminated with phase %s", podName, pod.Status.Phase)
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timeout waiting for pod %s to be running", podName)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Ensure Provider implements provider.Provider.
var _ provider.Provider = (*Provider)(nil)
