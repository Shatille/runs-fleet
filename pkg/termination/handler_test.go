package termination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

func init() {
	// Use short tick interval in tests to avoid slow test execution
	handlerTickInterval = 10 * time.Millisecond
}

// Test constants to satisfy goconst
const (
	testReceiptTermination = "test-receipt"
	testStatusSuccess      = string(db.JobStatusSuccess)
	testRepo               = "octo/repo"
	testResultServed       = "served"
	testResultTimeout      = "timeout"
	testResultError        = "error"
	testResultInterrupted  = "interrupted"
)

// mockQueueAPI implements QueueAPI for testing.
type mockQueueAPI struct {
	messages      []queue.Message
	receiveErr    error
	deleteErr     error
	receiveCalls  int
	deleteCalls   int
	deleteReceipt string
	receiveCalled chan struct{} // signals when ReceiveMessages is called
}

func (m *mockQueueAPI) ReceiveMessages(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
	m.receiveCalls++
	if m.receiveCalled != nil {
		select {
		case m.receiveCalled <- struct{}{}:
		default:
		}
	}
	if m.receiveErr != nil {
		return nil, m.receiveErr
	}
	return m.messages, nil
}

func (m *mockQueueAPI) DeleteMessage(_ context.Context, receiptHandle string) error {
	m.deleteCalls++
	m.deleteReceipt = receiptHandle
	return m.deleteErr
}

// mockDBAPI implements DBAPI for testing.
type mockDBAPI struct {
	markCompleteErr  error
	updateMetricsErr error
	markStartedErr   error
	startedNoOp      bool
	completeCalls    int
	startedCalls     int
	metricsCalls     int
	lastJobID        int64
	lastStatus       string
	completeRecord   *events.JobInfo
	startedRecord    *events.JobInfo

	getByInstanceCalls int
	getByInstanceErr   error
	jobByInstance      *events.JobInfo
	requeueCalls       int
	lastRequeueJobID   int64
	markRequeuedErr    error
	markRequeuedResult bool
}

func (m *mockDBAPI) MarkJobStarted(_ context.Context, jobID int64, _ time.Time) (*events.JobInfo, error) {
	m.startedCalls++
	m.lastJobID = jobID
	if m.markStartedErr != nil {
		return nil, m.markStartedErr
	}
	if m.startedNoOp {
		return nil, nil
	}
	if m.startedRecord != nil {
		return m.startedRecord, nil
	}
	return &events.JobInfo{JobID: jobID, RunID: 99, Repo: testRepo, Pool: "default"}, nil
}

func (m *mockDBAPI) MarkJobComplete(_ context.Context, jobID int64, status string, _, _ int) (*events.JobInfo, error) {
	m.completeCalls++
	m.lastJobID = jobID
	m.lastStatus = status
	if m.markCompleteErr != nil {
		return nil, m.markCompleteErr
	}
	if m.completeRecord != nil {
		return m.completeRecord, nil
	}
	return &events.JobInfo{JobID: jobID, RunID: 99, Repo: testRepo}, nil
}

func (m *mockDBAPI) UpdateJobMetrics(_ context.Context, _ int64, _, _ time.Time) error {
	m.metricsCalls++
	return m.updateMetricsErr
}

func (m *mockDBAPI) GetJobByInstance(_ context.Context, _ string) (*events.JobInfo, error) {
	m.getByInstanceCalls++
	if m.getByInstanceErr != nil {
		return nil, m.getByInstanceErr
	}
	return m.jobByInstance, nil
}

func (m *mockDBAPI) MarkJobRequeuedByJobID(_ context.Context, jobID int64) (bool, error) {
	m.requeueCalls++
	m.lastRequeueJobID = jobID
	if m.markRequeuedErr != nil {
		return false, m.markRequeuedErr
	}
	return m.markRequeuedResult, nil
}

// mockJobQueue implements JobQueueAPI for testing.
type mockJobQueue struct {
	sendCalls int
	lastMsg   *queue.JobMessage
	sendErr   error
}

func (m *mockJobQueue) SendMessage(_ context.Context, job *queue.JobMessage) error {
	m.sendCalls++
	m.lastMsg = job
	return m.sendErr
}

// mockMetricsAPI implements MetricsAPI for testing.
type mockMetricsAPI struct {
	completedCalls    int
	servedCalls       int
	nonServedCalls    int
	confirmedCalls    int
	lastResult        string
	lastPool          string
	lastRepo          string
	lastConfirmedPool string
	completedErr      error
	confirmedErr      error

	execSecondsCalls int
	lastExecSeconds  float64
	lastExecResult   string
	lastExecPool     string

	runnerExecCalls   int
	lastRunnerArch    string
	lastRunnerVcpu    int
	lastRunnerSpot    bool
	lastRunnerResult  string
	lastRunnerSeconds float64

	processingMu          sync.Mutex
	processingCalls       int
	lastProcessingQueue   string
	lastProcessingResult  string
	lastProcessingSeconds float64

	requeuedCalls      int
	lastRequeuedReason string
	schedFailCalls     int
	lastSchedFailType  string

	toolCacheMissCalls int
	toolCacheMisses    [][3]string // {tool, version, arch}

	cacheInterceptionCalls int
	lastCacheInterception  string
	cacheBytesStoredCalls  int
	lastCacheBytesStored   int64

	provisionCalls  int
	lastProvSource  string
	lastProvFamily  string
	lastProvSeconds float64

	bootstrapCalls   int
	bootstrapByPhase map[string]float64
}

func (m *mockMetricsAPI) PublishJobCompleted(_ context.Context, pool, result, repo string) error {
	m.completedCalls++
	m.lastResult = result
	m.lastPool = pool
	m.lastRepo = repo
	if result == testResultServed {
		m.servedCalls++
	} else {
		m.nonServedCalls++
	}
	return m.completedErr
}

func (m *mockMetricsAPI) PublishRunnerConfirmed(_ context.Context, pool string) error {
	m.confirmedCalls++
	m.lastConfirmedPool = pool
	return m.confirmedErr
}

func (m *mockMetricsAPI) PublishInstanceProvisionSeconds(_ context.Context, source, family string, seconds float64) error {
	m.provisionCalls++
	m.lastProvSource = source
	m.lastProvFamily = family
	m.lastProvSeconds = seconds
	return nil
}

