// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// forge_git_policy.go — git operation allow/deny policy for the Forge HTTP
// surface (Session 36 / Forge Phase 3, matrix.kvx sess#36).
//
// Git ops live alongside ForgeFSPolicy as the second trust gate. Where
// ForgeFSPolicy fences the agent's reach over individual files,
// GitOpsPolicy fences which git verbs may run AT ALL on the repo.
// Both gates fire on every reach path: HTTP /git/* and the agent-side
// git MCP server (defense in depth — the MCP server enforces its own
// jail via mcp-server-git's repository arg, the daemon checks the
// gitOps allow/deny at the route layer).
//
// Q3=c lock (matrix.kvx sess#34) carries forward: full self-edit RW
// under /root/matrix EXCEPT the three sacred subdirs. GitOpsPolicy
// adds an OPS axis on top: which verbs are reachable, which branches
// are protected from delete, whether non-fast-forward merges are
// allowed by default.
//
// Phase 3 ships a conservative posture:
//
//	AllowedVerbs  = [status, diff, branch_create, branch_list,
//	                  branch_delete, merge]    (READS + safe MUTATIONS)
//	DeniedVerbs   = [push, force_push, reset_hard, clean]
//	                                            (NEVER auto-runnable)
//	ProtectedRefs = [main, master, HEAD]
//	                  (delete refused; merge target requires
//	                   explicit --no-ff opt-in to land non-ff)
//	DefaultFastForward = true                   (merge -ff-only)

import (
	"errors"
	"fmt"
	"strings"
)

// GitOp is the closed enum of git verbs reachable through the daemon
// HTTP surface. Mirrors the route -> handler dispatch in
// daemon_git_routes.go. Strings are stable: stamped onto the
// `git.op.<verb>` transcript event per call.
type GitOp string

const (
	GitOpStatus       GitOp = "status"
	GitOpDiff         GitOp = "diff"
	GitOpBranchCreate GitOp = "branch_create"
	GitOpBranchList   GitOp = "branch_list"
	GitOpBranchDelete GitOp = "branch_delete"
	GitOpMerge        GitOp = "merge"

	// Deny-by-default verbs — listed so DeniedVerbs in DefaultGitOpsPolicy
	// has a stable contract; daemon does NOT expose routes for these.
	// Future Phase 3.1+ may surface them behind a one-time human gate.
	GitOpPush      GitOp = "push"
	GitOpForcePush GitOp = "force_push"
	GitOpResetHard GitOp = "reset_hard"
	GitOpClean     GitOp = "clean"
)

// AllGitOps lists every defined verb. Used by tests + observability.
func AllGitOps() []GitOp {
	return []GitOp{
		GitOpStatus, GitOpDiff,
		GitOpBranchCreate, GitOpBranchList, GitOpBranchDelete,
		GitOpMerge,
		GitOpPush, GitOpForcePush, GitOpResetHard, GitOpClean,
	}
}

// GitOpsPolicy gates which git verbs the daemon will run + which refs
// are protected from destructive operations.
type GitOpsPolicy struct {
	// Repo is the absolute path to the working tree the daemon will
	// shell `git` against. Every route asserts that the requested
	// repo (when non-empty in the request) NormalizePath-cleans to
	// this exact path; cross-repo reach is refused.
	Repo string

	// AllowedVerbs is the closed set of GitOps reachable. Empty means
	// no verb is allowed (default-deny). Lookup is O(n) but the set
	// is tiny (≤10 entries) so a map is overkill.
	AllowedVerbs []GitOp

	// DeniedVerbs is an explicit blocklist that overrides AllowedVerbs
	// when both contain the same verb (defense in depth).
	DeniedVerbs []GitOp

	// ProtectedRefs are branch / tag names that branch_delete refuses
	// outright. merge targets in this set require ExplicitNoFF=true
	// at the request layer to land a non-fast-forward merge.
	ProtectedRefs []string

	// DefaultFastForward: when true, merge runs `git merge --ff-only`
	// unless the request explicitly opts into --no-ff. When false,
	// merge runs `git merge` (history-preserving merge commit).
	DefaultFastForward bool

	// MaxDiffBytes caps GET /git/diff response size. Files with a
	// diff body larger than this 413 out instead of streaming the
	// full payload. Default 4 MiB (matches ForgeFSPolicy.MaxReadBytes).
	MaxDiffBytes int64
}

