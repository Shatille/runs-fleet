package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/db"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/google/go-github/v57/github"
)

// fakeDynamoForFailure is a minimal db.DynamoDBAPI for exercising the
// HandleJobFailure requeue path. Only GetItem/UpdateItem carry behavior; the
// rest satisfy the interface.
type fakeDynamoForFailure struct {
	getItem     func() (*dynamodb.GetItemOutput, error)
	getItemSeen bool
}

func (f *fakeDynamoForFailure) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getItemSeen = true
	return f.getItem()
}

func (f *fakeDynamoForFailure) UpdateItem(_ context.Context, _ *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *fakeDynamoForFailure) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return &dynamodb.ScanOutput{}, nil
}

func (f *fakeDynamoForFailure) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func (f *fakeDynamoForFailure) PutItem(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamoForFailure) DeleteItem(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

// MockQueue implements queue.Queue for testing.
type MockQueue struct {
	SendMessageFunc     func(ctx context.Context, job *queue.JobMessage) error
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessageFunc   func(ctx context.Context, handle string) error
	SentMessages        []*queue.JobMessage
}

func (m *MockQueue) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	m.SentMessages = append(m.SentMessages, job)
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, job)
	}
	return nil
}

func (m *MockQueue) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error) {
	if m.ReceiveMessagesFunc != nil {
		return m.ReceiveMessagesFunc(ctx, maxMessages, waitTimeSeconds)
	}
	return nil, nil
}

func (m *MockQueue) DeleteMessage(ctx context.Context, handle string) error {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, handle)
	}
	return nil
}

// MockMetrics implements metrics.Publisher for testing.
type MockMetrics struct {
	metrics.NoopPublisher
	JobQueuedCalled        bool
	QueueDepthCalled       bool
	LastPool               string
	LastRepo               string
	LastCapacity           string
	PublishJobEnqueuedFunc func(ctx context.Context) error
}

func (m *MockMetrics) PublishJobEnqueued(ctx context.Context, pool, _, capacity, repo string) error {
	m.JobQueuedCalled = true
	m.LastPool = pool
	m.LastCapacity = capacity
	m.LastRepo = repo
	if m.PublishJobEnqueuedFunc != nil {
		return m.PublishJobEnqueuedFunc(ctx)
	}
	return nil
}

func (m *MockMetrics) PublishQueueDepth(_ context.Context, _ string, _ float64) error {
	m.QueueDepthCalled = true
	return nil
}

// MockDBClient implements db.Client methods needed for EnsureEphemeralPool testing.
type MockDBClient struct {
	GetPoolConfigFunc       func(ctx context.Context, poolName string) (*db.PoolConfig, error)
	CreateEphemeralPoolFunc func(ctx context.Context, config *db.PoolConfig) error
	TouchPoolActivityFunc   func(ctx context.Context, poolName string) error
	CreatePoolCalled        bool
	TouchActivityCalled     bool
	LastCreatedConfig       *db.PoolConfig
}

func (m *MockDBClient) GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error) {
	if m.GetPoolConfigFunc != nil {
		return m.GetPoolConfigFunc(ctx, poolName)
	}
	return nil, nil
}

func (m *MockDBClient) CreateEphemeralPool(ctx context.Context, config *db.PoolConfig) error {
	m.CreatePoolCalled = true
	m.LastCreatedConfig = config
	if m.CreateEphemeralPoolFunc != nil {
		return m.CreateEphemeralPoolFunc(ctx, config)
	}
	return nil
}

func (m *MockDBClient) TouchPoolActivity(ctx context.Context, poolName string) error {
	m.TouchActivityCalled = true
	if m.TouchPoolActivityFunc != nil {
		return m.TouchPoolActivityFunc(ctx, poolName)
	}
	return nil
}