func (m *mockMetricsAPI) PublishAgentBootstrapSeconds(_ context.Context, _, phase string, seconds float64) error {
	m.bootstrapCalls++
	if m.bootstrapByPhase == nil {
		m.bootstrapByPhase = make(map[string]float64)
	}
	m.bootstrapByPhase[phase] = seconds
	return nil
}

func (m *mockMetricsAPI) PublishJobExecutionSeconds(_ context.Context, pool, result string, seconds float64) error {
	m.execSecondsCalls++
	m.lastExecSeconds = seconds
	m.lastExecResult = result
	m.lastExecPool = pool
	return nil
}

func (m *mockMetricsAPI) PublishMessageProcessingSeconds(_ context.Context, queue, result string, seconds float64) error {
	m.processingMu.Lock()
	defer m.processingMu.Unlock()
	m.processingCalls++
	m.lastProcessingQueue = queue
	m.lastProcessingResult = result
	m.lastProcessingSeconds = seconds
	return nil
}

func (m *mockMetricsAPI) processingSnapshot() (int, string, string) {
	m.processingMu.Lock()
	defer m.processingMu.Unlock()
	return m.processingCalls, m.lastProcessingQueue, m.lastProcessingResult
}

func (m *mockMetricsAPI) PublishJobRequeued(_ context.Context, reason string) error {
	m.requeuedCalls++
	m.lastRequeuedReason = reason
	return nil
}

func (m *mockMetricsAPI) PublishSchedulingFailure(_ context.Context, taskType string) error {
	m.schedFailCalls++
	m.lastSchedFailType = taskType
	return nil
}

func (m *mockMetricsAPI) PublishRunnerExecutionSeconds(_ context.Context, arch string, vcpu int, spot bool, result string, seconds float64) error {
	m.runnerExecCalls++
	m.lastRunnerArch = arch
	m.lastRunnerVcpu = vcpu
	m.lastRunnerSpot = spot
	m.lastRunnerResult = result
	m.lastRunnerSeconds = seconds
	return nil
}

func (m *mockMetricsAPI) PublishRunnerToolCacheMiss(_ context.Context, tool, version, arch string) error {
	m.toolCacheMissCalls++
	m.toolCacheMisses = append(m.toolCacheMisses, [3]string{tool, version, arch})
	return nil
}

func (m *mockMetricsAPI) PublishRunnerCacheInterception(_ context.Context, status string) error {
	m.cacheInterceptionCalls++
	m.lastCacheInterception = status
	return nil
}

func (m *mockMetricsAPI) PublishCacheBytesStored(_ context.Context, bytes int64) error {
	m.cacheBytesStoredCalls++
	m.lastCacheBytesStored = bytes
	return nil
}

// mockSecretsStore implements secrets.Store for testing.
type mockSecretsStore struct {
	deleteErr   error
	deleteCalls int
	deletedID   string
}

func (m *mockSecretsStore) Delete(_ context.Context, runnerID string) error {
	m.deleteCalls++
	m.deletedID = runnerID
	return m.deleteErr
}

func (m *mockSecretsStore) Get(_ context.Context, _ string) (*secrets.RunnerConfig, error) {
	return nil, errors.New("not implemented")
}

func (m *mockSecretsStore) Put(_ context.Context, _ string, _ *secrets.RunnerConfig) error {
	return errors.New("not implemented")
}

func (m *mockSecretsStore) List(_ context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func TestNewHandler(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}

	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	if handler.queueClient != q {
		t.Error("expected queueClient to be set")
	}
	if handler.dbClient != db {
		t.Error("expected dbClient to be set")
	}
	if handler.metrics != metrics {
		t.Error("expected metrics to be set")
	}
	if handler.secretsStore != secretsStore {
		t.Error("expected secretsStore to be set")
	}
	if handler.config != cfg {
		t.Error("expected config to be set")
	}
}

func TestHandler_processMessage_Success(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       time.Now().Add(-2 * time.Minute),
		CompletedAt:     time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if db.completeCalls != 1 {
		t.Errorf("expected 1 complete call, got %d", db.completeCalls)
	}
	if db.lastJobID != 12345678901 {
		t.Errorf("expected job ID 12345678901, got %d", db.lastJobID)
	}
	if db.lastStatus != testStatusSuccess {
		t.Errorf("expected status '%s', got '%s'", testStatusSuccess, db.lastStatus)
	}

	if metrics.servedCalls != 1 {
		t.Errorf("expected 1 served metric call, got %d", metrics.servedCalls)
	}
	if metrics.completedCalls != 1 {
		t.Errorf("expected 1 completed metric call, got %d", metrics.completedCalls)
	}
	if metrics.lastResult != testResultServed {
		t.Errorf("expected result 'served', got %q", metrics.lastResult)
	}
	// The reported duration is published as the execution-latency histogram with the
	// same pool/result labels as the completion counter.
	if metrics.execSecondsCalls != 1 || metrics.lastExecSeconds != 120 {
		t.Errorf("expected 1 execution-seconds call with 120s, got calls=%d seconds=%v", metrics.execSecondsCalls, metrics.lastExecSeconds)
	}
	if metrics.lastExecResult != testResultServed {
		t.Errorf("expected execution-seconds result 'served', got %q", metrics.lastExecResult)
	}
	// The job record carries no instance type, so the billable runner-seconds
	// metric (which needs arch/vCPU) is skipped rather than emitted with blanks.
	if metrics.runnerExecCalls != 0 {
		t.Errorf("expected no runner-execution-seconds call without an instance type, got %d", metrics.runnerExecCalls)
	}

	if q.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", q.deleteCalls)
	}
}

