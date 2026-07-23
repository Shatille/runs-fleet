package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// errStandbyDeadline is returned by standbyWaitForConfig when the standby budget
// expires before a job config is written. main maps it to a clean exit(0): the
// instance was never assigned a job, so there is nothing to fail. The reconciler
// is the primary decay path for an unused spare; this deadline is only a failsafe
// against a leaked instance polling forever.
var errStandbyDeadline = errors.New("standby deadline reached without job config")

// Standby polling cadence. Early polls are fast so a config written shortly after
// boot (the cold-start window, or a hot-spare assignment) is picked up quickly;
// after standbyFastPolls the cadence backs off to standbySlowDelay with jitter to
// shed backend load while a spare lingers idle. Vars (not consts) so the fast
// count could be tuned; the delays are consts since jitter is applied on top.
const (
	standbyFastDelay = 2 * time.Second
	standbySlowDelay = 5 * time.Second
	standbyFastPolls = 15 // ~30s of 2s polls covers the config-write window
	standbyJitterPct = 0.2
)

// standbyPollDelay returns the sleep before poll attempt n (0-indexed). The fast
// phase is exact 2s; the slow phase is 5s +-20% jitter so many idle spares don't
// hammer the backend in lockstep.
func standbyPollDelay(n int) time.Duration {
	if n < standbyFastPolls {
		return standbyFastDelay
	}
	// jitter in [-standbyJitterPct, +standbyJitterPct]
	jitter := (rand.Float64()*2 - 1) * standbyJitterPct
	return time.Duration(float64(standbySlowDelay) * (1 + jitter))
}

// standbyWaitForConfig polls the secrets store for this instance's runner config
// until it appears, the deadline passes, or the context is cancelled. It is the
// uniform not-found path for every instance: a cold-start instance whose config
// has not been written yet, and a hot-pool spare waiting to be assigned a job,
// wait the same way. This also heals the old zombie-on-exhaustion bug — an agent
// that used to exit 0 after a short retry budget and leave the instance running
// until the unconfirmed-runner watchdog reaped it.
//
// Any transient backend error is treated like not-found (log and keep polling)
// so a single SSM/Vault blip cannot strand a standby instance. A cancelled
// context (SIGTERM from an instance stop) returns promptly with the context
// error so main can exit 0. Deadline expiry returns errStandbyDeadline.
//
// os.Exit is intentionally kept OUT of this function: it returns a sentinel and
// main maps outcomes to exit codes, so the loop is unit-testable under synctest.
func standbyWaitForConfig(ctx context.Context, store secrets.Store, instanceID string, deadline time.Time, logger *stdLogger) (*secrets.RunnerConfig, error) {
	logger.Printf("entering standby: polling for job config (deadline %s)", deadline.Format(time.RFC3339))

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			logger.Printf("standby aborted: %v", err)
			return nil, err
		}

		cfg, err := store.Get(ctx, instanceID)
		if err == nil {
			logger.Printf("standby: job config found after %d poll(s)", attempt+1)
			return cfg, nil
		}
		if !errors.Is(err, secrets.ErrConfigNotFound) {
			logger.Printf("standby: config fetch error (will retry): %v", err)
		}

		if time.Now().After(deadline) {
			return nil, errStandbyDeadline
		}

		delay := standbyPollDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			logger.Printf("standby aborted: %v", ctx.Err())
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