// TestEnsureEphemeralPool_CreatesNewPool verifies that a new ephemeral pool is created
// with correct configuration when no existing pool is found.
func TestEnsureEphemeralPool_CreatesNewPool(t *testing.T) {
	mockDB := &MockDBClient{
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return nil, nil // No existing pool
		},
	}

	jobConfig := &gh.JobConfig{
		Pool:         "test-ephemeral",
		InstanceType: "c7g.xlarge",
		Arch:         "arm64",
		CPUMin:       4,
		CPUMax:       8,
		RAMMin:       8,
		RAMMax:       16,
		Families:     []string{"c7g", "m7g"},
	}

	err := EnsureEphemeralPool(context.Background(), mockDB, jobConfig)
	if err != nil {
		t.Fatalf("EnsureEphemeralPool() error = %v", err)
	}

	if !mockDB.CreatePoolCalled {
		t.Error("CreateEphemeralPool should be called for new pool")
	}

	cfg := mockDB.LastCreatedConfig
	if cfg == nil {
		t.Fatal("LastCreatedConfig should not be nil")
	}
	if cfg.PoolName != "test-ephemeral" {
		t.Errorf("PoolName = %s, want test-ephemeral", cfg.PoolName)
	}
	if !cfg.Ephemeral {
		t.Error("Ephemeral should be true")
	}
	if cfg.DesiredRunning != 0 {
		t.Errorf("DesiredRunning = %d, want 0 (warm pool)", cfg.DesiredRunning)
	}
	if cfg.DesiredStopped != 1 {
		t.Errorf("DesiredStopped = %d, want 1", cfg.DesiredStopped)
	}
	if cfg.Arch != "arm64" {
		t.Errorf("Arch = %s, want arm64", cfg.Arch)
	}
	if mockDB.TouchActivityCalled {
		t.Error("TouchPoolActivity should NOT be called for new pool")
	}
}

// TestEnsureEphemeralPool_ExistingEphemeral verifies that existing ephemeral pools
// only have their activity touched, not recreated.
func TestEnsureEphemeralPool_ExistingEphemeral_TouchesActivity(t *testing.T) {
	mockDB := &MockDBClient{
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:  "existing-ephemeral",
				Ephemeral: true,
			}, nil
		},
	}

	jobConfig := &gh.JobConfig{
		Pool: "existing-ephemeral",
	}

	err := EnsureEphemeralPool(context.Background(), mockDB, jobConfig)
	if err != nil {
		t.Fatalf("EnsureEphemeralPool() error = %v", err)
	}

	if mockDB.CreatePoolCalled {
		t.Error("CreateEphemeralPool should NOT be called for existing ephemeral pool")
	}
	if !mockDB.TouchActivityCalled {
		t.Error("TouchPoolActivity should be called for existing ephemeral pool")
	}
}

// TestEnsureEphemeralPool_ExistingDeclarative verifies that declarative (non-ephemeral)
// pools are not modified.
func TestEnsureEphemeralPool_ExistingDeclarative_NoOp(t *testing.T) {
	mockDB := &MockDBClient{
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:  "declarative-pool",
				Ephemeral: false, // Declarative pool
			}, nil
		},
	}

	jobConfig := &gh.JobConfig{
		Pool: "declarative-pool",
	}

	err := EnsureEphemeralPool(context.Background(), mockDB, jobConfig)
	if err != nil {
		t.Fatalf("EnsureEphemeralPool() error = %v", err)
	}

	if mockDB.CreatePoolCalled {
		t.Error("CreateEphemeralPool should NOT be called for declarative pool")
	}
	if mockDB.TouchActivityCalled {
		t.Error("TouchPoolActivity should NOT be called for declarative pool")
	}
}

