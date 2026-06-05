// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"errors"
	"testing"
)

// TestNormalizePath enforces the absolute-path contract.
func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/root/matrix/foo", "/root/matrix/foo", false},
		{"/root/matrix/foo/../bar", "/root/matrix/bar", false},
		{"/root/matrix//foo", "/root/matrix/foo", false},
		{"/root/matrix/", "/root/matrix", false},
		{"relative/path", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := NormalizePath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizePath(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizePath(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDefaultForgeFSPolicy_AllowDeny exercises the Q3=c lock surface:
//   - everything under /root/matrix is reachable
//   - cortex/store, knowledge, journal are denied (BOTH read AND write)
//   - paths outside /root/matrix are outside-allowlist
func TestDefaultForgeFSPolicy_AllowDeny(t *testing.T) {
	p := DefaultForgeFSPolicy()

	cases := []struct {
		path        string
		canRead     bool
		canWrite    bool
		expectedErr error // expected sentinel from CheckRead OR CheckWrite (read-first)
	}{
		// Reachable + writable
		{"/root/matrix/cortex/keys/keys.go", true, true, nil},
		{"/root/matrix/MCL/llm/identity.go", true, true, nil},
		{"/root/matrix/skills/brainstorming/SKILL.mtx", true, true, nil},
		{"/root/matrix", true, true, nil},

		// Reachable parent but denied child (the cortex/store DB)
		{"/root/matrix/cortex/store", false, false, ErrPathDenied},
		{"/root/matrix/cortex/store/000001.sst", false, false, ErrPathDenied},

		// Reachable parent but denied (knowledge — kvx + whitepaper)
		{"/root/matrix/knowledge/matrix.kvx", false, false, ErrPathDenied},

		// Reachable parent but denied (journal — envelope chain audit log)
		{"/root/matrix/journal/logs/intent-abc/0001.json", false, false, ErrPathDenied},

		// Sibling that shares a textual prefix — must NOT be denied
		{"/root/matrix/cortex/storefoo", true, true, nil},

		// Outside the allowlist entirely
		{"/etc/passwd", false, false, ErrPathOutsideAllowlist},
		{"/root", false, false, ErrPathOutsideAllowlist},
	}

	for _, c := range cases {
		readErr := p.CheckRead(c.path)
		writeErr := p.CheckWrite(c.path)

		if (readErr == nil) != c.canRead {
			t.Errorf("CheckRead(%q) err=%v, canRead=%v", c.path, readErr, c.canRead)
		}
		if (writeErr == nil) != c.canWrite {
			t.Errorf("CheckWrite(%q) err=%v, canWrite=%v", c.path, writeErr, c.canWrite)
		}
		if c.expectedErr != nil {
			if !errors.Is(readErr, c.expectedErr) {
				t.Errorf("CheckRead(%q) sentinel = %v, want %v", c.path, readErr, c.expectedErr)
			}
		}
	}
}

// TestDefaultForgeFSPolicy_ReadOnlyPrefix guards the read-only carve-out
// path even though Phase 1 ships nothing read-only. Future skills may
// want to mark e.g. agents/forge.json as read-only.
func TestDefaultForgeFSPolicy_ReadOnlyPrefix(t *testing.T) {
	p := DefaultForgeFSPolicy()
	p.ReadOnlyPrefixes = []string{"/root/matrix/agents"}

	if err := p.CheckRead("/root/matrix/agents/forge.json"); err != nil {
		t.Errorf("CheckRead under ReadOnlyPrefix must succeed: %v", err)
	}
	werr := p.CheckWrite("/root/matrix/agents/forge.json")
	if werr == nil {
		t.Errorf("CheckWrite under ReadOnlyPrefix must reject")
	}
	if !errors.Is(werr, ErrPathReadOnly) {
		t.Errorf("CheckWrite sentinel = %v, want ErrPathReadOnly", werr)
	}
}

// TestForgeFSPolicy_NilPolicySafeDeny ensures CheckRead/CheckWrite on a
// nil policy fails cleanly rather than panicking. Defensive — the
// daemon dispatcher 404s before reaching the check, but a future caller
// could still hit this path.
func TestForgeFSPolicy_NilPolicySafeDeny(t *testing.T) {
	var p *ForgeFSPolicy
	if err := p.CheckRead("/root/matrix/x"); err == nil {
		t.Errorf("nil policy CheckRead must error")
	}
}

// TestIsUnderPrefix_EdgeCases enforces the path-separator boundary so
// sibling directories that share textual prefixes don't get falsely
// matched as descendants.
func TestIsUnderPrefix_EdgeCases(t *testing.T) {
	cases := []struct {
		path   string
		prefix string
		want   bool
	}{
		{"/root/matrix", "/root/matrix", true},             // exact equality
		{"/root/matrix/foo", "/root/matrix", true},         // strict descendant
		{"/root/matrixfoo", "/root/matrix", false},         // sibling textual prefix
		{"/root/matrix/foo/bar", "/root/matrix/foo", true}, // deeper descendant
		{"/root/matrix/foobar", "/root/matrix/foo", false}, // sibling at deeper level
		{"/root/matrix", "/root/matrix/foo", false},        // parent of prefix
		{"", "/root/matrix", false},                        // empty path
	}
	for _, c := range cases {
		got := isUnderPrefix(c.path, c.prefix)
		if got != c.want {
			t.Errorf("isUnderPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
