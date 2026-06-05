// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_async.go — async-mode runner + per-intent gate broker.
//
// Surfaces:
//
//   1. asyncRegistry — tracks every async-mode /messages/async job
//      keyed by intent_id with status + terminal result.
//
//   2. gateBroker — per-(intent_id,node_id) registry of pending
//      runtime.Gate prompts. The walker emits walk.gate.invoked, the
//      gate is registered as pending, and the daemon blocks the
//      walker on a typed channel until POST /intents/:id/gates/:nid/
//      answer writes a decision (or the gate times out).
//
//   3. httpGateHandler — implements runtime.GateHandler. Replaces the
//      stdinGateHandler when the daemon services HTTP requests. The
//      gate question is broadcast via SSE (the walker already emits
//      walk.gate.invoked); the answer arrives over HTTP.
//
//   4. cancelRegistry — per-intent cancel channels so POST /intents/:id/
//      cancel can cancel the in-flight context. Cleanly disabled (no-op)
//      for sync-mode callers that don't track context propagation.
//
// Concurrency posture: the existing daemonState.busy mutex still
// gates one /messages run at a time (cortex single-writer invariant).
// Async mode does NOT bypass that mutex; it just allows the HTTP
// caller to walk away while the goroutine queues + runs. Multiple
// async POSTs serialise on busy in goroutines — clients see "queued"
// state until their turn comes up.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"matrix/executor/runtime"
	"matrix/mcl/ir"
)

// asyncStatus is the closed enum of async-job lifecycle states.
type asyncStatus string

const (
	asyncQueued    asyncStatus = "queued"
	asyncRunning   asyncStatus = "running"
	asyncCompleted asyncStatus = "completed"
	asyncFailed    asyncStatus = "failed"
	asyncCancelled asyncStatus = "cancelled"
)

