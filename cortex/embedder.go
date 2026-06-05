// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 5 — async embedding worker.
//
// Spec: research/04-cortex.md §11.2 (embeddings are async; worker tails
// the journal and writes vec/meta + EmbeddingRef without blocking writes)
// and §13.1 (vector index + per-actor model pin).
//
// Lifecycle:
//   - StartEmbedder(opts) opens or rebuilds the HNSW index, loads the
//     embed cursor (next journal seq to process), and launches one
//     worker goroutine.
//   - Cortex.Write / Update / Tombstone call notifyEmbedder() after
//     commit to nudge the worker. The send is non-blocking; missed
//     notifications are recovered by the periodic tick OR by the worker
//     re-checking the journal head on each drain.
//   - DrainEmbedder(ctx) blocks until the worker has processed every
//     journal entry through the current head. Used by tests; production
//     callers seldom need it.
//   - StopEmbedder() signals the worker, waits for it to exit, and
//     persists the index file to disk.
//
// The worker is **single-threaded by design**. Multiple goroutines would
// race on HNSW Add ordering and break determinism. Throughput is bounded
// by Embed() latency, which is acceptable since the worker NEVER blocks
// writes — late embeddings just mean Find Near has reduced coverage for
// the few minutes between Write and embed completion.
//
// Atomic embed write: every successful embed runs one Pebble batch
// containing
//   1. vec/meta/<id> = canonical CBOR VectorMeta
//   2. m/<id> = updated Head with EmbeddingRef populated
//   3. meta/embed_cursor = next seq
//   4. meta/embed_vertex_next = next vertex id
//   5. journal KindEmbed entry
//
// Either all five commit or none does, preserving the replay invariant.

package cortex

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"matrix/cortex/embed"
	"matrix/cortex/forms"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/cortex/vector"
)

// EmbedderOptions configures StartEmbedder.
type EmbedderOptions struct {
	// Embedder is required. Phase 5 ships embed.NewHashEmbedder() as the
	// default deterministic stub; production swaps in an HTTP-backed
	// nomic-embed-text-v1.5 client.
	Embedder embed.Embedder

	// IndexPath is the on-disk location of the HNSW graph file. If empty,
	// the index is held in memory only (useful for tests). When non-empty
	// and the file exists, the index is loaded; otherwise an empty index
	// is constructed and any existing vec/meta entries are replayed into
	// it before the worker takes new entries.
	IndexPath string

	// HNSWParams overrides the default HNSW construction parameters. Zero
	// values fall back to the spec §19 defaults (M=16, efC=200, efS=64).
	// Dim is derived from Embedder.Dim().
	HNSWParams vector.Params

	// TickInterval is how often the worker re-checks the journal head as
	// a safety net against missed notifications. Defaults to 5 seconds.
	// Tests can override to a small value (e.g. 50 ms) so DrainEmbedder
	// completes quickly even without explicit notifications.
	TickInterval time.Duration

	// Logf, if non-nil, receives one-line error / progress messages from
	// the worker. Defaults to a no-op so library users don't get stderr
	// spam; tests usually wire t.Logf.
	Logf func(format string, args ...any)
}

// embedderState holds the running worker. Stored on Cortex.embed.
type embedderState struct {
	c        *Cortex
	embedder embed.Embedder
	index    *vector.Index
	store    *pebbleVectorStore
	opts     EmbedderOptions

	cursor     uint64 // next journal seq to process
	vertexNext uint64 // next HNSW vertex id to allocate

	notifyCh chan struct{}
	doneCh   chan struct{}
	drainCh  chan chan struct{} // requests for a drain barrier
	wg       sync.WaitGroup

	stopped atomic.Bool
}

