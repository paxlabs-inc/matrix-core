// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package ledger writes credit_ledger rows + reads daily-spend totals
// for the gateway. The Postgres-backed implementation lives in
// postgres.go; this file declares the abstract Ledger interface plus
// an in-memory implementation used by tests + early local-dev posture.
//
// Wire model:
//
//	On every successful upstream LLM call:
//	  1. The proxy computes cost_pax via internal/rates.Cost.
//	  2. The proxy calls Ledger.Record with the actor + cost.
//	  3. Ledger.DailySpend(actor) (called BEFORE the upstream call)
//	     computes how much the actor has already spent today; if
//	     spend + worst-case-projected-cost would exceed the cap, the
//	     proxy returns 429 instead of forwarding.
//
// The cap-vs-spend gate is intentionally soft: we charge the call AFTER
// it succeeds (so failed calls aren't billed), but we predict the
// worst-case using PreEstimate before forwarding. PreEstimate is a
// generous over-estimate (assumes max-tokens fully consumed). Tradeoff:
// a few PAX may slip past the cap on the boundary call; the alternative
// (charge before, refund on failure) is non-atomic against upstream
// failures and was rejected in plan §5.15.
//
// Concurrency: every Ledger implementation MUST be safe for concurrent
// use. The in-memory Memory struct uses a single Mutex; the Postgres
// struct relies on database/sql connection pooling.
package ledger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"matrix/gateway/internal/rates"
)

// Entry is one row in credit_ledger.
type Entry struct {
	ActorDID         string
	IntentID         string
	GoalID           string
	Model            string
	Slot             string
	KindRoute        string
	TokensInput      int
	TokensOutput     int
	CostPax          string // canonical NUMERIC(20,12) decimal string
	RateTableVersion int
	OccurredAt       time.Time
}

// DailyCap is the per-actor daily PAX limit. Read by the gateway via
// Ledger.DailyCap to gate calls before forwarding.
type DailyCap struct {
	ActorDID    string
	DailyPaxMax string // canonical decimal string
	UpdatedAt   time.Time
}

// DefaultDailyPaxCap is the seed value applied when an actor has no
// row in daily_budget_caps. Plan §5.15 quotes "10 PAX/day default".
const DefaultDailyPaxCap = "10"

// Ledger is the abstract interface implemented by both in-memory and
// Postgres backends.
type Ledger interface {
	// Record persists a single credit_ledger row. MUST be idempotent
	// w.r.t. (actor, intent, occurred_at) — duplicate writes are
	// allowed (Postgres rolls them up downstream); see plan §5.15.
	Record(ctx context.Context, e Entry) error

	// DailySpend returns the actor's PAX-denominated total spent
	// during the UTC date containing `now`. Returned as a canonical
	// decimal string so callers can pass it straight to rates.AddPax
	// without re-parsing.
	DailySpend(ctx context.Context, actor string, now time.Time) (string, error)

	// DailyCap returns the actor's PAX-denominated daily cap. When the
	// actor has no row in daily_budget_caps, the default is returned.
	DailyCap(ctx context.Context, actor string) (string, error)

	// Close releases any underlying resources (connection pool).
	Close() error
}

// Memory is an in-memory Ledger used by tests + the local-dev posture
// when a Postgres URI is not supplied. Safe for concurrent use.
type Memory struct {
	mu      sync.Mutex
	rows    []Entry
	caps    map[string]string
	defCap  string
	nowFunc func() time.Time
}

// NewMemory constructs a Memory ledger with the supplied default cap.
// Empty defCap → DefaultDailyPaxCap.
func NewMemory(defCap string) *Memory {
	if defCap == "" {
		defCap = DefaultDailyPaxCap
	}
	return &Memory{
		caps:   map[string]string{},
		defCap: defCap,
	}
}

// SetClock injects a clock for tests so DailySpend boundary cases are
// deterministic. nil resets to wall clock.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFunc = now
}

func (m *Memory) clock() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now().UTC()
}

// Record appends an entry. The OccurredAt field is filled with the
// memory clock when zero so tests don't need to populate it.
func (m *Memory) Record(_ context.Context, e Entry) error {
	if e.ActorDID == "" {
		return fmt.Errorf("gateway.ledger: empty ActorDID")
	}
	if e.RateTableVersion == 0 {
		e.RateTableVersion = rates.RateTableVersion
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = m.clock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, e)
	return nil
}

// DailySpend sums entries occurring on the same UTC calendar day as now.
func (m *Memory) DailySpend(_ context.Context, actor string, now time.Time) (string, error) {
	if now.IsZero() {
		now = m.clock()
	}
	dayStart := startOfUTCDay(now)
	dayEnd := dayStart.Add(24 * time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	total := "0"
	for _, e := range m.rows {
		if e.ActorDID != actor {
			continue
		}
		if e.OccurredAt.Before(dayStart) || !e.OccurredAt.Before(dayEnd) {
			continue
		}
		next, err := rates.AddPax(total, e.CostPax)
		if err != nil {
			return "", fmt.Errorf("gateway.ledger: daily spend sum: %w", err)
		}
		total = next
	}
	return total, nil
}

// DailyCap returns the configured cap for the actor, defaulting when
// none has been set.
func (m *Memory) DailyCap(_ context.Context, actor string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.caps[actor]; ok && v != "" {
		return v, nil
	}
	return m.defCap, nil
}

// SetCap updates the cap for an actor. Test/admin-only.
func (m *Memory) SetCap(actor, cap string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps[actor] = cap
}

// Snapshot returns a copy of all recorded rows. Test-only.
func (m *Memory) Snapshot() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.rows))
	copy(out, m.rows)
	return out
}

// Close is a no-op; Memory holds no resources.
func (m *Memory) Close() error { return nil }

// startOfUTCDay zeroes the H/M/S/ns fields and forces UTC.
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// CheckBudget reports whether an actor has remaining headroom for a
// projected cost. spend + projection must be <= cap. Wraps the typed
// budget-exhausted error so callers can use errors.Is.
func CheckBudget(spent, projection, cap string) (remaining string, exhausted bool, err error) {
	projTotal, err := rates.AddPax(spent, projection)
	if err != nil {
		return "", false, err
	}
	cmp, err := rates.CmpPax(projTotal, cap)
	if err != nil {
		return "", false, err
	}
	if cmp > 0 {
		// Actor over the cap once this projection lands.
		rem, _ := rates.SubPax(cap, spent)
		return rem, true, nil
	}
	rem, err := rates.SubPax(cap, spent)
	if err != nil {
		return "", false, err
	}
	return rem, false, nil
}

// ErrBudgetExhausted is the typed error returned by the proxy when an
// actor crosses their daily cap. Wrap with fmt.Errorf using %w so
// callers can errors.Is(... ErrBudgetExhausted).
var ErrBudgetExhausted = fmt.Errorf("gateway.ledger: budget exhausted")

// Compile-time assertion: Memory satisfies Ledger.
var _ Ledger = (*Memory)(nil)

// Copyright © 2026 Paxlabs Inc. All rights reserved.
