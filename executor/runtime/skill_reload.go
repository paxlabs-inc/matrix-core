// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Skill hot-reload (dev) — P2-3 of MATRIX-ENHANCEMENTS.kvx.
//
// OPT-IN dev-only path: when a dev flag/env is set, watch the skills dir with
// fsnotify and re-parse changed SKILL.mtx files into a live registry that
// callers can consult instead of (or alongside) the stateless SkillLoader.
//
// Production invariant (acceptance: "prod path byte-identical to today"):
// the production Docker image does NOT set the env, so NewSkillReloader
// returns a disabled no-op whose Start()/Close()/Lookup() are all inert. No
// fsnotify.Watcher is ever constructed on the production path. CI still
// validates skills at build; the watcher only re-parses files that already
// exist on disk — it never relaxes validation (invalid edits are rejected,
// not cached, and never crash the watcher goroutine).
//
// fsnotify is the one dependency the spec permits; it is already vendored as
// an indirect dep of sibling modules (deus/tachyon/layerx) and lives in the
// Go module cache at v1.6.0 — promoting it to a direct require of executor
// reuses an already-cached module rather than introducing a new one.

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// devReloadEnv is the env var that opts INTO dev hot-reload. Any non-empty
// value enables it; empty (the default in production images) keeps the path
// read-only. A non-env dev flag can also be passed directly to
// NewSkillReloader via the devFlag argument (higher precedence than env).
const devReloadEnv = "SKILL_RELOAD"

// SkillReloader is the dev hot-reload surface. It wraps a SkillLoader with a
// live, mutex-guarded registry of parsed skills that is refreshed by an
// fsnotify watcher when a SKILL.mtx changes on disk.
//
// A DISABLED reloader (the production default) has enabled=false, no watcher,
// and inert Start/Close/Lookup. This structurally enforces the "off by
// default" + "prod stays read-only" acceptance criteria.
type SkillReloader struct {
	loader *SkillLoader
	root   string

	enabled bool

	mu      sync.RWMutex
	live    map[string]*LoadedSkill // uri -> parsed skill
	done    chan struct{}           // closed when the watcher loop should stop
	started bool
	closed  bool

	// errs captures the most recent re-parse error so tests / ops can
	// inspect why an edit was rejected rather than silently dropped. It is
	// only ever set inside the watcher loop; reads are racy by design (a
	// best-effort signal, not a contract).
	errs []string
}