// asyncJob tracks one async /messages run.
type asyncJob struct {
	IntentID   string         `json:"intent_id"`
	Status     asyncStatus    `json:"status"`
	Request    messageRequest `json:"request"`
	CreatedAt  time.Time      `json:"created_at"`
	StartedAt  time.Time      `json:"started_at,omitempty"`
	EndedAt    time.Time      `json:"ended_at,omitempty"`
	Result     *messageResult `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
	UserID     string         `json:"user_id,omitempty"` // X-Matrix-User
	clarify    *clarifyRequiredError
	cancelFunc context.CancelFunc // set when goroutine is running
}

// asyncRegistry is the daemon-wide map of async jobs by intent_id.
// Bounded at maxAsyncJobs entries; oldest terminal entries are pruned
// as new jobs arrive so the registry never unboundedly grows.
type asyncRegistry struct {
	mu   sync.Mutex
	jobs map[string]*asyncJob
	max  int
	// dir is the on-volume directory where each job is persisted as
	// <intent_id>.json. Empty disables persistence (sync-only / test
	// daemons). When set, jobs survive suspend, crash, and redeploy:
	// queued jobs are guaranteed to run, and terminal outcomes stay
	// pull-retrievable — this is the durable-inbox half of the "a
	// message is never dropped" guarantee.
	dir   string
	clock func() time.Time
}

const defaultMaxAsyncJobs = 1024

// newAsyncRegistry builds the registry. When dir is non-empty it is
// created if missing and any previously-persisted jobs are loaded back
// into memory (so /messages/async/:id keeps answering across restarts).
func newAsyncRegistry(max int, dir string) *asyncRegistry {
	if max <= 0 {
		max = defaultMaxAsyncJobs
	}
	r := &asyncRegistry{
		jobs:  make(map[string]*asyncJob, 64),
		max:   max,
		dir:   dir,
		clock: func() time.Time { return time.Now().UTC() },
	}
	r.loadFromDir()
	return r
}

// CreateQueued registers a new job in queued state. Returns an error
// when intent_id is already in flight (caller should generate a fresh
// id rather than trample state).
func (r *asyncRegistry) CreateQueued(intentID, userID string, req messageRequest) (*asyncJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.jobs[intentID]; exists {
		return nil, fmt.Errorf("intent %q already exists in async registry", intentID)
	}
	r.evictOldestTerminalLocked()
	job := &asyncJob{
		IntentID:  intentID,
		Status:    asyncQueued,
		Request:   req,
		CreatedAt: r.clock(),
		UserID:    userID,
	}
	r.jobs[intentID] = job
	r.persistLocked(job)
	return job, nil
}

// MarkRunning transitions a job to running. Captures the cancelFunc
// so /intents/:id/cancel can interrupt the in-flight runMessage.
func (r *asyncRegistry) MarkRunning(intentID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[intentID]
	if !ok {
		return
	}
	j.Status = asyncRunning
	j.StartedAt = r.clock()
	j.cancelFunc = cancel
	r.persistLocked(j)
}

// MarkResult records the terminal outcome of a job and clears the
// cancelFunc.
func (r *asyncRegistry) MarkResult(intentID string, res *messageResult, err error, clarify *clarifyRequiredError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[intentID]
	if !ok {
		return
	}
	j.EndedAt = r.clock()
	j.cancelFunc = nil
	j.clarify = clarify
	j.Result = res
	defer r.persistLocked(j)
	if clarify != nil {
		j.Status = asyncFailed
		j.Error = clarify.Error()
		return
	}
	if err != nil {
		j.Status = asyncFailed
		j.Error = err.Error()
		return
	}
	if res != nil && res.Status == "failed" {
		j.Status = asyncFailed
		j.Error = res.Error
		return
	}
	if res != nil && res.Status == "cancelled" {
		j.Status = asyncCancelled
		return
	}
	j.Status = asyncCompleted
}

// Cancel attempts to cancel an in-flight async job. Returns an error
// when the job is not in flight.
func (r *asyncRegistry) Cancel(intentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[intentID]
	if !ok {
		return fmt.Errorf("intent %q not found", intentID)
	}
	switch j.Status {
	case asyncQueued, asyncRunning:
		// Mark cancelled even before the cancel context fires; the
		// goroutine will record the terminal envelope then call
		// MarkResult which is idempotent on Status.
		if j.cancelFunc != nil {
			j.cancelFunc()
		}
		j.Status = asyncCancelled
		j.EndedAt = r.clock()
		r.persistLocked(j)
		return nil
	}
	return fmt.Errorf("intent %q is in terminal state %q (cancel rejected)", intentID, j.Status)
}

// Get returns a snapshot of an async job, or nil when unknown.
func (r *asyncRegistry) Get(intentID string) *asyncJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[intentID]
	if !ok {
		return nil
	}
	cp := *j
	cp.cancelFunc = nil
	return &cp
}

// List returns all jobs in CreatedAt-descending order.
func (r *asyncRegistry) List() []*asyncJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*asyncJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		cp := *j
		cp.cancelFunc = nil
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// evictOldestTerminalLocked drops the oldest terminal job when the
// registry is at capacity. Caller MUST hold r.mu.
func (r *asyncRegistry) evictOldestTerminalLocked() {
	if len(r.jobs) < r.max {
		return
	}
	var oldest *asyncJob
	for _, j := range r.jobs {
		switch j.Status {
		case asyncQueued, asyncRunning:
			continue // never evict in-flight
		}
		if oldest == nil || j.EndedAt.Before(oldest.EndedAt) {
			oldest = j
		}
	}
	if oldest != nil {
		delete(r.jobs, oldest.IntentID)
		r.removeFileLocked(oldest.IntentID)
	}
}

// ---- gate broker ----

// gateAnswer is the typed result of one POST /intents/:id/gates/:nid/answer.
type gateAnswer struct {
	Approved   bool   `json:"approved"`
	Answer     string `json:"answer,omitempty"`
	Correction string `json:"correction,omitempty"`
}

// pendingGate is one walker-blocking gate awaiting a user decision.
type pendingGate struct {
	IntentID  string    `json:"intent_id"`
	NodeID    string    `json:"node_id"`
	Question  string    `json:"question"`
	Options   []string  `json:"options,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	answer    chan gateAnswer
}

// gateBroker tracks every walker-blocked gate across all intents.
type gateBroker struct {
	mu      sync.RWMutex
	pending map[string]*pendingGate // key: intent_id + ":" + node_id
}

func newGateBroker() *gateBroker {
	return &gateBroker{
		pending: make(map[string]*pendingGate),
	}
}

func gateKey(intentID, nodeID string) string {
	return intentID + ":" + nodeID
}

// Open registers a new pending gate. Returns the gate handle whose
// `answer` channel the caller must select on.
func (b *gateBroker) Open(intentID, nodeID, question string, options []string) *pendingGate {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := &pendingGate{
		IntentID:  intentID,
		NodeID:    nodeID,
		Question:  question,
		Options:   options,
		CreatedAt: time.Now().UTC(),
		answer:    make(chan gateAnswer, 1),
	}
	b.pending[gateKey(intentID, nodeID)] = g
	return g
}

