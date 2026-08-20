// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package surfacestore

// Unit tests for the Construct OS Shell durable surface-store sidecar. This is
// the F3 trace store (agents/neo/internal/trace) re-keyed by conversationID and
// recording the typed Construct surface event stream
// (construct.surface[.patch]). Every test drives the REAL store against a real
// temp dir — no fakes/mocks (the store's whole job is real disk persistence).
//
// Coverage mirrors the ported agents/neo/internal/trace suite, plus the cases called
// out by R15.2/R16.1/R16.2: async record → flush durability, the amortized
// atomic rollup at 2×retain trimming back to retain, crash-truncated final-line
// skip on Load, the disabled (empty-dir) no-op, path-separator rejection (the
// per-user/per-conversation isolation guard), and a construct.surface[.patch]
// frame round-trip preserving Seq/Ts/Phase/Type/Fields.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"centra/packages/construct/transport"
)

// surfaceFrame builds a full-surface frame with the real wire header
// (phase "construct", type "construct.surface"), so the round-trip asserts the
// exact transcript frame shape the client reducer consumes.
func surfaceFrame(seq int, fields map[string]interface{}) Frame {
	return Frame{
		Seq:    seq,
		Ts:     "2026-01-01T00:00:0" + strconv.Itoa(seq%10) + "Z",
		Phase:  transport.Phase,
		Type:   transport.EventSurface,
		Fields: fields,
	}
}

// rawLines counts the physical JSONL lines on disk for a conversation (i.e. the
// untrimmed file), so a test can distinguish the on-disk rollup state from
// what Load presents (Load caps to retain regardless of file size).
func rawLines(t *testing.T, s *Store, conversationID string) int {
	t.Helper()
	path := filepath.Join(s.dir, conversationID+".surfaces.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read raw jsonl %s: %v", path, err)
	}
	n := 0
	for _, line := range []byte(b) {
		if line == '\n' {
			n++
		}
	}
	return n
}

// TestRecordFlushDurable records the two Construct surface event families
// through the async path and reads them back, in order, after a Flush. This is
// the heart of the store: a reopened conversation rehydrates because frames are
// written to disk, not just streamed to a dropped broker buffer.
func TestRecordFlushDurable(t *testing.T) {
	st := Open(t.TempDir())
	defer st.Close()
	if !st.Enabled() {
		t.Fatal("store with a non-empty dir must be enabled")
	}

	const convID = "conv_1"
	st.Record(convID, Frame{Seq: 1, Ts: "t1", Phase: transport.Phase, Type: transport.EventSurface, Fields: map[string]interface{}{"id": "term", "kind": "Stream"}})
	st.Record(convID, Frame{Seq: 2, Ts: "t2", Phase: transport.Phase, Type: transport.EventSurfacePatch, Fields: map[string]interface{}{"id": "term", "append": "ls -la"}})
	st.Record(convID, Frame{Seq: 3, Ts: "t3", Phase: transport.Phase, Type: transport.EventSurfacePatch, Fields: map[string]interface{}{"id": "term", "append": "done"}})

	// Record is async: nothing is guaranteed on disk until Flush drains the
	// writer queue.
	st.Flush()

	got := st.Load(convID)
	if len(got) != 3 {
		t.Fatalf("want 3 persisted frames, got %d (%+v)", len(got), got)
	}
	wantTypes := []string{transport.EventSurface, transport.EventSurfacePatch, transport.EventSurfacePatch}
	for i, w := range wantTypes {
		if got[i].Type != w {
			t.Errorf("frame %d: want type %q got %q", i, w, got[i].Type)
		}
		if got[i].Seq != i+1 {
			t.Errorf("frame %d: want seq %d got %d (order must be preserved)", i, i+1, got[i].Seq)
		}
	}
	// Fields round-trip intact so the client reducer rebuilds the workspace.
	if got[0].Fields["kind"] != "Stream" {
		t.Errorf("surface kind field lost: %+v", got[0].Fields)
	}

	// A different conversation is isolated (no cross-conversation bleed) and an
	// unknown conversation is empty (not an error).
	if other := st.Load("conv_other"); other != nil {
		t.Errorf("unknown conversation must Load to nil, got %+v", other)
	}
}