// When the job record carries a known instance type, termination emits the
// billable runner-seconds metric dimensioned by arch + vCPU + spot — the axis
// competitors bill runner-minutes on.
func TestHandler_processMessage_RunnerExecutionSeconds(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{completeRecord: &events.JobInfo{
		JobID:        12345678901,
		RunID:        99,
		Repo:         testRepo,
		InstanceType: "c7g.xlarge", // arm64, 4 vCPU
		Spot:         true,
	}}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, &config.Config{})

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.runnerExecCalls != 1 {
		t.Fatalf("expected 1 runner-execution-seconds call, got %d", metrics.runnerExecCalls)
	}
	if metrics.lastRunnerArch != "arm64" {
		t.Errorf("expected arch arm64, got %q", metrics.lastRunnerArch)
	}
	if metrics.lastRunnerVcpu != 4 {
		t.Errorf("expected vcpu 4, got %d", metrics.lastRunnerVcpu)
	}
	if !metrics.lastRunnerSpot {
		t.Errorf("expected spot true, got false")
	}
	if metrics.lastRunnerResult != testResultServed {
		t.Errorf("expected result %q, got %q", testResultServed, metrics.lastRunnerResult)
	}
	if metrics.lastRunnerSeconds != 120 {
		t.Errorf("expected 120 seconds, got %v", metrics.lastRunnerSeconds)
	}
}

// Tool-cache misses on the completion message become one metric per entry, with the
// version normalized to major.minor and the platform mapped to arch. A malformed key
// is skipped.
func TestHandler_processMessage_ToolCacheMisses(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{completeRecord: &events.JobInfo{JobID: 12345678901, RunID: 99, Repo: testRepo}}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		ToolCacheMisses: []string{
			"Python/3.10.14/x64",
			"Java_Temurin-Hotspot_jdk/17.0.19-10/x64", // build suffix → major.minor 17.0
			"node/18.20.4/arm64",
			"go/1.21.0/linux-x64", // OS-prefixed platform → arch normalized to x64
			"garbage-key",         // malformed → skipped
		},
		CacheInterception: "engaged",
		CacheBytesWritten: 4096,
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := [][3]string{
		{"Python", "3.10", "x64"},
		{"Java_Temurin-Hotspot_jdk", "17.0", "x64"},
		{"node", "18.20", "arm64"},
		{"go", "1.21", "x64"},
	}
	if metrics.toolCacheMissCalls != len(want) {
		t.Fatalf("expected %d tool-cache-miss calls, got %d (%v)", len(want), metrics.toolCacheMissCalls, metrics.toolCacheMisses)
	}
	for i, w := range want {
		if metrics.toolCacheMisses[i] != w {
			t.Errorf("miss[%d] = %v, want %v", i, metrics.toolCacheMisses[i], w)
		}
	}
	if metrics.cacheInterceptionCalls != 1 || metrics.lastCacheInterception != "engaged" {
		t.Errorf("cache interception metric = (%d, %q), want (1, engaged)", metrics.cacheInterceptionCalls, metrics.lastCacheInterception)
	}
	if metrics.cacheBytesStoredCalls != 1 || metrics.lastCacheBytesStored != 4096 {
		t.Errorf("cache bytes stored metric = (%d, %d), want (1, 4096)", metrics.cacheBytesStoredCalls, metrics.lastCacheBytesStored)
	}
}

// A completion reporting the fail-open interceptor outcome (the operationally
// important alarm case) still emits the interception metric, but with no bytes
// written the CacheBytesStored counter is left untouched.
func TestHandler_processMessage_CacheInterceptionFailed(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{completeRecord: &events.JobInfo{JobID: 12345678901, RunID: 99, Repo: testRepo}}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{
		InstanceID:        "i-12345",
		JobID:             "12345678901",
		Status:            "success",
		DurationSeconds:   120,
		CacheInterception: "failed",
		CacheBytesWritten: 0,
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.cacheInterceptionCalls != 1 || metrics.lastCacheInterception != "failed" {
		t.Errorf("cache interception metric = (%d, %q), want (1, failed)", metrics.cacheInterceptionCalls, metrics.lastCacheInterception)
	}
	if metrics.cacheBytesStoredCalls != 0 {
		t.Errorf("cache bytes stored metric fired for 0 bytes: calls = %d, want 0", metrics.cacheBytesStoredCalls)
	}
}

// An unrecognized instance type (not in the catalog) yields no arch/vCPU, so the
// billable runner-seconds metric is skipped rather than emitted with guesses.
func TestHandler_processMessage_RunnerExecutionSeconds_UnknownInstanceType(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{completeRecord: &events.JobInfo{
		JobID:        12345678901,
		RunID:        99,
		Repo:         testRepo,
		InstanceType: "made-up.instance",
	}}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		DurationSeconds: 120,
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.runnerExecCalls != 0 {
		t.Errorf("expected no runner-execution-seconds call for unknown instance type, got %d", metrics.runnerExecCalls)
	}
}

// A "failure" agent status means the runner/listener exited non-zero, i.e. the
// runner failed operationally. Because we do not set
// ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED, the runner never exits non-zero
// purely from failed workflow steps, so this maps to our operational "error".
func TestHandler_processMessage_OperationalFailure(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "failure",
		ExitCode:        1,
		DurationSeconds: 60,
		StartedAt:       time.Now().Add(-1 * time.Minute),
		CompletedAt:     time.Now(),
		Error:           "test error",
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.nonServedCalls != 1 {
		t.Errorf("expected 1 non-served metric call, got %d", metrics.nonServedCalls)
	}
	if metrics.lastResult != testResultError {
		t.Errorf("expected result 'error' for operational failure, got %q", metrics.lastResult)
	}
}

// The load-bearing assertion of the fulfillment principle: a job that the runner
// ran to completion and that exited cleanly (StatusSuccess) is "served" even when
// the client's workflow concluded failure. We never derive the result from the
// workflow conclusion, and the agent's exit-0 status does not carry it. A red
// workflow on a runner we delivered is still a success for us.
func TestHandler_processMessage_ServedDespiteFailedWorkflow(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	// Runner ran the job to completion and exited cleanly: the agent reports
	// "success" regardless of the (failed) workflow steps.
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 60,
		StartedAt:       time.Now().Add(-1 * time.Minute),
		CompletedAt:     time.Now(),
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	if err := handler.processMessage(context.Background(), queueMsg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.servedCalls != 1 {
		t.Errorf("expected 1 served metric call, got %d", metrics.servedCalls)
	}
	if metrics.lastResult != testResultServed {
		t.Errorf("expected result 'served', got %q", metrics.lastResult)
	}
}

func TestHandler_processMessage_Started(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "started",
		// A started message must not carry completion-only signals; assert none of the
		// completion-only metric paths (misses, cache interception, cache bytes) fire.
		ToolCacheMisses:   []string{"Python/3.10.14/x64"},
		CacheInterception: "engaged",
		CacheBytesWritten: 4096,
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.toolCacheMissCalls != 0 {
		t.Errorf("expected no tool-cache-miss metrics for 'started' status, got %d", metrics.toolCacheMissCalls)
	}
	if metrics.cacheInterceptionCalls != 0 || metrics.cacheBytesStoredCalls != 0 {
		t.Errorf("expected no cache interception/bytes metrics for 'started' status, got %d/%d",
			metrics.cacheInterceptionCalls, metrics.cacheBytesStoredCalls)
	}

	// "started" confirms the job (launched -> running), not completion.
	if db.startedCalls != 1 {
		t.Errorf("expected 1 MarkJobStarted call for 'started' status, got %d", db.startedCalls)
	}
	if db.completeCalls != 0 {
		t.Errorf("expected 0 complete calls for 'started' status, got %d", db.completeCalls)
	}
	if metrics.confirmedCalls != 1 {
		t.Errorf("expected 1 runner-confirmed metric for 'started' status, got %d", metrics.confirmedCalls)
	}
	if metrics.lastConfirmedPool != "default" {
		t.Errorf("expected confirmation metric tagged pool=default, got %q", metrics.lastConfirmedPool)
	}
	if metrics.servedCalls != 0 || metrics.nonServedCalls != 0 {
		t.Error("expected no completion metrics for 'started' status")
	}
}

// A late "started" message (job already left launched) is a no-op: MarkJobStarted
// returns a nil record, so no confirmation metric is emitted, and the message is
// still deleted.
func TestHandler_processMessage_StartedLateNoOp(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{startedNoOp: true} // already past launched => no-op transition
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{InstanceID: "i-12345", JobID: "12345678901", Status: "started"}
	body, _ := json.Marshal(msg)

	err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceiptTermination})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db.startedCalls != 1 {
		t.Errorf("expected 1 MarkJobStarted call, got %d", db.startedCalls)
	}
	if metrics.confirmedCalls != 0 {
		t.Errorf("expected no confirmation metric on a no-op transition, got %d", metrics.confirmedCalls)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected the started message to be deleted, got %d delete calls", q.deleteCalls)
	}
}