// TestEnsureEphemeralPool_RaceCondition verifies that when pool creation fails
// due to race condition (pool already exists), it falls back to touching activity.
func TestEnsureEphemeralPool_RaceCondition_FallsBackToTouch(t *testing.T) {
	mockDB := &MockDBClient{
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return nil, nil // No existing pool on first check
		},
		CreateEphemeralPoolFunc: func(_ context.Context, _ *db.PoolConfig) error {
			return db.ErrPoolAlreadyExists // Race condition: another instance created it
		},
	}

	jobConfig := &gh.JobConfig{
		Pool: "race-pool",
	}

	err := EnsureEphemeralPool(context.Background(), mockDB, jobConfig)
	if err != nil {
		t.Fatalf("EnsureEphemeralPool() error = %v", err)
	}

	if !mockDB.CreatePoolCalled {
		t.Error("CreateEphemeralPool should be called")
	}
	if !mockDB.TouchActivityCalled {
		t.Error("TouchPoolActivity should be called as fallback on race condition")
	}
}

func TestHandleWorkflowJobQueued(t *testing.T) {
	tests := []struct {
		name    string
		event   *github.WorkflowJobEvent
		sendErr error
		wantMsg bool
		wantErr bool
	}{
		{
			name: "valid runs-fleet job",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64/pool=default"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			wantMsg: true,
			wantErr: false,
		},
		{
			name: "non runs-fleet job",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"self-hosted", "linux"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			wantMsg: false,
			wantErr: false,
		},
		{
			name: "queue send error",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"runs-fleet=67890/cpu=2/arch=arm64"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			sendErr: errors.New("queue error"),
			wantMsg: false,
			wantErr: true,
		},
		{
			name: "job with spot disabled",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64/spot=false"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			wantMsg: true,
			wantErr: false,
		},
		{
			name: "job with pool",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"runs-fleet=67890/cpu=8/arch=arm64/pool=high-mem"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			wantMsg: true,
			wantErr: false,
		},
		{
			name: "job with unknown label is silently ignored",
			event: &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(12345),
					Name:   github.String("test-job"),
					Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64/unknown=value"},
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			},
			wantMsg: true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockQueue := &MockQueue{
				SendMessageFunc: func(_ context.Context, _ *queue.JobMessage) error {
					return tt.sendErr
				},
			}
			mockMetrics := &MockMetrics{}

			msg, err := HandleWorkflowJobQueued(context.Background(), tt.event, mockQueue, nil, nil)

			if (err != nil) != tt.wantErr {
				t.Errorf("HandleWorkflowJobQueued() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantMsg {
				if msg == nil {
					t.Error("HandleWorkflowJobQueued() expected message, got nil")
					return
				}
				if msg.JobID != tt.event.GetWorkflowJob().GetID() {
					t.Errorf("Message JobID = %d, want %d", msg.JobID, tt.event.GetWorkflowJob().GetID())
				}
				if msg.Repo != tt.event.GetRepo().GetFullName() {
					t.Errorf("Message Repo = %s, want %s", msg.Repo, tt.event.GetRepo().GetFullName())
				}
				PublishJobQueuedMetrics(context.Background(), mockMetrics, msg)
				if !mockMetrics.JobQueuedCalled {
					t.Error("Expected PublishJobEnqueued to be called")
				}
				if mockMetrics.LastRepo != msg.Repo {
					t.Errorf("Enqueued repo = %s, want %s", mockMetrics.LastRepo, msg.Repo)
				}
			} else if msg != nil && !tt.wantErr {
				t.Errorf("HandleWorkflowJobQueued() got message when not expected: %v", msg)
			}
		})
	}
}

