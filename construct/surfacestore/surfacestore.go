// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package surfacestore is the Construct OS Shell's durable, per-user, per-
// conversation workspace memory — the "never vanishing" record of the computer
// Neo projects.
//
// It is a generalization of Neo's F3 trace sidecar (neo/internal/trace): same
// async-background-writer + append-only JSONL + amortized atomic-rollup +
// /data-rooted discipline, but keyed by conversationID instead of run and
// recording the typed Construct surface event stream specifically. The live
// Construct workspace — surfaces and their progressive patches — streams to the
// client as SSE events that live ONLY in the in-memory broker's replay buffer,
// so once a run settles or the buffer drops, a reopened conversation shows an
// EMPTY computer. This store fixes that: it persists every
// construct.surface[.patch] frame per conversation as append-only JSONL (one
// line per frame), so the environment rehydrates exactly as the user left it
// across reload, suspend, redeploy, and device switch.
//
// SIDE-CHANNEL INVARIANT (load-bearing, D11): this is a pure VIEW/persistence
// sidecar. It NEVER writes cortex, NEVER signs an envelope, and NEVER touches
// plan/walk — exactly like the Liaison and the F3 trace store. The persisted
// frames are the exact transcript SSE frames the client already renders, so a
// reopen replays them through the SAME reducer (transport.ApplyPatch mirror)
// the live stream uses, guaranteeing the rehydrated computer is byte-identical
// to the live one. Persistence is therefore incapable of perturbing the D11
// replay byte-identity invariant.
//
// Hot-path discipline (R16): Record is a NON-BLOCKING enqueue onto a buffered
// channel drained by a single background writer goroutine. It never does disk
// I/O on the caller's goroutine (the broker publish path) and, like the
// broker's own slow-subscriber handling, DROPS rather than blocks if the queue
// is saturated — the surface timeline is a best-effort observability record,
// never on the critical path of the agent loop.
//
// Retained-frame cap: each conversation's JSONL is bounded by the retained-
// frame cap (config CONSTRUCT_SURFACE_RETAIN, default DefaultRetainedFrames).
// When a conversation exceeds the cap, the oldest frames are trimmed via an
// atomic rewrite (tmp+rename) — bounded like the broker's replay buffer, just
// larger and durable.
package surfacestore

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

	"matrix/construct/schema"
	"matrix/vault"
)

// Store type and schema bound into each frame's associated data, so a sealed
// surface line cannot be replayed across users, conversations, or positions.
const (
	storeSurface  = "construct.surface"
	schemaFrameV1 = "frame.v1"
)

// DefaultRetainedFrames caps how many surface frames are retained per
// conversation on disk. Larger than the broker's live replay buffer because
// this is the durable record a reopen rebuilds from; still bounded so a runaway
// conversation can't grow the file without limit. It is also the bound the
// rehydration read path returns (R16.4), so cold-open cost is bounded.
const DefaultRetainedFrames = 2000

// queueDepth is how many pending frames the async writer buffers before Record
// starts dropping. Generous: a single user's single daemon emits tens-to-low-
// hundreds of surface frames per run, so this is never reached in practice; it
// exists only so a pathological burst can't block broker.publish.
const queueDepth = 4096

// Frame is one persisted Construct surface event. It is an alias of the
// canonical wire type schema.Frame, which is the single source of truth that
// the codegen mirrors into the client's types.gen.ts — so the persisted shape
// can never drift from the generated client type. Its shape mirrors the daemon
// SSE Event ({seq,ts,phase,type,fields}) — and neo/internal/trace.Event — so
// the persisted stream replays through the client's existing reducer
// byte-for-byte on a reopen.
//
// Phase is always transport.Phase ("construct"); Type is one of
// transport.EventSurface ("construct.surface") or transport.EventSurfacePatch
// ("construct.surface.patch").
type Frame = schema.Frame

// record is one queued write. A non-nil flush channel marks a flush sentinel
// (no frame payload); the writer closes it once it has drained every record
// enqueued before it.
type record struct {
	conversationID string
	fr             Frame
	flush          chan struct{}
}