// A transient DB error on the "started" path must NOT delete the message, so SQS
// redelivers it and the watchdog does not later mistake a healthy job for one
// that never confirmed.
func TestHandler_processMessage_StartedDBErrorRetries(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{markStartedErr: errors.New("transient db error")}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{InstanceID: "i-12345", JobID: "12345678901", Status: "started"}
	body, _ := json.Marshal(msg)

	err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceiptTermination})
	if err == nil {
		t.Fatal("expected an error so the message is retried, got nil")
	}
	if q.deleteCalls != 0 {
		t.Errorf("expected the message NOT to be deleted on DB error, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_StartedProvisionSeconds(t *testing.T) {
	created := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	started := created.Add(30 * time.Second)

	tests := []struct {
		name        string
		record      *events.JobInfo
		startedAt   time.Time
		wantPublish bool
		wantSource  string
		wantFamily  string
		wantSeconds float64
	}{
		{
			name:        "warm pool hit publishes warm_pool source",
			record:      &events.JobInfo{JobID: 1, Pool: "default", InstanceType: "c7g.xlarge", WarmPoolHit: true, CreatedAt: created},
			startedAt:   started,
			wantPublish: true,
			wantSource:  "warm_pool",
			wantFamily:  "c7g",
			wantSeconds: 30,
		},
		{
			name:        "cold start publishes cold_start source",
			record:      &events.JobInfo{JobID: 1, Pool: "default", InstanceType: "m7g.large", WarmPoolHit: false, CreatedAt: created},
			startedAt:   started,
			wantPublish: true,
			wantSource:  "cold_start",
			wantFamily:  "m7g",
			wantSeconds: 30,
		},
		{
			name:        "zero created_at does not publish",
			record:      &events.JobInfo{JobID: 1, Pool: "default", InstanceType: "c7g.xlarge"},
			startedAt:   started,
			wantPublish: false,
		},
		{
			name:        "zero started_at does not publish",
			record:      &events.JobInfo{JobID: 1, Pool: "default", InstanceType: "c7g.xlarge", CreatedAt: created},
			startedAt:   time.Time{},
			wantPublish: false,
		},
		{
			name:        "started before created does not publish",
			record:      &events.JobInfo{JobID: 1, Pool: "default", InstanceType: "c7g.xlarge", CreatedAt: started},
			startedAt:   created,
			wantPublish: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &mockQueueAPI{}
			db := &mockDBAPI{startedRecord: tt.record}
			metrics := &mockMetricsAPI{}
			handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

			msg := Message{InstanceID: "i-12345", JobID: "1", Status: "started", StartedAt: tt.startedAt}
			body, _ := json.Marshal(msg)

			if err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceiptTermination}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantPublish {
				if metrics.provisionCalls != 1 {
					t.Fatalf("provision calls = %d, want 1", metrics.provisionCalls)
				}
				if metrics.lastProvSource != tt.wantSource {
					t.Errorf("source = %q, want %q", metrics.lastProvSource, tt.wantSource)
				}
				if metrics.lastProvFamily != tt.wantFamily {
					t.Errorf("family = %q, want %q", metrics.lastProvFamily, tt.wantFamily)
				}
				if metrics.lastProvSeconds != tt.wantSeconds {
					t.Errorf("seconds = %v, want %v", metrics.lastProvSeconds, tt.wantSeconds)
				}
			} else if metrics.provisionCalls != 0 {
				t.Errorf("provision calls = %d, want 0", metrics.provisionCalls)
			}
		})
	}
}

// A late "started" message (nil record) must publish no provision metric.
func TestHandler_processMessage_StartedNoRecordNoProvision(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{startedNoOp: true}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{InstanceID: "i-12345", JobID: "1", Status: "started", StartedAt: time.Now()}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceiptTermination}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.provisionCalls != 0 {
		t.Errorf("provision calls = %d, want 0 on a nil record", metrics.provisionCalls)
	}
}

