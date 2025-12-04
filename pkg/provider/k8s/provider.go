// Package k8s implements the provider interfaces for Kubernetes.
package k8s

import (
	"context"
	"encoding/json"
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
// Creates ConfigMap, Secret, and PVC for agent config before Pod creation.
// For private jobs, also creates a NetworkPolicy to restrict egress.
func (p *Provider) CreateRunner(ctx context.Context, spec *provider.RunnerSpec) (*provider.RunnerResult, error) {
	podName := sanitizePodName(spec.RunID)
	namespace := p.config.KubeNamespace

	// cleanup helper for failure cases
	cleanup := func(includePod bool) {
		if includePod {
			if podErr := p.clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{}); podErr != nil && !errors.IsNotFound(podErr) {
				log.Printf("Failed to cleanup Pod %s: %v", podName, podErr)
			}
		}
		if pvcErr := p.deleteRunnerPVC(ctx, podName); pvcErr != nil && !errors.IsNotFound(pvcErr) {
			log.Printf("Failed to cleanup PVC %s: %v", pvcName(podName), pvcErr)
		}
		if secErr := p.deleteRunnerSecret(ctx, podName); secErr != nil && !errors.IsNotFound(secErr) {
			log.Printf("Failed to cleanup Secret %s: %v", secretName(podName), secErr)
		}
		if cmErr := p.deleteRunnerConfigMap(ctx, podName); cmErr != nil && !errors.IsNotFound(cmErr) {
			log.Printf("Failed to cleanup ConfigMap %s: %v", configMapName(podName), cmErr)
		}
	}

	// Validate daemon.json ConfigMap exists if configured
	if p.config.KubeDaemonJSONConfigMap != "" {
		_, err := p.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, p.config.KubeDaemonJSONConfigMap, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("daemon.json ConfigMap %q not found: %w", p.config.KubeDaemonJSONConfigMap, err)
		}
	}

	// Create ConfigMap with runner config (non-sensitive)
	if err := p.createRunnerConfigMap(ctx, podName, spec); err != nil {
		return nil, fmt.Errorf("failed to create ConfigMap for runner: %w", err)
	}

	// Create Secret with sensitive data (JIT token, cache token)
	if err := p.createRunnerSecret(ctx, podName, spec); err != nil {
		cleanup(false)
		return nil, fmt.Errorf("failed to create Secret for runner: %w", err)
	}

	// Create PVC for workspace storage (EBS via CSI driver)
	if err := p.createRunnerPVC(ctx, podName, spec); err != nil {
		cleanup(false)
		return nil, fmt.Errorf("failed to create PVC for runner: %w", err)
	}

	pod := p.buildPodSpec(podName, spec)

	_, err := p.clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		cleanup(false)
		return nil, fmt.Errorf("failed to create pod %s: %w", podName, err)
	}

	// Create NetworkPolicy for private jobs to restrict internal cluster access
	if spec.Private {
		netpol := p.buildNetworkPolicy(podName, spec)
		_, npErr := p.clientset.NetworkingV1().NetworkPolicies(namespace).Create(ctx, netpol, metav1.CreateOptions{})
		if npErr != nil && !errors.IsAlreadyExists(npErr) {
			log.Printf("Failed to create NetworkPolicy for private pod %s: %v, cleaning up", podName, npErr)
			cleanup(true)
			return nil, fmt.Errorf("failed to create NetworkPolicy for private job: %w", npErr)
		}
	}

	return &provider.RunnerResult{
		RunnerIDs: []string{podName},
		ProviderData: map[string]string{
			"namespace": namespace,
			"private":   fmt.Sprintf("%t", spec.Private),
		},
	}, nil
}

// configMapName returns the ConfigMap name for a runner.
func configMapName(podName string) string {
	return podName + "-config"
}

// secretName returns the Secret name for a runner.
func secretName(podName string) string {
	return podName + "-secrets"
}