// Store is the Construct OS Shell's durable per-conversation surface timeline. A
// single background writer goroutine owns all disk writes (and the per-
// conversation frame counts), so the only shared state is the enqueue channel;
// reads (Load) go straight to the filesystem and are safe against the writer
// because every write is atomic (an O_APPEND single-line write, or a
// tmp+rename rollup).
type Store struct {
	dir    string
	retain int

	ch     chan record
	stop   chan struct{}
	closed chan struct{}
	once   sync.Once

	// vault seals each JSONL frame when encrypting; nil = plaintext dev/CLI.
	// user is the DID bound into each frame's associated data. Set once at engine
	// assembly (before any Record) so the writer/Load goroutines observe it via
	// the channel/serving happens-before edge without a lock.
	vault *vault.Session
	user  string
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store. Called once at assembly before any frame is recorded; a nil session
// leaves the store writing legacy plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.vault = sess
	s.user = user
}

// ad reconstructs the associated data for a frame from where it lives — never
// stored, so a frame moved between users, conversations, or positions fails auth.
func (s *Store) ad(conversationID string, seq uint64) vault.AD {
	return vault.AD{User: s.user, Store: storeSurface, Stream: conversationID, Seq: seq, Schema: schemaFrameV1}
}

// Open builds a store rooted at dir (shares the per-user /data volume with the
// cortex, conversation, task, and trace stores) and starts its background
// writer. An empty dir yields a disabled store (every method is a safe no-op)
// so dev/CLI runs work unchanged. The retained-frame cap defaults to
// DefaultRetainedFrames and can be overridden via CONSTRUCT_SURFACE_RETAIN
// (read once at Open).
func Open(dir string) *Store {
	return openWithCap(dir, retainFromEnv())
}