// meta keys used by the embedder. These are not journaled — they're
// derived state that can be recomputed by walking the journal forward
// from seq=0. Persisted only for fast restart.
//
// metaEmbedModel records the Embedder.Model() string of the last
// completed pass. On StartEmbedder with a different model, the worker
// rewinds the cursor to 0 so it re-walks the journal and re-embeds every
// memory under the new model (Q3 α-locked lazy-migrate policy, sess#19).
var (
	metaEmbedCursor     = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_cursor")...)
	metaEmbedVertexNext = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_vertex_next")...)
	metaEmbedModel      = append(append([]byte{}, keys.PrefixMeta...), []byte("embed_model")...)
)

// StartEmbedder boots the async embedding worker. Returns an error if a
// worker is already running on this Cortex or if the index file is
// corrupted. The worker is fully drained of the existing journal backlog
// before the call returns, so writes that happen *after* StartEmbedder
// returns are guaranteed to be processed in order.
func (c *Cortex) StartEmbedder(opts EmbedderOptions) error {
	if c.embed != nil {
		return errors.New("cortex: embedder already running")
	}
	if opts.Embedder == nil {
		return errors.New("cortex: StartEmbedder requires opts.Embedder")
	}
	if opts.TickInterval <= 0 {
		opts.TickInterval = 5 * time.Second
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	// HNSW dimensionality follows the embedder; reject mismatched override.
	if opts.HNSWParams.Dim != 0 && opts.HNSWParams.Dim != opts.Embedder.Dim() {
		return fmt.Errorf("cortex: HNSW Dim %d != Embedder.Dim %d",
			opts.HNSWParams.Dim, opts.Embedder.Dim())
	}
	opts.HNSWParams.Dim = opts.Embedder.Dim()
	if opts.HNSWParams.Model == "" {
		opts.HNSWParams.Model = opts.Embedder.Model()
	}

	idx, err := loadOrBuildIndex(c.s, opts)
	if err != nil {
		return fmt.Errorf("cortex: load index: %w", err)
	}
	pStore := newPebbleVectorStore(c.s)
	idx.BindStore(pStore)

	cursor, err := readMetaUint64(c.s, metaEmbedCursor)
	if err != nil {
		return fmt.Errorf("cortex: read embed_cursor: %w", err)
	}
	vnext, err := readMetaUint64(c.s, metaEmbedVertexNext)
	if err != nil {
		return fmt.Errorf("cortex: read embed_vertex_next: %w", err)
	}
	if vnext == 0 {
		vnext = 1 // vertex IDs are 1-indexed (0 reserved as "absent")
	}

	// Q3 lazy-migrate (sess#19): if the last completed pass used a different
	// embedder model, rewind the cursor so the worker re-walks j/<seq> and
	// re-embeds every memory under the new model. The atomic per-memory check
	// at processWriteEntry (head.EmbeddingRef.Model == s.embedder.Model())
	// makes this idempotent: memories already at the new model are skipped.
	currentModel := opts.Embedder.Model()
	prevModelBytes, _, err := c.s.Get(metaEmbedModel)
	if err != nil {
		return fmt.Errorf("cortex: read embed_model: %w", err)
	}
	prevModel := string(prevModelBytes)
	if prevModel != "" && prevModel != currentModel {
		opts.Logf("embedder: model change detected (%q → %q); rewinding cursor for lazy re-embed",
			prevModel, currentModel)
		cursor = 0
	}
	if prevModel != currentModel {
		if err := c.s.SetMeta(metaEmbedModel, []byte(currentModel)); err != nil {
			return fmt.Errorf("cortex: persist embed_model: %w", err)
		}
	}

	state := &embedderState{
		c:          c,
		embedder:   opts.Embedder,
		index:      idx,
		store:      pStore,
		opts:       opts,
		cursor:     cursor,
		vertexNext: vnext,
		notifyCh:   make(chan struct{}, 1),
		doneCh:     make(chan struct{}),
		drainCh:    make(chan chan struct{}, 4),
	}
	c.embed = state

	state.wg.Add(1)
	go state.run()

	// Drain the initial backlog before returning so the caller observes
	// a "ready" embedder. Use a short timeout to avoid hanging tests if
	// the worker can't make progress.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.DrainEmbedder(ctx); err != nil {
		// Don't tear down — the worker is still running; just surface
		// the timeout so callers can decide.
		return fmt.Errorf("cortex: initial embedder drain: %w", err)
	}
	return nil
}

