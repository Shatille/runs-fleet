package housekeeping

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type mockK8sCleanupClient struct {
	pvcs         []corev1.PersistentVolumeClaim
	pods         []corev1.Pod
	listPVCErr   error
	listPodsErr  error
	deletePVCErr error
	deletedPVCs  []string
}

func (m *mockK8sCleanupClient) ListPVCs(_ context.Context, _, _ string) ([]corev1.PersistentVolumeClaim, error) {
	if m.listPVCErr != nil {
		return nil, m.listPVCErr
	}
	return m.pvcs, nil
}

func (m *mockK8sCleanupClient) ListPods(_ context.Context, _, _ string) ([]corev1.Pod, error) {
	if m.listPodsErr != nil {
		return nil, m.listPodsErr
	}
	return m.pods, nil
}

func (m *mockK8sCleanupClient) DeletePVC(_ context.Context, _, name string) error {
	if m.deletePVCErr != nil {
		return m.deletePVCErr
	}
	m.deletedPVCs = append(m.deletedPVCs, name)
	return nil
}

func TestFindOrphanedPVCs_OrphanedPVCNoMatchingPod(t *testing.T) {
	t.Parallel()

	client := &mockK8sCleanupClient{
		pvcs: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100-workspace"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-200-workspace"}},
		},
		pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100"}},
		},
	}

	orphaned, err := FindOrphanedPVCs(context.Background(), client, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned PVC, got %d", len(orphaned))
	}
	if orphaned[0].Name != "runner-200-workspace" {
		t.Errorf("expected orphaned PVC runner-200-workspace, got %s", orphaned[0].Name)
	}
}

func TestFindOrphanedPVCs_SkipsPVCsWithMatchingPods(t *testing.T) {
	t.Parallel()

	client := &mockK8sCleanupClient{
		pvcs: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100-workspace"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-200-workspace"}},
		},
		pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-200"}},
		},
	}

	orphaned, err := FindOrphanedPVCs(context.Background(), client, "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(orphaned) != 0 {
		t.Errorf("expected 0 orphaned PVCs, got %d", len(orphaned))
	}
}

func TestDeleteOrphanedPVCs_RespectsAgeThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now()
	client := &mockK8sCleanupClient{
		pvcs: []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "runner-old-workspace",
					CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Minute)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "runner-young-workspace",
					CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)),
				},
			},
		},
		pods: []corev1.Pod{},
	}

	deleted, err := DeleteOrphanedPVCs(context.Background(), client, "test-ns", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if deleted != 1 {
		t.Errorf("expected 1 deletion, got %d", deleted)
	}
	if len(client.deletedPVCs) != 1 || client.deletedPVCs[0] != "runner-old-workspace" {
		t.Errorf("expected runner-old-workspace deleted, got %v", client.deletedPVCs)
	}
}

func TestDeleteOrphanedPVCs_NoOrphans(t *testing.T) {
	t.Parallel()

	client := &mockK8sCleanupClient{
		pvcs: []corev1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100-workspace"}},
		},
		pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "runner-100"}},
		},
	}

	deleted, err := DeleteOrphanedPVCs(context.Background(), client, "test-ns", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions, got %d", deleted)
	}
}

func TestFindOrphanedPVCs_ListPVCError(t *testing.T) {
	t.Parallel()

	client := &mockK8sCleanupClient{
		listPVCErr: errors.New("api unavailable"),
	}

	_, err := FindOrphanedPVCs(context.Background(), client, "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, client.listPVCErr) {
		t.Errorf("expected wrapped api error, got: %v", err)
	}
}

func TestFindOrphanedPVCs_ListPodsError(t *testing.T) {
	t.Parallel()

	client := &mockK8sCleanupClient{
		pvcs:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "runner-100-workspace"}}},
		listPodsErr: errors.New("api unavailable"),
	}

	_, err := FindOrphanedPVCs(context.Background(), client, "test-ns")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, client.listPodsErr) {
		t.Errorf("expected wrapped api error, got: %v", err)
	}
}

func TestDeleteOrphanedPVCs_DeleteError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	client := &mockK8sCleanupClient{
		pvcs: []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "runner-fail-workspace",
					CreationTimestamp: metav1.NewTime(now.Add(-20 * time.Minute)),
				},
			},
		},
		pods:         []corev1.Pod{},
		deletePVCErr: errors.New("delete denied"),
	}

	deleted, err := DeleteOrphanedPVCs(context.Background(), client, "test-ns", now)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions, got %d", deleted)
	}
}

func TestPvcNameToPodName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"runner-123-workspace", "runner-123"},
		{"runner-abc-workspace", "runner-abc"},
		{"no-suffix", "no-suffix"},
		{"workspace", "workspace"},
		{"-workspace", ""},
	}

	for _, tt := range tests {
		if got := pvcNameToPodName(tt.input); got != tt.want {
			t.Errorf("pvcNameToPodName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
