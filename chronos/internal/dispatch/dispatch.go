// Package dispatch is the durable poll-and-claim worker that fires due alarms.
// Single instance now, HA-ready by construction (the DB claim uses FOR UPDATE
// SKIP LOCKED + a lease, so adding workers never double-fires). A fire is
// recorded only AFTER the router confirms the wake (at-least-once, invariant i3).
package dispatch

import (
	"context"
	"log/slog"
	"time"

	"github.com/paxlabs-inc/chronos/internal/schedule"
	"github.com/paxlabs-inc/chronos/internal/store"
	"github.com/paxlabs-inc/chronos/internal/wake"
	"github.com/paxlabs-inc/chronos/pkg/types"
)

const (
	retryBaseBackoff = 30 * time.Second
	retryMaxBackoff  = 15 * time.Minute
)

// Worker polls the store for due alarms and delivers them via the Waker.
type Worker struct {
	store       *store.Store
	waker       wake.Waker
	log         *slog.Logger
	tick        time.Duration
	lease       time.Duration
	batch       int
	maxFailures int
}

// Config carries the dispatch tunables.
type Config struct {
	Tick        time.Duration
	Lease       time.Duration
	Batch       int
	MaxFailures int
}

// New constructs a dispatch worker.
func New(st *store.Store, waker wake.Waker, log *slog.Logger, cfg Config) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store:       st,
		waker:       waker,
		log:         log,
		tick:        cfg.Tick,
		lease:       cfg.Lease,
		batch:       cfg.Batch,
		maxFailures: cfg.MaxFailures,
	}
}

// Run polls until ctx is cancelled. The DB is the source of truth; the ticker
// is just a heartbeat (dispatch.tick).
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("chronos dispatch worker started", "tick", w.tick.String(), "batch", w.batch)
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("chronos dispatch worker stopped")
			return
		case <-ticker.C:
			w.tickOnce(ctx)
		}
	}
}

func (w *Worker) tickOnce(ctx context.Context) {
	alarms, err := w.store.ClaimDue(ctx, w.batch, w.lease)
	if err != nil {
		w.log.Error("claim due failed", "error", err.Error())
		return
	}
	for _, a := range alarms {
		if ctx.Err() != nil {
			return
		}
		w.fire(ctx, a)
	}
}

func (w *Worker) fire(ctx context.Context, a types.Alarm) {
	now := time.Now().UTC()
	err := w.waker.Wake(ctx, wake.Request{
		UserID:         a.UserID,
		ConversationID: a.ConversationID,
		Message:        a.WakeMessage,
		Payload:        a.Payload,
		AlarmID:        a.ID,
	})
	if err == nil {
		w.onSuccess(ctx, a, now)
		return
	}
	w.onFailure(ctx, a, now, err.Error())
}

func (w *Worker) onSuccess(ctx context.Context, a types.Alarm, now time.Time) {
	if a.Kind == types.KindOnce {
		if e := w.store.MarkFired(ctx, a.ID); e != nil {
			w.log.Error("mark fired failed", "alarm", a.ID, "error", e.Error())
		}
		w.log.Info("alarm fired", "alarm", a.ID, "kind", a.Kind, "user", a.UserID)
		return
	}
	next, e := schedule.NextCron(a.CronExpr, a.Timezone, now)
	if e != nil {
		// A cron that no longer yields a future time is terminal; fire-and-retire.
		w.log.Warn("cron yields no next fire; retiring", "alarm", a.ID, "error", e.Error())
		if me := w.store.MarkFired(ctx, a.ID); me != nil {
			w.log.Error("mark fired failed", "alarm", a.ID, "error", me.Error())
		}
		return
	}
	if e := w.store.Reschedule(ctx, a.ID, next); e != nil {
		w.log.Error("reschedule failed", "alarm", a.ID, "error", e.Error())
	}
	w.log.Info("alarm fired", "alarm", a.ID, "kind", a.Kind, "user", a.UserID, "next_fire_at", next.Format(time.RFC3339))
}

func (w *Worker) onFailure(ctx context.Context, a types.Alarm, now time.Time, errMsg string) {
	ceiling := a.MaxFailures
	if ceiling <= 0 {
		ceiling = w.maxFailures
	}
	willBe := a.FailureCount + 1
	if willBe < ceiling {
		retryAt := now.Add(backoff(a.FailureCount))
		if e := w.store.RecordRetry(ctx, a.ID, retryAt, errMsg); e != nil {
			w.log.Error("record retry failed", "alarm", a.ID, "error", e.Error())
		}
		w.log.Warn("alarm wake failed; will retry", "alarm", a.ID, "attempt", willBe, "retry_at", retryAt.Format(time.RFC3339), "error", errMsg)
		return
	}
	// Retries exhausted.
	if a.Kind == types.KindOnce {
		if e := w.store.MarkFailed(ctx, a.ID, errMsg); e != nil {
			w.log.Error("mark failed failed", "alarm", a.ID, "error", e.Error())
		}
		w.log.Error("alarm permanently failed", "alarm", a.ID, "attempts", willBe, "error", errMsg)
		return
	}
	// cron: skip-and-advance so one bad fire does not wedge the series.
	next, e := schedule.NextCron(a.CronExpr, a.Timezone, now)
	if e != nil {
		w.log.Error("cron skip-advance has no next fire; leaving leased", "alarm", a.ID, "error", e.Error())
		return
	}
	if e := w.store.RescheduleAfterFailure(ctx, a.ID, next, errMsg); e != nil {
		w.log.Error("reschedule after failure failed", "alarm", a.ID, "error", e.Error())
	}
	w.log.Warn("cron alarm fire failed; advanced to next occurrence", "alarm", a.ID, "next_fire_at", next.Format(time.RFC3339), "error", errMsg)
}

// backoff returns an exponential delay for the given (pre-increment) failure
// count, capped at retryMaxBackoff.
func backoff(failureCount int) time.Duration {
	d := retryBaseBackoff
	for i := 0; i < failureCount; i++ {
		d *= 2
		if d >= retryMaxBackoff {
			return retryMaxBackoff
		}
	}
	if d > retryMaxBackoff {
		return retryMaxBackoff
	}
	return d
}
