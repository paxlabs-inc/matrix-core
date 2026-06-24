// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package trace is Neo's durable "Neo's Computer" workspace memory.
//
// The live workspace — the animated tool steps (terminal / browser / editor),
// web-search source cards, generated media, Construct surfaces, and the Agent
// Swarm windows — is streamed to the client as SSE events that live ONLY in the
// in-memory broker (a 512-event replay buffer per run, dropped two minutes after
// the run ends). So once a run settles, scrolls past the buffer, or the topic is
// reclaimed, the workspace is unrecoverable: a reopened thread shows the text
// turns but an EMPTY computer. This store fixes that — it persists the workspace
// timeline per run as append-only JSONL (one line per event), so the workspace
// survives reload, suspend, redeploy, and reopen-from-history.
//
// It is a pure SIDECAR: it never touches cortex, signs anything, or perturbs
// replay. The persisted events are the exact transcript SSE frames the client
// already renders, so a reopen replays them through the same reducer the live
// stream uses.
//
// Hot-path discipline: Record is a NON-BLOCKING enqueue onto a buffered channel
// drained by a single background writer goroutine. It never does disk I/O on the
// caller's goroutine (the broker publish path) and, like the broker's own
// slow-subscriber handling, drops rather than blocks if the queue is saturated —
// the workspace timeline is a best-effort observability record, never on the
// critical path of the agent loop.
//
// Retained-event cap: each run's JSONL is bounded by RetainedEvents (config
// NEO_TRACE_RETAIN, default 2000). When a run exceeds the cap, the oldest events
// are trimmed via an atomic rewrite (tmp+rename) — bounded like the broker's
// replay buffer, just larger and durable.
package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultRetainedEvents caps how many workspace events are retained per run on
// disk. Larger than the broker's 512-event live buffer because this is the
// durable record a reopen rebuilds from; still bounded so a runaway run can't
// grow the file without limit.
const DefaultRetainedEvents = 2000

// queueDepth is how many pending events the async writer buffers before Record
// starts dropping. Generous: a single user's single daemon emits tens-to-low-
// hundreds of workspace events per run, so this is never reached in practice;
// it exists only so a pathological burst can't block broker.publish.
const queueDepth = 4096

