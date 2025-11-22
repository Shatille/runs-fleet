package queue

import (
	"encoding/json"
	"testing"
)

func TestJobMessage_Marshal(t *testing.T) {
	job := &JobMessage{
		RunID:        "123",
		InstanceType: "t4g.medium",
		Pool:         "default",
		Private:      true,
		Spot:         false,
		RunnerSpec:   "2cpu-linux-arm64",
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	expected := `{"run_id":"123","instance_type":"t4g.medium","pool":"default","private":true,"spot":false,"runner_spec":"2cpu-linux-arm64"}`
	if string(data) != expected {
		t.Errorf("Marshal result = %s, want %s", string(data), expected)
	}
}

func TestJobMessage_Unmarshal(t *testing.T) {
	jsonStr := `{"run_id":"456","instance_type":"c7g.xlarge","private":false,"spot":true,"runner_spec":"4cpu-linux-arm64"}`

	var job JobMessage
	if err := json.Unmarshal([]byte(jsonStr), &job); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.RunID != "456" {
		t.Errorf("RunID = %s, want 456", job.RunID)
	}
	if job.InstanceType != "c7g.xlarge" {
		t.Errorf("InstanceType = %s, want c7g.xlarge", job.InstanceType)
	}
	if job.Pool != "" {
		t.Errorf("Pool = %s, want empty", job.Pool)
	}
	if job.Private != false {
		t.Errorf("Private = %v, want false", job.Private)
	}
	if job.Spot != true {
		t.Errorf("Spot = %v, want true", job.Spot)
	}
}
