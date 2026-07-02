// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	e, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return e, root
}

func seed(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAnchoredEdit(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	if _, err := e.Read("main.go"); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply("main.go", `println("hello")`, `println("world")`, false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "main.go"))
	if string(data) != "package main\n\nfunc main() {\n\tprintln(\"world\")\n}\n" {
		t.Fatalf("content after edit: %q", data)
	}
}

func TestEditWithoutReadRefused(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "a.txt", "one two\n")
	if err := e.Apply("a.txt", "one", "1", false); !errors.Is(err, ErrNotRead) {
		t.Fatalf("Apply without read = %v, want ErrNotRead", err)
	}
	if err := e.Overwrite("a.txt", "new"); !errors.Is(err, ErrNotRead) {
		t.Fatalf("Overwrite without read = %v, want ErrNotRead", err)
	}
	if err := e.Delete("a.txt"); !errors.Is(err, ErrNotRead) {
		t.Fatalf("Delete without read = %v, want ErrNotRead", err)
	}
}

func TestStalenessDetection(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "a.txt", "alpha\n")
	if _, err := e.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	// The file drifts underneath the engine (another process, the user, ...).
	seed(t, root, "a.txt", "alpha drifted\n")
	if err := e.Apply("a.txt", "alpha", "beta", false); !errors.Is(err, ErrStale) {
		t.Fatalf("Apply on drifted file = %v, want ErrStale", err)
	}
	if err := e.Overwrite("a.txt", "beta\n"); !errors.Is(err, ErrStale) {
		t.Fatalf("Overwrite on drifted file = %v, want ErrStale", err)
	}
	// Drift is never silently overwritten.
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "alpha drifted\n" {
		t.Fatalf("drifted content clobbered: %q", data)
	}
	// Re-reading heals the anchor.
	if _, err := e.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply("a.txt", "alpha drifted", "beta", false); err != nil {
		t.Fatal(err)
	}
}

func TestAnchorMissingAndAmbiguous(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "a.txt", "x = 1\nx = 1\n")
	if _, err := e.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply("a.txt", "y = 2", "y = 3", false); !errors.Is(err, ErrAnchorNotFound) {
		t.Fatalf("missing anchor = %v, want ErrAnchorNotFound", err)
	}
	if err := e.Apply("a.txt", "x = 1", "x = 2", false); !errors.Is(err, ErrAnchorAmbiguous) {
		t.Fatalf("ambiguous anchor = %v, want ErrAnchorAmbiguous", err)
	}
	if err := e.Apply("a.txt", "x = 1", "x = 2", true); err != nil {
		t.Fatalf("replace-all = %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "x = 2\nx = 2\n" {
		t.Fatalf("replace-all content: %q", data)
	}
}

func TestChainedEditsStayFresh(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "a.txt", "one\ntwo\nthree\n")
	if _, err := e.Read("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply("a.txt", "one", "1", false); err != nil {
		t.Fatal(err)
	}
	// A second edit by the same holder needs no re-read: commit refreshed the anchor.
	if err := e.Apply("a.txt", "two", "2", false); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(data) != "1\n2\nthree\n" {
		t.Fatalf("chained content: %q", data)
	}
}

func TestCreateOnlyNewFiles(t *testing.T) {
	e, root := newEngine(t)
	if err := e.Create("pkg/new.go", "package pkg\n"); err != nil {
		t.Fatal(err)
	}
	if err := e.Create("pkg/new.go", "package clobber\n"); !errors.Is(err, ErrExists) {
		t.Fatalf("Create over existing = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "pkg/new.go"))
	if string(data) != "package pkg\n" {
		t.Fatalf("existing file clobbered by Create: %q", data)
	}
	// A created file is immediately editable (Create records the read anchor).
	if err := e.Apply("pkg/new.go", "package pkg", "package pkg // v2", false); err != nil {
		t.Fatal(err)
	}
}

func TestPathEscapeRefused(t *testing.T) {
	e, _ := newEngine(t)
	for _, p := range []string{"../outside.txt", "../../etc/passwd", "/etc/passwd"} {
		if _, err := e.Read(p); !errors.Is(err, ErrOutsideRoot) && !os.IsNotExist(err) {
			// Absolute paths inside the root are allowed; these are outside.
			if !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("Read(%q) = %v, want ErrOutsideRoot", p, err)
			}
		}
		if err := e.Create(p, "x"); !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("Create(%q) = %v, want ErrOutsideRoot", p, err)
		}
	}
}

func TestDeleteGatedOnFreshRead(t *testing.T) {
	e, root := newEngine(t)
	seed(t, root, "gone.txt", "bye\n")
	if _, err := e.Read("gone.txt"); err != nil {
		t.Fatal(err)
	}
	seed(t, root, "gone.txt", "changed\n")
	if err := e.Delete("gone.txt"); !errors.Is(err, ErrStale) {
		t.Fatalf("Delete of drifted file = %v, want ErrStale", err)
	}
	if _, err := e.Read("gone.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.Delete("gone.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("file survived Delete")
	}
}