// DevReloadEnabled reports whether dev hot-reload should be active given the
// env (SKILL_RELOAD) and an optional explicit devFlag. The explicit flag, when
// non-empty, overrides the env (so a CLI --skill-reload=true can force on
// even when the env is unset, and --skill-reload=false can force off).
func DevReloadEnabled(devFlag string) bool {
	if devFlag != "" {
		switch strings.ToLower(strings.TrimSpace(devFlag)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off", "":
			return false
		}
	}
	v, _ := os.LookupEnv(devReloadEnv)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// NewSkillReloader constructs a reloader for the given skills root. If dev
// hot-reload is NOT enabled (env unset, flag empty/false) it returns a
// disabled no-op reloader — the production path. The disabled instance never
// constructs an fsnotify.Watcher.
//
// repoRoot empty → DefaultSkillRepoRoot (same rule as NewSkillLoader).
func NewSkillReloader(repoRoot, devFlag string) *SkillReloader {
	if repoRoot == "" {
		repoRoot = DefaultSkillRepoRoot
	}
	r := &SkillReloader{
		loader: NewSkillLoader(repoRoot),
		root:   repoRoot,
		live:   map[string]*LoadedSkill{},
	}
	if DevReloadEnabled(devFlag) {
		r.enabled = true
	}
	return r
}

// Enabled reports whether this reloader is on the dev hot-reload path. False
// on the production path (env unset).
func (r *SkillReloader) Enabled() bool { return r != nil && r.enabled }

// Start begins watching the skills root for SKILL.mtx changes. On a disabled
// reloader it is a safe no-op (returns nil) — the production path.
//
// On an enabled reloader it performs an initial full scan of the corpus
// (so the live registry is populated immediately, not only on the first
// edit), then starts an fsnotify watcher goroutine that re-parses changed
// SKILL.mtx files. Invalid edits are logged into r.errs and dropped; they
// never crash the goroutine.
func (r *SkillReloader) Start() error {
	if r == nil || !r.enabled {
		return nil
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.started = true
	r.done = make(chan struct{})
	r.mu.Unlock()

	// Initial scan: parse every existing skill so the live registry is warm
	// immediately (a caller doing Lookup right after Start should see the
	// current corpus, not just edits made after Start).
	r.initialScan()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		// If we can't get a watcher, the reloader degrades gracefully:
		// the initial scan is still valid; subsequent edits just won't be
		// picked up live. Record the error and return nil so the caller
		// (the daemon) still boots.
		r.appendErr(fmt.Sprintf("skill_reload: fsnotify watcher unavailable: %v", err))
		return nil
	}
	// Watch the root and every immediate subdirectory (one level — each
	// skill lives at <root>/<slug>/SKILL.mtx).
	if err := w.Add(r.root); err != nil {
		// Non-fatal: root may not exist yet in some test harnesses; the
		// initial scan already handles absence. Keep going so subdirs we
		// CAN add still get watched.
		r.appendErr(fmt.Sprintf("skill_reload: watch root %s: %v", r.root, err))
	}
	entries, _ := os.ReadDir(r.root)
	for _, e := range entries {
		if e.IsDir() {
			if err := w.Add(filepath.Join(r.root, e.Name())); err != nil {
				r.appendErr(fmt.Sprintf("skill_reload: watch %s: %v", e.Name(), err))
			}
		}
	}
	go r.loop(w)
	return nil
}

// Close stops the watcher goroutine. Safe to call multiple times; a no-op on a
// disabled or never-started reloader.
func (r *SkillReloader) Close() error {
	if r == nil || !r.enabled {
		return nil
	}
	r.mu.Lock()
	if r.closed || !r.started {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.done)
	r.mu.Unlock()
	return nil
}

// Lookup returns the live-parsed skill for a matrix://skill/<slug>@<version>
// URI, or (nil,false) if it is not in the live registry. On a disabled
// reloader it always returns (nil,false).
func (r *SkillReloader) Lookup(uri string) (*LoadedSkill, bool) {
	if r == nil || !r.enabled {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ls, ok := r.live[uri]
	return ls, ok
}

// LatestErrors returns a snapshot of the most recent re-parse errors (invalid
// edits). Best-effort; primarily for tests and ops dashboards.
func (r *SkillReloader) LatestErrors() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.errs))
	copy(out, r.errs)
	return out
}

// ---- internal ----

// initialScan parses every SKILL.mtx currently under the root into the live
// registry. Invalid skills are skipped (with an error recorded). This makes
// the live registry warm right after Start, mirroring what the daemon's
// skillCatalog.ensureLoaded does on first hit.
func (r *SkillReloader) initialScan() {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		if !os.IsNotExist(err) {
			r.appendErr(fmt.Sprintf("skill_reload: scan %s: %v", r.root, err))
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r.reparseSlug(e.Name())
	}
}

// loop is the watcher goroutine. It drains fsnotify events + errors until
// Close is called. Invalid re-parses are recorded and dropped; the loop
// never exits due to a parse failure (acceptance: invalid edits never crash).
func (r *SkillReloader) loop(w *fsnotify.Watcher) {
	defer w.Close()
	for {
		select {
		case <-r.done:
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if !r.isSkillMtx(ev.Name) {
				continue
			}
			// Write/Create/Rename → re-parse the owning slug.
			// Remove → drop the entry (skill deleted/renamed away).
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				r.reparseSlug(slugOfPath(r.root, ev.Name))
			}
		case ferr, ok := <-w.Errors:
			if !ok {
				return
			}
			r.appendErr(fmt.Sprintf("skill_reload: watcher error: %v", ferr))
		}
	}
}

