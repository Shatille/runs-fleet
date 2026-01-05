package queue

import (
	"testing"
)

func TestExtractTraceContext(t *testing.T) {
	tests := []struct {
		name         string
		msg          Message
		wantTraceID  string
		wantSpanID   string
		wantParentID string
	}{
		{
			name: "all trace context present",
			msg: Message{
				ID:     "msg-1",
				Body:   `{"run_id":"123"}`,
				Handle: "handle-1",
				Attributes: map[string]string{
					"TraceID":  "trace-abc123",
					"SpanID":   "span-def456",
					"ParentID": "parent-ghi789",
				},
			},
			wantTraceID:  "trace-abc123",
			wantSpanID:   "span-def456",
			wantParentID: "parent-ghi789",
		},
		{
			name: "nil attributes",
			msg: Message{
				ID:         "msg-2",
				Body:       `{"run_id":"456"}`,
				Handle:     "handle-2",
				Attributes: nil,
			},
			wantTraceID:  "",
			wantSpanID:   "",
			wantParentID: "",
		},
		{
			name: "empty attributes",
			msg: Message{
				ID:         "msg-3",
				Body:       `{"run_id":"789"}`,
				Handle:     "handle-3",
				Attributes: map[string]string{},
			},
			wantTraceID:  "",
			wantSpanID:   "",
			wantParentID: "",
		},
		{
			name: "partial trace context - only trace ID",
			msg: Message{
				ID:     "msg-4",
				Body:   `{"run_id":"101"}`,
				Handle: "handle-4",
				Attributes: map[string]string{
					"TraceID": "trace-only",
				},
			},
			wantTraceID:  "trace-only",
			wantSpanID:   "",
			wantParentID: "",
		},
		{
			name: "partial trace context - only span ID",
			msg: Message{
				ID:     "msg-5",
				Body:   `{"run_id":"102"}`,
				Handle: "handle-5",
				Attributes: map[string]string{
					"SpanID": "span-only",
				},
			},
			wantTraceID:  "",
			wantSpanID:   "span-only",
			wantParentID: "",
		},
		{
			name: "partial trace context - only parent ID",
			msg: Message{
				ID:     "msg-6",
				Body:   `{"run_id":"103"}`,
				Handle: "handle-6",
				Attributes: map[string]string{
					"ParentID": "parent-only",
				},
			},
			wantTraceID:  "",
			wantSpanID:   "",
			wantParentID: "parent-only",
		},
		{
			name: "with extra attributes",
			msg: Message{
				ID:     "msg-7",
				Body:   `{"run_id":"104"}`,
				Handle: "handle-7",
				Attributes: map[string]string{
					"TraceID":       "trace-with-extras",
					"SpanID":        "span-with-extras",
					"ParentID":      "parent-with-extras",
					"CustomAttr":    "custom-value",
					"AnotherAttr":   "another-value",
				},
			},
			wantTraceID:  "trace-with-extras",
			wantSpanID:   "span-with-extras",
			wantParentID: "parent-with-extras",
		},
		{
			name: "case sensitive attribute keys",
			msg: Message{
				ID:     "msg-8",
				Body:   `{"run_id":"105"}`,
				Handle: "handle-8",
				Attributes: map[string]string{
					"traceid":  "lowercase-trace",
					"spanid":   "lowercase-span",
					"parentid": "lowercase-parent",
				},
			},
			wantTraceID:  "",
			wantSpanID:   "",
			wantParentID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTraceID, gotSpanID, gotParentID := ExtractTraceContext(tt.msg)

			if gotTraceID != tt.wantTraceID {
				t.Errorf("ExtractTraceContext() traceID = %q, want %q", gotTraceID, tt.wantTraceID)
			}
			if gotSpanID != tt.wantSpanID {
				t.Errorf("ExtractTraceContext() spanID = %q, want %q", gotSpanID, tt.wantSpanID)
			}
			if gotParentID != tt.wantParentID {
				t.Errorf("ExtractTraceContext() parentID = %q, want %q", gotParentID, tt.wantParentID)
			}
		})
	}
}

func TestMessage_Structure(t *testing.T) {
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
		TraceID:       "trace-xyz",
		SpanID:        "span-abc",
		ParentID:      "parent-def",
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
	if job.TraceID != "trace-xyz" {
		t.Errorf("JobMessage.TraceID = %q, want %q", job.TraceID, "trace-xyz")
	}
	if job.SpanID != "span-abc" {
		t.Errorf("JobMessage.SpanID = %q, want %q", job.SpanID, "span-abc")
	}
	if job.ParentID != "parent-def" {
		t.Errorf("JobMessage.ParentID = %q, want %q", job.ParentID, "parent-def")
	}
}

