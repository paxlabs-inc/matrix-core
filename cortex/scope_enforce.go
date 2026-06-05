// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 10 scope enforcement choke point.
//
// Spec: research/04-cortex.md §10 ("Visibility and scope enforcement"),
// research/06-agents.md §7.2 ("Cortex API calls from a sub-agent carry
// the CortexScope. The cortex query engine: 1. Verifies the parent's
// signature on the scope. 2. Verifies the scope's snapshot_hash is
// still resolvable. 3. For ID-based access, verifies the Merkle proof.
// 4. For typed/tagged access, restricts the query plan."), §7.2
// continued ("Any violation returns cortex.scope.violation and is
// logged as a j/ journal event with severity: high.").
//
// Locks (Phase 10 Q3, Q7 in matrix.ctx phase10_locked_design):
//
//   Q3 read API gating = single enforceScope choke point; Scope rides
//      on Query / ContextOpts / ResolveScoped — NOT a wrapper type.
//   Q7 sub-agent UpdateHead requires Writable=true. Default-deny.
//
// Choke point split:
//
//   - VerifyScope: ONCE per call, validates signature + snapshot
//     resolvability + multi-proof (delegates to scope.Verify).
//   - enforceRead: PER-CANDIDATE, returns ErrViolation if scope.Allows
//     misses; logs KindScopeViolation.
//   - enforceWrite: PER-TARGET, requires scope.Writable=true AND
//     scope.Allows; logs KindScopeViolation on either failure.

package cortex

import (
	"errors"
	"fmt"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/memory"
	"matrix/cortex/scope"
)

// WithKeyResolver injects the agent-pubkey resolver used by the scope
// verifier. Required when callers pass a non-nil scope to Find /
// Context / ResolveScoped / scope-gated writes; if absent the cortex
// rejects scoped calls with ErrNoKeyResolver. Cortex never holds key
// material itself per D4 — the resolver lives in the agent runtime /
// tools/registry layer and is plumbed in here.
func WithKeyResolver(r scope.KeyResolver) Option {
	return func(c *Cortex) { c.keyResolver = r }
}

// ErrNoKeyResolver is returned when a scoped call is made on a Cortex
// that was constructed without WithKeyResolver. Surfaces as a clear
// configuration error rather than a silent "scope can't be verified".
var ErrNoKeyResolver = errors.New("cortex: scope used but no KeyResolver injected (use WithKeyResolver)")

// VerifyScope runs the once-per-call scope verification chain
// (signature, schema version, expiry, snapshot resolvability,
// multi-proof). Returns nil on success. Callers (Find/Context/
// ResolveScoped/UpdateHead) MUST call this before any per-candidate
// enforceRead/enforceWrite check; otherwise they'd be applying
// scope.Allows to a scope whose authenticity hasn't been established.
//
// `now` is the wall-clock used for expiry comparison. Production
// callers pass c.now(); tests pass a fixed time.
func (c *Cortex) VerifyScope(s *scope.Scope, now time.Time) error {
	if s == nil {
		return nil
	}
	if c.keyResolver == nil {
		return ErrNoKeyResolver
	}
	if s.Actor != "" && s.Actor != c.s.Actor() {
		return fmt.Errorf("%w: scope.Actor=%s store.Actor=%s", scope.ErrActorMismatch, s.Actor, c.s.Actor())
	}
	return scope.Verify(s, c.snap, c.keyResolver, scope.VerifyOpts{Now: now})
}

// enforceRead is called per-candidate in read paths (Find / Context /
// ResolveScoped). nil scope means actor-direct access — no constraint.
//
// On scope.Allows(h)==false: journals a KindScopeViolation entry
// (best-effort; the read still returns the error so the caller
// sees the violation regardless of journal write success), and
// returns scope.ErrViolation.
func (c *Cortex) enforceRead(s *scope.Scope, h *memory.Head) error {
	if s == nil || h == nil {
		return nil
	}
	if !s.Allows(h) {
		c.logScopeViolation(s, h.ID, "violation", "read")
		return fmt.Errorf("%w: id=%s type=%s", scope.ErrViolation, h.ID, h.Type)
	}
	return nil
}

// enforceWrite is called per-target in write paths (UpdateHead and any
// future scoped writes). Requires scope.Writable=true (Q7) AND
// scope.Allows on the current Head. Both failure modes journal a
// KindScopeViolation with the appropriate Reason code.
func (c *Cortex) enforceWrite(s *scope.Scope, h *memory.Head) error {
	if s == nil {
		return nil
	}
	if !s.Writable {
		var id memory.ID
		if h != nil {
			id = h.ID
		}
		c.logScopeViolation(s, id, "not_writable", "write")
		return fmt.Errorf("%w: granted_to=%s", scope.ErrNotWritable, s.GrantedTo)
	}
	if h != nil && !s.Allows(h) {
		c.logScopeViolation(s, h.ID, "violation", "write")
		return fmt.Errorf("%w: id=%s type=%s", scope.ErrViolation, h.ID, h.Type)
	}
	return nil
}

// enforceContextBudget is the Context-path companion to enforceRead. If
// scope.BudgetTokens > 0, the caller's requested BudgetTokens cannot
// exceed it. Returns ErrBudgetExceeded on overage; nil otherwise.
func (c *Cortex) enforceContextBudget(s *scope.Scope, requested int) error {
	if s == nil || s.BudgetTokens <= 0 || requested <= 0 {
		return nil
	}
	if requested > s.BudgetTokens {
		return fmt.Errorf("%w: requested=%d cap=%d", scope.ErrBudgetExceeded, requested, s.BudgetTokens)
	}
	return nil
}

// logScopeViolation appends a KindScopeViolation entry to the journal.
// Best-effort: errors are swallowed (the caller's primary error path
// is the violation itself, not the audit write). This still
// participates in the MMR via the JournalHook installed in
// cortex.New, so OverallRoot moves on every violation — making the
// audit trail tamper-evident.
func (c *Cortex) logScopeViolation(s *scope.Scope, mid memory.ID, reason, mode string) {
	if s == nil {
		return
	}
	// Phase 14 R5 gate: bound the Pebble-sync + MMR-cascade cost a
	// malicious sub-agent can impose by looping scope violations.
	// Under-rate: journal as before. Over-rate: drop the journal write
	// silently. The caller's primary error path (scope.ErrViolation)
	// has already been returned upstack, so callers see identical
	// semantics either way. See ratelimit.go Q5 lock for rationale.
	now := c.now()
	if !c.rl.allowScopeViolation(s.GrantedTo, s.GrantedBy, now) {
		return
	}
	payload := &journal.ScopeViolationPayload{
		SchemaVersion: 1,
		GrantedTo:     s.GrantedTo,
		GrantedBy:     s.GrantedBy,
		MemoryID:      [16]byte(mid),
		Reason:        reason,
		Mode:          mode,
	}
	enc, err := journal.EncodeScopeViolationPayload(payload)
	if err != nil {
		return
	}
	wb := c.s.BeginWrite()
	defer wb.Abort()
	je := &journal.Entry{
		Kind:      journal.KindScopeViolation,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(s.GrantedTo),
		Payload:   enc,
	}
	if err := wb.AppendJournal(je); err != nil {
		return
	}
	_ = wb.Commit()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
