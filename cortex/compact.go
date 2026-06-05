// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 9: budget-aware compaction (research/03-retrieval-patterns.md §5 +
// research/04-cortex.md §11/§12 + matrix.ctx IMPL_ORDER step 9).
//
// cortex.Compact takes a snapshot of the agent's "what is currently loaded"
// set, summarizes non-load-bearing items into {ref, short_form, salience}
// stubs per §5.1 step 2, and emits a journal checkpoint a re-entry path can
// later use to rebuild reasoning state (§5.2). Source memories are never
// mutated; salience is not bumped (research/03 §6 lists salience triggers
// exhaustively and Compact is not among them).
//
// Design decisions locked with Andrew (knowledge/matrix.ctx Phase 9 entry):
//
//	A1 — Hybrid storage. Canonical record at Pebble chk/<intent>/<step>
//	     (replay-invariant). Optional human-readable JSON mirror at
//	     <CheckpointDir>/<intent>/<step>.snapshot — matches the literal
//	     filesystem path in research/03 §5.1 step 3 and the workspace
//	     taxonomy in research/01 §4.10 (journal/thoughts/ = reasoning
//	     traces for replay/debugging/audit).
//	A2 — KindCompact journal entry emitted; MMR participation comes for
//	     free via the JournalHook installed in cortex.New (cortex.go:79).
//	     Phase 7's snapshot.State.MMRHook stages an MMR leaf inside the
//	     same Pebble batch.
//	A3 — Cortex auto-protects pinned items (Identity ∪ Constraint{Hard}
//	     ∪ Goal{Active}) present in opts.InContext, in addition to the
//	     caller's LoadBearing list. Mirrors the tierPinned predicates
//	     from context.go:435-515.
//	A4 — If the post-summarization token total still exceeds
//	     BudgetTokens, return ErrBudgetUnreachable. NO stage-2 drop
//	     ("summarize-and-link, never truncate" — matrix.ctx Phase 9
//	     framing; research/03 §5 step 2 only summarizes, never drops).
//	A5 — SnapshotURI returned for agent consumption is
//	     matrix://journal/logs/<intent>/<step> (Andrew D1 lock: kind =
//	     "logs", matching research/01 §4.10 workspace journal/logs/ =
//	     "the ledger of what actually happened"). SnapshotPath returned
//	     for human/dev debug is the filesystem mirror path (empty when
//	     CheckpointDir is "" or the mirror write failed).
//
// Determinism. The journal payload carries the inputs (intent_id, step_id,
// budget_tokens) plus the canonical CheckpointHash, not the kept/compacted
// listing. On replay, the same opts.InContext + cortex state at that journal
// seq produce byte-identical CheckpointRecord bytes, and therefore the same
// CheckpointHash — same posture as KindEmbed (journal/journal.go:48-67).
//
// Latency. p50 < 100 ms / ceiling 400 ms per §5.4. All reads are point reads
// on m/, mv/, salience/; no vector / no traversal / no idx scan.

package cortex

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
)

// DefaultCompactBudgetTokens is the value substituted when CompactOpts
// .BudgetTokens is zero. Matches the research/03 §5.3 example value
// (`budget_tokens: 4000`). No upper clamp — caller picks budget.
const DefaultCompactBudgetTokens = 4000

// CompactSchemaVersion is stamped on every emitted CompactPayload and
// CheckpointRecord. Bumping requires a journal-kind migration.
const CompactSchemaVersion uint8 = 1

// CompactedItem is the stub shape from research/03 §5.1 step 2:
//
//	Replace each [non-load-bearing item] with
//	`{ ref: "matrix://cortex/...", short_form: "<= 50 tokens>",
//	   salience: 0.83 }`.
//
// Returned in CompactResult.Compacted and persisted in CheckpointRecord.
// Salience is taken from the live salience cache at compaction time
// (NOT bumped — Compact is read-only with respect to salience).
type CompactedItem struct {
	Ref       memory.URI `cbor:"0,keyasint" json:"ref"`
	ShortForm string     `cbor:"1,keyasint" json:"short_form"`
	Salience  float32    `cbor:"2,keyasint" json:"salience"`
}

