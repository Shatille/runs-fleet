package queue

import (
	"testing"
)

func TestMessage_Structure(t *testing.T) {
	t.Parallel()

	msg := Message{
		ID:     "test-id",
		Body:   "test-body",
		Handle: "test-handle",
		Attributes: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}

	if msg.ID != "test-id" {
		t.Errorf("Message.ID = %q, want %q", msg.ID, "test-id")
	}
	if msg.Body != "test-body" {
		t.Errorf("Message.Body = %q, want %q", msg.Body, "test-body")
	}
	if msg.Handle != "test-handle" {
		t.Errorf("Message.Handle = %q, want %q", msg.Handle, "test-handle")
	}
	if len(msg.Attributes) != 2 {
		t.Errorf("Message.Attributes length = %d, want 2", len(msg.Attributes))
	}
}

func TestJobMessage_Fields(t *testing.T) {
	t.Parallel()

	job := JobMessage{
		JobID:         123,
		RunID:         456,
		Repo:          "owner/repo",
		InstanceType:  "t4g.medium",
		Pool:          "default",
		Spot:          false,
		OriginalLabel: "runs-on: self-hosted",
		RetryCount:    2,
		ForceOnDemand: true,
		Region:        "us-east-1",
		Environment:   "production",
		OS:            "linux",
		Arch:          "arm64",
		InstanceTypes: []string{"t4g.medium", "t4g.large"},
		Traceparent: testTraceparent,
	}

	if job.JobID != 123 {
		t.Errorf("JobMessage.JobID = %d, want %d", job.JobID, 123)
	}
	if job.RunID != 456 {
		t.Errorf("JobMessage.RunID = %d, want %d", job.RunID, 456)
	}
	if job.Repo != "owner/repo" {
		t.Errorf("JobMessage.Repo = %q, want %q", job.Repo, "owner/repo")
	}
	if job.InstanceType != "t4g.medium" {
		t.Errorf("JobMessage.InstanceType = %q, want %q", job.InstanceType, "t4g.medium")
	}
	if job.Pool != "default" {
		t.Errorf("JobMessage.Pool = %q, want %q", job.Pool, "default")
	}
	if job.Spot {
		t.Error("JobMessage.Spot = true, want false")
	}
	if job.OriginalLabel != "runs-on: self-hosted" {
		t.Errorf("JobMessage.OriginalLabel = %q, want %q", job.OriginalLabel, "runs-on: self-hosted")
	}
	if job.RetryCount != 2 {
		t.Errorf("JobMessage.RetryCount = %d, want 2", job.RetryCount)
	}
	if !job.ForceOnDemand {
		t.Error("JobMessage.ForceOnDemand = false, want true")
	}
	if job.Region != "us-east-1" {
		t.Errorf("JobMessage.Region = %q, want %q", job.Region, "us-east-1")
	}
	if job.Environment != "production" {
		t.Errorf("JobMessage.Environment = %q, want %q", job.Environment, "production")
	}
	if job.OS != "linux" {
		t.Errorf("JobMessage.OS = %q, want %q", job.OS, "linux")
	}
	if job.Arch != "arm64" {
		t.Errorf("JobMessage.Arch = %q, want %q", job.Arch, "arm64")
	}
	if len(job.InstanceTypes) != 2 {
		t.Errorf("JobMessage.InstanceTypes length = %d, want 2", len(job.InstanceTypes))
	}
	if job.Traceparent != testTraceparent {
		t.Errorf("JobMessage.Traceparent = %q, want %q", job.Traceparent, testTraceparent)
	}
}