// StopEmbedder signals the worker, waits for it to exit, persists the
// index file (if IndexPath was set), and clears the embedder state.
// Idempotent.
func (c *Cortex) StopEmbedder() error {
	if c.embed == nil {
		return nil
	}
	state := c.embed
	if state.stopped.Swap(true) {
		return nil
	}
	close(state.doneCh)
	state.wg.Wait()
	c.embed = nil

	if state.opts.IndexPath != "" {
		if err := state.index.Save(state.opts.IndexPath); err != nil {
			return fmt.Errorf("cortex: save index: %w", err)
		}
	}
	return nil
}

// DrainEmbedder blocks until the worker's cursor catches up with the
// current journal head. Returns ctx.Err() on timeout / cancellation.
func (c *Cortex) DrainEmbedder(ctx context.Context) error {
	if c.embed == nil {
		return errors.New("cortex: embedder not running")
	}
	state := c.embed
	target := c.s.NextSeq()
	if target == 0 {
		return nil
	}
	// Fast path: cursor already past target.
	if atomic.LoadUint64(&state.cursor) >= target {
		return nil
	}
	ack := make(chan struct{})
	select {
	case state.drainCh <- ack:
	case <-ctx.Done():
		return ctx.Err()
	case <-state.doneCh:
		return errors.New("cortex: embedder stopped")
	}
	// Kick the worker so it processes immediately rather than waiting
	// for the next tick.
	select {
	case state.notifyCh <- struct{}{}:
	default:
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-state.doneCh:
		return errors.New("cortex: embedder stopped")
	}
}

// notifyEmbedder is called by Write/Update/Tombstone after their commit.
// Non-blocking: a missed notification is recovered by the periodic tick.
// Safe to call when no embedder is running (no-op).
func (c *Cortex) notifyEmbedder() {
	if c.embed == nil {
		return
	}
	select {
	case c.embed.notifyCh <- struct{}{}:
	default:
	}
}

// Index returns the live HNSW index handle if the embedder is running;
// nil otherwise. Query.Run reads this to honour Near / NearURI.
func (c *Cortex) Index() *vector.Index {
	if c.embed == nil {
		return nil
	}
	return c.embed.index
}

// EmbedderModel returns the model identifier of the running embedder, or
// "" if no embedder is running. Used by Find Near callers that want to
// audit "what embedder produced this result?".
func (c *Cortex) EmbedderModel() string {
	if c.embed == nil {
		return ""
	}
	return c.embed.embedder.Model()
}

// EmbedText is a small helper that runs the active embedder. Used by
// query.Run when honoring Query.Near (text → vector → HNSW search).
// Returns an error if no embedder is running.
func (c *Cortex) EmbedText(text string) ([]float32, error) {
	if c.embed == nil {
		return nil, errors.New("cortex: no embedder running")
	}
	return c.embed.embedder.Embed(text)
}

// --- worker -------------------------------------------------------------

func (s *embedderState) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.opts.TickInterval)
	defer ticker.Stop()

	for {
		// Drain pending work first so a notify / tick / drain request
		// all converge on the same processing loop.
		pending := s.drainPending()
		if pending != nil {
			s.opts.Logf("embedder drain: %v", pending)
		}

		// Service any drain requests whose target the cursor now satisfies.
		s.serviceDrainRequests()

		select {
		case <-s.doneCh:
			return
		case <-s.notifyCh:
			// loop and drain again
		case <-ticker.C:
			// periodic safety net
		}
	}
}