// createRunnerConfigMap creates a ConfigMap with non-sensitive runner config.
// Returns nil if ConfigMap already exists (idempotent for multi-instance deployments).
func (p *Provider) createRunnerConfigMap(ctx context.Context, podName string, spec *provider.RunnerSpec) error {
	labelsJSON := "[]"
	if len(spec.Labels) > 0 {
		if data, err := json.Marshal(spec.Labels); err == nil {
			labelsJSON = string(data)
		}
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(podName),
			Namespace: p.config.KubeNamespace,
			Labels: map[string]string{
				"app":                  "runs-fleet-runner",
				"runs-fleet.io/run-id": spec.RunID,
			},
		},
		Data: map[string]string{
			"repo":         spec.Repo,
			"labels":       labelsJSON,
			"runner_group": spec.RunnerGroup,
			"job_id":       spec.JobID,
		},
	}

	_, err := p.clientset.CoreV1().ConfigMaps(p.config.KubeNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// createRunnerSecret creates a Secret with sensitive runner config.
// Returns nil if Secret already exists (idempotent for multi-instance deployments).
func (p *Provider) createRunnerSecret(ctx context.Context, podName string, spec *provider.RunnerSpec) error {
	if spec.JITToken == "" {
		return fmt.Errorf("JITToken is required for runner registration")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(podName),
			Namespace: p.config.KubeNamespace,
			Labels: map[string]string{
				"app":                  "runs-fleet-runner",
				"runs-fleet.io/run-id": spec.RunID,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"jit_token":   spec.JITToken,
			"cache_token": spec.CacheToken,
		},
	}

	_, err := p.clientset.CoreV1().Secrets(p.config.KubeNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// deleteRunnerConfigMap deletes the ConfigMap for a runner.
func (p *Provider) deleteRunnerConfigMap(ctx context.Context, podName string) error {
	return p.clientset.CoreV1().ConfigMaps(p.config.KubeNamespace).Delete(ctx, configMapName(podName), metav1.DeleteOptions{})
}

// deleteRunnerSecret deletes the Secret for a runner.
func (p *Provider) deleteRunnerSecret(ctx context.Context, podName string) error {
	return p.clientset.CoreV1().Secrets(p.config.KubeNamespace).Delete(ctx, secretName(podName), metav1.DeleteOptions{})
}

// pvcName returns the PVC name for a runner.
func pvcName(podName string) string {
	return podName + "-workspace"
}

// createRunnerPVC creates a PersistentVolumeClaim for runner workspace storage.
// Uses EBS CSI driver via StorageClass for dynamic provisioning.
// Returns nil if PVC already exists (idempotent for multi-instance deployments).
func (p *Provider) createRunnerPVC(ctx context.Context, podName string, spec *provider.RunnerSpec) error {
	storageGiB := spec.StorageGiB
	if storageGiB <= 0 {
		storageGiB = defaultStorageGiB
	}

	storageQuantity := resource.MustParse(fmt.Sprintf("%dGi", storageGiB))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName(podName),
			Namespace: p.config.KubeNamespace,
			Labels: map[string]string{
				"app":                  "runs-fleet-runner",
				"runs-fleet.io/run-id": spec.RunID,
				"runs-fleet.io/pool":   spec.Pool,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageQuantity,
				},
			},
		},
	}

	// Set StorageClass if configured (empty = cluster default)
	if p.config.KubeStorageClass != "" {
		pvc.Spec.StorageClassName = &p.config.KubeStorageClass
	}

	_, err := p.clientset.CoreV1().PersistentVolumeClaims(p.config.KubeNamespace).Create(ctx, pvc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// deleteRunnerPVC deletes the PVC for a runner.
func (p *Provider) deleteRunnerPVC(ctx context.Context, podName string) error {
	return p.clientset.CoreV1().PersistentVolumeClaims(p.config.KubeNamespace).Delete(ctx, pvcName(podName), metav1.DeleteOptions{})
}

// TerminateRunner deletes a Kubernetes Pod and its associated resources.
// Cleanup order: Pod first (releases volume), then NetworkPolicy, PVC, ConfigMap, Secret.
// NotFound is treated as success for idempotency - termination is a desired end state.
// Returns error if any cleanup operation fails (except NotFound).
func (p *Provider) TerminateRunner(ctx context.Context, runnerID string) error {
	namespace := p.config.KubeNamespace
	var cleanupErrors []string

	// Delete Pod first (releases PVC mount, allows CSI driver to detach volume)
	// Continue with cleanup even if Pod deletion fails to avoid resource leaks
	if podErr := p.clientset.CoreV1().Pods(namespace).Delete(ctx, runnerID, metav1.DeleteOptions{}); podErr != nil && !errors.IsNotFound(podErr) {
		log.Printf("Failed to delete Pod %s: %v", runnerID, podErr)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("Pod: %v", podErr))
	}

	// Delete associated NetworkPolicy (may not exist for non-private jobs)
	netpolName := networkPolicyName(runnerID)
	if err := p.clientset.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, netpolName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		log.Printf("Failed to delete NetworkPolicy %s: %v", netpolName, err)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("NetworkPolicy: %v", err))
	}

	// Delete PVC after pod (CSI driver will detach and delete EBS volume)
	if pvcErr := p.deleteRunnerPVC(ctx, runnerID); pvcErr != nil && !errors.IsNotFound(pvcErr) {
		log.Printf("Failed to delete PVC for %s: %v", runnerID, pvcErr)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("PVC: %v", pvcErr))
	}

	// Delete ConfigMap
	if cmErr := p.deleteRunnerConfigMap(ctx, runnerID); cmErr != nil && !errors.IsNotFound(cmErr) {
		log.Printf("Failed to delete ConfigMap for %s: %v", runnerID, cmErr)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("ConfigMap: %v", cmErr))
	}

	// Delete Secret
	if secErr := p.deleteRunnerSecret(ctx, runnerID); secErr != nil && !errors.IsNotFound(secErr) {
		log.Printf("Failed to delete Secret for %s: %v", runnerID, secErr)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("Secret: %v", secErr))
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("cleanup incomplete for %s: %v", runnerID, cleanupErrors)
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