func TestHandleWorkflowJobQueued_RunIDFromWebhook(t *testing.T) {
	tests := []struct {
		name      string
		labels    []string
		runID     int64
		wantRunID int64
	}{
		{
			name:      "run-id-less new form sources run_id from webhook",
			labels:    []string{"runs-fleet/cpu=4/arch=arm64/pool=default"},
			runID:     987654,
			wantRunID: 987654,
		},
		{
			name:      "marker-only form sources run_id from webhook",
			labels:    []string{"self-hosted", "runs-fleet"},
			runID:     555,
			wantRunID: 555,
		},
		{
			name:      "legacy form prefers webhook run_id over label",
			labels:    []string{"runs-fleet=12345/cpu=4/arch=arm64"},
			runID:     67890,
			wantRunID: 67890,
		},
		{
			name:      "legacy form with non-numeric label run_id is not dropped",
			labels:    []string{"runs-fleet=abc/cpu=4/arch=arm64"},
			runID:     42,
			wantRunID: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &github.WorkflowJobEvent{
				WorkflowJob: &github.WorkflowJob{
					ID:     github.Int64(111),
					RunID:  github.Int64(tt.runID),
					Name:   github.String("test-job"),
					Labels: tt.labels,
				},
				Repo: &github.Repository{
					FullName: github.String("owner/repo"),
				},
			}

			mockQueue := &MockQueue{}

			msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
			if err != nil {
				t.Fatalf("HandleWorkflowJobQueued() unexpected error: %v", err)
			}
			if msg == nil {
				t.Fatal("HandleWorkflowJobQueued() should not drop run-id-less job")
			}
			if msg.RunID != tt.wantRunID {
				t.Errorf("Message RunID = %d, want %d", msg.RunID, tt.wantRunID)
			}
		})
	}
}

func TestBuildRunnerLabel(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "with original label",
			job: &queue.JobMessage{
				RunID:         12345,
				Pool:          "default",
				Spot:          true,
				OriginalLabel: "runs-fleet=12345/cpu=4/arch=arm64",
			},
			want: "runs-fleet=12345/cpu=4/arch=arm64",
		},
		{
			name: "without original label, with pool",
			job: &queue.JobMessage{
				RunID: 12345,
				Pool:  "high-mem",
				Spot:  true,
			},
			want: "runs-fleet=12345/pool=high-mem",
		},
		{
			name: "without original label, no pool, spot enabled",
			job: &queue.JobMessage{
				RunID: 12345,
				Pool:  "",
				Spot:  true,
			},
			want: "runs-fleet=12345",
		},
		{
			name: "without original label, no pool, spot disabled",
			job: &queue.JobMessage{
				RunID: 12345,
				Pool:  "",
				Spot:  false,
			},
			want: "runs-fleet=12345/spot=false",
		},
		{
			name: "with pool and spot disabled",
			job: &queue.JobMessage{
				RunID: 12345,
				Pool:  "default",
				Spot:  false,
			},
			want: "runs-fleet=12345/pool=default/spot=false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("BuildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleWorkflowJobQueued_MetricsPublishErrors(t *testing.T) {
	// Enqueue is durable and runs before the ack; the best-effort metrics run
	// after and must swallow their own errors so they never affect delivery.
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64"},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}
	mockMetrics := &MockMetrics{
		PublishJobEnqueuedFunc: func(_ context.Context) error {
			return errors.New("metrics error")
		},
	}

	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleWorkflowJobQueued() should not fail: %v", err)
	}
	if msg == nil {
		t.Error("HandleWorkflowJobQueued() should return message")
	}

	PublishJobQueuedMetrics(context.Background(), mockMetrics, msg)
	if !mockMetrics.JobQueuedCalled {
		t.Error("PublishJobQueuedMetrics should attempt PublishJobEnqueued despite errors")
	}
}

func TestHandleWorkflowJobQueued_MultipleInstanceTypes(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{"runs-fleet=67890/cpu=4+16/ram=8+32/family=c7g+m7g/arch=arm64"},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}

	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg == nil {
		t.Fatal("HandleWorkflowJobQueued() expected message, got nil")
		return
	}
	if len(msg.InstanceTypes) == 0 {
		t.Error("Expected multiple instance types for flexible spec")
	}
}

