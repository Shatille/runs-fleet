package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	k8sOrphanPVCAgeThreshold = 10 * time.Minute
	k8sRunnerLabel           = "app=runs-fleet-runner"
)

var k8sCleanupLog = logging.WithComponent(logging.LogTypeHousekeep, "k8s-cleanup")

// K8sCleanupAPI defines the Kubernetes operations needed for orphaned PVC cleanup.
type K8sCleanupAPI interface {
	ListPVCs(ctx context.Context, namespace string, labelSelector string) ([]corev1.PersistentVolumeClaim, error)
	ListPods(ctx context.Context, namespace string, labelSelector string) ([]corev1.Pod, error)
	DeletePVC(ctx context.Context, namespace, name string) error
}

// k8sClientAdapter adapts a kubernetes.Interface to K8sCleanupAPI.
type k8sClientAdapter struct {
	clientset kubernetes.Interface
}

func (a *k8sClientAdapter) ListPVCs(ctx context.Context, namespace string, labelSelector string) ([]corev1.PersistentVolumeClaim, error) {
	pvcList, err := a.clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	return pvcList.Items, nil
}

func (a *k8sClientAdapter) ListPods(ctx context.Context, namespace string, labelSelector string) ([]corev1.Pod, error) {
	podList, err := a.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func (a *k8sClientAdapter) DeletePVC(ctx context.Context, namespace, name string) error {
	return a.clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// NewK8sCleanupClient creates a K8sCleanupAPI from a kubernetes.Interface.
func NewK8sCleanupClient(clientset kubernetes.Interface) K8sCleanupAPI {
	return &k8sClientAdapter{clientset: clientset}
}

// FindOrphanedPVCs returns PVCs labeled as runs-fleet runners that have no matching pod.
// A PVC is considered orphaned when its corresponding pod (derived by stripping the
// "-workspace" suffix) does not exist.
func FindOrphanedPVCs(ctx context.Context, client K8sCleanupAPI, namespace string) ([]corev1.PersistentVolumeClaim, error) {
	pvcs, err := client.ListPVCs(ctx, namespace, k8sRunnerLabel)
	if err != nil {
		return nil, fmt.Errorf("failed to list PVCs: %w", err)
	}

	pods, err := client.ListPods(ctx, namespace, k8sRunnerLabel)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	activePods := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		activePods[pod.Name] = struct{}{}
	}

	var orphaned []corev1.PersistentVolumeClaim
	for _, pvc := range pvcs {
		podName := pvcNameToPodName(pvc.Name)
		if _, exists := activePods[podName]; !exists {
			orphaned = append(orphaned, pvc)
		}
	}

	return orphaned, nil
}

// DeleteOrphanedPVCs finds and deletes orphaned PVCs older than the age threshold.
// Returns the number of PVCs deleted.
func DeleteOrphanedPVCs(ctx context.Context, client K8sCleanupAPI, namespace string, now time.Time) (int, error) {
	orphaned, err := FindOrphanedPVCs(ctx, client, namespace)
	if err != nil {
		return 0, err
	}

	if len(orphaned) == 0 {
		k8sCleanupLog.Debug("no orphaned PVCs found")
		return 0, nil
	}

	threshold := now.Add(-k8sOrphanPVCAgeThreshold)
	var deleted int
	var errs []error

	for _, pvc := range orphaned {
		if pvc.CreationTimestamp.After(threshold) {
			k8sCleanupLog.Debug("skipping young orphaned PVC",
				slog.String("pvc", pvc.Name),
				slog.Duration("age", now.Sub(pvc.CreationTimestamp.Time)))
			continue
		}

		if err := client.DeletePVC(ctx, namespace, pvc.Name); err != nil {
			k8sCleanupLog.Error("failed to delete orphaned PVC",
				slog.String("pvc", pvc.Name),
				slog.String("error", err.Error()))
			errs = append(errs, fmt.Errorf("delete PVC %s: %w", pvc.Name, err))
			continue
		}

		deleted++
		k8sCleanupLog.Info("deleted orphaned PVC",
			slog.String("pvc", pvc.Name),
			slog.Duration("age", now.Sub(pvc.CreationTimestamp.Time)))
	}

	if len(errs) > 0 {
		return deleted, fmt.Errorf("failed to delete %d orphaned PVC(s): %w", len(errs), errors.Join(errs...))
	}
	return deleted, nil
}

// pvcNameToPodName derives the pod name from a PVC name by stripping the "-workspace" suffix.
// PVC naming convention: <pod-name>-workspace (see pvcName in provider/k8s/provider.go).
func pvcNameToPodName(name string) string {
	const suffix = "-workspace"
	if len(name) >= len(suffix) && name[len(name)-len(suffix):] == suffix {
		return name[:len(name)-len(suffix)]
	}
	return name
}