// CompactOpts mirrors the research/03 §5.3 primitive signature
// (in_context, load_bearing, budget_tokens) plus the §5.1 step 3
// path components (intent_id, step_id) and the A5 CheckpointDir.
type CompactOpts struct {
	// InContext is the agent-supplied "what's currently loaded" snapshot.
	// Spec §5.3: `in_context: TypedMemory[] // what's currently loaded`.
	// Tombstoned entries are silently dropped from both Kept and
	// Compacted (a tombstoned memory's #version may not Resolve cleanly
	// from a later turn; matches context.go:355 defensive filter).
	InContext []*memory.Memory

	// LoadBearing is the caller's explicit URI list of items that must
	// survive full (Kept). Spec §5.3: `load_bearing: string[] // URIs
	// the agent says it must keep full`. The cortex augments this set
	// with auto-detected pinned items per Andrew lock A3.
	LoadBearing []memory.URI

	// BudgetTokens is the post-compaction target. Spec §5.3
	// (`budget_tokens: 4000`). Zero → DefaultCompactBudgetTokens.
	BudgetTokens int

	// IntentID and StepID identify the checkpoint. Both required. Spec
	// §5.1 step 3 path: `journal/thoughts/<intent_id>/<step>.snapshot`.
	IntentID string
	StepID   string

	// CheckpointDir is the absolute filesystem path under which the
	// human-readable JSON mirror is written:
	//   <CheckpointDir>/<intent_id>/<step_id>.snapshot
	// Empty → Pebble-only (no mirror). Andrew lock A5. Failure to write
	// the mirror after a successful Pebble commit is logged and
	// signaled by SnapshotPath="" in the result (D3: filesystem mirror
	// is best-effort; Pebble is canonical).
	CheckpointDir string
}

// CompactResult mirrors research/03 §5.3 return shape
// (`{ kept, compacted, snapshot_uri }`), plus SnapshotPath from Andrew
// lock A5 to surface the human-readable mirror location for debug.
type CompactResult struct {
	Kept         []*memory.Memory
	Compacted    []CompactedItem
	SnapshotURI  memory.URI
	SnapshotPath string // empty when no filesystem mirror was written
}

// CheckpointRecord is the canonical CBOR-encoded blob persisted at
// chk/<intent>/<step> and mirrored as pretty JSON at
// <CheckpointDir>/<intent>/<step>.snapshot. Captures the compaction
// outcome — kept URIs + compacted stubs + the bounding metadata
// (budget, timestamp) — so a re-entry path (research/03 §5.2 step 1
// "Load the most recent journal checkpoint") can rebuild context
// without re-running Compact's partition decision.
//
// Scope note. research/03 §5.1 step 3 mentions a checkpoint also
// captures "reasoning state, working hypotheses, open sub-goals".
// Those are the agent's responsibility (its own journal/thoughts/
// payload, separate from cortex.Compact's output). The cortex
// checkpoint covers only what cortex itself authored — kept refs,
// compacted stubs, and the bounding parameters.
type CheckpointRecord struct {
	SchemaVersion uint8           `cbor:"0,keyasint" json:"schema_version"`
	IntentID      string          `cbor:"1,keyasint" json:"intent_id"`
	StepID        string          `cbor:"2,keyasint" json:"step_id"`
	CreatedAt     int64           `cbor:"3,keyasint" json:"created_at_unix_nano"`
	BudgetTokens  uint32          `cbor:"4,keyasint" json:"budget_tokens"`
	KeptURIs      []memory.URI    `cbor:"5,keyasint" json:"kept_uris"`
	Compacted     []CompactedItem `cbor:"6,keyasint" json:"compacted"`
}

// Errors raised by Compact.
var (
	// ErrEmptyInContext: §5.3 contract requires at least one TypedMemory.
	ErrEmptyInContext = errors.New("cortex.Compact: in_context is empty")

	// ErrEmptyIntentID, ErrEmptyStepID: §5.1 step 3 requires both.
	ErrEmptyIntentID = errors.New("cortex.Compact: intent_id is empty")
	ErrEmptyStepID   = errors.New("cortex.Compact: step_id is empty")

	// ErrBudgetUnreachable: post-summarization total still exceeds
	// BudgetTokens. Andrew lock A4: hard error, no stage-2 drop.
	// "Summarize-and-link, never truncate" — matrix.ctx Phase 9 framing.
	ErrBudgetUnreachable = errors.New(
		"cortex.Compact: budget unreachable after full summarization")
)