func TestHandleWorkflowJobQueued_LabelAlias(t *testing.T) {
	resolver, err := gh.ParseAliasRules(`[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`)
	if err != nil {
		t.Fatalf("ParseAliasRules() unexpected error: %v", err)
	}

	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{"self-hosted", "shared-8cpu-arm64"},
		},
		Repo: &github.Repository{FullName: github.String("owner/repo")},
	}

	// Without a resolver the aliased label is not claimed.
	msg, err := HandleWorkflowJobQueued(context.Background(), event, &MockQueue{}, nil, nil)
	if err != nil {
		t.Fatalf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg != nil {
		t.Fatal("HandleWorkflowJobQueued() should not claim aliased label without a resolver")
	}

	// With the resolver the job is claimed and registered under the original label.
	msg, err = HandleWorkflowJobQueued(context.Background(), event, &MockQueue{}, nil, resolver)
	if err != nil {
		t.Fatalf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg == nil {
		t.Fatal("HandleWorkflowJobQueued() expected message for aliased label, got nil")
	}
	if msg.OriginalLabel != "shared-8cpu-arm64" {
		t.Errorf("OriginalLabel = %q, want %q (runner must register under the requested label)", msg.OriginalLabel, "shared-8cpu-arm64")
	}
	if msg.Arch != gh.ArchARM64 {
		t.Errorf("Arch = %q, want %q", msg.Arch, gh.ArchARM64)
	}
	if msg.CPUMin != 8 || msg.CPUMax != 8 {
		t.Errorf("CPU range = [%d,%d], want [8,8]", msg.CPUMin, msg.CPUMax)
	}
	if len(msg.InstanceTypes) == 0 {
		t.Error("InstanceTypes is empty; alias spec did not resolve instance types")
	}
}

func TestHandleJobFailure_LabelAliasRequeues(t *testing.T) {
	resolver, err := gh.ParseAliasRules(`[{"match":"^shared-(\\d+)cpu-(x64|arm64)$","regex":true,"spec":"cpu=${1}+${1}/arch=${2}"}]`)
	if err != nil {
		t.Fatalf("ParseAliasRules() unexpected error: %v", err)
	}

	item, err := attributevalue.MarshalMap(map[string]any{
		"job_id":      int64(12345),
		"run_id":      int64(67890),
		"repo":        "owner/repo",
		"retry_count": 0,
		"status":      "running",
	})
	if err != nil {
		t.Fatalf("MarshalMap() unexpected error: %v", err)
	}

	newFake := func() *fakeDynamoForFailure {
		return &fakeDynamoForFailure{
			getItem: func() (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: item}, nil
			},
		}
	}

	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String("runs-fleet-i-1234567890abcdef0"),
			Labels:     []string{"self-hosted", "shared-8cpu-arm64"},
		},
	}

	// With a resolver the aliased label passes the parse gate, reaches the DB,
	// and the job is re-queued onto an on-demand instance.
	fake := newFake()
	dbc := db.NewClientWithAPI(fake, "pools", "jobs")
	var sent *queue.JobMessage
	mockQueue := &MockQueue{
		SendMessageFunc: func(_ context.Context, m *queue.JobMessage) error {
			sent = m
			return nil
		},
	}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, dbc, resolver)
	if err != nil {
		t.Fatalf("HandleJobFailure() unexpected error: %v", err)
	}
	if !requeued {
		t.Fatal("HandleJobFailure() should requeue an aliased job when a resolver is configured")
	}
	if !fake.getItemSeen {
		t.Error("HandleJobFailure() should reach the DB lookup for an aliased job")
	}
	if sent == nil || sent.RunID != 67890 || sent.Repo != "owner/repo" || !sent.ForceOnDemand {
		t.Errorf("requeue message = %+v, want RunID=67890, Repo=owner/repo, ForceOnDemand=true", sent)
	}

	// Without a resolver the same aliased label is rejected at the parse gate,
	// before the DB is ever consulted.
	fakeNil := newFake()
	dbcNil := db.NewClientWithAPI(fakeNil, "pools", "jobs")
	requeued, err = HandleJobFailure(context.Background(), event, &MockQueue{}, dbcNil, nil)
	if err != nil {
		t.Fatalf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue an aliased job without a resolver")
	}
	if fakeNil.getItemSeen {
		t.Error("HandleJobFailure() should reject the aliased label before the DB lookup when no resolver is set")
	}
}

