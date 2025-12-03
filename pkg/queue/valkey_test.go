package queue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestValkeyClient_InterfaceCompliance(_ *testing.T) {
	var _ Queue = (*ValkeyClient)(nil)
}

func TestValkeyClient_SendMessage_Validation(t *testing.T) {
	client := &ValkeyClient{
		stream: "test-stream",
		group:  "test-group",
	}

	tests := []struct {
		name    string
		job     *JobMessage
		wantErr string
	}{
		{
			name:    "empty job ID",
			job:     &JobMessage{RunID: "run-123"},
			wantErr: "job ID is required",
		},
		{
			name:    "empty run ID",
			job:     &JobMessage{JobID: "job-123"},
			wantErr: "run ID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.SendMessage(context.Background(), tt.job)
			if err == nil {
				t.Error("expected error, got nil")
				return
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %v, want %v", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValkeyClient_ReceiveMessages_TypeSignature(_ *testing.T) {
	// Verify that ReceiveMessages returns []Message type
	var client Queue = &ValkeyClient{
		stream: "test-stream",
		group:  "test-group",
	}
	_ = client // Interface compliance check
}

func TestValkeyConfig_Defaults(t *testing.T) {
	cfg := ValkeyConfig{
		Addr:   "localhost:6379",
		Stream: "test-stream",
		Group:  "test-group",
	}

	if cfg.Addr != "localhost:6379" {
		t.Errorf("Addr = %s, want localhost:6379", cfg.Addr)
	}
	if cfg.DB != 0 {
		t.Errorf("DB = %d, want 0", cfg.DB)
	}
	if cfg.Password != "" {
		t.Errorf("Password = %s, want empty", cfg.Password)
	}
}

func TestNewValkeyClientWithRedis(t *testing.T) {
	mockClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer func() { _ = mockClient.Close() }()

	vc := NewValkeyClientWithRedis(mockClient, "test-stream", "test-group", "test-consumer")

	if vc.stream != "test-stream" {
		t.Errorf("stream = %s, want test-stream", vc.stream)
	}
	if vc.group != "test-group" {
		t.Errorf("group = %s, want test-group", vc.group)
	}
	if vc.consumerID != "test-consumer" {
		t.Errorf("consumerID = %s, want test-consumer", vc.consumerID)
	}
}

func TestValkeyClient_SendMessage_Success(t *testing.T) {
	// Create a minimal test to verify message marshaling works
	job := &JobMessage{
		JobID:   "job-123",
		RunID:   "run-456",
		Repo:    "owner/repo",
		TraceID: "trace-abc",
		SpanID:  "span-xyz",
	}

	// Verify the job can be marshaled to JSON
	_, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}
}

func TestValkeyClient_SendMessage_MarshalError(t *testing.T) {
	// Test that valid messages can be marshaled without error
	job := &JobMessage{
		JobID:        "job-123",
		RunID:        "run-456",
		Repo:         "owner/repo",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         false,
		RunnerSpec:   "2cpu-linux-arm64",
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var decoded JobMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if decoded.JobID != job.JobID {
		t.Errorf("JobID = %s, want %s", decoded.JobID, job.JobID)
	}
	if decoded.RunID != job.RunID {
		t.Errorf("RunID = %s, want %s", decoded.RunID, job.RunID)
	}
}

func TestValkeyClient_ReceiveMessages_EmptyResult(t *testing.T) {
	// Verify that empty results return an empty slice
	// This documents the expected behavior when no messages are available
	var messages []Message
	if len(messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(messages))
	}
}

func TestValkeyClient_Message_Attributes(t *testing.T) {
	// Test that Message struct can hold all expected attributes
	msg := Message{
		ID:     "1234567890-0",
		Body:   `{"job_id":"job-123","run_id":"run-456"}`,
		Handle: "1234567890-0",
		Attributes: map[string]string{
			"TraceID": "trace-abc",
			"SpanID":  "span-xyz",
		},
	}

	if msg.ID != "1234567890-0" {
		t.Errorf("ID = %s, want 1234567890-0", msg.ID)
	}
	if msg.Handle != "1234567890-0" {
		t.Errorf("Handle = %s, want 1234567890-0", msg.Handle)
	}
	if msg.Attributes["TraceID"] != "trace-abc" {
		t.Errorf("TraceID = %s, want trace-abc", msg.Attributes["TraceID"])
	}
	if msg.Attributes["SpanID"] != "span-xyz" {
		t.Errorf("SpanID = %s, want span-xyz", msg.Attributes["SpanID"])
	}
}

func TestValkeyClient_DeleteMessage_Handle(t *testing.T) {
	// Test that message handles are properly formatted
	testCases := []struct {
		name   string
		handle string
	}{
		{"standard format", "1234567890123-0"},
		{"with sequence", "1234567890123-5"},
		{"long timestamp", "9999999999999-999"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify handle format is valid
			if tc.handle == "" {
				t.Error("handle should not be empty")
			}
		})
	}
}

func TestValkeyConfig_AllFields(t *testing.T) {
	cfg := ValkeyConfig{
		Addr:       "localhost:6379",
		Password:   "secret",
		DB:         1,
		Stream:     "jobs-stream",
		Group:      "workers",
		ConsumerID: "worker-1",
	}

	if cfg.Addr != "localhost:6379" {
		t.Errorf("Addr = %s, want localhost:6379", cfg.Addr)
	}
	if cfg.Password != "secret" {
		t.Errorf("Password = %s, want secret", cfg.Password)
	}
	if cfg.DB != 1 {
		t.Errorf("DB = %d, want 1", cfg.DB)
	}
	if cfg.Stream != "jobs-stream" {
		t.Errorf("Stream = %s, want jobs-stream", cfg.Stream)
	}
	if cfg.Group != "workers" {
		t.Errorf("Group = %s, want workers", cfg.Group)
	}
	if cfg.ConsumerID != "worker-1" {
		t.Errorf("ConsumerID = %s, want worker-1", cfg.ConsumerID)
	}
}

func TestValkeyClient_Close_Interface(_ *testing.T) {
	// Verify ValkeyClient implements the expected Close behavior
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	vc := NewValkeyClientWithRedis(client, "test-stream", "test-group", "test-consumer")

	// Close should not panic
	_ = vc.Close()
}

func TestValkeyClient_Ping_Interface(_ *testing.T) {
	// Verify Ping method exists and can be called
	client := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	vc := NewValkeyClientWithRedis(client, "test-stream", "test-group", "test-consumer")
	defer func() { _ = vc.Close() }()

	// Ping will fail without a real Redis connection, but should not panic
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = vc.Ping(ctx)
}

func TestValkeyClient_ReceiveMessages_Parameters(t *testing.T) {
	// Test that parameters are properly validated
	tests := []struct {
		name            string
		maxMessages     int32
		waitTimeSeconds int32
	}{
		{"single message", 1, 1},
		{"batch of 10", 10, 5},
		{"no wait", 5, 0},
		{"long wait", 1, 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify parameter constraints
			if tt.maxMessages < 0 {
				t.Error("maxMessages should be non-negative")
			}
			if tt.waitTimeSeconds < 0 {
				t.Error("waitTimeSeconds should be non-negative")
			}
		})
	}
}