// Canonical CBOR encoder for CheckpointRecord. Mirrors the journal
// package's init() pattern (journal/journal.go:170-183). CoreDetEncOptions
// produces RFC 8949 §4.2.1 deterministic encoding; required because the
// encoded bytes are integrity-hashed into CompactPayload.CheckpointHash.
var (
	cpEnc cbor.EncMode
	cpDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex/compact: build EncMode: %w", err))
	}
	cpEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex/compact: build DecMode: %w", err))
	}
	cpDec = dm
}

// EncodeCheckpointRecord returns canonical deterministic CBOR for r.
func EncodeCheckpointRecord(r *CheckpointRecord) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("cortex/compact: nil CheckpointRecord")
	}
	return cpEnc.Marshal(r)
}

// DecodeCheckpointRecord parses canonical CBOR into out.
func DecodeCheckpointRecord(b []byte, out *CheckpointRecord) error {
	return cpDec.Unmarshal(b, out)
}

// BuildCheckpointURI returns the agent-facing canonical URI for a
// checkpoint. Andrew lock D1: kind="logs". Format:
//
//	matrix://journal/logs/<intent_id>/<step_id>
//
// Extends research/02-protocol.md §URIs scheme matrix://journal/{kind}/
// {id} to the two-component {intent}/{step} naming required by §5.1
// step 3.
func BuildCheckpointURI(intentID, stepID string) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://journal/logs/%s/%s",
		intentID, stepID))
}

// BuildCheckpointFilePath returns the filesystem mirror path used when
// CheckpointOpts.CheckpointDir is non-empty:
//
//	<dir>/<intent_id>/<step_id>.snapshot
//
// Matches the literal §5.1 step 3 form `journal/thoughts/<intent_id>/
// <step>.snapshot`. The .snapshot suffix is preserved verbatim.
func BuildCheckpointFilePath(dir, intentID, stepID string) string {
	return filepath.Join(dir, intentID, stepID+".snapshot")
}

