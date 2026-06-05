// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrix/executor/tool"
)

// ActorIdentity bundles the test actor's keypair + DID URI used as the
// envelope From field. ed25519 stdlib; KeyResolver in main wraps Public.
type ActorIdentity struct {
	DID      string
	Public   ed25519.PublicKey
	Private  ed25519.PrivateKey
	UserURI  string // matrix://user/<did> for envelope From
	AgentURI string // matrix://agent/<did> for plan CreatedBy etc.
}

// NewActorIdentity derives a deterministic ed25519 keypair from a fixed
// 32-byte seed so cross-run hashes can match. seedHex must be 64 hex chars.
func NewActorIdentity(seedHex, didLabel string) (*ActorIdentity, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return nil, fmt.Errorf("setup: bad seed hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("setup: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	pubHex := hex.EncodeToString(pub)
	did := "did:matrix:" + didLabel + ":" + pubHex[:16]
	return &ActorIdentity{
		DID:      did,
		Public:   pub,
		Private:  priv,
		UserURI:  "matrix://user/" + did,
		AgentURI: "matrix://agent/" + did,
	}, nil
}

// staticKeyResolver implements envelope.KeyResolver against a single principal.
// Production resolves matrix://agent/<did> against tools/registry; for the
// e2e harness we know exactly one principal (the test actor) and short-circuit.
type staticKeyResolver struct {
	principals map[string]ed25519.PublicKey
}

func (r *staticKeyResolver) ResolveKey(principal string) (ed25519.PublicKey, error) {
	if k, ok := r.principals[principal]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("e2e: unknown principal %q", principal)
}

// Workspace is the per-run directory layout. Every artefact lives under
// Root so a single rm -rf wipes the run.
type Workspace struct {
	Root        string // runs/<ts>/<run>/
	CortexRoot  string // runs/<ts>/<run>/cortex/
	JournalDir  string // runs/<ts>/<run>/journal/  (envelope JSON files)
	WorkspaceFS string // runs/<ts>/<run>/workspace/ (fs-mcp jail)
	GitRepoDir  string // runs/<ts>/<run>/repo/      (git-mcp jail)
	Transcript  string // runs/<ts>/<run>/transcript.jsonl
	StderrLog   string // runs/<ts>/<run>/mcp-stderr.log
}

// NewWorkspace creates the per-run directory tree. All paths absolute.
func NewWorkspace(rootBase, run string) (*Workspace, error) {
	root, err := filepath.Abs(filepath.Join(rootBase, run))
	if err != nil {
		return nil, err
	}
	w := &Workspace{
		Root:        root,
		CortexRoot:  filepath.Join(root, "cortex"),
		JournalDir:  filepath.Join(root, "journal"),
		WorkspaceFS: filepath.Join(root, "workspace"),
		GitRepoDir:  filepath.Join(root, "repo"),
		Transcript:  filepath.Join(root, "transcript.jsonl"),
		StderrLog:   filepath.Join(root, "mcp-stderr.log"),
	}
	for _, d := range []string{w.CortexRoot, w.JournalDir, w.WorkspaceFS, w.GitRepoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("setup: mkdir %s: %w", d, err)
		}
	}
	return w, nil
}

// SeedGitRepo initialises the git-mcp jail with one commit so git_log/git_status
// have something real to report.
func SeedGitRepo(dir string) error {
	cmds := [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "config", "user.email", "e2e@matrix.local"},
		{"git", "config", "user.name", "Matrix E2E"},
	}
	for _, c := range cmds {
		if err := runCmdInDir(dir, c[0], c[1:]...); err != nil {
			return fmt.Errorf("setup: git seed %v: %w", c, err)
		}
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# Matrix E2E test repo\n\nThis is a sandbox repo created by the mcl-e2e harness for git-mcp to operate on.\n"), 0o644); err != nil {
		return err
	}
	for _, c := range [][]string{
		{"git", "add", "README.md"},
		{"git", "commit", "-q", "-m", "initial commit"},
	} {
		if err := runCmdInDir(dir, c[0], c[1:]...); err != nil {
			return fmt.Errorf("setup: git seed %v: %w", c, err)
		}
	}
	return nil
}