// openWithCap is the test seam that sets an explicit retained-frame cap.
func openWithCap(dir string, cap int) *Store {
	s := &Store{
		dir:    strings.TrimSpace(dir),
		retain: cap,
	}
	if s.retain <= 0 {
		s.retain = DefaultRetainedFrames
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

// retainFromEnv reads CONSTRUCT_SURFACE_RETAIN; an absent/invalid value yields 0
// (Open then applies the default).
func retainFromEnv() int {
	v := strings.TrimSpace(os.Getenv("CONSTRUCT_SURFACE_RETAIN"))
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

// Record enqueues one surface frame for asynchronous persistence. It NEVER
// blocks (the broker publish path calls it): if the writer queue is saturated,
// the frame is dropped, exactly like the broker drops events to slow
// subscribers. A disabled store, a blank conversation id, or a conversation id
// containing a path separator is ignored (never persisted) — the latter both
// as input validation and as a path-traversal guard.
func (s *Store) Record(conversationID string, fr Frame) {
	if !s.Enabled() || !validConversationID(conversationID) {
		return
	}
	select {
	case s.ch <- record{conversationID: conversationID, fr: fr}:
	default:
		// Queue saturated — drop rather than block the agent loop / publish
		// hot path. Best-effort sidecar: never on the critical path (R16.3).
		fmt.Fprintf(os.Stderr, "construct/surfacestore: queue full, dropped %s frame for %s\n", fr.Type, conversationID)
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

// writer is the single goroutine that owns all disk writes and the per-
// conversation frame counts. It drains the queue until Close, persisting each
// frame and enforcing the retained-frame cap.
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
	path := s.surfacePath(rec.conversationID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "construct/surfacestore: mkdir %s: %v\n", s.dir, err)
		return
	}
	// Lazily seed the count for a conversation the writer hasn't seen this
	// process (a resumed/reopened conversation whose file already exists), so
	// the cap stays correct across restarts.
	if _, ok := counts[rec.conversationID]; !ok {
		counts[rec.conversationID] = countLines(path)
	}
	if err := s.appendFrame(rec.conversationID, uint64(counts[rec.conversationID]), path, rec.fr); err != nil {
		fmt.Fprintf(os.Stderr, "construct/surfacestore: append %s: %v\n", path, err)
		return
	}
	counts[rec.conversationID]++
	// Amortized cap: roll only once the file grows to twice the retained cap,
	// then trim back to `retain`. This keeps the rewrite cost O(total) over a
	// conversation (a rewrite every `retain` frames) instead of O(total·retain)
	// — a per-frame rewrite would starve the writer on a long task. Load() still
	// presents at most `retain` frames, so the cap is exact for consumers even
	// while the file briefly holds up to 2·retain.
	if counts[rec.conversationID] > 2*s.retain {
		if kept := s.rollLocked(rec.conversationID, path); kept >= 0 {
			counts[rec.conversationID] = kept
		}
	}
}

// Load returns the persisted surface frames for one conversation, oldest-first,
// or nil when there are none / persistence is disabled / the conversation id is
// invalid (contains a path separator). Safe to call concurrently with the
// writer: every write is atomic, so a read sees a consistent file.
func (s *Store) Load(conversationID string) []Frame {
	if !s.Enabled() || !validConversationID(conversationID) {
		return nil
	}
	frames := s.readFrames(conversationID, s.surfacePath(conversationID))
	// Present at most `retain` frames (newest), so the cap is exact for
	// consumers even though the on-disk file is trimmed only amortized
	// (handle rolls at 2·retain). This is the bound the rehydration read path
	// surfaces so a cold open is O(retain) (R16.4).
	if len(frames) > s.retain {
		frames = frames[len(frames)-s.retain:]
	}
	return frames
}

// validConversationID rejects a blank id or one containing a path separator
// (both POSIX "/" and Windows "\\"), so a conversation id can never escape the
// store directory (R15: per-user isolation / conversation scoping).
func validConversationID(conversationID string) bool {
	return conversationID != "" && !strings.ContainsAny(conversationID, "/\\")
}

func (s *Store) surfacePath(conversationID string) string {
	if !s.Enabled() || !validConversationID(conversationID) {
		return ""
	}
	return filepath.Join(s.dir, conversationID+".surfaces.jsonl")
}

// rollLocked trims the conversation's JSONL to the newest `retain` frames via an
// atomic tmp+rename rewrite. Returns the number of frames kept, or -1 on error.
func (s *Store) rollLocked(conversationID, path string) int {
	frames := s.readFrames(conversationID, path)
	if len(frames) <= s.retain {
		return len(frames)
	}
	keep := frames[len(frames)-s.retain:]
	var buf bytes.Buffer
	for i, fr := range keep {
		b, err := json.Marshal(fr)
		if err != nil {
			continue
		}
		line, err := s.vault.EncodeLine(s.ad(conversationID, uint64(i)), b)
		if err != nil {
			return -1
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := path + ".rollup.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "construct/surfacestore: rollup write %s: %v\n", tmp, err)
		return -1
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "construct/surfacestore: rollup rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
		return -1
	}
	return len(keep)
}

// appendFrame appends one frame as a single (sealed, when encrypting) JSONL
// line. O_APPEND + a single Write makes the line atomically visible (POSIX); a
// torn write yields an unparseable final line that readFrames skips
// (crash-atomic). seq binds the frame to its position in the conversation.
func (s *Store) appendFrame(conversationID string, seq uint64, path string, fr Frame) error {
	b, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	line, err := s.vault.EncodeLine(s.ad(conversationID, seq), b)
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

// readFrames parses a newline-delimited frame file, decrypting each record under
// the reconstructed associated data (legacy plaintext lines pass through until
// migrated). A line that fails to decrypt or unmarshal — including a wrong-key,
// tampered, or crash-truncated final line — is skipped; the record sequence
// still advances so surviving frames stay bound to their positions.
func (s *Store) readFrames(conversationID, path string) []Frame {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Frame
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var seq uint64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		plain, derr := s.vault.DecodeLine(s.ad(conversationID, seq), line)
		seq++
		if derr != nil {
			continue
		}
		var fr Frame
		if err := json.Unmarshal(plain, &fr); err != nil {
			continue
		}
		out = append(out, fr)
	}
	return out
}

// countLines returns the number of non-empty JSONL lines in a file (0 if
// missing). Used to seed the writer's per-conversation next-sequence for a
// pre-existing file; it counts raw lines (no decryption) so it stays cheap and
// matches how readFrames advances the sequence per line.
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

// Dir resolves the surface-store directory. An explicit override wins; else it
// is derived from the cortex root's parent (matching the conversation, task,
// and trace stores, so the surface record shares the per-user /data volume with
// them and survives suspend / redeploy). Returns "" when neither is available
// (persistence disabled).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "surfaces")
	}
	return ""
}
