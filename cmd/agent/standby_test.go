package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// mockStore is a minimal secrets.Store for standby-poll tests. Get consults
// getFunc; the other methods are unused by the standby loop.
type mockStore struct {
	getFunc func(ctx context.Context, id string) (*secrets.RunnerConfig, error)
	calls   atomic.Int64
}

func (m *mockStore) Put(context.Context, string, *secrets.RunnerConfig) error { return nil }
func (m *mockStore) Get(ctx context.Context, id string) (*secrets.RunnerConfig, error) {
	m.calls.Add(1)
	return m.getFunc(ctx, id)
}
func (m *mockStore) Delete(context.Context, string) error   { return nil }
func (m *mockStore) List(context.Context) ([]string, error) { return nil, nil }

func TestStandbyWaitForConfig_ConfigAppearsAfterPolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const appearAfter = 3
		store := &mockStore{}
		store.getFunc = func(context.Context, string) (*secrets.RunnerConfig, error) {
			if store.calls.Load() >= appearAfter {
				return &secrets.RunnerConfig{RunID: "42"}, nil
			}
			return nil, secrets.ErrConfigNotFound
		}

		deadline := time.Now().Add(2 * time.Hour)
		cfg, err := standbyWaitForConfig(context.Background(), store, "i-123", deadline, &stdLogger{})

		synctest.Wait()
		if err != nil {
			t.Fatalf("standbyWaitForConfig() error = %v, want nil", err)
		}
		if cfg == nil || cfg.RunID != "42" {
			t.Fatalf("standbyWaitForConfig() cfg = %+v, want RunID=42", cfg)
		}
		if got := store.calls.Load(); got != appearAfter {
			t.Errorf("Get called %d times, want %d", got, appearAfter)
		}
	})
}

func TestStandbyWaitForConfig_DeadlineReturnsSentinel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &mockStore{getFunc: func(context.Context, string) (*secrets.RunnerConfig, error) {
			return nil, secrets.ErrConfigNotFound
		}}

		deadline := time.Now().Add(90 * time.Second)
		cfg, err := standbyWaitForConfig(context.Background(), store, "i-123", deadline, &stdLogger{})

		synctest.Wait()
		if !errors.Is(err, errStandbyDeadline) {
			t.Fatalf("standbyWaitForConfig() error = %v, want errStandbyDeadline", err)
		}
		if cfg != nil {
			t.Errorf("standbyWaitForConfig() cfg = %+v, want nil on deadline", cfg)
		}
	})
}

func TestStandbyWaitForConfig_ContextCancelledPromptExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &mockStore{getFunc: func(context.Context, string) (*secrets.RunnerConfig, error) {
			return nil, secrets.ErrConfigNotFound
		}}
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		var cfg *secrets.RunnerConfig
		var err error
		go func() {
			cfg, err = standbyWaitForConfig(ctx, store, "i-123", time.Now().Add(2*time.Hour), &stdLogger{})
			close(done)
		}()

		// Let the loop settle into its first sleep, then simulate SIGTERM.
		synctest.Wait()
		cancel()
		synctest.Wait()

		<-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("standbyWaitForConfig() error = %v, want context.Canceled", err)
		}
		if cfg != nil {
			t.Errorf("standbyWaitForConfig() cfg = %+v, want nil on cancel", cfg)
		}
	})
}

func TestStandbyWaitForConfig_TransientErrorKeepsPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &mockStore{}
		store.getFunc = func(context.Context, string) (*secrets.RunnerConfig, error) {
			switch store.calls.Load() {
			case 1:
				return nil, errors.New("ssm throttled")
			case 2:
				return nil, secrets.ErrConfigNotFound
			default:
				return &secrets.RunnerConfig{RunID: "7"}, nil
			}
		}

		cfg, err := standbyWaitForConfig(context.Background(), store, "i-123", time.Now().Add(2*time.Hour), &stdLogger{})

		synctest.Wait()
		if err != nil {
			t.Fatalf("standbyWaitForConfig() error = %v, want nil (transient errors retry)", err)
		}
		if cfg == nil || cfg.RunID != "7" {
			t.Fatalf("standbyWaitForConfig() cfg = %+v, want RunID=7", cfg)
		}
	})
}

func TestStandbyPollDelayBackoff(t *testing.T) {
	// Early polls are fast (2s) so the cold-start config-write window is caught
	// quickly; later polls back off to ~5s with jitter to shed load. The jitter
	// stays within +-20% of the base.
	for i := 0; i < standbyFastPolls; i++ {
		if d := standbyPollDelay(i); d != standbyFastDelay {
			t.Errorf("standbyPollDelay(%d) = %s, want %s (fast phase)", i, d, standbyFastDelay)
		}
	}
	lo := time.Duration(float64(standbySlowDelay) * 0.8)
	hi := time.Duration(float64(standbySlowDelay) * 1.2)
	for i := standbyFastPolls; i < standbyFastPolls+50; i++ {
		d := standbyPollDelay(i)
		if d < lo || d > hi {
			t.Errorf("standbyPollDelay(%d) = %s, want within [%s,%s]", i, d, lo, hi)
		}
	}
}
