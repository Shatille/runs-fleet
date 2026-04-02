package db

import "testing"

func TestJobStatusConstants(t *testing.T) {
	tests := []struct {
		status   JobStatus
		expected string
	}{
		{JobStatusRunning, "running"},
		{JobStatusClaiming, "claiming"},
		{JobStatusTerminating, "terminating"},
		{JobStatusRequeued, "requeued"},
		{JobStatusCompleted, "completed"},
		{JobStatusSuccess, "success"},
		{JobStatusFailed, "failed"},
		{JobStatusError, "error"},
		{JobStatusOrphaned, "orphaned"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if string(tt.status) != tt.expected {
				t.Errorf("JobStatus constant = %q, want %q", string(tt.status), tt.expected)
			}
		})
	}
}

func TestJobStatusString(t *testing.T) {
	tests := []struct {
		status   JobStatus
		expected string
	}{
		{JobStatusRunning, "running"},
		{JobStatusClaiming, "claiming"},
		{JobStatusTerminating, "terminating"},
		{JobStatusRequeued, "requeued"},
		{JobStatusCompleted, "completed"},
		{JobStatusSuccess, "success"},
		{JobStatusFailed, "failed"},
		{JobStatusError, "error"},
		{JobStatusOrphaned, "orphaned"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if tt.status.String() != tt.expected {
				t.Errorf("String() = %q, want %q", tt.status.String(), tt.expected)
			}
		})
	}
}