// buildPodSpec creates a Pod specification for a runner with DinD sidecar.
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

	// Karpenter annotations to prevent disruption during job execution
	annotations := map[string]string{
		"karpenter.sh/do-not-evict":   "true",
		"karpenter.sh/do-not-disrupt": "true",
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
	case "amd64":
		nodeSelector["kubernetes.io/arch"] = "amd64"
	}

	// Build tolerations for spot/preemptible nodes
	tolerations := p.buildTolerations(spec)

	// Resource requests from spec (Karpenter provisions nodes based on these)
	resources := p.getResourceRequirements(spec)

	// Build environment variables for runner container
	envVars := []corev1.EnvVar{
		{Name: "RUNS_FLEET_RUN_ID", Value: spec.RunID},
		{Name: "RUNS_FLEET_JOB_ID", Value: spec.JobID},
		{Name: "RUNS_FLEET_REPO", Value: spec.Repo},
		{Name: "RUNS_FLEET_POOL", Value: spec.Pool},
		{Name: "DOCKER_HOST", Value: "unix:///var/run/docker.sock"},
		{Name: "RUNNER_WAIT_FOR_DOCKER_IN_SECONDS", Value: fmt.Sprintf("%d", p.config.KubeDockerWaitSeconds)},
	}

	// Add Valkey address for telemetry if configured
	if p.config.ValkeyAddr != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "RUNS_FLEET_VALKEY_ADDR", Value: p.config.ValkeyAddr})
		if p.config.ValkeyPassword != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "RUNS_FLEET_VALKEY_PASSWORD", Value: p.config.ValkeyPassword})
		}
	}

	// Build volumes: base volumes + DinD volumes
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMapName(name),
					},
				},
			},
		},
		{
			Name: "secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName(name),
				},
			},
		},
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName(name),
				},
			},
		},
		// DinD shared volumes
		{
			Name: "dind-sock",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "dind-externals",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// Add daemon.json ConfigMap volume if configured
	if p.config.KubeDaemonJSONConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "daemon-json",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: p.config.KubeDaemonJSONConfigMap,
					},
				},
			},
		})
	}

	// Runner container volume mounts
	runnerVolumeMounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/runs-fleet/config", ReadOnly: true},
		{Name: "secrets", MountPath: "/etc/runs-fleet/secrets", ReadOnly: true},
		{Name: "workspace", MountPath: "/runner/_work"},
		{Name: "dind-sock", MountPath: "/var/run"},
		{Name: "dind-externals", MountPath: "/home/runner/externals"},
	}

	// DinD container volume mounts
	dindVolumeMounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/runner/_work"},
		{Name: "dind-sock", MountPath: "/var/run"},
		{Name: "dind-externals", MountPath: "/home/runner/externals"},
	}
	if p.config.KubeDaemonJSONConfigMap != "" {
		dindVolumeMounts = append(dindVolumeMounts, corev1.VolumeMount{
			Name:      "daemon-json",
			MountPath: "/etc/docker/daemon.json",
			SubPath:   "daemon.json",
			ReadOnly:  true,
		})
	}

	// Build DinD container args
	dindArgs := []string{
		"dockerd",
		"--host=unix:///var/run/docker.sock",
		"--group=$(DOCKER_GROUP_GID)",
		"--storage-driver=overlay2",
	}
	// Add registry mirror if configured
	if p.config.KubeRegistryMirror != "" {
		dindArgs = append(dindArgs, "--registry-mirror="+p.config.KubeRegistryMirror)
	}

	// Init container copies runner externals to shared volume
	initContainer := corev1.Container{
		Name:            "init-dind-externals",
		Image:           p.config.KubeRunnerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"cp", "-r", "-v", "/home/runner/externals/.", "/home/runner/tmpDir/"},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "dind-externals", MountPath: "/home/runner/tmpDir"},
		},
	}

	// DinD sidecar container - privileged required for Docker daemon.
	// Resource limits match runner container to prevent resource starvation.
	dindContainer := corev1.Container{
		Name:            "dind",
		Image:           p.config.KubeDindImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            dindArgs,
		Env: []corev1.EnvVar{
			{
				Name:  "DOCKER_GROUP_GID",
				Value: fmt.Sprintf("%d", p.config.KubeDockerGroupGID),
			},
		},
		Resources: resources,
		SecurityContext: &corev1.SecurityContext{
			Privileged: ptr.To(true),
		},
		VolumeMounts: dindVolumeMounts,
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   p.config.KubeNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: p.config.KubeServiceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			NodeSelector:       nodeSelector,
			Tolerations:        tolerations,
			InitContainers:     []corev1.Container{initContainer},
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup: ptr.To(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
				// Enable binding to privileged ports (< 1024) for services like nginx
				Sysctls: []corev1.Sysctl{
					{Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"},
				},
			},
			Volumes: volumes,
			Containers: []corev1.Container{
				{
					Name:            "runner",
					Image:           p.config.KubeRunnerImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Resources:       resources,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						ReadOnlyRootFilesystem:   ptr.To(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Env:          envVars,
					VolumeMounts: runnerVolumeMounts,
				},
				dindContainer,
			},
		},
	}

	return pod
}

