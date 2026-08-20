// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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
// It is a pure SIDECAR: it never touches Neocortex, signs anything, or perturbs
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

	"centra/packages/vault"
)

// Store type and schema bound into each event's associated data, so a sealed
// trace line cannot be replayed across users, runs, or positions.
const (
	storeTrace    = "neo.trace"
	schemaEventV1 = "event.v1"
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
	// remove marks a deletion sentinel: the writer removes runID's trace file
	// (and forgets its count) on its own goroutine, so a CHAT-01 conversation
	// delete cannot race an in-flight append. The result channel makes cleanup
	// disclosure wait for the real filesystem outcome.
	remove chan error
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

	// vault seals each JSONL event when encrypting; nil = plaintext dev/CLI.
	// user is the DID bound into each event's associated data. Set once at engine
	// assembly (before any Record), so the writer/Load goroutines observe it via
	// the channel/serving happens-before edge without a lock.
	vault *vault.Session
	user  string
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store. Called once at engine assembly before any event is recorded; a nil
// session leaves the store writing legacy plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.vault = sess
	s.user = user
}

// ad reconstructs the associated data for an event from where it lives — never
// stored, so an event moved between users, runs, or positions fails auth.
func (s *Store) ad(runID string, seq uint64) vault.AD {
	return vault.AD{User: s.user, Store: storeTrace, Stream: runID, Seq: seq, Schema: schemaEventV1}
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

// Remove deletes one run's durable trace (CHAT-01 conversation delete cleanup).
// The removal runs on the writer goroutine so it cannot race an in-flight
// append. It waits for the filesystem result so cleanup is never reported from
// a merely queued request. A blank run id or disabled store returns false.
func (s *Store) Remove(runID string) bool {
	if !s.Enabled() || runID == "" {
		return false
	}
	done := make(chan error, 1)
	select {
	case s.ch <- record{runID: runID, remove: done}:
	case <-s.closed:
		return false
	}
	select {
	case err := <-done:
		return err == nil
	case <-s.closed:
		return false
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
		if rec.remove != nil {
			rec.remove <- os.ErrInvalid
			close(rec.remove)
		}
		return
	}
	if rec.remove != nil {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "neo/trace: remove %s: %v\n", path, err)
		}
		delete(counts, rec.runID)
		rec.remove <- err
		close(rec.remove)
		return
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "neo/trace: mkdir %s: %v\n", s.dir, err)
		return
	}
	// Lazily seed the count for a run the writer hasn't seen this process (a
	// resumed/reopened run whose file already exists), so the cap stays correct
	// across restarts.
	if _, ok := counts[rec.runID]; !ok {
		counts[rec.runID] = countLines(path)
	}
	if err := s.appendEvent(rec.runID, uint64(counts[rec.runID]), path, rec.ev); err != nil {
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
		if kept := s.rollLocked(rec.runID, path); kept >= 0 {
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
	events := s.readEvents(runID, s.tracePath(runID))
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
func (s *Store) rollLocked(runID, path string) int {
	events := s.readEvents(runID, path)
	if len(events) <= s.retain {
		return len(events)
	}
	keep := events[len(events)-s.retain:]
	var buf bytes.Buffer
	for i, ev := range keep {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		line, err := s.vault.EncodeLine(s.ad(runID, uint64(i)), b)
		if err != nil {
			return -1
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".rollup.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
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

// appendEvent appends one event as a single (sealed, when encrypting) JSONL
// line. O_APPEND + a single Write makes the line atomically visible (POSIX); a
// torn write yields an unparseable final line that readEvents skips
// (crash-atomic). seq binds the event to its position in the run.
func (s *Store) appendEvent(runID string, seq uint64, path string, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line, err := s.vault.EncodeLine(s.ad(runID, seq), b)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readEvents parses a newline-delimited event file, decrypting each record under
// the reconstructed associated data (legacy plaintext lines pass through until
// migrated). A line that fails to decrypt or unmarshal — including a wrong-key,
// tampered, or crash-truncated final line — is skipped; the record sequence
// still advances so surviving events stay bound to their positions.
func (s *Store) readEvents(runID, path string) []Event {
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
	var seq uint64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		plain, derr := s.vault.DecodeLine(s.ad(runID, seq), line)
		seq++
		if derr != nil {
			continue
		}
		var ev Event
		if err := json.Unmarshal(plain, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// countLines returns the number of non-empty JSONL lines in a file (0 if
// missing). Used to seed the writer's per-run next-sequence for a pre-existing
// file; it counts raw lines (no decryption) so it stays cheap and matches how
// readEvents advances the sequence per line.
func countLines(path string) int {
	if path == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		n++
	}
	return n
}

// Dir resolves Neo's trace directory. An explicit override wins; else it is
// derived from the Neocortex root's parent (matching the conversation + task
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