// TestRoundTripSurfaceAndPatch asserts a full-surface frame and a patch frame
// survive the JSONL round-trip with every field preserved (Seq/Ts/Phase/Type/
// Fields), including a nested Fields map. This is what guarantees the
// rehydrated computer replays byte-identically to the live stream.
func TestRoundTripSurfaceAndPatch(t *testing.T) {
	st := Open(t.TempDir())
	defer st.Close()

	const convID = "conv_roundtrip"
	// String-valued fields (including a nested map) so the assertion is exact:
	// JSON numbers would decode to float64 and complicate a DeepEqual.
	surface := Frame{
		Seq:   1,
		Ts:    "2026-02-03T04:05:06Z",
		Phase: transport.Phase,
		Type:  transport.EventSurface,
		Fields: map[string]interface{}{
			"id":   "timeline",
			"kind": "Timeline",
			"attributes": map[string]interface{}{
				"region":  "activity",
				"density": "summary",
			},
		},
	}
	patch := Frame{
		Seq:   2,
		Ts:    "2026-02-03T04:05:07Z",
		Phase: transport.Phase,
		Type:  transport.EventSurfacePatch,
		Fields: map[string]interface{}{
			"id":   "timeline",
			"step": "step-2",
		},
	}

	st.Record(convID, surface)
	st.Record(convID, patch)
	st.Flush()

	got := st.Load(convID)
	if len(got) != 2 {
		t.Fatalf("want 2 frames, got %d (%+v)", len(got), got)
	}
	if !reflect.DeepEqual(got[0], surface) {
		t.Errorf("construct.surface frame not preserved:\n want %+v\n  got %+v", surface, got[0])
	}
	if !reflect.DeepEqual(got[1], patch) {
		t.Errorf("construct.surface.patch frame not preserved:\n want %+v\n  got %+v", patch, got[1])
	}
}

// TestAtomicRollupAt2xRetain confirms the amortized atomic rollup: the on-disk
// JSONL is allowed to grow to 2×retain, then a single tmp+rename rewrite trims
// it back to the newest `retain` frames. Load always presents at most `retain`,
// so the cap is exact for consumers even at the boundary.
func TestAtomicRollupAt2xRetain(t *testing.T) {
	const retain = 4

	tests := []struct {
		name         string
		total        int
		wantRawLines int // physical lines on disk after Flush
	}{
		// At exactly 2×retain the file has not yet rolled (condition is
		// strictly greater-than) — it holds all 8 lines.
		{name: "at 2x retain does not roll yet", total: 2 * retain, wantRawLines: 2 * retain},
		// One past 2×retain triggers the rollup, trimming the file to retain.
		{name: "past 2x retain rolls and trims to retain", total: 2*retain + 1, wantRawLines: retain},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openWithCap(t.TempDir(), retain)
			defer st.Close()

			const convID = "conv_capped"
			for i := 1; i <= tc.total; i++ {
				st.Record(convID, surfaceFrame(i, map[string]interface{}{"id": "s", "n": strconv.Itoa(i)}))
			}
			st.Flush()

			if raw := rawLines(t, st, convID); raw != tc.wantRawLines {
				t.Errorf("on-disk rollup: want %d physical lines, got %d", tc.wantRawLines, raw)
			}

			// Load always caps to the newest `retain` frames regardless of the
			// on-disk file size.
			got := st.Load(convID)
			if len(got) != retain {
				t.Fatalf("Load cap not enforced: want %d frames, got %d", retain, len(got))
			}
			wantFirst := tc.total - retain + 1
			if got[0].Seq != wantFirst {
				t.Errorf("oldest retained frame: want seq %d got %d (newest must be kept)", wantFirst, got[0].Seq)
			}
			if got[len(got)-1].Seq != tc.total {
				t.Errorf("newest retained frame: want seq %d got %d", tc.total, got[len(got)-1].Seq)
			}
		})
	}
}