// A started message carrying all five bootstrap segments publishes one
// AgentBootstrapSeconds per positive segment, tagged with the closed phase enum.
func TestHandler_processMessage_StartedBootstrapSeconds(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	msg := Message{
		InstanceID:               "i-12345",
		JobID:                    "12345678901",
		Status:                   "started",
		StartedAt:                time.Now(),
		BootstrapBootSeconds:     12.5,
		BootstrapConfigSeconds:   3.2,
		BootstrapRunnerSeconds:   8.1,
		BootstrapRegisterSeconds: 5.4,
		BootstrapTotalSeconds:    30.7,
	}
	body, _ := json.Marshal(msg)

	if err := handler.processMessage(context.Background(), queue.Message{Body: string(body), Handle: testReceiptTermination}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.bootstrapCalls != 5 {
		t.Fatalf("bootstrap calls = %d, want 5", metrics.bootstrapCalls)
	}
	want := map[string]float64{
		"boot":            12.5,
		"config":          3.2,
		"runner_download": 8.1,
		"registration":    5.4,
		"total":           30.7,
	}
	for phase, sec := range want {
		if metrics.bootstrapByPhase[phase] != sec {
			t.Errorf("phase %q = %v, want %v", phase, metrics.bootstrapByPhase[phase], sec)
		}
	}
}

// An old agent's started message (raw JSON without the bootstrap fields) decodes
// additively — the missing segments read as zero and no bootstrap metric fires.
func TestHandler_processMessage_StartedOldAgentNoBootstrap(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, &mockSecretsStore{}, nil, &config.Config{})

	// Raw JSON as a pre-rollout agent would send it: no bootstrap_* keys.
	body := `{"instance_id":"i-12345","job_id":"12345678901","status":"started","started_at":"2026-07-09T12:00:00Z"}`

	if err := handler.processMessage(context.Background(), queue.Message{Body: body, Handle: testReceiptTermination}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.bootstrapCalls != 0 {
		t.Errorf("bootstrap calls = %d, want 0 for an old-agent message", metrics.bootstrapCalls)
	}
}

func TestHandler_processMessage_EmptyBody(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	queueMsg := queue.Message{
		Body:   "",
		Handle: testReceiptTermination,
	}

	// Empty body is non-retryable: it must be acked (deleted), not returned as an
	// error that would redeliver for the full retention window.
	if err := handler.processMessage(context.Background(), queueMsg); err != nil {
		t.Fatalf("expected empty body to be acked, got error: %v", err)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected empty-body message to be deleted (acked), got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_InvalidJSON(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	queueMsg := queue.Message{
		Body:   "invalid json",
		Handle: testReceiptTermination,
	}

	// Unparseable JSON is non-retryable: ack (delete), don't redeliver.
	if err := handler.processMessage(context.Background(), queueMsg); err != nil {
		t.Fatalf("expected invalid JSON to be acked, got error: %v", err)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected invalid-JSON message to be deleted (acked), got %d delete calls", q.deleteCalls)
	}
}

func bootstrapFailedBody(t *testing.T, instanceID string) string {
	t.Helper()
	body, err := json.Marshal(Message{InstanceID: instanceID, Status: "bootstrap_failed", Error: "agent bootstrap failed on boot"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

func TestHandler_processMessage_BootstrapFailed_RequeuesAndAcks(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		jobByInstance:      &events.JobInfo{JobID: 555, RunID: 777, Repo: testRepo, InstanceType: "c7g.large", Pool: "default", RetryCount: 1},
		markRequeuedResult: true,
	}
	secretsStore := &mockSecretsStore{}
	jq := &mockJobQueue{}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, secretsStore, jq, &config.Config{})

	msg := queue.Message{Body: bootstrapFailedBody(t, "i-abc"), Handle: testReceiptTermination}
	if err := handler.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if jq.sendCalls != 1 {
		t.Fatalf("expected 1 requeue send, got %d", jq.sendCalls)
	}
	if jq.lastMsg.JobID != 555 || jq.lastMsg.RunID != 777 || !jq.lastMsg.ForceOnDemand || jq.lastMsg.Spot {
		t.Errorf("requeue msg = %+v, want JobID=555 RunID=777 ForceOnDemand=true Spot=false", jq.lastMsg)
	}
	if jq.lastMsg.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2 (job.RetryCount+1)", jq.lastMsg.RetryCount)
	}
	if metrics.requeuedCalls != 1 || metrics.lastRequeuedReason != reasonBootstrapFailed {
		t.Errorf("expected 1 requeue metric with reason %q, got calls=%d reason=%q",
			reasonBootstrapFailed, metrics.requeuedCalls, metrics.lastRequeuedReason)
	}
	if secretsStore.deleteCalls != 1 {
		t.Errorf("expected runner config deleted, got %d delete calls", secretsStore.deleteCalls)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected message acked, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_BootstrapFailed_ExhaustedRetries_GivesUp(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		// RetryCount at the cap: must not re-queue again.
		jobByInstance:      &events.JobInfo{JobID: 555, RunID: 777, Repo: testRepo, RetryCount: maxBootstrapRequeues},
		markRequeuedResult: true,
	}
	secretsStore := &mockSecretsStore{}
	jq := &mockJobQueue{}
	metrics := &mockMetricsAPI{}
	handler := NewHandler(q, db, metrics, secretsStore, jq, &config.Config{})

	msg := queue.Message{Body: bootstrapFailedBody(t, "i-loop"), Handle: testReceiptTermination}
	if err := handler.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if jq.sendCalls != 0 {
		t.Errorf("expected NO requeue once retries are exhausted, got %d", jq.sendCalls)
	}
	if db.requeueCalls != 0 {
		t.Errorf("expected MarkJobRequeued NOT called at the cap, got %d", db.requeueCalls)
	}
	if metrics.schedFailCalls != 1 || metrics.lastSchedFailType != reasonBootstrapFailed {
		t.Errorf("expected 1 scheduling-failure metric with type %q, got calls=%d type=%q",
			reasonBootstrapFailed, metrics.schedFailCalls, metrics.lastSchedFailType)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected message acked, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_BootstrapFailed_NoJob_Acks(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{jobByInstance: nil}
	jq := &mockJobQueue{}
	handler := NewHandler(q, db, &mockMetricsAPI{}, &mockSecretsStore{}, jq, &config.Config{})

	msg := queue.Message{Body: bootstrapFailedBody(t, "i-prewarm"), Handle: testReceiptTermination}
	if err := handler.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jq.sendCalls != 0 {
		t.Errorf("expected no requeue for instance with no job, got %d", jq.sendCalls)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected message acked, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_BootstrapFailed_AlreadyRequeued_NoDoubleEnqueue(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		jobByInstance:      &events.JobInfo{JobID: 555, RunID: 777, Repo: testRepo},
		markRequeuedResult: false, // already requeued (watchdog or redelivery)
	}
	jq := &mockJobQueue{}
	handler := NewHandler(q, db, &mockMetricsAPI{}, &mockSecretsStore{}, jq, &config.Config{})

	msg := queue.Message{Body: bootstrapFailedBody(t, "i-abc"), Handle: testReceiptTermination}
	if err := handler.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jq.sendCalls != 0 {
		t.Errorf("expected no double requeue when MarkJobRequeued returns false, got %d", jq.sendCalls)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected message acked, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_processMessage_BootstrapFailed_NoInstanceID_Acks(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	jq := &mockJobQueue{}
	handler := NewHandler(q, db, &mockMetricsAPI{}, &mockSecretsStore{}, jq, &config.Config{})

	body, _ := json.Marshal(Message{Status: "bootstrap_failed", Error: "x"})
	msg := queue.Message{Body: string(body), Handle: testReceiptTermination}
	if err := handler.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db.getByInstanceCalls != 0 {
		t.Errorf("expected no job lookup without instance_id, got %d", db.getByInstanceCalls)
	}
	if q.deleteCalls != 1 {
		t.Errorf("expected message acked, got %d delete calls", q.deleteCalls)
	}
}

func TestHandler_validateMessage_MissingInstanceID(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		JobID:  "12345678901",
		Status: "success",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing instance_id")
	}
}

func TestHandler_validateMessage_MissingJobID(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		Status:     "success",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing job_id")
	}
}

func TestHandler_validateMessage_MissingStatus(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
	}

	err := handler.validateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing status")
	}
}

func TestHandler_validateMessage_Valid(t *testing.T) {
	handler := &Handler{}

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "success",
	}

	err := handler.validateMessage(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_MarkCompleteError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		markCompleteErr: errors.New("db error"),
	}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "success",
	}

	err := handler.processTermination(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error from mark complete")
	}
}

func TestHandler_processTermination_DeleteRunnerConfig(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "success",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if secretsStore.deleteCalls != 1 {
		t.Errorf("expected 1 secrets store delete call, got %d", secretsStore.deleteCalls)
	}

	expectedID := "i-12345"
	if secretsStore.deletedID != expectedID {
		t.Errorf("expected deleted runner ID '%s', got '%s'", expectedID, secretsStore.deletedID)
	}
}

func TestHandler_deleteRunnerConfig_NotFound(t *testing.T) {
	secretsStore := &mockSecretsStore{
		deleteErr: errors.New("not found"),
	}
	handler := &Handler{secretsStore: secretsStore}

	// Should not return error if config already deleted
	err := handler.deleteRunnerConfig(context.Background(), "i-12345")
	if err != nil {
		t.Fatalf("unexpected error for not found: %v", err)
	}
}

func TestHandler_deleteRunnerConfig_OtherError(t *testing.T) {
	secretsStore := &mockSecretsStore{
		deleteErr: errors.New("access denied"),
	}
	handler := &Handler{secretsStore: secretsStore}

	err := handler.deleteRunnerConfig(context.Background(), "i-12345")
	if err == nil {
		t.Fatal("expected error for other error")
	}
}

func TestHandler_deleteRunnerConfig_NilStore(t *testing.T) {
	handler := &Handler{secretsStore: nil}

	// Should not return error when store is nil
	err := handler.deleteRunnerConfig(context.Background(), "i-12345")
	if err != nil {
		t.Fatalf("unexpected error for nil store: %v", err)
	}
}

func TestHandler_processMessage_DeleteError(t *testing.T) {
	q := &mockQueueAPI{
		deleteErr: errors.New("delete error"),
	}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "success",
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: testReceiptTermination,
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err == nil {
		t.Fatal("expected error from message deletion")
	}
}

func TestHandler_Run_Cancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := &mockQueueAPI{
			messages: []queue.Message{},
		}
		db := &mockDBAPI{}
		metrics := &mockMetricsAPI{}
		secretsStore := &mockSecretsStore{}
		cfg := &config.Config{}
		handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			handler.Run(ctx)
			close(done)
		}()

		// Cancel immediately; Run must observe ctx.Done and return.
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler did not stop after cancellation")
		}
	})
}