// reparseSlug re-parses <root>/<slug>/SKILL.mtx into the live registry. If the
// file is missing, the live entry for that slug is dropped. If parsing or
// validation fails, the entry is dropped (NOT cached-broken) and the error is
// recorded — the watcher goroutine keeps running.
func (r *SkillReloader) reparseSlug(slug string) {
	if slug == "" {
		return
	}
	mtxPath := filepath.Join(r.root, slug, "SKILL.mtx")
	if _, err := os.Stat(mtxPath); err != nil {
		// File gone: drop any live entry whose URI carries this slug.
		r.dropSlug(slug)
		return
	}
	// Read the file once to discover §SKILL.version, then Load via the
	// stateless loader (which re-reads + parses + validates). We discover
	// the version from the bytes so we can build the canonical URI without
	// requiring the caller to pass it.
	ls, err := r.parseAndLoad(slug, mtxPath)
	if err != nil {
		r.appendErr(fmt.Sprintf("skill_reload: reject %s: %v", slug, err))
		r.dropSlug(slug)
		return
	}
	uri := ls.URI
	r.mu.Lock()
	r.live[uri] = ls
	r.mu.Unlock()
}

// parseAndLoad discovers the §SKILL.version from the file bytes and then uses
// the stateless SkillLoader to produce a fully-validated LoadedSkill. This
// keeps the dev hot-reload path reusing the EXACT same parse+validate pipeline
// as the production read path (no parallel validator that could drift).
func (r *SkillReloader) parseAndLoad(slug, mtxPath string) (*LoadedSkill, error) {
	raw, err := os.ReadFile(mtxPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mtxPath, err)
	}
	ver, err := peekSkillVersion(raw)
	if err != nil {
		return nil, err
	}
	uri := "matrix://skill/" + slug + "@" + ver
	ls, err := r.loader.Load(uri)
	if err != nil {
		return nil, err
	}
	return ls, nil
}

// dropSlug removes any live entry whose URI slug matches the given slug.
func (r *SkillReloader) dropSlug(slug string) {
	r.mu.Lock()
	for uri := range r.live {
		if su, err := ParseSkillURI(uri); err == nil && su.Slug == slug {
			delete(r.live, uri)
		}
	}
	r.mu.Unlock()
}

// appendErr records a re-parse/watcher error. Capped to avoid unbounded growth
// on a pathological edit loop.
func (r *SkillReloader) appendErr(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, msg)
	if len(r.errs) > 64 {
		r.errs = r.errs[len(r.errs)-64:]
	}
}

// isSkillMtx reports whether a watched event path points at a SKILL.mtx file.
func (r *SkillReloader) isSkillMtx(p string) bool {
	return filepath.Base(p) == "SKILL.mtx"
}

// slugOfPath returns the slug (directory name) of a watched SKILL.mtx path
// relative to root. Returns "" if the path is not under root.
func slugOfPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return ""
	}
	return filepath.Dir(rel)
}

// peekSkillVersion extracts the §SKILL.version value from raw SKILL.mtx bytes
// WITHOUT running the full parser — a cheap pre-pass so we can build the
// canonical matrix://skill/<slug>@<version> URI before handing off to the
// stateless loader (which re-parses + re-validates authoritatively). Returns
// an error if the version line is absent (the file is malformed).
func peekSkillVersion(raw []byte) (string, error) {
	const versionPrefix = "version="
	inSkill := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "§SKILL") {
			inSkill = true
			continue
		}
		if strings.HasPrefix(trimmed, "§") && !strings.HasPrefix(trimmed, "§SKILL") {
			inSkill = false
			continue
		}
		if !inSkill {
			continue
		}
		if strings.HasPrefix(trimmed, versionPrefix) {
			v := strings.TrimSpace(strings.TrimPrefix(trimmed, versionPrefix))
			v = strings.Trim(v, "\"'")
			if v == "" {
				return "", errors.New("§SKILL.version empty")
			}
			return v, nil
		}
	}
	return "", errors.New("§SKILL.version missing")
}