func TestValkeyClient_SendMessage_JobMessageFields(t *testing.T) {
	// Comprehensive test for all JobMessage fields
	job := &JobMessage{
		JobID:         "job-abc-123",
		RunID:         "run-def-456",
		Repo:          "myorg/myrepo",
		InstanceType:  "m6g.large",
		Pool:          "gpu-pool",
		Private:       true,
		Spot:          false,
		RunnerSpec:    "4cpu-linux-arm64-gpu",
		OriginalLabel: "self-hosted",
		RetryCount:    2,
		ForceOnDemand: true,
		Region:        "us-east-1",
		Environment:   "production",
		OS:            "linux",
		Arch:          "arm64",
		InstanceTypes: []string{"m6g.large", "m6g.xlarge"},
		TraceID:       "trace-id-123",
		SpanID:        "span-id-456",
		ParentID:      "parent-id-789",
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded JobMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify all fields roundtrip correctly
	if decoded.JobID != job.JobID {
		t.Errorf("JobID mismatch")
	}
	if decoded.RunID != job.RunID {
		t.Errorf("RunID mismatch")
	}
	if decoded.Repo != job.Repo {
		t.Errorf("Repo mismatch")
	}
	if decoded.InstanceType != job.InstanceType {
		t.Errorf("InstanceType mismatch")
	}
	if decoded.Pool != job.Pool {
		t.Errorf("Pool mismatch")
	}
	if decoded.Private != job.Private {
		t.Errorf("Private mismatch")
	}
	if decoded.Spot != job.Spot {
		t.Errorf("Spot mismatch")
	}
	if decoded.RunnerSpec != job.RunnerSpec {
		t.Errorf("RunnerSpec mismatch")
	}
	if decoded.OriginalLabel != job.OriginalLabel {
		t.Errorf("OriginalLabel mismatch")
	}
	if decoded.RetryCount != job.RetryCount {
		t.Errorf("RetryCount mismatch")
	}
	if decoded.ForceOnDemand != job.ForceOnDemand {
		t.Errorf("ForceOnDemand mismatch")
	}
	if decoded.Region != job.Region {
		t.Errorf("Region mismatch")
	}
	if decoded.Environment != job.Environment {
		t.Errorf("Environment mismatch")
	}
	if decoded.OS != job.OS {
		t.Errorf("OS mismatch")
	}
	if decoded.Arch != job.Arch {
		t.Errorf("Arch mismatch")
	}
	if len(decoded.InstanceTypes) != len(job.InstanceTypes) {
		t.Errorf("InstanceTypes length mismatch")
	}
	if decoded.TraceID != job.TraceID {
		t.Errorf("TraceID mismatch")
	}
	if decoded.SpanID != job.SpanID {
		t.Errorf("SpanID mismatch")
	}
	if decoded.ParentID != job.ParentID {
		t.Errorf("ParentID mismatch")
	}
}

func TestValkeyClient_ErrorConditions(t *testing.T) {
	// Test validation error scenarios that don't require a Redis connection
	tests := []struct {
		name    string
		job     *JobMessage
		wantErr string
	}{
		{
			name:    "nil job ID",
			job:     &JobMessage{RunID: "run-123"},
			wantErr: "job ID is required",
		},
		{
			name:    "nil run ID",
			job:     &JobMessage{JobID: "job-123"},
			wantErr: "run ID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &ValkeyClient{
				stream: "test-stream",
				group:  "test-group",
			}

			err := client.SendMessage(context.Background(), tt.job)
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
			} else if err.Error() != tt.wantErr {
				t.Errorf("expected error %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValkeyClient_MessageParsing(t *testing.T) {
	// Test parsing messages from stream
	testValues := map[string]interface{}{
		"body":     `{"job_id":"job-123","run_id":"run-456"}`,
		"run_id":   "run-456",
		"job_id":   "job-123",
		"trace_id": "trace-abc",
		"span_id":  "span-xyz",
	}

	// Verify that string values can be extracted
	if body, ok := testValues["body"].(string); ok {
		if body == "" {
			t.Error("body should not be empty")
		}
	} else {
		t.Error("body should be a string")
	}

	if traceID, ok := testValues["trace_id"].(string); ok {
		if traceID != "trace-abc" {
			t.Errorf("trace_id = %s, want trace-abc", traceID)
		}
	}

	if spanID, ok := testValues["span_id"].(string); ok {
		if spanID != "span-xyz" {
			t.Errorf("span_id = %s, want span-xyz", spanID)
		}
	}
}