// Close removes a pending gate (call after the walker unblocks). Safe
// to call when the gate is already closed.
func (b *gateBroker) Close(intentID, nodeID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, gateKey(intentID, nodeID))
}

// Answer delivers a user decision to a pending gate. Returns an error
// when no gate is pending for that (intent_id, node_id).
func (b *gateBroker) Answer(intentID, nodeID string, ans gateAnswer) error {
	b.mu.RLock()
	g, ok := b.pending[gateKey(intentID, nodeID)]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no pending gate for intent=%s node=%s", intentID, nodeID)
	}
	select {
	case g.answer <- ans:
		return nil
	default:
		return fmt.Errorf("gate already answered (intent=%s node=%s)", intentID, nodeID)
	}
}

// ListByIntent returns the currently-pending gates for one intent in
// CreatedAt order.
func (b *gateBroker) ListByIntent(intentID string) []*pendingGate {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*pendingGate, 0, 4)
	for _, g := range b.pending {
		if g.IntentID == intentID {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// All returns all pending gates across all intents.
func (b *gateBroker) All() []*pendingGate {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*pendingGate, 0, len(b.pending))
	for _, g := range b.pending {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---- httpGateHandler ----

// httpGateHandler implements runtime.GateHandler by registering each
// gate with the broker and blocking on its answer channel.
//
// Default timeout is 24h to support long human deliberation; the gate
// node may override via PlanNode.Gate.TimeoutMs. Context cancellation
// (e.g. /intents/:id/cancel) interrupts the wait.
type httpGateHandler struct {
	broker         *gateBroker
	intentID       string
	actor          string
	t              *transcript
	defaultTimeout time.Duration
}

func newHTTPGateHandler(b *gateBroker, intentID, actor string, t *transcript, defaultTimeout time.Duration) *httpGateHandler {
	if defaultTimeout <= 0 {
		defaultTimeout = 24 * time.Hour
	}
	return &httpGateHandler{
		broker:         b,
		intentID:       intentID,
		actor:          actor,
		t:              t,
		defaultTimeout: defaultTimeout,
	}
}

// HandleGate implements runtime.GateHandler.
func (g *httpGateHandler) HandleGate(ctx context.Context, node *ir.PlanNode) (*runtime.GateDecision, error) {
	if node == nil || node.Gate == nil {
		return nil, fmt.Errorf("gate: nil gate body")
	}
	timeout := g.defaultTimeout
	if node.Gate.TimeoutMs > 0 {
		timeout = time.Duration(node.Gate.TimeoutMs) * time.Millisecond
	}
	gate := g.broker.Open(g.intentID, node.ID, node.Gate.Question, node.Gate.Options)
	defer g.broker.Close(g.intentID, node.ID)

	g.t.Event("gate.invoked", "walk", map[string]interface{}{
		"intent_id":  g.intentID,
		"node_id":    node.ID,
		"actor":      g.actor,
		"question":   node.Gate.Question,
		"options":    node.Gate.Options,
		"timeout_ms": timeout.Milliseconds(),
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ans := <-gate.answer:
		g.t.Event("gate.decided", "walk", map[string]interface{}{
			"intent_id":  g.intentID,
			"node_id":    node.ID,
			"actor":      g.actor,
			"answer":     ans.Answer,
			"approved":   ans.Approved,
			"correction": ans.Correction != "",
		})
		return &runtime.GateDecision{
			Approved: ans.Approved,
			Answer:   ans.Answer,
		}, nil
	case <-timer.C:
		g.t.Event("gate.timeout", "walk", map[string]interface{}{
			"intent_id": g.intentID,
			"node_id":   node.ID,
			"timeout":   timeout.String(),
		})
		// Timed-out gates auto-deny so the walk halts cleanly rather
		// than blocking forever (research/02 §14: silence == deny).
		return &runtime.GateDecision{
			Approved: false,
			Answer:   fmt.Sprintf("gate %s timed out after %s", node.ID, timeout),
		}, nil
	case <-ctx.Done():
		g.t.Event("gate.context.cancelled", "walk", map[string]interface{}{
			"intent_id": g.intentID,
			"node_id":   node.ID,
			"error":     ctx.Err().Error(),
		})
		return nil, ctx.Err()
	}
}

// errGateAlreadyAnswered is returned by gateBroker.Answer when the
// gate channel is non-empty (duplicate answer).
var errGateAlreadyAnswered = errors.New("gate already answered")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