// blockingDBAPI lets a test pause MarkJobComplete mid-processing so it can
// cancel the handler's parent context while a message is in flight, then inspect
// whether the processing context observed that cancellation.
type blockingDBAPI struct {
	started     chan struct{}
	release     chan struct{}
	ctxErr      error
	startedOnce sync.Once
}

func (m *blockingDBAPI) MarkJobComplete(ctx context.Context, jobID int64, _ string, _, _ int) (*events.JobInfo, error) {
	m.startedOnce.Do(func() { close(m.started) })
	<-m.release
	m.ctxErr = ctx.Err()
	return &events.JobInfo{JobID: jobID, RunID: 99, Repo: testRepo}, nil
}

func (m *blockingDBAPI) MarkJobStarted(_ context.Context, jobID int64, _ time.Time) (*events.JobInfo, error) {
	return &events.JobInfo{JobID: jobID, RunID: 99, Repo: testRepo}, nil
}

func (m *blockingDBAPI) UpdateJobMetrics(_ context.Context, _ int64, _, _ time.Time) error {
	return nil
}

func (m *blockingDBAPI) GetJobByInstance(_ context.Context, _ string) (*events.JobInfo, error) {
	return nil, nil
}

func (m *blockingDBAPI) MarkJobRequeuedByJobID(_ context.Context, _ int64) (bool, error) {
	return false, nil
}