func TestHandleWorkflowJobQueued_EmptyLabels(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}

	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg != nil {
		t.Error("HandleWorkflowJobQueued() should return nil for non-runs-fleet job")
	}
}

func TestHandleJobFailure_NoRunner(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String(""),
			Labels:     []string{"runs-fleet=67890/cpu=4"},
		},
	}

	mockQueue := &MockQueue{}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue when no runner assigned")
	}
}

func TestHandleJobFailure_NonRunsFleetRunner(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String("github-hosted-runner"),
			Labels:     []string{"runs-fleet=67890/cpu=4"},
		},
	}

	mockQueue := &MockQueue{}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue for non-runs-fleet runner")
	}
}

func TestHandleJobFailure_ShortRunnerName(t *testing.T) {
	// Test runner name shorter than the prefix
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String("runs"),
			Labels:     []string{"runs-fleet=67890/cpu=4"},
		},
	}

	mockQueue := &MockQueue{}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue for runner with short name")
	}
}

func TestHandleJobFailure_NoDBClient(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String("runs-fleet-i-abc123"),
			Labels:     []string{"runs-fleet=67890/cpu=4"},
		},
	}

	mockQueue := &MockQueue{}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue when DB client is nil")
	}
}

func TestHandleJobFailure_NonRunsFleetLabels(t *testing.T) {
	// Job completed on a runs-fleet runner but without runs-fleet labels
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:         github.Int64(12345),
			RunnerName: github.String("runs-fleet-i-abc123"),
			Labels:     []string{"self-hosted", "linux"},
		},
	}

	mockQueue := &MockQueue{}

	requeued, err := HandleJobFailure(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleJobFailure() unexpected error: %v", err)
	}
	if requeued {
		t.Error("HandleJobFailure() should not requeue for non-runs-fleet labels")
	}
}

func TestHandleWorkflowJobQueued_DiskStorage(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64/disk=100"},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}

	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	if msg == nil {
		t.Fatal("HandleWorkflowJobQueued() expected message, got nil")
		return
	}
	if msg.StorageGiB != 100 {
		t.Errorf("StorageGiB = %d, want 100", msg.StorageGiB)
	}
}

func TestHandleWorkflowJobQueued_InvalidDisk(t *testing.T) {
	event := &github.WorkflowJobEvent{
		WorkflowJob: &github.WorkflowJob{
			ID:     github.Int64(12345),
			Name:   github.String("test-job"),
			Labels: []string{"runs-fleet=67890/cpu=4/arch=arm64/disk=invalid"},
		},
		Repo: &github.Repository{
			FullName: github.String("owner/repo"),
		},
	}

	mockQueue := &MockQueue{}

	msg, err := HandleWorkflowJobQueued(context.Background(), event, mockQueue, nil, nil)
	if err != nil {
		t.Errorf("HandleWorkflowJobQueued() unexpected error: %v", err)
	}
	// Invalid disk label should cause label parsing to fail, treating as non-runs-fleet
	if msg != nil {
		t.Error("HandleWorkflowJobQueued() should return nil for invalid disk label")
	}
}

func TestBuildRunnerLabel_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "zero RunID",
			job: &queue.JobMessage{
				RunID: 0,
				Pool:  "",
				Spot:  true,
			},
			want: "runs-fleet=0",
		},
		{
			name: "negative RunID",
			job: &queue.JobMessage{
				RunID: -1,
				Pool:  "",
				Spot:  true,
			},
			want: "runs-fleet=-1",
		},
		{
			name: "large RunID",
			job: &queue.JobMessage{
				RunID: 9223372036854775807,
				Pool:  "",
				Spot:  true,
			},
			want: "runs-fleet=9223372036854775807",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("BuildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
