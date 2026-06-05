// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newForgeTestDaemon returns a daemonState with forge-mode enabled and
// an AllowRoot pointing at a tmpdir. Used by every fs route test.
func newForgeTestDaemon(t *testing.T) (*daemonState, string) {
	t.Helper()
	tmp := t.TempDir()

	policy := &ForgeFSPolicy{
		AllowRoots:    []string{tmp},
		DenyPrefixes:  []string{filepath.Join(tmp, "denied")},
		MaxReadBytes:  64 * 1024,
		MaxWriteBytes: 64 * 1024,
	}
	d := &daemonState{
		forgeFS: policy,
		// authToken intentionally empty so requireAuth passes.
	}
	return d, tmp
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestForgeFS_Disabled404 verifies the routes 404 when forge-mode is off.
func TestForgeFS_Disabled404(t *testing.T) {
	d := &daemonState{forgeFS: nil}
	req := httptest.NewRequest(http.MethodGet, "/fs/tree?path=/tmp", nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestForgeFS_ReadHappyPath fetches a utf-8 file.
func TestForgeFS_ReadHappyPath(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "hello.txt")
	mustWriteFile(t, target, "hello matrix")

	req := httptest.NewRequest(http.MethodGet, "/fs/read?path="+target, nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp fsReadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Encoding != "utf8" {
		t.Errorf("encoding = %q, want utf8", resp.Encoding)
	}
	if resp.Content != "hello matrix" {
		t.Errorf("content = %q, want %q", resp.Content, "hello matrix")
	}
	if resp.SHA256 == "" {
		t.Errorf("sha256 must be populated for optimistic-concurrency follow-ups")
	}
}

// TestForgeFS_Read_OutsideAllowlist returns 403.
func TestForgeFS_Read_OutsideAllowlist(t *testing.T) {
	d, _ := newForgeTestDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/fs/read?path=/etc/passwd", nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestForgeFS_Read_DenyPrefix returns 403 even though the path is under
// an AllowRoot (the cortex/store + knowledge + journal carve-outs).
func TestForgeFS_Read_DenyPrefix(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "denied", "secret.txt")
	mustWriteFile(t, target, "shh")
	req := httptest.NewRequest(http.MethodGet, "/fs/read?path="+target, nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestForgeFS_Read_TooLarge enforces MaxReadBytes.
func TestForgeFS_Read_TooLarge(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	d.forgeFS.MaxReadBytes = 8
	target := filepath.Join(tmp, "big.txt")
	mustWriteFile(t, target, strings.Repeat("X", 16))
	req := httptest.NewRequest(http.MethodGet, "/fs/read?path="+target, nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// TestForgeFS_Read_NonExistent returns 404.
func TestForgeFS_Read_NonExistent(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/fs/read?path="+filepath.Join(tmp, "missing.txt"), nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestForgeFS_WriteCreatesFile covers the create-new-file happy path.
func TestForgeFS_WriteCreatesFile(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "newfile.go")
	body, _ := json.Marshal(fsWriteRequest{
		Path:    target,
		Content: "package main",
	})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp fsWriteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Created {
		t.Errorf("Created = false, want true on first write")
	}
	on, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(on) != "package main" {
		t.Errorf("on-disk content = %q, want %q", string(on), "package main")
	}
}

// TestForgeFS_Write_OptimisticConcurrencyHit succeeds when PrevSHA256
// matches the on-disk hash.
func TestForgeFS_Write_OptimisticConcurrencyHit(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "concurrency.txt")
	mustWriteFile(t, target, "original")

	body, _ := json.Marshal(fsWriteRequest{
		Path:       target,
		Content:    "updated",
		PrevSHA256: sha256HexBytes([]byte("original")),
	})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(target)
	if string(got) != "updated" {
		t.Errorf("content = %q, want %q", string(got), "updated")
	}
}

// TestForgeFS_Write_OptimisticConcurrencyMiss returns 409 with the
// current hash so the SPA can re-fetch + merge.
func TestForgeFS_Write_OptimisticConcurrencyMiss(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "concurrency.txt")
	mustWriteFile(t, target, "current value")

	body, _ := json.Marshal(fsWriteRequest{
		Path:       target,
		Content:    "stale write",
		PrevSHA256: "deadbeef" + strings.Repeat("00", 28), // fake mismatch
	})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	// Disk MUST be unchanged.
	got, _ := os.ReadFile(target)
	if string(got) != "current value" {
		t.Errorf("file mutated despite 409: %q", string(got))
	}
}

// TestForgeFS_Write_Denied403 rejects writes under DenyPrefix even if
// the parent dir exists and is reachable.
func TestForgeFS_Write_Denied403(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	deniedDir := filepath.Join(tmp, "denied")
	if err := os.MkdirAll(deniedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(deniedDir, "fail.txt")
	body, _ := json.Marshal(fsWriteRequest{Path: target, Content: "x"})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file created despite denial: %v", err)
	}
}

// TestForgeFS_Write_TooLargeBodyRejected enforces MaxWriteBytes.
func TestForgeFS_Write_TooLargeBodyRejected(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	d.forgeFS.MaxWriteBytes = 16
	target := filepath.Join(tmp, "huge.txt")
	body, _ := json.Marshal(fsWriteRequest{
		Path:    target,
		Content: strings.Repeat("Y", 64),
	})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestForgeFS_Write_CreateDirs requires the create_dirs flag for nested
// targets whose parents don't yet exist.
func TestForgeFS_Write_CreateDirs(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "deep", "nested", "newfile.go")
	body, _ := json.Marshal(fsWriteRequest{
		Path:       target,
		Content:    "package nested",
		CreateDirs: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/fs/write", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "package nested" {
		t.Errorf("content = %q, want %q", string(got), "package nested")
	}
}

// TestForgeFS_TreeHappyPath returns the directory listing sorted by name
// with denied subdirs flagged.
func TestForgeFS_TreeHappyPath(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	mustWriteFile(t, filepath.Join(tmp, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(tmp, "b.txt"), "b")
	mustWriteFile(t, filepath.Join(tmp, "denied", "x.txt"), "shh")
	mustWriteFile(t, filepath.Join(tmp, "sub", "c.txt"), "c")

	req := httptest.NewRequest(http.MethodGet, "/fs/tree?path="+tmp+"&depth=2", nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var root fsTreeNode
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !root.IsDir {
		t.Errorf("root must be a directory")
	}
	if len(root.Children) < 3 {
		t.Fatalf("children = %d, want >= 3", len(root.Children))
	}

	var sawDenied, sawSub bool
	for _, c := range root.Children {
		switch c.Name {
		case "denied":
			sawDenied = true
			if !c.Denied {
				t.Errorf("denied dir must have Denied=true; got %+v", c)
			}
			if len(c.Children) != 0 {
				t.Errorf("denied dir must NOT have children listed; got %d", len(c.Children))
			}
		case "sub":
			sawSub = true
			if c.Denied {
				t.Errorf("non-denied dir must have Denied=false")
			}
			if len(c.Children) != 1 || c.Children[0].Name != "c.txt" {
				t.Errorf("sub children = %+v", c.Children)
			}
		}
	}
	if !sawDenied {
		t.Errorf("denied subdir missing from listing")
	}
	if !sawSub {
		t.Errorf("sub subdir missing from listing")
	}

	// Children must be sorted alphabetically.
	for i := 1; i < len(root.Children); i++ {
		if root.Children[i-1].Name > root.Children[i].Name {
			t.Errorf("children not sorted: %s > %s", root.Children[i-1].Name, root.Children[i].Name)
		}
	}
}

// TestForgeFS_Tree_DefaultsToFirstAllowRoot exercises the "no path query"
// shortcut so the SPA can issue GET /fs/tree without knowing the root.
func TestForgeFS_Tree_DefaultsToFirstAllowRoot(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	mustWriteFile(t, filepath.Join(tmp, "rootfile.txt"), "x")

	req := httptest.NewRequest(http.MethodGet, "/fs/tree", nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var root fsTreeNode
	_ = json.Unmarshal(rec.Body.Bytes(), &root)
	if root.Path != tmp {
		t.Errorf("default root = %q, want %q", root.Path, tmp)
	}
}

// TestForgeFS_Tree_DepthCap clamps depth at 3 so an attacker can't ask
// for the whole tree in one call.
func TestForgeFS_Tree_DepthCap(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	// Build a 5-deep chain so we can verify the cap.
	deep := tmp
	for i := 0; i < 5; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	mustWriteFile(t, filepath.Join(deep, "leaf.txt"), "x")

	// Ask for depth=999; expect cap=3.
	req := httptest.NewRequest(http.MethodGet, "/fs/tree?path="+tmp+"&depth=999", nil)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var root fsTreeNode
	_ = json.Unmarshal(rec.Body.Bytes(), &root)
	// Walk down counting levels; should bottom out before reaching d4.
	cur := root
	levels := 0
	for len(cur.Children) > 0 {
		levels++
		cur = cur.Children[0]
		if levels > 4 {
			t.Errorf("tree exceeded depth cap; reached level %d", levels)
			break
		}
	}
}

// Helper for round-trip test below — sends a body, decodes the response.
func sendForgeFSRequest(t *testing.T, d *daemonState, method, path string, body interface{}) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	d.handleForgeFSRouter(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestForgeFS_RoundTrip exercises the create→read→update→read flow the
// SPA's Monaco editor will drive: write a file, read it back to seed
// the editor + capture sha, write again with that sha as PrevSHA256.
func TestForgeFS_RoundTrip(t *testing.T) {
	d, tmp := newForgeTestDaemon(t)
	target := filepath.Join(tmp, "roundtrip.go")

	// CREATE
	status, _ := sendForgeFSRequest(t, d, http.MethodPost, "/fs/write",
		fsWriteRequest{Path: target, Content: "v1"})
	if status != http.StatusOK {
		t.Fatalf("create status = %d", status)
	}

	// READ — capture sha
	status, body := sendForgeFSRequest(t, d, http.MethodGet, "/fs/read?path="+target, nil)
	if status != http.StatusOK {
		t.Fatalf("read status = %d", status)
	}
	var read fsReadResponse
	_ = json.Unmarshal(body, &read)
	if read.Content != "v1" {
		t.Errorf("read = %q, want v1", read.Content)
	}

	// UPDATE with captured sha — must succeed.
	status, _ = sendForgeFSRequest(t, d, http.MethodPost, "/fs/write",
		fsWriteRequest{Path: target, Content: "v2", PrevSHA256: read.SHA256})
	if status != http.StatusOK {
		t.Fatalf("update status = %d", status)
	}

	// READ again — confirm v2 + new sha differs.
	status, body = sendForgeFSRequest(t, d, http.MethodGet, "/fs/read?path="+target, nil)
	if status != http.StatusOK {
		t.Fatalf("re-read status = %d", status)
	}
	var read2 fsReadResponse
	_ = json.Unmarshal(body, &read2)
	if read2.Content != "v2" {
		t.Errorf("re-read = %q, want v2", read2.Content)
	}
	if read2.SHA256 == read.SHA256 {
		t.Errorf("sha must change across content update")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