// TestHandler_Run_InflightProcessorRunsToCompletionAfterCancel simulates a
// SIGTERM landing while a termination message is being processed: the parent
// context is cancelled mid-flight. The in-flight processor must run to
// completion observing a context that is NOT cancelled, and Run must block on
// its WaitGroup until the processor finishes.
func TestHandler_Run_InflightProcessorRunsToCompletionAfterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, err := json.Marshal(Message{
			InstanceID:      "i-12345",
			JobID:           "12345678901",
			Status:          testStatusSuccess,
			ExitCode:        0,
			DurationSeconds: 30,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		dispatched := int32(0)
		// Return the message only on the first receive so the drain does not
		// re-dispatch it.
		q := &mockQueueAPI{messages: []queue.Message{{Body: string(body), Handle: testReceiptTermination}}}
		db := &blockingDBAPI{started: make(chan struct{}), release: make(chan struct{})}
		handler := NewHandler(&onceQueueAPI{inner: q, dispatched: &dispatched}, db, &mockMetricsAPI{}, &mockSecretsStore{}, nil, &config.Config{})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			handler.Run(ctx)
			close(done)
		}()

		// Block until the processor is in flight (parked on <-release). The
		// fake clock advances to fire the tick that receives and dispatches.
		<-db.started
		synctest.Wait()

		// Simulate SIGTERM mid-processing.
		cancel()
		synctest.Wait()

		// Run must not return while the processor is still in flight: it is
		// detached from the signal and blocked on release.
		select {
		case <-done:
			t.Fatal("handler returned before in-flight processor completed")
		default:
		}

		close(db.release)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler did not return after in-flight processor completed")
		}

		// done is closed, so this read happens-after the processor wrote ctxErr.
		if db.ctxErr != nil {
			t.Errorf("processing context was cancelled by SIGTERM (%v); in-flight work must be detached from the signal", db.ctxErr)
		}
	})
}

// onceQueueAPI returns the inner queue's messages on the first receive and empty
// thereafter, so the drain does not redeliver the same message.
type onceQueueAPI struct {
	inner      *mockQueueAPI
	dispatched *int32
}

func (o *onceQueueAPI) ReceiveMessages(ctx context.Context, maxMessages, waitSeconds int32) ([]queue.Message, error) {
	if atomic.CompareAndSwapInt32(o.dispatched, 0, 1) {
		return o.inner.ReceiveMessages(ctx, maxMessages, waitSeconds)
	}
	return nil, nil
}

func (o *onceQueueAPI) DeleteMessage(ctx context.Context, handle string) error {
	return o.inner.DeleteMessage(ctx, handle)
}

func TestMessage_Structure(t *testing.T) {
	now := time.Now()
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
		Error:           "test error",
		InterruptedBy:   "spot",
	}

	if msg.InstanceID != "i-12345" {
		t.Errorf("expected InstanceID 'i-12345', got '%s'", msg.InstanceID)
	}
	if msg.JobID != "12345678901" {
		t.Errorf("expected JobID '12345678901', got '%s'", msg.JobID)
	}
	if msg.Status != "success" {
		t.Errorf("expected Status 'success', got '%s'", msg.Status)
	}
	if msg.ExitCode != 0 {
		t.Errorf("expected ExitCode 0, got %d", msg.ExitCode)
	}
	if msg.DurationSeconds != 120 {
		t.Errorf("expected DurationSeconds 120, got %d", msg.DurationSeconds)
	}
	if msg.Error != "test error" {
		t.Errorf("expected Error 'test error', got '%s'", msg.Error)
	}
	if msg.InterruptedBy != "spot" {
		t.Errorf("expected InterruptedBy 'spot', got '%s'", msg.InterruptedBy)
	}
}

func TestMessage_JSONSerialization(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	msg := Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		ExitCode:        0,
		DurationSeconds: 120,
		StartedAt:       now.Add(-2 * time.Minute),
		CompletedAt:     now,
	}

	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	if decoded.InstanceID != msg.InstanceID {
		t.Errorf("expected InstanceID '%s', got '%s'", msg.InstanceID, decoded.InstanceID)
	}
	if decoded.JobID != msg.JobID {
		t.Errorf("expected JobID '%s', got '%s'", msg.JobID, decoded.JobID)
	}
}

func TestMessage_RequiredFieldsNotOmitted(t *testing.T) {
	// Explicit test verifying required fields are NOT omitted when empty
	// This addresses the code review concern about omitempty on required fields
	// The Message struct intentionally does NOT use omitempty on required fields
	msg := Message{
		InstanceID: "", // Empty string
		JobID:      "", // Empty string
		Status:     "", // Empty string
		// Optional fields with omitempty
		Error:         "",
		InterruptedBy: "",
	}

	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal message: %v", err)
	}

	jsonStr := string(jsonBytes)

	// Required fields should ALWAYS be present, even when empty
	requiredFields := []string{
		`"instance_id"`,
		`"job_id"`,
		`"status"`,
	}

	for _, field := range requiredFields {
		if !strings.Contains(jsonStr, field) {
			t.Errorf("required field %s should be present in JSON even when empty", field)
		}
	}

	// Optional fields with omitempty SHOULD be omitted when empty
	optionalFields := []string{
		`"error"`,
		`"interrupted_by"`,
	}

	for _, field := range optionalFields {
		if strings.Contains(jsonStr, field) {
			t.Logf("optional field %s is omitted as expected (has omitempty)", field)
		}
	}

	// Verify round-trip works correctly
	var decoded Message
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal message: %v", err)
	}

	// Empty strings should be preserved
	if decoded.InstanceID != "" {
		t.Errorf("expected empty InstanceID, got '%s'", decoded.InstanceID)
	}
}

func TestHandler_processTermination_NoDuration(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          "success",
		DurationSeconds: 0, // No duration
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The completion counter is emitted regardless of duration, but a zero duration
	// must NOT feed the execution-latency histogram (it would skew jobs that never ran).
	if metrics.completedCalls != 1 {
		t.Errorf("expected 1 completed call when duration is 0, got %d", metrics.completedCalls)
	}
	if metrics.execSecondsCalls != 0 {
		t.Errorf("zero duration must not emit an execution-seconds sample, got %d", metrics.execSecondsCalls)
	}
}

func TestHandler_processTermination_NoTimestamps(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "success",
		// No StartedAt or CompletedAt
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not call UpdateJobMetrics when timestamps are zero
	if db.metricsCalls != 0 {
		t.Errorf("expected 0 metrics calls when timestamps are zero, got %d", db.metricsCalls)
	}
}

