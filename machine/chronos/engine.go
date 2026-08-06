// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type EngineConfig struct {
	Store      *Store
	Target     Target
	Lease      time.Duration
	RetryBase  time.Duration
	RetryLimit time.Duration
	Now        func() time.Time
	OnError    func(error)
}

type Engine struct {
	store      *Store
	target     Target
	lease      time.Duration
	retryBase  time.Duration
	retryLimit time.Duration
	now        func() time.Time
	onError    func(error)
	wake       chan struct{}
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	statusMu   sync.RWMutex
	lastError  string
}

type Health struct {
	Running   bool      `json:"running"`
	NextDue   time.Time `json:"next_due,omitempty"`
	Overdue   int       `json:"overdue"`
	LastError string    `json:"last_error,omitempty"`
}

func Start(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if cfg.Store == nil || cfg.Target == nil {
		return nil, fmt.Errorf("local chronos: store and delivery target are required")
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.RetryLimit <= 0 {
		cfg.RetryLimit = 5 * time.Minute
	}
	if cfg.RetryLimit < cfg.RetryBase {
		return nil, fmt.Errorf("local chronos: retry limit must be at least retry base")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if _, err := cfg.Store.Recover(ctx, cfg.Now().UTC()); err != nil {
		return nil, fmt.Errorf("local chronos: startup recovery: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	engine := &Engine{
		store: cfg.Store, target: cfg.Target, lease: cfg.Lease,
		retryBase: cfg.RetryBase, retryLimit: cfg.RetryLimit,
		now: cfg.Now, onError: cfg.OnError, wake: make(chan struct{}, 1),
		cancel: cancel, done: make(chan struct{}),
	}
	go engine.run(runCtx)
	return engine, nil
}

func (engine *Engine) Create(ctx context.Context, request CreateRequest) (Alarm, bool, error) {
	alarm, created, err := engine.store.Create(ctx, request)
	if err == nil {
		engine.notify()
	}
	return alarm, created, err
}

func (engine *Engine) List(ctx context.Context) ([]Alarm, error) {
	return engine.store.List(ctx)
}

func (engine *Engine) Cancel(ctx context.Context, id string) (bool, error) {
	changed, err := engine.store.Cancel(ctx, id)
	if err == nil {
		engine.notify()
	}
	return changed, err
}

func (engine *Engine) Reschedule(ctx context.Context, id string, next time.Time) (bool, error) {
	changed, err := engine.store.Reschedule(ctx, id, next)
	if err == nil {
		engine.notify()
	}
	return changed, err
}

func (engine *Engine) Health(ctx context.Context) Health {
	next, ok, nextErr := engine.store.NextDue(ctx)
	overdue, overdueErr := engine.store.overdueCount(ctx, engine.now())
	engine.statusMu.RLock()
	health := Health{Running: !channelClosed(engine.done), Overdue: overdue, LastError: engine.lastError}
	engine.statusMu.RUnlock()
	if ok {
		health.NextDue = next
	}
	if err := errors.Join(nextErr, overdueErr); err != nil {
		health.LastError = err.Error()
	}
	return health
}

func (engine *Engine) Close(ctx context.Context) error {
	engine.closeOnce.Do(engine.cancel)
	select {
	case <-engine.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("local chronos: wait for engine: %w", ctx.Err())
	}
}

// Wake interrupts the current deadline sleep so an externally resumed machine
// immediately claims any overdue local alarm. It carries no alarm payload.
func (engine *Engine) Wake() { engine.notify() }

func (engine *Engine) run(ctx context.Context) {
	defer close(engine.done)
	for {
		deadlines, err := engine.store.deadlines(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			engine.report(err)
			if !engine.wait(ctx, time.Second) {
				return
			}
			continue
		}
		queue := make(deadlineHeap, len(deadlines))
		copy(queue, deadlines)
		heap.Init(&queue)
		if len(queue) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-engine.wake:
				continue
			}
		}
		delay := queue[0].at.Sub(engine.now())
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return
			case <-engine.wake:
				stopTimer(timer)
				continue
			case <-timer.C:
			}
		}
		engine.deliverDue(ctx)
	}
}

func (engine *Engine) deliverDue(ctx context.Context) {
	for {
		now := engine.now().UTC()
		claim, err := engine.store.ClaimDue(ctx, now, engine.lease)
		if err != nil {
			if ctx.Err() == nil {
				engine.report(err)
			}
			return
		}
		if claim == nil {
			return
		}
		err = engine.target.Deliver(ctx, Delivery{Alarm: claim.Alarm, ScheduledFor: claim.ScheduledFor})
		completedAt := engine.now().UTC()
		if err == nil {
			if ackErr := engine.store.Acknowledge(ctx, claim.Alarm.ID, claim.LeaseToken, completedAt); ackErr != nil {
				engine.report(ackErr)
				return
			}
			continue
		}
		engine.report(fmt.Errorf("deliver alarm %s: %w", claim.Alarm.ID, err))
		maxFailures := claim.Alarm.Body.MaxFailures
		if maxFailures <= 0 {
			maxFailures = 5
		}
		if claim.Alarm.DeliveryAttempts >= maxFailures {
			if failErr := engine.store.Fail(ctx, claim.Alarm.ID, claim.LeaseToken, err.Error(), completedAt); failErr != nil {
				engine.report(failErr)
			}
			return
		}
		retry := engine.retryDelay(claim.Alarm.DeliveryAttempts)
		if retryErr := engine.store.Retry(ctx, claim.Alarm.ID, claim.LeaseToken, err.Error(), completedAt.Add(retry)); retryErr != nil {
			engine.report(retryErr)
		}
		return
	}
}

func (engine *Engine) retryDelay(attempt int) time.Duration {
	delay := engine.retryBase
	for index := 1; index < attempt && delay < engine.retryLimit; index++ {
		if delay > engine.retryLimit/2 {
			return engine.retryLimit
		}
		delay *= 2
	}
	if delay > engine.retryLimit {
		return engine.retryLimit
	}
	return delay
}

func (engine *Engine) report(err error) {
	engine.statusMu.Lock()
	engine.lastError = err.Error()
	engine.statusMu.Unlock()
	if engine.onError != nil {
		engine.onError(err)
	}
}

func (engine *Engine) notify() {
	select {
	case engine.wake <- struct{}{}:
	default:
	}
}

func (engine *Engine) wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer stopTimer(timer)
	select {
	case <-ctx.Done():
		return false
	case <-engine.wake:
		return true
	case <-timer.C:
		return true
	}
}

type deadlineHeap []deadline

func (items deadlineHeap) Len() int { return len(items) }
func (items deadlineHeap) Less(i, j int) bool {
	if items[i].at.Equal(items[j].at) {
		return items[i].id < items[j].id
	}
	return items[i].at.Before(items[j].at)
}
func (items deadlineHeap) Swap(i, j int)   { items[i], items[j] = items[j], items[i] }
func (items *deadlineHeap) Push(value any) { *items = append(*items, value.(deadline)) }
func (items *deadlineHeap) Pop() any {
	old := *items
	value := old[len(old)-1]
	*items = old[:len(old)-1]
	return value
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}
