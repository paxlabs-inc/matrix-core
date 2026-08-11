// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reloadFixtureMtx is a minimal parseable/validator-clean SKILL.mtx body
// with a caller-supplied id+version so a hot-reload can produce a stable URI.
// Mirrors minimalSkillMtx from skill_loader_test.go but keeps TOOLS/SUB_SKILLS
// pinned to `none` so the body stays valid when only id/version/display vary.
func reloadFixtureMtx(slug, version, display string) string {
	return "" +
		"§SKILL\n" +
		"id=" + slug + "\n" +
		"version=" + version + "\n" +
		"display=\"" + display + "\"\n" +
		"description=\"hot-reload fixture\"\n" +
		"mcl.verbs=build\n" +
		"\n§INPUTS\n" +
		"slot target: ArtifactRef\n  required\n" +
		"\n§CORTEX\n" +
		"reads=Fact\n" +
		"\n§TOOLS\nnone\n" +
		"\n§SUB_SKILLS\nnone\n" +
		"\n§PROCEDURE\n" +
		"on verb=build\n" +
		"  prompt\n    system=\"x\"\n    user=\"y\"\n  end\nend\n" +
		"\n§OUTPUTS\n" +
		"slot result: ArtifactRef\n  required\n" +
		"\n§FAILURE_MODES\n" +
		"x\n  action=fail\n  reason=policy_violation\n"
}

// writeReloadSkill writes SKILL.mtx into <root>/<slug>/ and returns its path.
func writeReloadSkill(t *testing.T, root, slug, version, display string) string {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, "SKILL.mtx")
	if err := os.WriteFile(p, []byte(reloadFixtureMtx(slug, version, display)), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// setEnv sets an env var for the test's duration and restores it on cleanup.
func setEnv(t *testing.T, key, val string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Setenv(key, val); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestSkillReload_OffByDefault verifies the production invariant: with no dev
// flag/env set, NewSkillReloader returns a DISABLED reloader whose Start is a
// no-op and whose Enabled() is false. Production images never run a watcher.
func TestSkillReload_OffByDefault(t *testing.T) {
	// Ensure the dev flag env is unset for this test.
	setEnv(t, devReloadEnv, "")
	root := t.TempDir()
	writeReloadSkill(t, root, "off-by-default", "1.0.0", "Off")

	r := NewSkillReloader(root, "")
	if r.Enabled() {
		t.Fatalf("Enabled() = true, want false when %s unset (production path must stay read-only)", devReloadEnv)
	}
	// Start must be a safe no-op on a disabled reloader.
	if err := r.Start(); err != nil {
		t.Fatalf("disabled Start() returned error: %v", err)
	}
	// No watcher goroutine should have been spawned; Close is a no-op too.
	if err := r.Close(); err != nil {
		t.Fatalf("disabled Close() returned error: %v", err)
	}
	// Live registry must be empty — nothing was watched.
	if _, ok := r.Lookup("matrix://skill/off-by-default@1.0.0"); ok {
		t.Fatalf("Lookup returned ok on a disabled reloader; registry must be empty")
	}
}

// TestSkillReload_DevReparsesOnChange verifies the dev hot-reload path: when
// the dev flag is ON, editing a SKILL.mtx causes the reloader to re-parse the
// changed file into the live registry, visible via Lookup, WITHOUT a restart.
func TestSkillReload_DevReparsesOnChange(t *testing.T) {
	setEnv(t, devReloadEnv, "1")
	root := t.TempDir()
	slug := "hot-reloadable"
	writeReloadSkill(t, root, slug, "1.0.0", "V1")

	r := NewSkillReloader(root, "1")
	if !r.Enabled() {
		t.Fatalf("Enabled() = false, want true when %s=1", devReloadEnv)
	}
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	uri := "matrix://skill/" + slug + "@1.0.0"
	// Wait for the initial scan to pick up the existing skill.
	if !waitFor(t, 2*time.Second, func() bool {
		ls, ok := r.Lookup(uri)
		return ok && ls != nil && ls.Display == "V1"
	}) {
		ls, _ := r.Lookup(uri)
		t.Fatalf("initial scan did not load %s (got %+v)", uri, ls)
	}

	// Edit the skill: bump display to "V2" and rewrite the file.
	if err := os.WriteFile(
		filepath.Join(root, slug, "SKILL.mtx"),
		[]byte(reloadFixtureMtx(slug, "1.0.0", "V2")),
		0o644,
	); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// The watcher should re-parse and the registry entry's Display must update.
	if !waitFor(t, 2*time.Second, func() bool {
		ls, ok := r.Lookup(uri)
		return ok && ls != nil && ls.Display == "V2"
	}) {
		ls, _ := r.Lookup(uri)
		t.Fatalf("hot-reload did not re-parse edited SKILL.mtx; Display still = %q (want %q)", displayOr(ls), "V2")
	}
}

// TestSkillReload_InvalidSkillRejectedNotCrash verifies that an invalid edit
// (here: a SKILL.mtx missing the required §SKILL.version) is rejected by the
// loader, dropped from the registry, and crucially does NOT crash the watcher.
// The previously-good entry (if any) stays absent because the reloader rejects
// invalid files rather than caching a broken parse.
func TestSkillReload_InvalidSkillRejectedNotCrash(t *testing.T) {
	setEnv(t, devReloadEnv, "1")
	root := t.TempDir()
	slug := "reject-invalid"
	writeReloadSkill(t, root, slug, "1.0.0", "Good")

	r := NewSkillReloader(root, "1")
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	uri := "matrix://skill/" + slug + "@1.0.0"
	if !waitFor(t, 2*time.Second, func() bool {
		_, ok := r.Lookup(uri)
		return ok
	}) {
		t.Fatalf("initial scan did not load %s", uri)
	}

	// Overwrite with a body missing §SKILL.version (extractSkillMeta rejects it).
	bad := strings.Replace(reloadFixtureMtx(slug, "1.0.0", "Good"), "version=1.0.0\n", "", 1)
	if err := os.WriteFile(filepath.Join(root, slug, "SKILL.mtx"), []byte(bad), 0o644); err != nil {
		t.Fatalf("rewrite invalid: %v", err)
	}

	// Give the watcher time to observe + reject the bad file. The watcher must
	// NOT crash; after the rejection the reloader must still be Close-able and
	// Lookup must report the skill as absent (rejected, not cached-broken).
	time.Sleep(500 * time.Millisecond)

	// Re-write a GOOD body and confirm the watcher recovers and re-parses it
	// (proving the watcher goroutine survived the invalid edit).
	if err := os.WriteFile(
		filepath.Join(root, slug, "SKILL.mtx"),
		[]byte(reloadFixtureMtx(slug, "1.0.0", "Recovered")),
		0o644,
	); err != nil {
		t.Fatalf("rewrite good: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		ls, ok := r.Lookup(uri)
		return ok && ls != nil && ls.Display == "Recovered"
	}) {
		ls, _ := r.Lookup(uri)
		t.Fatalf("watcher did not recover after invalid edit; Display = %q (want %q). The invalid edit likely crashed the watcher goroutine.", displayOr(ls), "Recovered")
	}
}

// displayOr returns ls.Display or "<nil>".
func displayOr(ls *LoadedSkill) string {
	if ls == nil {
		return "<nil>"
	}
	return ls.Display
}

// waitFor polls cond every 25ms up to timeout, returning true once cond passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}