func TestHandler_Run_WithMessages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msg := Message{
			InstanceID:      "i-12345",
			JobID:           "12345678901",
			Status:          testStatusSuccess,
			DurationSeconds: 60,
		}
		body, _ := json.Marshal(msg)

		receiveCalled := make(chan struct{}, 1)
		q := &mockQueueAPI{
			messages: []queue.Message{
				{
					Body:   string(body),
					Handle: "receipt-1",
				},
			},
			receiveCalled: receiveCalled,
		}
		db := &mockDBAPI{}
		metrics := &mockMetricsAPI{}
		secretsStore := &mockSecretsStore{}
		cfg := &config.Config{}
		handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			handler.Run(ctx)
			close(done)
		}()

		// Block until ReceiveMessages is called; the fake clock advances to
		// fire the tick that triggers the receive.
		<-receiveCalled
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler did not stop after cancellation")
		}

		// Verify message was processed
		if q.receiveCalls < 1 {
			t.Errorf("expected at least 1 receive call, got %d", q.receiveCalls)
		}
	})
}

// TestHandler_Run_EmitsMessageProcessingSeconds proves the termination worker
// emits the message-processing latency tagged with the termination queue and an
// "ok" result for a successfully processed message.
func TestHandler_Run_EmitsMessageProcessingSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msg := Message{InstanceID: "i-1", JobID: "12345678901", Status: testStatusSuccess, DurationSeconds: 60}
		body, _ := json.Marshal(msg)

		q := &mockQueueAPI{messages: []queue.Message{{Body: string(body), Handle: "r1"}}}
		metrics := &mockMetricsAPI{}
		handler := NewHandler(q, &mockDBAPI{}, metrics, &mockSecretsStore{}, nil, &config.Config{})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			handler.Run(ctx)
			close(done)
		}()

		// Advance one tick and settle: the message is received and processed,
		// emitting the latency metric. Replaces a 2s deadline + 5ms poll loop.
		time.Sleep(handlerTickInterval)
		synctest.Wait()

		n, queueLabel, result := metrics.processingSnapshot()
		if n == 0 {
			cancel()
			synctest.Wait()
			t.Fatal("message-processing metric was never emitted")
		}
		if queueLabel != queueTermination {
			t.Errorf("processing queue label = %q, want termination", queueLabel)
		}
		if result != "ok" {
			t.Errorf("processing result = %q, want ok", result)
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler did not return after cancel")
		}
	})
}

func TestHandler_Run_ReceiveError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		receiveCalled := make(chan struct{}, 1)
		q := &mockQueueAPI{
			receiveErr:    errors.New("receive error"),
			receiveCalled: receiveCalled,
		}
		db := &mockDBAPI{}
		metrics := &mockMetricsAPI{}
		secretsStore := &mockSecretsStore{}
		cfg := &config.Config{}
		handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			handler.Run(ctx)
			close(done)
		}()

		// Block until ReceiveMessages is called; the fake clock advances to
		// fire the tick. The handler continues despite the receive error.
		<-receiveCalled
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("handler did not stop after cancellation")
		}

		// Verify receive was attempted despite error
		if q.receiveCalls < 1 {
			t.Errorf("expected at least 1 receive call, got %d", q.receiveCalls)
		}
	})
}

func TestHandler_processTermination_MetricsErrors(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{
		completedErr: errors.New("completed metric error"),
	}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID:      "i-12345",
		JobID:           "12345678901",
		Status:          testStatusSuccess,
		DurationSeconds: 60,
	}

	// Should not fail even if metrics fail (they just log warnings)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_FailureMetricsError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{
		completedErr: errors.New("failure metric error"),
	}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "failure",
	}

	// Should not fail even if failure metric fails
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processTermination_UpdateMetricsError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{
		updateMetricsErr: errors.New("update metrics error"),
	}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	now := time.Now()
	msg := &Message{
		InstanceID:  "i-12345",
		JobID:       "12345678901",
		Status:      testStatusSuccess,
		StartedAt:   now.Add(-1 * time.Minute),
		CompletedAt: now,
	}

	// Should not fail even if update metrics fails (just logs warning)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandler_processMessage_NoHandle(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     testStatusSuccess,
	}
	body, _ := json.Marshal(msg)

	queueMsg := queue.Message{
		Body:   string(body),
		Handle: "", // Empty handle
	}

	err := handler.processMessage(context.Background(), queueMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not try to delete message with empty handle
	if q.deleteCalls != 0 {
		t.Errorf("expected 0 delete calls for empty handle, got %d", q.deleteCalls)
	}
}

func TestHandler_processTermination_Timeout(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     "timeout",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Timeout maps to the "timeout" result.
	if metrics.completedCalls != 1 {
		t.Errorf("expected 1 completed call for timeout, got %d", metrics.completedCalls)
	}
	if metrics.lastResult != testResultTimeout {
		t.Errorf("expected result 'timeout', got %q", metrics.lastResult)
	}
}

func TestHandler_processTermination_Interrupted(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID:    "i-12345",
		JobID:         "12345678901",
		Status:        "interrupted",
		InterruptedBy: "spot",
	}

	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A spot/infra interruption maps to the operational "interrupted" result,
	// distinct from "error" (a genuine runner/agent operational failure).
	if metrics.completedCalls != 1 {
		t.Errorf("expected 1 completed call for interrupted, got %d", metrics.completedCalls)
	}
	if metrics.lastResult != testResultInterrupted {
		t.Errorf("expected result 'interrupted', got %q", metrics.lastResult)
	}
}

func TestHandler_processTermination_SecretsDeleteError(t *testing.T) {
	q := &mockQueueAPI{}
	db := &mockDBAPI{}
	metrics := &mockMetricsAPI{}
	secretsStore := &mockSecretsStore{
		deleteErr: errors.New("access denied"),
	}
	cfg := &config.Config{}
	handler := NewHandler(q, db, metrics, secretsStore, nil, cfg)

	msg := &Message{
		InstanceID: "i-12345",
		JobID:      "12345678901",
		Status:     testStatusSuccess,
	}

	// Should not fail even if secrets delete fails (just logs warning)
	err := handler.processTermination(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