// Event is one persisted workspace frame. Its shape mirrors the server's SSE
// Event ({seq,ts,phase,type,fields}) so the persisted trace replays through the
// client's existing reducer byte-for-byte.
type Event struct {
	Seq    int                    `json:"seq"`
	Ts     string                 `json:"ts"`
	Phase  string                 `json:"phase"`
	Type   string                 `json:"type"`
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// record is one queued write. A non-nil flush channel marks a flush sentinel
// (no event payload); the writer closes it once it has drained every record
// enqueued before it.
type record struct {
	runID string
	ev    Event
	flush chan struct{}
}

// Store is Neo's durable per-run workspace timeline. A single background writer
// goroutine owns all disk writes (and the per-run event counts), so the only
// shared state is the enqueue channel; reads (Load) go straight to the
// filesystem and are safe against the writer because every write is atomic (an
// O_APPEND single-line write, or a tmp+rename rollup).
type Store struct {
	dir    string
	retain int

	ch     chan record
	stop   chan struct{}
	closed chan struct{}
	once   sync.Once
}

// Open builds a store rooted at dir and starts its background writer. An empty
// dir yields a disabled store (every method is a safe no-op) so dev/CLI runs
// work unchanged. The retained-event cap defaults to DefaultRetainedEvents and
// can be overridden via NEO_TRACE_RETAIN (read once at Open).
func Open(dir string) *Store {
	return openWithCap(dir, retainFromEnv())
}

// openWithCap is the test seam that sets an explicit retained-event cap.
func openWithCap(dir string, cap int) *Store {
	s := &Store{
		dir:    strings.TrimSpace(dir),
		retain: cap,
	}
	if s.retain <= 0 {
		s.retain = DefaultRetainedEvents
	}
	if s.dir == "" {
		return s // disabled: no writer goroutine, all methods no-op
	}
	s.ch = make(chan record, queueDepth)
	s.stop = make(chan struct{})
	s.closed = make(chan struct{})
	go s.writer()
	return s
}

// retainFromEnv reads NEO_TRACE_RETAIN; an absent/invalid value yields 0 (Open
// then applies the default).
func retainFromEnv() int {
	v := strings.TrimSpace(os.Getenv("NEO_TRACE_RETAIN"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// Record enqueues one workspace event for asynchronous persistence. It NEVER
// blocks (the broker publish path calls it): if the writer queue is saturated,
// the event is dropped, exactly like the broker drops events to slow
// subscribers. A blank run id or a disabled store is ignored.
func (s *Store) Record(runID string, ev Event) {
	if !s.Enabled() || runID == "" {
		return
	}
	select {
	case s.ch <- record{runID: runID, ev: ev}:
	default:
		// Queue saturated — drop rather than block the agent loop / publish
		// hot path. Best-effort sidecar (i_trace: never on the critical path).
		fmt.Fprintf(os.Stderr, "neo/trace: queue full, dropped %s event for %s\n", ev.Type, runID)
	}
}

// Flush blocks until the writer has drained every record enqueued before this
// call. Used by tests (to make the async write deterministic) and by graceful
// shutdown; it uses a blocking send, so it must NOT be called on the publish
// hot path. A disabled/closed store returns immediately.
func (s *Store) Flush() {
	if !s.Enabled() {
		return
	}
	done := make(chan struct{})
	select {
	case s.ch <- record{flush: done}:
		<-done
	case <-s.closed:
	}
}

// Close stops the background writer after draining the queue. Idempotent.
func (s *Store) Close() {
	if !s.Enabled() {
		return
	}
	s.once.Do(func() { close(s.stop) })
	<-s.closed
}

// writer is the single goroutine that owns all disk writes and the per-run
// event counts. It drains the queue until Close, persisting each event and
// enforcing the retained-event cap.
func (s *Store) writer() {
	defer close(s.closed)
	counts := map[string]int{}
	for {
		select {
		case <-s.stop:
			// Drain whatever is already queued, then exit.
			for {
				select {
				case rec := <-s.ch:
					s.handle(rec, counts)
				default:
					return
				}
			}
		case rec := <-s.ch:
			s.handle(rec, counts)
		}
	}
}

// handle persists one queued record (or resolves a flush sentinel). Runs only
// on the writer goroutine, so `counts` needs no lock.
func (s *Store) handle(rec record, counts map[string]int) {
	if rec.flush != nil {
		close(rec.flush)
		return
	}
	path := s.tracePath(rec.runID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "neo/trace: mkdir %s: %v\n", s.dir, err)
		return
	}
	// Lazily seed the count for a run the writer hasn't seen this process (a
	// resumed/reopened run whose file already exists), so the cap stays correct
	// across restarts.
	if _, ok := counts[rec.runID]; !ok {
		counts[rec.runID] = countLines(path)
	}
	if err := appendJSONL(path, rec.ev); err != nil {
		fmt.Fprintf(os.Stderr, "neo/trace: append %s: %v\n", path, err)
		return
	}
	counts[rec.runID]++
	// Amortized cap: roll only once the file grows to twice the retained cap,
	// then trim back to `retain`. This keeps the rewrite cost O(total) over a
	// run (a rewrite every `retain` events) instead of O(total·retain) — a
	// per-event rewrite would starve the writer on a long task. Load() still
	// presents at most `retain` events, so the cap is exact for consumers even
	// while the file briefly holds up to 2·retain.
	if counts[rec.runID] > 2*s.retain {
		if kept := s.rollLocked(path); kept >= 0 {
			counts[rec.runID] = kept
		}
	}
}

// Load returns the persisted workspace events for one run, oldest-first, or nil
// when there are none / persistence is disabled. Safe to call concurrently with
// the writer: every write is atomic, so a read sees a consistent file.
func (s *Store) Load(runID string) []Event {
	if !s.Enabled() || runID == "" {
		return nil
	}
	events := readJSONL(s.tracePath(runID))
	// Present at most `retain` events (newest), so the cap is exact for
	// consumers even though the on-disk file is trimmed only amortized
	// (handle rolls at 2·retain).
	if len(events) > s.retain {
		events = events[len(events)-s.retain:]
	}
	return events
}

func (s *Store) tracePath(runID string) string {
	if !s.Enabled() || runID == "" || strings.ContainsAny(runID, "/\\") {
		return ""
	}
	return filepath.Join(s.dir, runID+".trace.jsonl")
}

// rollLocked trims the run's JSONL to the newest `retain` events via an atomic
// tmp+rename rewrite. Returns the number of events kept, or -1 on error.
func (s *Store) rollLocked(path string) int {
	events := readJSONL(path)
	if len(events) <= s.retain {
		return len(events)
	}
	keep := events[len(events)-s.retain:]
	var buf bytes.Buffer
	for _, ev := range keep {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := path + ".rollup.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "neo/trace: rollup write %s: %v\n", tmp, err)
		return -1
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "neo/trace: rollup rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
		return -1
	}
	return len(keep)
}

// appendJSONL appends one event as a single JSON line. O_APPEND + a single
// Write makes the line atomically visible (POSIX); a torn write yields an
// unparseable final line that readJSONL skips (crash-atomic).
func appendJSONL(path string, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readJSONL parses a newline-delimited JSON event file. A line that fails to
// unmarshal (including a crash-truncated final line) is skipped — crash-atomic.
func readJSONL(path string) []Event {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// countLines returns the number of parseable JSONL events in a file (0 if
// missing). Used to seed the writer's per-run count for a pre-existing file.
func countLines(path string) int {
	return len(readJSONL(path))
}

// Dir resolves Neo's trace directory. An explicit override wins; else it is
// derived from the cortex root's parent (matching the conversation + task
// stores, so the workspace trace shares /data with them and survives suspend /
// redeploy). Returns "" when neither is available (persistence disabled).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "trace")
	}
	return ""
}
