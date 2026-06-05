// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// forge_policy.go — filesystem allow/deny policy for the Forge HTTP surface
// (Session 34 / Forge Phase 1, matrix.kvx sess#34).
//
// Forge is the local self-maintenance Matrix instance (Opus 4.7 + GPT-5.5
// powered) whose sole purpose is to optimize its own codebase at /root/matrix.
// The Vue SPA at forge/ (Phase 2) reaches the daemon's GET /fs/tree, GET
// /fs/read, and POST /fs/write routes to render its file tree, hydrate
// Monaco, and persist Monaco edits. The agent's MCP fs server (agents/
// forge.json) is the parallel reach path; both MUST honour the same
// allowlist + denylist so a human-mode write and an agent-mode write are
// indistinguishable from a safety posture.
//
// Q3=c (matrix.kvx sess#34 lock): full write everywhere under /root/matrix
// EXCEPT cortex/store/ + knowledge/ + journal/. Those three protect cortex
// DB integrity (replay §13.4 invariant), the kvx ledger, and the envelope
// chain audit log respectively. Git history is the recovery anchor for
// every other path.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ForgeFSPolicy encodes the read/write allowlist + denylist for the Forge
// HTTP filesystem surface AND the parallel MCP fs server. Loaded from
// daemon CLI flags (default: DefaultForgeFSPolicy) or from agents/forge.json
// when forge-mode is enabled.
type ForgeFSPolicy struct {
	// AllowRoots are absolute directory prefixes any reachable path MUST
	// sit under. Empty AllowRoots means no path is reachable (default-
	// deny). Matched after filepath.Clean + symlink resolution.
	AllowRoots []string

	// DenyPrefixes are absolute prefixes that, when they match a
	// cleaned path, hard-deny BOTH read AND write. Match is path-
	// prefix-with-separator (a deny on "/root/matrix/cortex/store"
	// rejects "/root/matrix/cortex/store" AND "/root/matrix/cortex/
	// store/X" but NOT "/root/matrix/cortex/storefoo").
	DenyPrefixes []string

	// ReadOnlyPrefixes flag specific paths as readable but never
	// writable. Distinct from DenyPrefixes which blocks BOTH ops.
	// Empty by default — Phase 1 ships nothing read-only.
	ReadOnlyPrefixes []string

	// MaxReadBytes caps GET /fs/read responses. Files larger than this
	// 413 out instead of streaming. Default 4 MiB; Monaco-large files
	// (vendor bundles, generated SQL dumps) are exempt via .gitignore
	// at the SPA side rather than blanket-allowing them.
	MaxReadBytes int64

	// MaxWriteBytes caps POST /fs/write payloads. Default 4 MiB.
	// Defensive against runaway streaming uploads; not a security
	// boundary (the auth token is).
	MaxWriteBytes int64
}

// DefaultForgeFSPolicy returns the Phase 1 self-maintenance policy:
//
//	AllowRoots       = [/root/matrix]
//	DenyPrefixes     = [/root/matrix/cortex/store,
//	                    /root/matrix/knowledge,
//	                    /root/matrix/journal]
//	ReadOnlyPrefixes = []        (every reachable path is RW)
//	MaxReadBytes     = 4 MiB
//	MaxWriteBytes    = 4 MiB
//
// The denylist protects the cortex Pebble DB + the kvx ledger + the
// envelope chain audit log. Git history is the recovery anchor for
// every other path so writes elsewhere can be safely reverted.
func DefaultForgeFSPolicy() *ForgeFSPolicy {
	return &ForgeFSPolicy{
		AllowRoots: []string{"/root/matrix"},
		DenyPrefixes: []string{
			"/root/matrix/cortex/store",
			"/root/matrix/knowledge",
			"/root/matrix/journal",
		},
		ReadOnlyPrefixes: nil,
		MaxReadBytes:     4 * 1024 * 1024,
		MaxWriteBytes:    4 * 1024 * 1024,
	}
}

// ErrPathOutsideAllowlist signals the path is not under any AllowRoot.
var ErrPathOutsideAllowlist = errors.New("forge_policy: path outside allowlist")

// ErrPathDenied signals the path matches a DenyPrefix entry.
var ErrPathDenied = errors.New("forge_policy: path denied")

// ErrPathReadOnly signals the path is reachable but write-rejected.
var ErrPathReadOnly = errors.New("forge_policy: path is read-only")

// ErrPathNotAbsolute signals a relative or empty path was supplied.
var ErrPathNotAbsolute = errors.New("forge_policy: path must be absolute")

// NormalizePath cleans + asserts absolute. Returns the canonical path or
// an error. Does NOT resolve symlinks (callers do that via os.Lstat +
// filepath.EvalSymlinks when they want the strongest guarantee; cheap
// path-string checks happen first to short-circuit obvious denials).
func NormalizePath(path string) (string, error) {
	if path == "" {
		return "", ErrPathNotAbsolute
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q", ErrPathNotAbsolute, path)
	}
	clean := filepath.Clean(path)
	return clean, nil
}

// CheckRead returns nil iff the path is under an AllowRoot AND not under
// any DenyPrefix. Caller MUST pass a path already normalized via
// NormalizePath (uppercase contract — this function does not re-clean).
func (p *ForgeFSPolicy) CheckRead(path string) error {
	if p == nil {
		return errors.New("forge_policy: policy is nil")
	}
	if !p.underAllowRoot(path) {
		return fmt.Errorf("%w: %q", ErrPathOutsideAllowlist, path)
	}
	if p.underDenyPrefix(path) {
		return fmt.Errorf("%w: %q", ErrPathDenied, path)
	}
	return nil
}

// CheckWrite returns nil iff CheckRead passes AND the path is not under
// any ReadOnlyPrefix.
func (p *ForgeFSPolicy) CheckWrite(path string) error {
	if err := p.CheckRead(path); err != nil {
		return err
	}
	if p.underReadOnlyPrefix(path) {
		return fmt.Errorf("%w: %q", ErrPathReadOnly, path)
	}
	return nil
}

// underAllowRoot reports whether path sits under at least one AllowRoot.
// Empty AllowRoots returns false (default-deny).
func (p *ForgeFSPolicy) underAllowRoot(path string) bool {
	for _, root := range p.AllowRoots {
		if isUnderPrefix(path, root) {
			return true
		}
	}
	return false
}

// underDenyPrefix reports whether path sits under at least one DenyPrefix.
func (p *ForgeFSPolicy) underDenyPrefix(path string) bool {
	for _, deny := range p.DenyPrefixes {
		if isUnderPrefix(path, deny) {
			return true
		}
	}
	return false
}

// underReadOnlyPrefix reports whether path sits under at least one
// ReadOnlyPrefix.
func (p *ForgeFSPolicy) underReadOnlyPrefix(path string) bool {
	for _, ro := range p.ReadOnlyPrefixes {
		if isUnderPrefix(path, ro) {
			return true
		}
	}
	return false
}

// isUnderPrefix reports whether `path` equals `prefix` or sits under it
// in the filesystem sense (a separator immediately follows). Path-string
// equality short-circuit avoids false positives for sibling directories
// that share a textual prefix (e.g. "/var/foo" vs "/var/foobar").
//
// Both arguments are expected to be already normalized via filepath.Clean.
func isUnderPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) > len(prefix) && path[len(prefix)] == filepath.Separator
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
