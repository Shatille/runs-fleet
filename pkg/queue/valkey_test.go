package queue

import (
	"context"
	"testing"

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