func TestJobMessage_ZeroValues(t *testing.T) {
	// Test JobMessage with all zero values
	job := JobMessage{}

	if job.JobID != 0 {
		t.Errorf("JobMessage.JobID = %d, want 0", job.JobID)
	}
	if job.RunID != 0 {
		t.Errorf("JobMessage.RunID = %d, want 0", job.RunID)
	}
	if job.Repo != "" {
		t.Errorf("JobMessage.Repo = %q, want empty", job.Repo)
	}
	if job.InstanceType != "" {
		t.Errorf("JobMessage.InstanceType = %q, want empty", job.InstanceType)
	}
	if job.Pool != "" {
		t.Errorf("JobMessage.Pool = %q, want empty", job.Pool)
	}
	if job.Spot {
		t.Error("JobMessage.Spot default should be false")
	}
	if job.RetryCount != 0 {
		t.Errorf("JobMessage.RetryCount = %d, want 0", job.RetryCount)
	}
	if job.ForceOnDemand {
		t.Error("JobMessage.ForceOnDemand default should be false")
	}
	if len(job.InstanceTypes) != 0 {
		t.Errorf("JobMessage.InstanceTypes should be empty, got %d", len(job.InstanceTypes))
	}
	if job.StorageGiB != 0 {
		t.Errorf("JobMessage.StorageGiB = %d, want 0", job.StorageGiB)
	}
}

func TestJobMessage_StorageGiB(t *testing.T) {
	tests := []struct {
		name       string
		storageGiB int
	}{
		{"zero storage", 0},
		{"minimum storage", 1},
		{"typical storage", 100},
		{"large storage", 16384},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := JobMessage{
				JobID:      123,
				RunID:      456,
				StorageGiB: tt.storageGiB,
			}
			if job.StorageGiB != tt.storageGiB {
				t.Errorf("JobMessage.StorageGiB = %d, want %d", job.StorageGiB, tt.storageGiB)
			}
		})
	}
}

func TestMessage_EmptyFields(t *testing.T) {
	// Test with empty string fields
	msg := Message{
		ID:         "",
		Body:       "",
		Handle:     "",
		Attributes: nil,
	}

	if msg.ID != "" {
		t.Errorf("Message.ID = %q, want empty", msg.ID)
	}
	if msg.Body != "" {
		t.Errorf("Message.Body = %q, want empty", msg.Body)
	}
	if msg.Handle != "" {
		t.Errorf("Message.Handle = %q, want empty", msg.Handle)
	}
	if msg.Attributes != nil {
		t.Error("Message.Attributes should be nil")
	}
}

func TestExtractTraceContext_EmptyStringValues(t *testing.T) {
	// Test with attributes present but with empty string values
	msg := Message{
		ID:     "msg-1",
		Body:   `{"run_id":"123"}`,
		Handle: "handle-1",
		Attributes: map[string]string{
			"TraceID":  "",
			"SpanID":   "",
			"ParentID": "",
		},
	}

	traceID, spanID, parentID := ExtractTraceContext(msg)

	if traceID != "" {
		t.Errorf("ExtractTraceContext() traceID = %q, want empty", traceID)
	}
	if spanID != "" {
		t.Errorf("ExtractTraceContext() spanID = %q, want empty", spanID)
	}
	if parentID != "" {
		t.Errorf("ExtractTraceContext() parentID = %q, want empty", parentID)
	}
}

func TestJobMessage_LargeValues(t *testing.T) {
	// Test JobMessage with max values
	job := JobMessage{
		JobID:         9223372036854775807, // Max int64
		RunID:         9223372036854775807,
		RetryCount:    2147483647, // Max int
		StorageGiB:    16384,
		InstanceTypes: make([]string, 100), // Many instance types
	}

	if job.JobID != 9223372036854775807 {
		t.Error("JobMessage.JobID should handle max int64")
	}
	if job.RunID != 9223372036854775807 {
		t.Error("JobMessage.RunID should handle max int64")
	}
	if len(job.InstanceTypes) != 100 {
		t.Errorf("JobMessage.InstanceTypes length = %d, want 100", len(job.InstanceTypes))
	}
}
