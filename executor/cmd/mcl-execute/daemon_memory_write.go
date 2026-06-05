// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_memory_write.go — POST /memory route (sess#29).
//
// Single write surface for the Tauri shell's onboarding wizard:
//
//   POST /memory  { "type": "Identity", "data": {…}, "tags": [...], ... }
//        → 201 with memorySummaryDTO of the new memory
//
// Sess#27 lock said /settings is read-only at v1 (CLI-flag-only); writes
// to that surface stay deferred to v1.1. /memory IS data not settings,
// so a write surface here is consistent with the cortex.Write architecture
// — every entry journals + is replayable + participates in OverallRoot.
//
// Auth: authAny (matches the read surface). The local-auth bearer token
// from the Tauri shell is sufficient.
//
// Rate limit: best-effort N-per-minute per authenticated identity. Reuses
// a tiny in-memory token bucket so we don't have to touch cortex's
// existing rate limiter (which gates KindScopeViolation + KindAttest at
// the cortex layer; this is HTTP-layer protection for an entirely
// different DoS surface). Defaults: 30 writes / 60s burst 60 per (user).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
)

// memoryWriteRequest is the wire-shape accepted by POST /memory.
type memoryWriteRequest struct {
	Type               string          `json:"type"`
	Data               json.RawMessage `json:"data"`
	Tags               []string        `json:"tags,omitempty"`
	DeclaredImportance uint8           `json:"declared_importance,omitempty"`
	Visibility         string          `json:"visibility,omitempty"`  // "private"|"scoped"|"actor_public"
	ActorScope         string          `json:"actor_scope,omitempty"` // override; defaults to daemon's actor
	CreatedBy          string          `json:"created_by,omitempty"`  // override; defaults to authenticated user
	Confidence         float32         `json:"confidence,omitempty"`
	ProvenanceSource   string          `json:"provenance_source,omitempty"`
}

// memoryWriteLimiter — minimal sliding-counter rate limiter keyed by
// the authenticated user. 60-second window, configurable burst.
type memoryWriteLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	limit    int
	requests map[string][]time.Time
}

func newMemoryWriteLimiter(limit int, window time.Duration) *memoryWriteLimiter {
	return &memoryWriteLimiter{
		window:   window,
		limit:    limit,
		requests: make(map[string][]time.Time),
	}
}