// DefaultGitOpsPolicy returns the Phase 3 self-maintenance posture.
//
//	Repo               = /root/matrix
//	AllowedVerbs       = [status, diff, branch_create, branch_list,
//	                      branch_delete, merge]
//	DeniedVerbs        = [push, force_push, reset_hard, clean]
//	ProtectedRefs      = [main, master, HEAD]
//	DefaultFastForward = true
//	MaxDiffBytes       = 4 MiB
//
// The agent self-maintains on feature branches (forge/<intent_id>)
// per agents/forge.json convention; merges to main go fast-forward
// only by default so the human review surface stays visible (a
// non-ff merge commit hides the per-step transcript).
func DefaultGitOpsPolicy() *GitOpsPolicy {
	return &GitOpsPolicy{
		Repo: "/root/matrix",
		AllowedVerbs: []GitOp{
			GitOpStatus, GitOpDiff,
			GitOpBranchCreate, GitOpBranchList, GitOpBranchDelete,
			GitOpMerge,
		},
		DeniedVerbs: []GitOp{
			GitOpPush, GitOpForcePush, GitOpResetHard, GitOpClean,
		},
		ProtectedRefs:      []string{"main", "master", "HEAD"},
		DefaultFastForward: true,
		MaxDiffBytes:       4 * 1024 * 1024,
	}
}

// ErrGitOpDenied signals the requested verb is not reachable under
// this policy.
var ErrGitOpDenied = errors.New("git_policy: op denied")

// ErrGitRefProtected signals an attempt to delete (or non-ff-merge
// without explicit opt-in) a protected ref.
var ErrGitRefProtected = errors.New("git_policy: ref protected")

// ErrGitRepoMismatch signals the requested repo doesn't match the
// policy's Repo field.
var ErrGitRepoMismatch = errors.New("git_policy: repo mismatch")

// ErrGitInvalidRef signals a ref name with shell-metachars or path
// traversal characters. Branches / tags are constrained to git's
// own rules: alphanumerics, -, _, /, .; no leading dash; no '..'.
var ErrGitInvalidRef = errors.New("git_policy: invalid ref name")

// CheckOp returns nil iff the op is in AllowedVerbs AND not in
// DeniedVerbs. Caller uses this on every route entry before shelling
// out; defense in depth alongside route-level method/path checks.
func (p *GitOpsPolicy) CheckOp(op GitOp) error {
	if p == nil {
		return errors.New("git_policy: policy is nil")
	}
	for _, denied := range p.DeniedVerbs {
		if denied == op {
			return fmt.Errorf("%w: %q (blocklisted)", ErrGitOpDenied, op)
		}
	}
	for _, allowed := range p.AllowedVerbs {
		if allowed == op {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (not in allowlist)", ErrGitOpDenied, op)
}

// CheckRepo returns nil iff `repo` (cleaned absolute path) equals
// p.Repo. Empty `repo` is accepted as "use the policy default".
func (p *GitOpsPolicy) CheckRepo(repo string) error {
	if p == nil {
		return errors.New("git_policy: policy is nil")
	}
	if repo == "" {
		return nil
	}
	clean, err := NormalizePath(repo)
	if err != nil {
		return err
	}
	if clean != p.Repo {
		return fmt.Errorf("%w: %q (policy.repo=%q)", ErrGitRepoMismatch, clean, p.Repo)
	}
	return nil
}

// IsProtectedRef reports whether a branch / tag is in ProtectedRefs.
// Used by branch_delete (hard refusal) and merge (non-ff opt-in
// requirement when the target is protected).
func (p *GitOpsPolicy) IsProtectedRef(ref string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.ProtectedRefs {
		if r == ref {
			return true
		}
	}
	return false
}

// ValidateRefName enforces a strict subset of git's ref naming rules.
// Allows: alphanumerics, '-', '_', '/', '.'. Rejects: leading '-'
// (could be parsed as a flag by `git branch`), '..' (path traversal),
// any whitespace, any shell metachar. Rejects refs longer than 240
// chars (git's hard cap is 255 minus the heads/tags/ prefix).
func ValidateRefName(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty", ErrGitInvalidRef)
	}
	if len(ref) > 240 {
		return fmt.Errorf("%w: too long (%d > 240)", ErrGitInvalidRef, len(ref))
	}
	if ref[0] == '-' {
		return fmt.Errorf("%w: leading dash", ErrGitInvalidRef)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("%w: contains ..", ErrGitInvalidRef)
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("%w: leading/trailing slash", ErrGitInvalidRef)
	}
	for _, c := range ref {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '/' || c == '.':
		default:
			return fmt.Errorf("%w: invalid char %q", ErrGitInvalidRef, string(c))
		}
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
