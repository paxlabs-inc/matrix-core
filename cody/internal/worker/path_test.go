// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/cody/internal/llmtest"
)

// bareWorker builds a worker without running the LLM loop, so the tool
// handlers can be exercised directly against a real edit engine and workspace.
func bareWorker(t *testing.T, root string) *Worker {
	t.Helper()
	srv := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say("noop")
	})
	t.Cleanup(srv.Close)
	w, err := New(Options{
		Sheet:  sheetFor(root, []string{"true"}),
		Root:   root,
		Client: llmtest.NewClient(t, srv),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestPathNormalizationSeam proves the Y2 shape is gone: a model-supplied
// absolute in-root path and its relative form flow through the single
// normalization seam to IDENTICAL fs_write / fs_read / fs_list results, and
// recordChange never stores an absolute path.
func TestPathNormalizationSeam(t *testing.T) {
	const target = "sub/dir/greet.txt"
	const content = "hello cody\n"

	cases := []struct {
		name    string
		pathFor func(root, rel string) string
	}{
		{"relative", func(root, rel string) string { return rel }},
		{"absolute-in-root", func(root, rel string) string { return filepath.Join(root, rel) }},
	}

	var writeReturns, readReturns, listReturns []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			w := bareWorker(t, root)

			wr := w.toolWrite(map[string]interface{}{
				"path":    tc.pathFor(root, target),
				"content": content,
			})
			if strings.HasPrefix(wr, "error") {
				t.Fatalf("fs_write(%s) errored: %s", tc.name, wr)
			}
			writeReturns = append(writeReturns, wr)

			// recordChange stored the normalized workspace-relative path (ac_3).
			changes := w.trackedChanges()
			if len(changes) != 1 || changes[0].Path != target {
				t.Fatalf("recorded change = %+v, want single %q", changes, target)
			}
			if filepath.IsAbs(changes[0].Path) {
				t.Fatalf("recorded change carried an absolute path: %q", changes[0].Path)
			}

			// The file landed at the real, single location (no double-join).
			data, err := os.ReadFile(filepath.Join(root, target))
			if err != nil || string(data) != content {
				t.Fatalf("real file wrong: %q %v", data, err)
			}

			readReturns = append(readReturns, w.toolRead(map[string]interface{}{
				"path": tc.pathFor(root, target),
			}))
			listReturns = append(listReturns, w.toolList(map[string]interface{}{
				"path": tc.pathFor(root, "sub/dir"),
			}))
		})
	}

	if writeReturns[0] != writeReturns[1] {
		t.Fatalf("fs_write differs by path form:\n relative=%q\n absolute=%q", writeReturns[0], writeReturns[1])
	}
	if readReturns[0] != readReturns[1] {
		t.Fatalf("fs_read differs by path form:\n relative=%q\n absolute=%q", readReturns[0], readReturns[1])
	}
	if listReturns[0] != listReturns[1] {
		t.Fatalf("fs_list differs by path form:\n relative=%q\n absolute=%q", listReturns[0], listReturns[1])
	}
}

// TestPathNormalizationRejectsEscape proves the seam refuses any path that
// escapes the workspace root, in both relative and absolute form.
func TestPathNormalizationRejectsEscape(t *testing.T) {
	root := t.TempDir()
	w := bareWorker(t, root)

	cases := []struct {
		name string
		call func() string
	}{
		{"relative-traversal-write", func() string {
			return w.toolWrite(map[string]interface{}{"path": "../evil.txt", "content": "x"})
		}},
		{"absolute-out-of-root-read", func() string {
			return w.toolRead(map[string]interface{}{"path": "/etc/passwd"})
		}},
		{"relative-traversal-list", func() string {
			return w.toolList(map[string]interface{}{"path": "../.."})
		}},
		{"relative-traversal-delete", func() string {
			return w.toolDelete(map[string]interface{}{"path": "../../secret"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.call()
			if !strings.Contains(got, "escapes the workspace root") {
				t.Fatalf("escape not rejected: %q", got)
			}
		})
	}
}