// buildTolerations creates tolerations for runner pods.
// Includes user-configured tolerations and spot/preemptible node tolerations.
func (p *Provider) buildTolerations(spec *provider.RunnerSpec) []corev1.Toleration {
	var tolerations []corev1.Toleration

	// Add user-configured tolerations first
	for _, t := range p.config.KubeTolerations {
		tol := corev1.Toleration{
			Key:   t.Key,
			Value: t.Value,
		}
		switch t.Operator {
		case "Exists":
			tol.Operator = corev1.TolerationOpExists
		default:
			tol.Operator = corev1.TolerationOpEqual
		}
		switch t.Effect {
		case "NoSchedule":
			tol.Effect = corev1.TaintEffectNoSchedule
		case "PreferNoSchedule":
			tol.Effect = corev1.TaintEffectPreferNoSchedule
		case "NoExecute":
			tol.Effect = corev1.TaintEffectNoExecute
		}
		tolerations = append(tolerations, tol)
	}

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
// Uses spec.CPUCores and spec.MemoryGiB for K8s-native resource management
// with Karpenter. Storage is handled via PVC (not ephemeral storage).
func (p *Provider) getResourceRequirements(spec *provider.RunnerSpec) corev1.ResourceRequirements {
	cpu := spec.CPUCores
	if cpu <= 0 {
		cpu = defaultCPUCores
	}

	mem := spec.MemoryGiB
	if mem <= 0 {
		mem = defaultMemoryGiB
	}

	cpuQuantity := resource.MustParse(fmt.Sprintf("%d", cpu))
	memQuantity := resource.MustParse(fmt.Sprintf("%.1fGi", mem))

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    cpuQuantity,
			corev1.ResourceMemory: memQuantity,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    cpuQuantity,
			corev1.ResourceMemory: memQuantity,
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