// Compact runs the §5 phase-4 compaction primitive. See package comment
// for the full design contract; the algorithm comments below cite each
// step's spec / lock source.
func (c *Cortex) Compact(opts CompactOpts) (*CompactResult, error) {
	// --- Step 1: validate inputs (§5.3 contract) -------------------
	if len(opts.InContext) == 0 {
		return nil, ErrEmptyInContext
	}
	if opts.IntentID == "" {
		return nil, ErrEmptyIntentID
	}
	if opts.StepID == "" {
		return nil, ErrEmptyStepID
	}
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = DefaultCompactBudgetTokens
	}
	// CheckpointKey validates intent/step for forbidden '/' (§2
	// invariant). Surfacing that here short-circuits the algorithm.
	chkKey, err := keys.CheckpointKey(opts.IntentID, opts.StepID)
	if err != nil {
		return nil, fmt.Errorf("cortex.Compact: %w", err)
	}

	now := c.now()

	// Phase 12: load per-actor learned weights for live cold-score
	// recompute (matches find.go / context.go); cold start returns
	// DefaultWeights.
	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, fmt.Errorf("cortex.Compact: read weights: %w", err)
	}

	// --- Step 2: build effective load_bearing set (caller ∪ pinned)
	// Andrew lock A3: cortex auto-protects pinned items present in
	// in_context. Pinned predicates mirror context.go:435-515:
	// Identity, or Constraint{StrengthHard}, or Goal{GoalActive}.
	lbSet := map[memory.URI]struct{}{}
	for _, u := range opts.LoadBearing {
		lbSet[u] = struct{}{}
	}

	// --- Step 3: walk InContext once, classify each surviving item
	// into Kept (load-bearing or pinned) vs Compactable.
	type itemEntry struct {
		mem      *memory.Memory
		uri      memory.URI
		isKept   bool
		pinned   bool
		salience float32
	}
	entries := make([]itemEntry, 0, len(opts.InContext))

	for _, m := range opts.InContext {
		if m == nil {
			continue // defensive; spec doesn't say nil is meaningful
		}
		// Tombstone filter (mirrors context.go:355). A tombstoned
		// memory cannot meaningfully appear in a re-entry context.
		if m.Head.Tombstoned != nil {
			continue
		}
		uri := BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)

		_, listed := lbSet[uri]
		pinned, perr := c.isPinnedInCompact(m)
		if perr != nil {
			return nil, fmt.Errorf("cortex.Compact: pinned check %s: %w",
				m.Head.ID, perr)
		}

		// Phase 12: salience computed live from persisted factor inputs
		// using the actor's current learned weights (cf. find.go).
		var score float32
		sc, ok, err := salience.Read(c.s, m.Head.ID)
		if err != nil {
			return nil, fmt.Errorf("cortex.Compact: salience read %s: %w",
				m.Head.ID, err)
		}
		if ok {
			score = salience.ColdScoreWith(sc, weights, now)
		} else {
			seed := salience.Score{
				LastUsed:   m.Version.CreatedAt.UnixNano(),
				Importance: m.Head.DeclaredImportance,
			}
			score = salience.ColdScoreWith(&seed, weights, now)
		}

		entries = append(entries, itemEntry{
			mem:      m,
			uri:      uri,
			isKept:   listed || pinned,
			pinned:   pinned,
			salience: score,
		})
	}

	if len(entries) == 0 {
		// All entries were nil or tombstoned. Treat as empty-input.
		return nil, ErrEmptyInContext
	}

	// --- Step 4: build Kept[] and Compacted[] ----------------------
	var (
		kept       []*memory.Memory
		keptURIs   []memory.URI
		compacted  []CompactedItem
		keptTokens int
	)
	for _, e := range entries {
		if e.isKept {
			kept = append(kept, e.mem)
			keptURIs = append(keptURIs, e.uri)
			// Token cost of kept = Version.Forms.Medium count
			// (Andrew lock D2: kept items are counted at medium-form
			// budget — matches Phase 8 cold-start default,
			// context.go:253 + research/03 §7 row "medium_form ≤ 200
			// | normal cold-start bundle inclusion").
			keptTokens += memory.CountTokens(e.mem.Version.Forms.Medium)
			continue
		}
		// Compactable: build {ref, short_form, salience} stub per §5.1
		// step 2. ShortForm sourced from Version.Forms.Short (rendered
		// at write-time per cortex.go:147-150 via forms.Render; ≤ 50
		// tokens by §7 + forms.Render's budget enforcement).
		compacted = append(compacted, CompactedItem{
			Ref:       e.uri,
			ShortForm: e.mem.Version.Forms.Short,
			Salience:  e.salience,
		})
	}

	// --- Step 5: token accounting ---------------------------------
	compactedTokens := 0
	for _, ci := range compacted {
		compactedTokens += memory.CountTokens(ci.ShortForm)
	}
	total := keptTokens + compactedTokens
	if total > opts.BudgetTokens {
		// Andrew lock A4: hard error after full summarization. Carry
		// the diagnostics in the wrapped error so the caller can
		// decide which InContext items to shed before retrying.
		return nil, fmt.Errorf("%w: total=%d budget=%d (kept=%d compacted=%d, kept_tokens=%d compacted_tokens=%d)",
			ErrBudgetUnreachable, total, opts.BudgetTokens,
			len(kept), len(compacted), keptTokens, compactedTokens)
	}

	// --- Step 6: build + canonicalize CheckpointRecord ------------
	record := &CheckpointRecord{
		SchemaVersion: CompactSchemaVersion,
		IntentID:      opts.IntentID,
		StepID:        opts.StepID,
		CreatedAt:     now.UnixNano(),
		BudgetTokens:  uint32(opts.BudgetTokens),
		KeptURIs:      keptURIs,
		Compacted:     compacted,
	}
	encodedRec, err := EncodeCheckpointRecord(record)
	if err != nil {
		return nil, fmt.Errorf("cortex.Compact: encode checkpoint: %w", err)
	}
	checkpointHash := sha256.Sum256(encodedRec)

	// --- Step 7: build CompactPayload + Journal Entry -------------
	cp := &journal.CompactPayload{
		SchemaVersion:  CompactSchemaVersion,
		IntentID:       opts.IntentID,
		StepID:         opts.StepID,
		BudgetTokens:   uint32(opts.BudgetTokens),
		KeptCount:      uint32(len(kept)),
		CompactedCount: uint32(len(compacted)),
		CheckpointHash: checkpointHash,
	}
	cpBytes, err := journal.EncodeCompactPayload(cp)
	if err != nil {
		return nil, fmt.Errorf("cortex.Compact: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindCompact,
		CreatedAt: now.UnixNano(),
		// CreatedBy intentionally empty — Compact is not directly
		// triggered by an attestation. Audit chain via the surrounding
		// intent journal (caller's domain).
		Payload: cpBytes,
	}

	// --- Step 8: atomic Pebble batch (cortex.go:187-286 pattern) -
	// Two staged keys: chk/<intent>/<step> + j/<seq>. MMR leaf is
	// staged automatically by the JournalHook installed in cortex.New
	// (cortex.go:79). No SMT update — checkpoints are derived audit,
	// not canonical world-state (consistent with Phase 7 lock: only
	// "memories" and "edges" namespaces are anchored).
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(chkKey, encodedRec); err != nil {
		return nil, fmt.Errorf("cortex.Compact: set chk: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return nil, fmt.Errorf("cortex.Compact: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return nil, fmt.Errorf("cortex.Compact: commit: %w", err)
	}

	// --- Step 9: filesystem mirror (D3: best-effort) --------------
	// Pebble is canonical. If the mirror write fails (disk full,
	// permissions, etc.) we log a warning and return SnapshotPath="".
	// We never roll back the Pebble commit on mirror failure — that
	// would breach Phase 7's "every j/ entry is canonical" invariant.
	snapshotPath := ""
	if opts.CheckpointDir != "" {
		p := BuildCheckpointFilePath(opts.CheckpointDir, opts.IntentID, opts.StepID)
		if werr := writeCheckpointMirror(p, record); werr != nil {
			log.Printf("cortex.Compact: filesystem mirror write failed at %s: %v",
				p, werr)
		} else {
			snapshotPath = p
		}
	}

	return &CompactResult{
		Kept:         kept,
		Compacted:    compacted,
		SnapshotURI:  BuildCheckpointURI(opts.IntentID, opts.StepID),
		SnapshotPath: snapshotPath,
	}, nil
}

// isPinnedInCompact returns true when m is in the Pinned tier as
// defined by cortex.context (context.go:435-515 + research/03 §2.1 +
// research/04 §12.1): Identity always; Constraint iff StrengthHard;
// Goal iff GoalActive. Tombstoned items are always non-pinned (caller
// filters them before calling this).
//
// Decode errors are propagated; "data is the wrong shape for its
// type tag" is structurally impossible for a memory that survived
// write-time validation (memory.ValidateMemory at cortex.go:168), so
// surfacing the error helps diagnose corruption rather than mask it.
func (c *Cortex) isPinnedInCompact(m *memory.Memory) (bool, error) {
	switch m.Head.Type {
	case memory.TypeIdentity:
		return true, nil
	case memory.TypeConstraint:
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			return false, fmt.Errorf("decode constraint: %w", err)
		}
		cd, ok := data.(memory.ConstraintData)
		if !ok {
			return false, nil
		}
		return cd.StrengthVal == memory.StrengthHard, nil
	case memory.TypeGoal:
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			return false, fmt.Errorf("decode goal: %w", err)
		}
		gd, ok := data.(memory.GoalData)
		if !ok {
			return false, nil
		}
		return gd.Status == memory.GoalActive, nil
	}
	return false, nil
}