// TestCrashTruncatedLineSkipped confirms a torn/partial final JSONL line (the
// signature of a crash mid-write) is skipped on Load rather than failing the
// whole read — the crash-atomic property. The valid frames before it are
// returned intact.
func TestCrashTruncatedLineSkipped(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	defer st.Close()

	const convID = "conv_crash"
	st.Record(convID, surfaceFrame(1, map[string]interface{}{"id": "a"}))
	st.Record(convID, surfaceFrame(2, map[string]interface{}{"id": "b"}))
	st.Flush()

	// Simulate a crash-truncated final line: append a partial JSON fragment
	// (no newline-terminated, unparseable) exactly as a torn O_APPEND write
	// would leave behind.
	path := filepath.Join(dir, convID+".surfaces.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for truncated append: %v", err)
	}
	if _, err := f.WriteString(`{"seq":3,"ts":"t3","phase":"construct","ty`); err != nil {
		t.Fatalf("write truncated line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := st.Load(convID)
	if len(got) != 2 {
		t.Fatalf("crash-truncated line must be skipped: want 2 valid frames, got %d (%+v)", len(got), got)
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("valid frames before the torn line must survive: got seqs %d,%d", got[0].Seq, got[1].Seq)
	}
}

// TestDisabledStoreNoOp confirms an empty-dir store is disabled and every method
// is a safe no-op (never touches the filesystem, never panics) so dev/CLI runs
// work unchanged.
func TestDisabledStoreNoOp(t *testing.T) {
	dis := Open("")
	if dis.Enabled() {
		t.Fatal("empty-dir store must be disabled")
	}
	// None of these may panic or block.
	dis.Record("x", surfaceFrame(1, map[string]interface{}{"id": "s"}))
	dis.Flush()
	dis.Close()
	if got := dis.Load("x"); got != nil {
		t.Errorf("disabled store must Load to nil, got %+v", got)
	}

	// A whitespace-only dir is also disabled (trimmed to empty).
	if Open("   ").Enabled() {
		t.Error("whitespace-only dir must be disabled")
	}
}

// TestPathSeparatorRejected confirms a conversationID containing a path
// separator (POSIX "/" or Windows "\\") is rejected by BOTH Record and Load —
// both as input validation and as a path-traversal guard, so a conversation can
// never escape its store directory (R15 per-user/per-conversation isolation).
func TestPathSeparatorRejected(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	defer st.Close()

	bad := []string{
		"a/b",
		"../escape",
		"sub/dir/conv",
		`a\b`,
		`..\escape`,
		"", // blank id is also rejected
	}
	for _, id := range bad {
		st.Record(id, surfaceFrame(1, map[string]interface{}{"id": "s"}))
		if got := st.Load(id); got != nil {
			t.Errorf("Load(%q) must reject and return nil, got %+v", id, got)
		}
	}
	st.Flush()

	// Nothing may have been written: the only side effect a rejected Record is
	// allowed is none. A valid record afterward proves the store still works.
	st.Record("ok_conv", surfaceFrame(1, map[string]interface{}{"id": "s"}))
	st.Flush()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != "ok_conv.surfaces.jsonl" {
		t.Fatalf("a path-separator id must NEVER create a file; dir should hold only the valid conversation, got %v", names)
	}
}

// TestRecordNeverBlocks confirms Record returns promptly even under a burst far
// larger than the writer can drain synchronously — it enqueues (dropping on a
// full queue) and never blocks the caller (the broker publish hot path). A
// blocking implementation would exceed the deadline (R16.3).
func TestRecordNeverBlocks(t *testing.T) {
	st := Open(t.TempDir())
	defer st.Close()

	// The store's drop path logs each dropped frame to os.Stderr. A burst this
	// large drops thousands of frames, and writing that many lines to the
	// captured test stderr would dominate wall-clock (especially under -race),
	// masking the property under test. Redirect stderr to a temp file so the
	// deadline measures only whether Record blocks on the writer queue — not
	// terminal I/O speed.
	sink, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatalf("create stderr sink: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = sink
	defer func() { os.Stderr = origStderr; _ = sink.Close() }()

	done := make(chan struct{})
	go func() {
		for i := 0; i < queueDepth*4; i++ {
			st.Record("conv_burst", surfaceFrame(i, map[string]interface{}{"id": "s"}))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked under a burst — it must be non-blocking (drop-on-full), never stall the publish path")
	}
}

// TestDirResolution covers the /data-rooted Dir(override, cortexRoot) policy:
// an explicit override wins; otherwise the dir is derived as a "surfaces"
// sibling of the cortex root's parent (sharing the per-user /data volume with
// the cortex/conversation/task/trace stores); neither available → "" (disabled).
func TestDirResolution(t *testing.T) {
	tests := []struct {
		name       string
		override   string
		cortexRoot string
		want       string
	}{
		{name: "override wins", override: "/data/u/surfaces", cortexRoot: "/data/u/cortex", want: "/data/u/surfaces"},
		{name: "derived from cortex parent", override: "", cortexRoot: "/data/u/cortex", want: filepath.Join("/data/u", "surfaces")},
		{name: "override trimmed then wins", override: "  /x/surfaces  ", cortexRoot: "/data/u/cortex", want: "/x/surfaces"},
		{name: "neither available is disabled", override: "", cortexRoot: "", want: ""},
		{name: "whitespace cortex root is disabled", override: "  ", cortexRoot: "  ", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Dir(tc.override, tc.cortexRoot); got != tc.want {
				t.Errorf("Dir(%q, %q) = %q, want %q", tc.override, tc.cortexRoot, got, tc.want)
			}
		})
	}
}
