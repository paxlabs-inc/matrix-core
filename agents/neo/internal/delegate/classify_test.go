// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package delegate

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestClassOf_HTTPStatus pins the shared taxonomy against the REAL httpError
// shapes the delegate returns from a non-2xx daemon response. Bodies are the
// actual daemon strings (e.g. the deterministic-ID 409 "already exists in
// async registry" from daemon_async.CreateQueued, and the single-flight 409
// "another intent is in flight").
func TestClassOf_HTTPStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   FailureClass
	}{
		{"500 internal", 500, "boom", ClassTransient},
		{"503 unavailable", 503, "upstream down", ClassTransient},
		{"429 rate-limit", 429, "slow down", ClassTransient},
		{"409 already-exists is a conflict", 409, `intent "P3YNGER" already exists in async registry`, ClassConflict},
		{"409 single-flight busy is transient", 409, "another intent is in flight; retry after current message completes", ClassTransient},
		{"422 clarify is pending", 422, `{"clarify":{"slot_name":"token","prompt":"which token?"}}`, ClassPending},
		{"403 leash denial is deterministic", 403, "wallet mode does not allow this action", ClassDeterministic},
		{"400 validation is deterministic", 400, "invalid intent", ClassDeterministic},
		{"404 not found is deterministic", 404, "no such intent", ClassDeterministic},
	}
	for _, c := range cases {
		err := &httpError{status: c.status, body: c.body}
		if got := ClassOf(err); got != c.want {
			t.Errorf("%s: ClassOf = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExistingIntentID verifies the in-flight intent id is recovered from a
// real daemon conflict body (and through the seam's wrap chain), so a 409 can
// be attached-to rather than re-submitted.
func TestExistingIntentID(t *testing.T) {
	// The daemon's real conflict body: {"error":"intent \"ID\" already exists ..."}.
	body := `{"error":"intent \"01HZX9QABCDEF0123456789TVWX\" already exists in async registry"}`
	conflict := &httpError{status: 409, body: body}
	if got := existingIntentID(conflict); got != "01HZX9QABCDEF0123456789TVWX" {
		t.Errorf("existingIntentID = %q, want the quoted intent id", got)
	}
	// Survives the seam's wrap chain (errors.As must still find the httpError).
	wrapped := fmt.Errorf("core_execute: %w", fmt.Errorf("submit: %w", conflict))
	if got := existingIntentID(wrapped); got != "01HZX9QABCDEF0123456789TVWX" {
		t.Errorf("existingIntentID(wrapped) = %q, want the quoted intent id", got)
	}
	// A non-HTTP error carries no intent id.
	if got := existingIntentID(fmt.Errorf("connection refused")); got != "" {
		t.Errorf("existingIntentID(non-http) = %q, want empty", got)
	}
	// A conflict body without a quoted id yields empty (no false positive).
	if got := existingIntentID(&httpError{status: 409, body: `{"error":"already exists"}`}); got != "" {
		t.Errorf("existingIntentID(no-id) = %q, want empty", got)
	}
}

func TestClassOf_SurvivesWrapping(t *testing.T) {
	base := &httpError{status: 403, body: "read_only mode: action denied"}
	wrapped := fmt.Errorf("core_execute: %w", fmt.Errorf("could not reach the secure execution path: %w", base))
	if got := ClassOf(wrapped); got != ClassDeterministic {
		t.Errorf("ClassOf(wrapped) = %v, want deterministic", got)
	}
}

// TestClassOf_NonHTTP covers the non-status shapes: a nil error and context
// errors are unclassified; any other transport error is transient.
func TestClassOf_NonHTTP(t *testing.T) {
	if got := ClassOf(nil); got != ClassNone {
		t.Errorf("ClassOf(nil) = %v, want none", got)
	}
	if got := ClassOf(context.Canceled); got != ClassNone {
		t.Errorf("ClassOf(context.Canceled) = %v, want none", got)
	}
	if got := ClassOf(context.DeadlineExceeded); got != ClassNone {
		t.Errorf("ClassOf(context.DeadlineExceeded) = %v, want none", got)
	}
	if got := ClassOf(errors.New("connection refused")); got != ClassTransient {
		t.Errorf("ClassOf(transport error) = %v, want transient", got)
	}
}

// TestHTTPError_MessageUnchanged guards the log/diagnostic contract: the typed
// error renders identically to the prior fmt.Errorf("http %d: %s", ...).
func TestHTTPError_MessageUnchanged(t *testing.T) {
	e := &httpError{status: 409, body: "already exists"}
	if got, want := e.Error(), "http 409: already exists"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