// writeCheckpointMirror writes a pretty-printed JSON encoding of r to
// path, creating parent directories as needed. JSON (not CBOR) is used
// because Andrew lock A5 framing is "for human or dev debugging = file
// path"; pretty JSON is the most readable serialization.
//
// The file write uses 0o600 (owner read/write) because checkpoints may
// contain references / short forms with user-private content; matches
// the per-actor isolation principle (research/04 §1 "single Pebble DB
// per actor").
func writeCheckpointMirror(path string, r *CheckpointRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdirall: %w", err)
	}
	enc, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// LoadCheckpoint returns the persisted CheckpointRecord stored at
// chk/<intent>/<step>. Returns memory.ErrNotFound when no record
// exists. Used by re-entry helpers (research/03 §5.2 step 1 "Load
// the most recent journal checkpoint") and the cortex-shell dump
// surface.
func (c *Cortex) LoadCheckpoint(intentID, stepID string) (*CheckpointRecord, error) {
	k, err := keys.CheckpointKey(intentID, stepID)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadCheckpoint: %w", err)
	}
	raw, ok, err := c.s.Get(k)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadCheckpoint: get: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var rec CheckpointRecord
	if err := DecodeCheckpointRecord(raw, &rec); err != nil {
		return nil, fmt.Errorf("cortex.LoadCheckpoint: decode: %w", err)
	}
	return &rec, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