// BuildAgentManifest synthesises a Q22-compliant tool.AgentManifest pinned
// at the e2e-installed packages with deterministic synthetic digests.
//
// Real production deployments compute PackageDigest as sha256(installed
// tarball); for the harness we synthesise a stable digest from a canonical
// "<provider>:<package>@<version>" string so manifest validation passes
// while still being audit-stable across runs.
func BuildAgentManifest(agentDID, fsJail, gitRepoDir string) *tool.AgentManifest {
	return &tool.AgentManifest{
		SchemaVersion:      1,
		Agent:              "matrix://agent/" + agentDID,
		Description:        "Matrix e2e test agent (mcl-e2e harness)",
		AllowedSideEffects: []string{"read", "write", "network", "shell"},
		Servers: []tool.ServerEntry{
			{
				Alias:         "fs",
				Transport:     "stdio",
				Command:       "npx",
				Args:          []string{"-y", "@modelcontextprotocol/server-filesystem@2026.1.14", fsJail},
				Env:           []string{},
				PackageDigest: synthDigest("npm", "@modelcontextprotocol/server-filesystem", "2026.1.14"),
				Version:       "2026.1.14",
				Tools: []tool.ToolEntry{
					{Name: "read_file", SideEffectClass: "read", Description: "Read file (deprecated alias)"},
					{Name: "read_text_file", SideEffectClass: "read", Description: "Read text file contents"},
					{Name: "read_media_file", SideEffectClass: "read", Description: "Read image/audio as base64"},
					{Name: "read_multiple_files", SideEffectClass: "read", Description: "Read multiple files in one call"},
					{Name: "write_file", SideEffectClass: "write", Description: "Write file (creates or overwrites)"},
					{Name: "edit_file", SideEffectClass: "write", Description: "Apply line-based edits"},
					{Name: "create_directory", SideEffectClass: "write", Description: "Create directory tree"},
					{Name: "list_directory", SideEffectClass: "read", Description: "List directory entries"},
					{Name: "list_directory_with_sizes", SideEffectClass: "read", Description: "List directory with sizes"},
					{Name: "directory_tree", SideEffectClass: "read", Description: "Recursive tree view"},
					{Name: "move_file", SideEffectClass: "write", Description: "Move or rename file"},
					{Name: "search_files", SideEffectClass: "read", Description: "Recursive glob search"},
					{Name: "get_file_info", SideEffectClass: "read", Description: "File metadata"},
					{Name: "list_allowed_directories", SideEffectClass: "read", Description: "List allowed roots"},
				},
			},
			{
				Alias:         "fetch",
				Transport:     "stdio",
				Command:       uvxBin(),
				Args:          []string{"mcp-server-fetch"},
				Env:           []string{},
				PackageDigest: synthDigest("pypi", "mcp-server-fetch", "2025.4.7"),
				Version:       "2025.4.7",
				Tools: []tool.ToolEntry{
					{Name: "fetch", SideEffectClass: "network", Description: "Fetch URL contents as Markdown", TimeoutMs: 30000},
				},
			},
			{
				Alias:         "git",
				Transport:     "stdio",
				Command:       uvxBin(),
				Args:          []string{"mcp-server-git", "--repository", gitRepoDir},
				Env:           []string{},
				PackageDigest: synthDigest("pypi", "mcp-server-git", "2026.1.14"),
				Version:       "2026.1.14",
				Tools: []tool.ToolEntry{
					{Name: "git_status", SideEffectClass: "read", Description: "Working-tree status"},
					{Name: "git_diff_unstaged", SideEffectClass: "read", Description: "Diff unstaged"},
					{Name: "git_diff_staged", SideEffectClass: "read", Description: "Diff staged"},
					{Name: "git_diff", SideEffectClass: "read", Description: "Diff arbitrary refs"},
					{Name: "git_commit", SideEffectClass: "write", Description: "Create commit"},
					{Name: "git_add", SideEffectClass: "write", Description: "Stage files"},
					{Name: "git_reset", SideEffectClass: "write", Description: "Unstage files"},
					{Name: "git_log", SideEffectClass: "read", Description: "Commit history"},
					{Name: "git_create_branch", SideEffectClass: "write", Description: "Create branch"},
					{Name: "git_checkout", SideEffectClass: "write", Description: "Switch branches"},
					{Name: "git_show", SideEffectClass: "read", Description: "Show specific commit"},
					{Name: "git_branch", SideEffectClass: "read", Description: "List branches"},
				},
			},
		},
	}
}

// PersistManifest writes the manifest as JSON for audit.
func PersistManifest(m *tool.AgentManifest, path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// synthDigest builds a stable sha256:<64hex> digest from a canonical
// (provider, name, version) tuple. Used because npm/pypi don't expose
// sha256 of the unpacked package directly; for v1 the digest's role is
// audit recording (which version+provider was on the system at run time),
// not chain-grade attestation.
func synthDigest(provider, pkg, version string) string {
	src := provider + ":" + pkg + "@" + version
	sum := sha256.Sum256([]byte(src))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func uvxBin() string {
	if v := os.Getenv("UVX_BIN"); v != "" {
		return v
	}
	for _, p := range []string{"/root/.local/bin/uvx", "uvx"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "uvx"
}

func runCmdInDir(dir, name string, args ...string) error {
	cmd := newSysCmd(dir, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