// Allow returns true if the key may proceed, false if rate-limited.
// Garbage-collects stale entries inline so the map size tracks active
// callers, not lifetime callers.
func (l *memoryWriteLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	history := l.requests[key]
	// Drop entries older than cutoff. History is append-only chronological
	// so we can binary-trim from the front in O(log n) but linear is fine
	// at this scale (limit is tens, not thousands).
	pruned := history[:0]
	for _, ts := range history {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	if len(pruned) >= l.limit {
		l.requests[key] = pruned
		return false
	}
	pruned = append(pruned, now)
	l.requests[key] = pruned
	return true
}

const (
	// memoryWriteRateLimit is per-user per-window. 30 writes/minute is
	// generous for the wizard surface (~1 write per onboarding) plus any
	// future skill-authored memory-create. Honest abuse needs to exceed
	// this; legitimate flows nowhere near it.
	memoryWriteRateLimit  = 30
	memoryWriteRateWindow = 60 * time.Second
)

// handleMemoryWrite serves POST /memory.
//
// Body shape:
//
//	{
//	  "type":  "Identity" | "Fact" | ... | "Pattern",
//	  "data":  { ... }   // type-specific JSON; matches memory.<Type>Data
//	  "tags":  ["..."],
//	  "declared_importance": 0-255,
//	  "visibility":          "private" | "scoped" | "actor_public",
//	  "created_by":          "matrix://user/<did>",  // optional override
//	  "actor_scope":         "...",                  // optional override
//	  "confidence":          0.0-1.0,
//	  "provenance_source":   "<short tag>"
//	}
//
// On success returns 201 + memorySummaryDTO of the new memory.
func (d *daemonState) handleMemoryWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	ctx, ok := d.requireAuthPolicy(w, r, authAny)
	if !ok {
		return
	}
	if d.infra == nil || d.infra.cortex == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cortex not enabled"})
		return
	}

	// Rate limit per authenticated user (or per local-auth admin if no
	// user id present — local dev / Tauri case).
	rateKey := userIDFromContext(ctx)
	if rateKey == "" {
		if isAdminFromContext(ctx) {
			rateKey = "local-admin"
		} else {
			rateKey = r.RemoteAddr
		}
	}
	if !d.memoryWriteLimiter().Allow(rateKey, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
			"error":            "rate limit exceeded",
			"retry_after_sec":  60,
			"limit_per_window": memoryWriteRateLimit,
			"window_sec":       int(memoryWriteRateWindow / time.Second),
		})
		return
	}

	// Body parse + size cap (1 MiB is generous; Pattern.Coverage notes
	// the largest Data type stays well under this).
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req memoryWriteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "decode body: " + err.Error(),
		})
		return
	}

	// Resolve memory type.
	memType := parseTypeNameDTO(req.Type)
	if memType == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown memory type %q (want one of Identity/Fact/Preference/Belief/Event/Goal/Constraint/Capability/Pattern)", req.Type),
		})
		return
	}

	// Decode the typed Data body. The cortex memory.* types use cbor
	// tags, but encoding/json honors the same struct field names with
	// case-insensitive matching, so the wire form is JSON with PascalCase
	// keys (per the IdentityData{SchemaVersion, Name, DID, Wallets, ...}
	// shape — see matrix.kvx CORTEX_TYPED_DATA_LLM_PITFALLS for the same
	// constraints applied to LLM-emitted shapes).
	if len(req.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "data field is required",
		})
		return
	}
	typedData, err := decodeTypedData(memType, req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "decode data: " + err.Error(),
		})
		return
	}

	// Visibility
	var visibility memory.Visibility
	switch strings.ToLower(strings.TrimSpace(req.Visibility)) {
	case "", "private":
		visibility = memory.VisPrivate
	case "scoped":
		visibility = memory.VisScoped
	case "actor_public", "actor-public", "actorpublic":
		visibility = memory.VisActorPublic
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown visibility %q", req.Visibility),
		})
		return
	}

	// Head construction. ActorScope defaults to the daemon's bound
	// actor; tags get cast to memory.Tag through a tiny loop.
	actorScope := strings.TrimSpace(req.ActorScope)
	if actorScope == "" {
		// daemon's bound actor name is in d.actor.UserURI; cortex stores
		// the ActorScope as a free-form actor name string, with the
		// daemon actor's owner URI being a stable per-install value.
		actorScope = d.actor.UserURI
	}
	tags := make([]memory.Tag, 0, len(req.Tags))
	for _, raw := range req.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if len(tag) > 64 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("tag too long (max 64): %q", tag),
			})
			return
		}
		tags = append(tags, memory.Tag(tag))
	}
	head := memory.Head{
		ActorScope:         actorScope,
		Visibility:         visibility,
		DeclaredImportance: req.DeclaredImportance,
		Tags:               tags,
	}

	// Meta. CreatedBy defaults to the authenticated user (Tauri local-
	// auth → daemon owner URI; hosted → router-injected supabase user).
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		if userID := userIDFromContext(ctx); userID != "" {
			createdBy = "matrix://user/" + userID
		} else if d.actor != nil {
			createdBy = d.actor.UserURI
		}
	}
	meta := cortex.WriteMeta{
		CreatedBy:  createdBy,
		Confidence: req.Confidence,
	}
	if req.ProvenanceSource != "" {
		meta.Provenance = memory.Provenance{
			Source: memory.SourceKind(req.ProvenanceSource),
		}
	}

	// Write.
	uri, err := d.infra.cortex.Write(head, typedData, meta)
	if err != nil {
		// Validation errors are caller-fixable; surface as 400. Other
		// errors are server-side, return 500.
		if isValidationError(err) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "cortex.Write: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "cortex.Write: " + err.Error(),
		})
		return
	}

	// Resolve the freshly-written memory so we can return the canonical
	// summary shape (with hash + rendered forms) the read routes use.
	mem, err := d.infra.cortex.Resolve(uri)
	if err != nil {
		// Wrote successfully but couldn't read back — surface a minimal
		// success response so the client can still proceed.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uri":  string(uri),
			"warn": "wrote ok but read-back failed: " + err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(memToSummary(mem))
}

// decodeTypedData decodes a JSON Data payload into the appropriate
// memory.TypedData concrete type. Mirrors the per-type switch in
// memory/validate.go:121 (validateIdentity) etc.
func decodeTypedData(t memory.Type, raw json.RawMessage) (memory.TypedData, error) {
	switch t {
	case memory.TypeIdentity:
		var d memory.IdentityData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeFact:
		var d memory.FactData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypePreference:
		var d memory.PreferenceData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeBelief:
		var d memory.BeliefData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeEvent:
		var d memory.EventData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeGoal:
		var d memory.GoalData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeConstraint:
		var d memory.ConstraintData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypeCapability:
		var d memory.CapabilityData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case memory.TypePattern:
		var d memory.PatternData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	}
	return nil, errors.New("unknown memory type")
}

// isValidationError reports whether an error from cortex.Write is
// caller-fixable (missing required fields, bad enum values, etc.) vs.
// server-side (Pebble I/O, encoding bugs). The memory package returns
// errors prefixed "validate" for the caller-fixable class.
func isValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "validate") ||
		strings.Contains(msg, "required") ||
		strings.Contains(msg, "must be") ||
		strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "unknown")
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
