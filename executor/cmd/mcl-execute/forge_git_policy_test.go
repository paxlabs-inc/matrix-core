// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"errors"
	"strings"
	"testing"
)

// TestDefaultGitOpsPolicy_AllowDeny verifies the Phase 3 lock surface:
// reads + safe mutations allowed; push / force_push / reset_hard /
// clean denied even though they appear in AllGitOps().
func TestDefaultGitOpsPolicy_AllowDeny(t *testing.T) {
	p := DefaultGitOpsPolicy()

	cases := []struct {
		op      GitOp
		wantErr bool
	}{
		// Allowed
		{GitOpStatus, false},
		{GitOpDiff, false},
		{GitOpBranchCreate, false},
		{GitOpBranchList, false},
		{GitOpBranchDelete, false},
		{GitOpMerge, false},
		// Denied
		{GitOpPush, true},
		{GitOpForcePush, true},
		{GitOpResetHard, true},
		{GitOpClean, true},
		// Unknown
		{GitOp("rebase"), true},
	}
	for _, c := range cases {
		err := p.CheckOp(c.op)
		if c.wantErr && err == nil {
			t.Errorf("CheckOp(%q) = nil, want denial", c.op)
		}
		if !c.wantErr && err != nil {
			t.Errorf("CheckOp(%q) = %v, want nil", c.op, err)
		}
	}
}

// TestGitOpsPolicy_DenyOverridesAllow ensures DeniedVerbs wins even
// when the same verb is in AllowedVerbs (defense in depth).
func TestGitOpsPolicy_DenyOverridesAllow(t *testing.T) {
	p := &GitOpsPolicy{
		AllowedVerbs: []GitOp{GitOpStatus, GitOpPush}, // accidentally allows push
		DeniedVerbs:  []GitOp{GitOpPush},              // denies it
	}
	if err := p.CheckOp(GitOpPush); err == nil {
		t.Errorf("DeniedVerbs must win over AllowedVerbs")
	}
	if !errors.Is(err(p, GitOpPush), ErrGitOpDenied) {
		t.Errorf("error must wrap ErrGitOpDenied")
	}
	if err := p.CheckOp(GitOpStatus); err != nil {
		t.Errorf("CheckOp(status) = %v, want nil", err)
	}
}

// err helper for testing.
func err(p *GitOpsPolicy, op GitOp) error { return p.CheckOp(op) }

// TestGitOpsPolicy_CheckRepo enforces repo-mismatch refusal.
func TestGitOpsPolicy_CheckRepo(t *testing.T) {
	p := DefaultGitOpsPolicy()

	if err := p.CheckRepo(""); err != nil {
		t.Errorf("empty repo (use default) must succeed; got %v", err)
	}
	if err := p.CheckRepo("/root/matrix"); err != nil {
		t.Errorf("matching repo must succeed; got %v", err)
	}
	if err := p.CheckRepo("/root/matrix/"); err != nil {
		t.Errorf("trailing slash should normalize; got %v", err)
	}
	if err := p.CheckRepo("/root/other"); err == nil {
		t.Errorf("mismatched repo must fail")
	} else if !errors.Is(err, ErrGitRepoMismatch) {
		t.Errorf("error must wrap ErrGitRepoMismatch; got %v", err)
	}
	if err := p.CheckRepo("relative/path"); err == nil {
		t.Errorf("relative path must fail (NormalizePath)")
	}
}

// TestGitOpsPolicy_IsProtectedRef covers main / master / HEAD.
func TestGitOpsPolicy_IsProtectedRef(t *testing.T) {
	p := DefaultGitOpsPolicy()
	for _, ref := range []string{"main", "master", "HEAD"} {
		if !p.IsProtectedRef(ref) {
			t.Errorf("IsProtectedRef(%q) = false, want true", ref)
		}
	}
	for _, ref := range []string{"feature/x", "forge/abc123", "develop"} {
		if p.IsProtectedRef(ref) {
			t.Errorf("IsProtectedRef(%q) = true, want false", ref)
		}
	}
}

// TestValidateRefName covers the strict allowlist + edge cases.
func TestValidateRefName(t *testing.T) {
	good := []string{
		"main",
		"forge/01ABCXYZ",
		"feature/add-cache",
		"v1.0.0",
		"a-b_c.d/e",
	}
	for _, r := range good {
		if err := ValidateRefName(r); err != nil {
			t.Errorf("ValidateRefName(%q) = %v, want nil", r, err)
		}
	}
	bad := []struct {
		ref  string
		want string
	}{
		{"", "empty"},
		{"-bad", "leading dash"},
		{"foo..bar", "contains .."},
		{"foo bar", "invalid char"},
		{"foo;rm -rf", "invalid char"},
		{"foo$bar", "invalid char"},
		{"foo`bar", "invalid char"},
		{"foo|bar", "invalid char"},
		{"/leading", "leading/trailing slash"},
		{"trailing/", "leading/trailing slash"},
		{strings.Repeat("a", 241), "too long"},
	}
	for _, c := range bad {
		err := ValidateRefName(c.ref)
		if err == nil {
			t.Errorf("ValidateRefName(%q) must reject (%s)", c.ref, c.want)
			continue
		}
		if !errors.Is(err, ErrGitInvalidRef) {
			t.Errorf("ValidateRefName(%q) sentinel = %v, want ErrGitInvalidRef", c.ref, err)
		}
	}
}

// TestGitOpsPolicy_NilSafeDeny ensures policy nil produces a clean
// error rather than panicking. Defensive — handlers 404 before this
// path, but a future refactor could expose it.
func TestGitOpsPolicy_NilSafeDeny(t *testing.T) {
	var p *GitOpsPolicy
	if err := p.CheckOp(GitOpStatus); err == nil {
		t.Errorf("nil policy CheckOp must error")
	}
	if err := p.CheckRepo("/root/matrix"); err == nil {
		t.Errorf("nil policy CheckRepo must error")
	}
	if p.IsProtectedRef("main") {
		t.Errorf("nil policy IsProtectedRef must return false (defensive)")
	}
}

// TestAllGitOps_Stable guards the closed-enum contract: every verb the
// route layer dispatches over MUST be in AllGitOps() so the audit
// surface stays comprehensive.
func TestAllGitOps_Stable(t *testing.T) {
	want := map[GitOp]bool{
		GitOpStatus:       true,
		GitOpDiff:         true,
		GitOpBranchCreate: true,
		GitOpBranchList:   true,
		GitOpBranchDelete: true,
		GitOpMerge:        true,
		GitOpPush:         true,
		GitOpForcePush:    true,
		GitOpResetHard:    true,
		GitOpClean:        true,
	}
	got := AllGitOps()
	if len(got) != len(want) {
		t.Errorf("AllGitOps len = %d, want %d", len(got), len(want))
	}
	for _, op := range got {
		if !want[op] {
			t.Errorf("AllGitOps contained unexpected op %q", op)
		}
		delete(want, op)
	}
	for op := range want {
		t.Errorf("AllGitOps missing op %q", op)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