// drainPending walks the journal forward from cursor, processing each
// applicable entry. Returns the last non-nil error so the caller can log
// it without aborting the worker (transient failures are normal:
// embedder API may be temporarily down, file write may fail, etc.).
func (s *embedderState) drainPending() error {
	head := s.c.s.NextSeq()
	cursor := atomic.LoadUint64(&s.cursor)
	if cursor >= head {
		return nil
	}
	var lastErr error
	for cursor < head {
		entry, ok, err := readJournalEntry(s.c.s, cursor)
		if err != nil {
			lastErr = err
			break
		}
		if !ok {
			// gap — shouldn't happen given the gap-free invariant, but
			// guard against future migrations.
			cursor++
			continue
		}
		switch entry.Kind {
		case journal.KindWrite, journal.KindUpdate:
			if err := s.processWriteEntry(entry); err != nil {
				lastErr = fmt.Errorf("embedder: seq=%d %s: %w", entry.Seq, entry.Kind, err)
				// Don't advance cursor on real failure; retry next tick.
				if !isSkippable(err) {
					return lastErr
				}
			}
		case journal.KindTombstone:
			s.processTombstoneEntry(entry)
		default:
			// embed, find_late, gc, migration, raw — irrelevant to the
			// embedder.
		}
		cursor++
		atomic.StoreUint64(&s.cursor, cursor)
	}
	// Persist cursor outside the per-entry batches (cheap, async-OK).
	if err := writeMetaUint64(s.c.s, metaEmbedCursor, cursor); err != nil {
		s.opts.Logf("embedder: persist cursor: %v", err)
	}
	return lastErr
}

// isSkippable returns true for errors that should advance the cursor (we
// don't want to retry forever on a malformed entry).
func isSkippable(err error) bool {
	return errors.Is(err, memory.ErrNotFound) || errors.Is(err, errEmbedderSkip)
}

// errEmbedderSkip is returned by processWriteEntry when a memory is
// already-embedded-and-up-to-date or otherwise legitimately skippable.
var errEmbedderSkip = errors.New("embedder: skip")

