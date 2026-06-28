// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// FailureClass is the single shared taxonomy for an outcome crossing the
// Neo<->MCL seam. Both the tool-dispatch ladder (agent.dispatchWithRetry) and
// the task supervisor (server.superviseDecision) read THIS classification, so
// the retry / stop / attach / ask decision is made once, never re-guessed per
// layer.
type FailureClass int

const (
	// ClassNone means there is no failure to classify (a clean outcome, or an
	// error shape the seam does not recognize — left to the existing path).
	ClassNone FailureClass = iota
	// ClassTransient is safe to retry with bounded backoff: a connection
	// refusal, a timeout, a 5xx, or a rate-limit (429). The single-flight 409
	// ("another intent is in flight; retry") is transient too — it clears once
	// the in-flight intent completes.
	ClassTransient
	// ClassDeterministic will recur identically on a re-submit of the same
	// request: a validation reject, a leash/mode/cap denial (403), a
	// wrong-strategy / not-owner rejection, or an unparseable intent. It is
	// NEVER retried and NEVER respawned over — it is surfaced as a terminal
	// result so the agent stops-and-asks or reports an honest partial.
	ClassDeterministic
	// ClassConflict is the deterministic-ID 409 "already exists in async
	// registry": the work is already in flight. It is NOT a failure — the
	// delegate resolves the existing intent and attaches to it (the
	// attach-to-existing handler, P2).
	ClassConflict
	// ClassPending is a structured 422 needing slot values: a clarify. It is
	// carried back as an actionable slot-fill (the clarify pass-through, P4),
	// not reworded prose.
	ClassPending
)

// String renders the class for diagnostics.
func (c FailureClass) String() string {
	switch c {
	case ClassTransient:
		return "transient"
	case ClassDeterministic:
		return "deterministic"
	case ClassConflict:
		return "conflict"
	case ClassPending:
		return "pending"
	default:
		return "none"
	}
}

// httpError carries the daemon's HTTP status code and (truncated) response body
// so the classifier can map an outcome to a FailureClass. It is the typed error
// the delegate's request helper returns on a non-2xx response; it survives
// fmt.Errorf("...: %w", err) wrapping, so callers far up the chain (the
// dispatch ladder, the supervisor) recover the class via ClassOf.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("http %d: %s", e.status, e.body)
}

// Status reports the HTTP status code carried by the error.
func (e *httpError) Status() int { return e.status }

// classifyHTTP maps an HTTP status + response body to a FailureClass. The
// mapping is the design's taxonomy: 409 already-exists -> conflict, 403 ->
// deterministic, 422 -> pending, 5xx / 429 -> transient, any other 4xx
// (validation / unparseable / not-owner) -> deterministic.
func classifyHTTP(status int, body string) FailureClass {
	switch {
	case status >= 500:
		return ClassTransient
	case status == http.StatusTooManyRequests: // 429 rate-limit
		return ClassTransient
	case status == http.StatusConflict: // 409
		// The deterministic-ID collision ("already exists in async registry")
		// means the work is already in flight: attach, don't fail. A 409 that
		// is NOT an already-exists collision is the single-flight busy lock
		// ("another intent is in flight; retry"), which clears on its own.
		if strings.Contains(strings.ToLower(body), "already exists") {
			return ClassConflict
		}
		return ClassTransient
	case status == http.StatusUnprocessableEntity: // 422 structured clarify
		return ClassPending
	case status == http.StatusForbidden: // 403 leash / mode / cap denial
		return ClassDeterministic
	case status >= 400: // other 4xx: validation reject, not-owner, unparseable
		return ClassDeterministic
	default:
		return ClassNone
	}
}

// existingIntentID recovers the in-flight intent id from a conflict error (a
// 409 "already exists in async registry"), so the delegate can ATTACH to the
// work already underway instead of re-submitting. Returns "" when err is not a
// daemon HTTP error or no id is present in the body.
func existingIntentID(err error) string {
	var he *httpError
	if !errors.As(err, &he) {
		return ""
	}
	return parseExistingIntentID(he.body)
}

// parseExistingIntentID extracts the quoted intent id from a daemon conflict
// body. The daemon returns {"error":"intent \"<ID>\" already exists in async
// registry"}; the id is the first double-quoted token of that message.
func parseExistingIntentID(body string) string {
	msg := body
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && env.Error != "" {
		msg = env.Error
	}
	i := strings.IndexByte(msg, '"')
	if i < 0 {
		return ""
	}
	rest := msg[i+1:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// ClassOf extracts the shared FailureClass carried by err. A daemon HTTP error
// (anywhere in the wrap chain) is mapped by status+body; a context
// cancellation / deadline is left unclassified (the loop handles ctx
// separately); any other non-nil error is treated as transient (a transport
// failure — connection refused, reset, timeout). A nil error is ClassNone.
func ClassOf(err error) FailureClass {
	if err == nil {
		return ClassNone
	}
	var he *httpError
	if errors.As(err, &he) {
		return classifyHTTP(he.status, he.body)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassNone
	}
	return ClassTransient
}