// processWriteEntry embeds the memory referenced by entry and persists
// vec/meta + Head.EmbeddingRef + KindEmbed journal entry atomically.
func (s *embedderState) processWriteEntry(entry *journal.Entry) error {
	var p journal.WritePayload
	if err := journal.DecodeWritePayload(entry.Payload, &p); err != nil {
		return fmt.Errorf("decode WritePayload: %w", err)
	}
	mid := memory.ID(p.ID)

	// Load current head; skip if memory has been tombstoned in the
	// meantime (no point embedding a soft-deleted memory).
	head, ok, err := loadHead(s.c.s, mid)
	if err != nil {
		return fmt.Errorf("load head: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: head missing", memory.ErrNotFound)
	}
	if head.Tombstoned != nil {
		return errEmbedderSkip
	}

	// Skip if EmbeddingRef already points at this version's model — we
	// previously embedded this very memory and it hasn't changed. (Update
	// always changes version, so this only no-ops a Write that the
	// worker has already processed via a previous KindEmbed.)
	if head.EmbeddingRef != nil &&
		head.EmbeddingRef.Model == s.embedder.Model() &&
		!head.EmbeddingRef.Stale {
		// For KindUpdate we still need to re-embed since the Data
		// changed; check that the current version > the version we
		// embedded.
		if entry.Kind == journal.KindWrite {
			return errEmbedderSkip
		}
		// (KindUpdate falls through to re-embed below.)
	}

	// Fetch the latest version so we render against current Data.
	ver, ok, err := loadVersion(s.c.s, mid, head.CurrentVersion)
	if err != nil {
		return fmt.Errorf("load version: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w: version missing", memory.ErrNotFound)
	}

	// Render the full form to text and embed. Full form (rather than
	// short/medium) carries the most signal for semantic retrieval; the
	// budget-aware short/medium forms are for rendering, not recall.
	data, err := memory.DecodeData(ver.Type, ver.Data)
	if err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	text := forms.RenderFull(&head, data)
	vec, err := s.embedder.Embed(text)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	if len(vec) != s.embedder.Dim() {
		return embed.ErrDimMismatch
	}
	vecHash := memory.HashVector(vec)

	// Reuse the existing vertex id if we're re-embedding (an Update);
	// otherwise allocate a fresh one and insert into HNSW.
	var vid uint64
	if head.EmbeddingRef != nil && head.EmbeddingRef.VertexID != 0 {
		vid = head.EmbeddingRef.VertexID
		s.store.put(vid, vec)
		// HNSW updates are not supported by the simple Add path; we
		// model "update embedding" as "replace vector for the same
		// vertex". The graph stays connected because neighbours remain
		// reachable by id. Recall on the updated node may degrade
		// slightly until a future rebuild compacts the graph; this is
		// the simple-mode tradeoff documented in vector/vector.go.
	} else {
		vid = s.vertexNext
		s.vertexNext++
		s.store.put(vid, vec)
		if err := s.index.Add(vid, vector.MemoryID(mid), vec); err != nil {
			return fmt.Errorf("hnsw add: %w", err)
		}
	}

	// Build the atomic batch.
	now := s.c.now()
	newRef := &memory.VectorRef{
		VertexID: vid,
		Model:    s.embedder.Model(),
		Dim:      uint16(s.embedder.Dim()),
		Stale:    false,
	}
	updatedHead := head
	updatedHead.EmbeddingRef = newRef

	meta := &memory.VectorMeta{
		VertexID:      vid,
		Model:         s.embedder.Model(),
		Dim:           uint16(s.embedder.Dim()),
		Vector:        vec,
		SourceVersion: head.CurrentVersion,
		EmbeddedAt:    now,
		VectorHash:    vecHash,
	}
	metaBytes, err := memory.EncodeVectorMeta(meta)
	if err != nil {
		return fmt.Errorf("encode VectorMeta: %w", err)
	}
	headBytes, err := memory.EncodeHead(&updatedHead)
	if err != nil {
		return fmt.Errorf("encode head: %w", err)
	}

	ulid := toKeysULID(mid)
	wb := s.c.s.BeginWrite()
	defer wb.Abort()

	if err := wb.Set(keys.VecMetaKey(ulid), metaBytes); err != nil {
		return err
	}
	if err := wb.Set(keys.MemoryHeadKey(ulid), headBytes); err != nil {
		return err
	}
	// vertex_next is also persisted in this same batch so a crash between
	// batches can't reallocate the same vertex id.
	var vnextBuf [8]byte
	binary.BigEndian.PutUint64(vnextBuf[:], s.vertexNext)
	if err := wb.Set(metaEmbedVertexNext, vnextBuf[:]); err != nil {
		return err
	}

	embPayload := &journal.EmbedPayload{
		SchemaVersion: 1,
		ID:            p.ID,
		VertexID:      vid,
		Model:         s.embedder.Model(),
		Dim:           uint16(s.embedder.Dim()),
		VectorHash:    vecHash,
		SourceVersion: head.CurrentVersion,
	}
	embBytes, err := journal.EncodeEmbedPayload(embPayload)
	if err != nil {
		return fmt.Errorf("encode EmbedPayload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindEmbed,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte("embedder"),
		Payload:   embBytes,
	}
	if err := wb.AppendJournal(je); err != nil {
		return err
	}
	// Phase 7: the embedder rewrites Head with EmbeddingRef populated;
	// stage the memories SMT update so the snapshot root reflects the
	// new Head canonical bytes. Without this, replay (which re-runs the
	// embedder under HashEmbedder determinism) would produce a different
	// memories root than the live cortex.
	if err := s.c.snap.StageMemoryUpdate(wb, p.ID, headBytes); err != nil {
		return fmt.Errorf("stage SMT memories: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// processTombstoneEntry marks the HNSW node tombstoned so subsequent
// Search calls skip it. The vec/meta entry is left in place so a future
// un-tombstone path can re-index; only the in-memory HNSW flag is set.
func (s *embedderState) processTombstoneEntry(entry *journal.Entry) {
	// Tombstone payload is the raw 16-byte ID (Phase 2 shape, retained).
	if len(entry.Payload) != 16 {
		return
	}
	var mid memory.ID
	copy(mid[:], entry.Payload)
	s.index.Tombstone(vector.MemoryID(mid))
}

// serviceDrainRequests responds to any pending drain requests whose
// target seq has been reached.
func (s *embedderState) serviceDrainRequests() {
	cursor := atomic.LoadUint64(&s.cursor)
	head := s.c.s.NextSeq()
	if cursor < head {
		// More work to do — defer drain replies until next loop.
		return
	}
	for {
		select {
		case ack := <-s.drainCh:
			close(ack)
		default:
			return
		}
	}
}

// --- index load/rebuild --------------------------------------------------

// loadOrBuildIndex opens IndexPath if it exists; otherwise builds an empty
// index and rebuilds it from any existing vec/meta entries (so a startup
// with a deleted index file recovers without re-embedding).
func loadOrBuildIndex(s *store.Store, opts EmbedderOptions) (*vector.Index, error) {
	if opts.IndexPath != "" {
		if idx, err := vector.Load(opts.IndexPath); err == nil {
			return idx, nil
		}
		// Fall through to fresh build (covers missing-file and corrupted
		// cases; the corrupted case will be re-saved on next StopEmbedder).
	}
	idx := vector.NewIndex(opts.HNSWParams)
	idx.BindStore(newPebbleVectorStore(s))

	// Re-insert from vec/meta in vertex-id ascending order so the
	// resulting graph matches what the journal would produce on replay.
	var entries []rebuildEntry
	err := s.PrefixIter(keys.PrefixVecMeta, func(k, v []byte) error {
		if len(k) != len(keys.PrefixVecMeta)+keys.ULIDSize {
			return fmt.Errorf("malformed vec/meta key (len %d)", len(k))
		}
		var mid memory.ID
		copy(mid[:], k[len(keys.PrefixVecMeta):])
		var m memory.VectorMeta
		if err := memory.DecodeVectorMeta(v, &m); err != nil {
			return fmt.Errorf("decode VectorMeta: %w", err)
		}
		cp := make([]float32, len(m.Vector))
		copy(cp, m.Vector)
		entries = append(entries, rebuildEntry{vid: m.VertexID, mid: mid, vec: cp})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan vec/meta: %w", err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].vid < entries[b].vid })
	for _, e := range entries {
		if err := idx.Add(e.vid, vector.MemoryID(e.mid), e.vec); err != nil {
			// Duplicate id during rebuild is unexpected but not fatal —
			// skip and continue so an inconsistency in one record doesn't
			// brick the whole index.
			if errors.Is(err, vector.ErrDuplicateID) {
				continue
			}
			return nil, fmt.Errorf("rebuild Add vid=%d: %w", e.vid, err)
		}
	}
	return idx, nil
}

// rebuildEntry is a small named type the rebuild scan uses instead of an
// anonymous-struct slice; named so sort.Slice can index it cleanly.
type rebuildEntry struct {
	vid uint64
	mid memory.ID
	vec []float32
}

// --- helpers -------------------------------------------------------------

// loadHead reads m/<id> and decodes it.
func loadHead(s *store.Store, id memory.ID) (memory.Head, bool, error) {
	var u keys.ULID
	copy(u[:], id[:])
	b, ok, err := s.Get(keys.MemoryHeadKey(u))
	if err != nil || !ok {
		return memory.Head{}, ok, err
	}
	var h memory.Head
	if err := memory.DecodeHead(b, &h); err != nil {
		return memory.Head{}, false, err
	}
	return h, true, nil
}

// loadVersion reads mv/<id>/v/<n> and decodes it.
func loadVersion(s *store.Store, id memory.ID, ver uint64) (memory.Version, bool, error) {
	var u keys.ULID
	copy(u[:], id[:])
	b, ok, err := s.Get(keys.MemoryVersionKey(u, ver))
	if err != nil || !ok {
		return memory.Version{}, ok, err
	}
	var v memory.Version
	if err := memory.DecodeVersion(b, &v); err != nil {
		return memory.Version{}, false, err
	}
	return v, true, nil
}

// readJournalEntry reads j/<seq> directly. The Phase 1 store didn't expose
// a point-read on the journal (it shipped only IterJournal); this is a
// thin helper so the embedder doesn't have to walk from seq=0 every tick.
func readJournalEntry(s *store.Store, seq uint64) (*journal.Entry, bool, error) {
	b, ok, err := s.Get(keys.JournalKey(seq))
	if err != nil || !ok {
		return nil, ok, err
	}
	var e journal.Entry
	if err := journal.Decode(b, &e); err != nil {
		return nil, false, err
	}
	return &e, true, nil
}

// readMetaUint64 reads a uint64 from meta/<key>. Returns 0 if absent.
func readMetaUint64(s *store.Store, key []byte) (uint64, error) {
	b, ok, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	if len(b) != 8 {
		return 0, fmt.Errorf("cortex: meta key %q has length %d, want 8", key, len(b))
	}
	return binary.BigEndian.Uint64(b), nil
}

// writeMetaUint64 writes v to meta/<key>. NOT wrapped in a journaled
// batch — meta keys used here (embed_cursor, embed_vertex_next) are
// derived state, recomputable from the journal. Writing them outside the
// batch keeps the embedder's progress note independent of any specific
// commit.
func writeMetaUint64(s *store.Store, key []byte, v uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	// Use the underlying Pebble DB directly via the store's batch API
	// with a journal-only entry would be wrong (writeMetaUint64 isn't a
	// real mutation in the replay sense). Use the store's internal db.
	return s.SetMeta(key, buf[:])
}

// --- pebble-backed VectorStore ------------------------------------------

// pebbleVectorStore reads vectors from vec/meta in the per-actor Pebble
// DB. It caches recently-read vectors so HNSW search doesn't pay a
// decode hit per neighbour distance probe — caches are small and bounded.
type pebbleVectorStore struct {
	s     *store.Store
	mu    sync.RWMutex
	cache map[uint64][]float32
}

func newPebbleVectorStore(s *store.Store) *pebbleVectorStore {
	return &pebbleVectorStore{s: s, cache: map[uint64][]float32{}}
}

// put inserts vec into the cache. Called by the embedder after computing
// a new vector so subsequent neighbour probes during Add don't have to
// hit Pebble for it.
func (p *pebbleVectorStore) put(vid uint64, vec []float32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]float32, len(vec))
	copy(cp, vec)
	p.cache[vid] = cp
}

// GetVector implements vector.VectorStore by reading vec/meta and
// decoding the Vector field. Cache-on-read.
func (p *pebbleVectorStore) GetVector(vid uint64) ([]float32, bool) {
	p.mu.RLock()
	if v, ok := p.cache[vid]; ok {
		p.mu.RUnlock()
		return v, true
	}
	p.mu.RUnlock()
	// Slow path: scan vec/meta for the matching VertexID. We don't have
	// a vid→id index, so this is O(N); the cache amortises it after the
	// first miss. Phase 5 leaves this O(N) deliberately — a vid→id idx
	// is a Phase 8 follow-on if profile pressure shows it.
	var found []float32
	_ = p.s.PrefixIter(keys.PrefixVecMeta, func(k, v []byte) error {
		var m memory.VectorMeta
		if err := memory.DecodeVectorMeta(v, &m); err != nil {
			return nil
		}
		if m.VertexID == vid {
			cp := make([]float32, len(m.Vector))
			copy(cp, m.Vector)
			found = cp
			return errStopIter
		}
		return nil
	})
	if found == nil {
		return nil, false
	}
	p.mu.Lock()
	p.cache[vid] = found
	p.mu.Unlock()
	return found, true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
