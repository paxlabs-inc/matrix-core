// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 2.1: the temporal-ladder ROLLUP record (T0/T1
// substrate) + a deterministic extractive builder.
//
// A rollup is a DERIVED, journaled-but-NOT-anchored record keyed by TIME
// WINDOW — a roll/<tier><start> record in the SAME lane as cortex.Compact
// (compact.go:429-431) and the session store (session.go). BuildRollup NEVER
// calls snap.StageMemoryUpdate / StageEdgeUpdate, so it perturbs neither the
// anchored "memories"/"edges" SMT roots nor the D11 replay byte-identity of
// the canonical world-state. (OverallRoot itself moves because the journal
// MMR grows a KindRollup leaf — expected, identical to KindCompact /
// KindSession.)
//
// # Determinism (the whole point — req.4.2)
//
// A rollup is a PURE function of the journal facts in its window plus the
// stored salience factors and the actor's learned weights. Two properties
// guarantee byte-identical output for the same store state:
//
//  1. The salience ranking uses refTime = the WINDOW END as its reference
//     clock (NOT wall-now). ColdScoreWith decays recency against a fixed
//     window-relative clock, so re-running BuildRollup at any later wall time
//     yields the same scores and therefore the same record bytes.
//
//  2. Member ranking is (score DESC, then memory ID ASCENDING) — a total,
//     stable order with no wall-clock or map-iteration dependence. Map tallies
//     (KindTally / OutcomeTally) are encoded via the canonical CoreDetEnc
//     encoder, which sorts map keys deterministically, and the ShortForm
//     renders every tally with alphabetically-sorted keys.
//
// KindRollup journal entries are EXCLUDED from windowing (a rollup never
// summarizes rollups-of-itself). This is what makes BuildRollup idempotent:
// its own emitted KindRollup entry — whose CreatedAt is wall-now and may fall
// inside the window — is skipped on every subsequent rebuild, so re-running
// over the same window produces a byte-identical RollupRecord and RecordHash
// (req.4.1).
//
// Windows are fixed wall-clock UTC buckets: hour (3600s), day (86400s), and
// epoch (7-day week). Because Unix time is UTC-anchored at
// 1970-01-01T00:00:00Z (a Thursday), truncating a timestamp by the bucket
// width aligns hours/days to UTC boundaries and weeks (epochs) to Thursday
// 00:00 UTC. This anchor is fixed and deterministic.

package cortex

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
)

// Tier enumerates the temporal-ladder resolutions. Values double as the
// 1-byte tier component of the roll/ key (keys.RollupKey), so they are a
// closed, stable enum.
type RollupTier uint8

const (
	// TierHour is the finest resolution: a 1-hour window.
	TierHour RollupTier = 1
	// TierDay is the mid resolution: a 24-hour (UTC-aligned) window.
	TierDay RollupTier = 2
	// TierEpoch is the coarsest resolution: a 7-day week aligned to the
	// Unix epoch (Thursday 1970-01-01T00:00:00Z).
	TierEpoch RollupTier = 3
)

// String returns the canonical lowercase tier name (used in ShortForm and
// the rollup URI).
func (t RollupTier) String() string {
	switch t {
	case TierHour:
		return "hour"
	case TierDay:
		return "day"
	case TierEpoch:
		return "epoch"
	default:
		return fmt.Sprintf("tier(%d)", uint8(t))
	}
}

// Window is a half-open time window [Start, End) in Unix nanoseconds at a
// given tier.
type Window struct {
	Tier  RollupTier `cbor:"0,keyasint"`
	Start int64      `cbor:"1,keyasint"`
	End   int64      `cbor:"2,keyasint"`
}

// Ref is a resolvable reference to a rollup member: a memory URI (Kind ==
// RefKindMemory) or, in principle, a finer rollup / journal-seq ref. The
// deterministic lane emits only memory refs; the Kind tag keeps the shape
// open for the recursive-recall (T3-as-invocation) descent at the record
// level (req.3.3).
type Ref struct {
	URI  memory.URI `cbor:"0,keyasint"`
	Kind string     `cbor:"1,keyasint"`
}

// RefKindMemory is the Ref.Kind tag for a resolvable cortex memory URI.
const RefKindMemory = "memory"

// RollupRecord is the canonical CBOR-encoded blob persisted at
// roll/<tier><start>. It is a pure extractive summary of the journal facts in
// its window (req.4.2): kind/outcome tallies, top-salience member refs, a
// deterministic short-form, and a rollup-level salience score.
type RollupRecord struct {
	SchemaVersion uint8             `cbor:"0,keyasint"`
	Window        Window            `cbor:"1,keyasint"`
	SeqLo         uint64            `cbor:"2,keyasint"` // journal seq span covered (inclusive)
	SeqHi         uint64            `cbor:"3,keyasint"`
	EntryCount    uint32            `cbor:"4,keyasint"`
	KindTally     map[string]uint32 `cbor:"5,keyasint"` // journal Entry.Kind -> count
	OutcomeTally  map[string]uint32 `cbor:"6,keyasint"` // "success"/"failure" from KindAttest
	Members       []Ref             `cbor:"7,keyasint"` // top-salience memory refs, capped
	ShortForm     string            `cbor:"8,keyasint"`
	Salience      float64           `cbor:"9,keyasint"`
	EnrichRef     string            `cbor:"10,keyasint"` // optional; "" here (task 2.3 populates)
}

// RollupSchemaVersion is stamped on every emitted RollupRecord and
// RollupPayload. Bumping requires a journal-kind migration.
const RollupSchemaVersion uint8 = 1

// DefaultRollupEventCountFloor is the minimum in-window journal entry count
// (KindRollup entries excluded) below which BuildRollup writes NOTHING and
// returns ("", nil). Keeps sparse windows from producing degenerate records.
const DefaultRollupEventCountFloor = 1

// RollupMaxMembers caps the number of top-salience member refs carried in a
// RollupRecord (req.3.1 "capped").
const RollupMaxMembers = 16

// Canonical CBOR encoder for RollupRecord. Mirrors compact.go init()
// (compact.go:188-199): CoreDetEncOptions produces RFC 8949 §4.2.1
// deterministic encoding — required because the encoded bytes are
// integrity-hashed into RollupPayload.RecordHash and because map[string]uint32
// tallies must encode byte-stably (CoreDetEnc sorts map keys).
var (
	rollEnc cbor.EncMode
	rollDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("cortex/rollup: build EncMode: %w", err))
	}
	rollEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("cortex/rollup: build DecMode: %w", err))
	}
	rollDec = dm
}

// EncodeRollupRecord returns canonical deterministic CBOR for r.
func EncodeRollupRecord(r *RollupRecord) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("cortex/rollup: nil RollupRecord")
	}
	return rollEnc.Marshal(r)
}

// DecodeRollupRecord parses canonical CBOR into out.
func DecodeRollupRecord(b []byte, out *RollupRecord) error {
	return rollDec.Unmarshal(b, out)
}

// BuildRollupURI returns the agent-facing canonical URI for a rollup record:
//
//	matrix://cortex/rollup/<tier>/<start-unix-nano>
func BuildRollupURI(tier RollupTier, start int64) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://cortex/rollup/%s/%d", tier.String(), start))
}

// Window-alignment helpers. Each truncates a Unix-nanosecond timestamp down to
// its fixed UTC bucket and returns the half-open [start, end) window.

const (
	hourNanos  = int64(3600) * int64(time.Second)
	dayNanos   = int64(86400) * int64(time.Second)
	epochNanos = int64(7) * dayNanos
)

// alignDown floors ts to a multiple of bucket, correct for negative ts too.
func alignDown(ts, bucket int64) int64 {
	m := ts % bucket
	if m < 0 {
		m += bucket
	}
	return ts - m
}

// HourWindow returns the 1-hour UTC bucket [start, end) containing ts.
func HourWindow(tsUnixNano int64) Window {
	s := alignDown(tsUnixNano, hourNanos)
	return Window{Tier: TierHour, Start: s, End: s + hourNanos}
}

// DayWindow returns the 24-hour UTC bucket [start, end) containing ts.
func DayWindow(tsUnixNano int64) Window {
	s := alignDown(tsUnixNano, dayNanos)
	return Window{Tier: TierDay, Start: s, End: s + dayNanos}
}

// EpochWindow returns the 7-day week bucket [start, end) containing ts,
// aligned to the Unix epoch (Thursday 1970-01-01T00:00:00Z).
func EpochWindow(tsUnixNano int64) Window {
	s := alignDown(tsUnixNano, epochNanos)
	return Window{Tier: TierEpoch, Start: s, End: s + epochNanos}
}

// scoredMember is the internal ranking row for a candidate memory member.
type scoredMember struct {
	id    memory.ID
	uri   memory.URI
	score float32
}

// BuildRollup windows the journal by wall-clock over CreatedAt into w, builds
// the deterministic extractive RollupRecord, and persists it in the derived
// lane (roll/ record + KindRollup journal entry, NO SMT write).
//
// It is IDEMPOTENT: re-running over the same window and store state yields a
// byte-identical record + RecordHash (KindRollup entries are excluded from
// windowing so the builder's own output never feeds back). Windows with fewer
// than DefaultRollupEventCountFloor in-window entries produce NO record and
// return ("", nil).
//
// Returns the rollup's canonical URI on success.
func (c *Cortex) BuildRollup(w Window) (memory.URI, error) {
	if w.End <= w.Start {
		return "", fmt.Errorf("cortex.BuildRollup: empty window [%d,%d)", w.Start, w.End)
	}

	// refTime is the WINDOW END — the fixed reference clock for salience
	// ranking, so the record is a pure function of the window + stored
	// factors + weights (deterministic regardless of when this runs).
	refTime := time.Unix(0, w.End).UTC()

	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: read weights: %w", err)
	}

	kindTally := map[string]uint32{}
	outcomeTally := map[string]uint32{}
	var entryCount uint32
	var seqLo, seqHi uint64
	haveSeq := false

	// candidates: dedup by memory ID, keeping the highest version seen (so a
	// later Update in-window supersedes an earlier Write of the same memory).
	type cand struct {
		version uint64
		typ     uint8
	}
	candidates := map[memory.ID]*cand{}

	iterErr := c.s.IterJournal(func(e *journal.Entry) error {
		// Windowing is by CreatedAt in [Start, End). Journal is seq-ordered,
		// not strictly time-ordered, so we cannot early-break; scan fully.
		if e.CreatedAt < w.Start || e.CreatedAt >= w.End {
			return nil
		}
		// EXCLUDE KindRollup entries from windowing — a rollup never
		// summarizes rollups-of-itself; this is what makes BuildRollup
		// idempotent under a moving wall clock.
		if e.Kind == journal.KindRollup {
			return nil
		}

		entryCount++
		if !haveSeq {
			seqLo, seqHi = e.Seq, e.Seq
			haveSeq = true
		} else {
			if e.Seq < seqLo {
				seqLo = e.Seq
			}
			if e.Seq > seqHi {
				seqHi = e.Seq
			}
		}
		kindTally[string(e.Kind)]++

		switch e.Kind {
		case journal.KindWrite, journal.KindUpdate:
			var pl journal.WritePayload
			if derr := journal.DecodeWritePayload(e.Payload, &pl); derr != nil {
				return fmt.Errorf("decode write payload seq=%d: %w", e.Seq, derr)
			}
			id := memory.ID(pl.ID)
			if prev, ok := candidates[id]; !ok || pl.Version > prev.version {
				candidates[id] = &cand{version: pl.Version, typ: pl.Type}
			}
		case journal.KindAttest:
			var pl journal.AttestPayload
			if derr := journal.DecodeAttestPayload(e.Payload, &pl); derr != nil {
				return fmt.Errorf("decode attest payload seq=%d: %w", e.Seq, derr)
			}
			switch pl.Outcome {
			case journal.AttestOutcomeSuccess:
				outcomeTally["success"]++
			case journal.AttestOutcomeFailure:
				outcomeTally["failure"]++
			}
		}
		return nil
	})
	if iterErr != nil {
		return "", fmt.Errorf("cortex.BuildRollup: scan journal: %w", iterErr)
	}

	// Event-count floor: write nothing for sparse/empty windows (req.4.1).
	if entryCount < DefaultRollupEventCountFloor {
		return "", nil
	}

	// --- score + rank candidate members ---------------------------------
	members := make([]scoredMember, 0, len(candidates))
	for id, cd := range candidates {
		uri := BuildURI(memory.Type(cd.typ), id, cd.version)
		var score float32
		sc, ok, serr := salience.Read(c.s, id)
		if serr != nil {
			return "", fmt.Errorf("cortex.BuildRollup: salience read %s: %w", id, serr)
		}
		if ok {
			score = salience.ColdScoreWith(sc, weights, refTime)
		} else {
			// Deterministic fallback: a memory with no persisted salience
			// factors scores from a zero seed at the window-end clock. This
			// is a pure function of the window, so it stays byte-stable.
			seed := salience.Score{}
			score = salience.ColdScoreWith(&seed, weights, refTime)
		}
		members = append(members, scoredMember{id: id, uri: uri, score: score})
	}
	// Rank: score DESC, then memory ID ascending (stable, total order).
	sort.Slice(members, func(i, j int) bool {
		if members[i].score != members[j].score {
			return members[i].score > members[j].score
		}
		return idLess(members[i].id, members[j].id)
	})
	if len(members) > RollupMaxMembers {
		members = members[:RollupMaxMembers]
	}

	refs := make([]Ref, 0, len(members))
	for _, m := range members {
		refs = append(refs, Ref{URI: m.uri, Kind: RefKindMemory})
	}

	// Rollup-level salience = the top member's score (0.0 if no members).
	var topScore float64
	if len(members) > 0 {
		topScore = float64(members[0].score)
	}

	shortForm := buildRollupShortForm(w, entryCount, kindTally, outcomeTally, refs)

	record := &RollupRecord{
		SchemaVersion: RollupSchemaVersion,
		Window:        w,
		SeqLo:         seqLo,
		SeqHi:         seqHi,
		EntryCount:    entryCount,
		KindTally:     kindTally,
		OutcomeTally:  outcomeTally,
		Members:       refs,
		ShortForm:     shortForm,
		Salience:      topScore,
		EnrichRef:     "", // task 2.3 populates the optional enrichment ref
	}
	encodedRec, err := EncodeRollupRecord(record)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: encode record: %w", err)
	}
	recordHash := sha256.Sum256(encodedRec)

	// --- journal payload carries only identity + integrity pin ----------
	rp := &journal.RollupPayload{
		SchemaVersion: RollupSchemaVersion,
		Tier:          uint8(w.Tier),
		Start:         w.Start,
		End:           w.End,
		EntryCount:    entryCount,
		RecordHash:    recordHash,
	}
	rpBytes, err := journal.EncodeRollupPayload(rp)
	if err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindRollup,
		CreatedAt: c.now().UnixNano(),
		Payload:   rpBytes,
	}

	// --- atomic derived-lane batch (compact.go:426-442 posture) ---------
	// roll/<tier><start> record + journal entry. NO SMT update: rollups are
	// derived audit / working index, not canonical world-state.
	rollKey := keys.RollupKey(uint8(w.Tier), uint64(w.Start))
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(rollKey, encodedRec); err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: set roll: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.BuildRollup: commit: %w", err)
	}

	return BuildRollupURI(w.Tier, w.Start), nil
}

// buildRollupShortForm renders the deterministic extractive summary. Every
// tally is rendered with alphabetically-sorted keys and the member URIs are
// already in the deterministic (score DESC, id ASC) order, so the output is
// byte-stable for the same inputs (req.4.2).
//
// Format:
//
//	[<tier> <startRFC3339>..<endRFC3339>] N entries; kinds: k1=..,k2=..; outcomes: o1=..; top: <uri1>, <uri2>
//
// When a tally is empty it renders "(none)".
func buildRollupShortForm(w Window, entryCount uint32, kindTally, outcomeTally map[string]uint32, members []Ref) string {
	var b strings.Builder
	start := time.Unix(0, w.Start).UTC().Format(time.RFC3339)
	end := time.Unix(0, w.End).UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "[%s %s..%s] %d entries; kinds: %s; outcomes: %s",
		w.Tier.String(), start, end, entryCount,
		renderTally(kindTally), renderTally(outcomeTally))
	if len(members) > 0 {
		b.WriteString("; top: ")
		for i, m := range members {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(string(m.URI))
		}
	}
	return b.String()
}

// renderTally renders a map[string]uint32 as "k1=v1,k2=v2" with keys sorted
// alphabetically; "(none)" when empty. Byte-stable for the same map.
func renderTally(m map[string]uint32) string {
	if len(m) == 0 {
		return "(none)"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for i, k := range ks {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.FormatUint(uint64(m[k]), 10))
	}
	return b.String()
}

// LoadRollup returns the persisted RollupRecord stored at roll/<tier><start>.
// Returns memory.ErrNotFound when no record exists.
func (c *Cortex) LoadRollup(tier RollupTier, start int64) (*RollupRecord, error) {
	k := keys.RollupKey(uint8(tier), uint64(start))
	raw, ok, err := c.s.Get(k)
	if err != nil {
		return nil, fmt.Errorf("cortex.LoadRollup: get: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var rec RollupRecord
	if err := DecodeRollupRecord(raw, &rec); err != nil {
		return nil, fmt.Errorf("cortex.LoadRollup: decode: %w", err)
	}
	return &rec, nil
}

// Rollups returns every rollup of the given tier whose window Start lies in
// [since, until], in ascending window-start order (the roll/ key layout
// guarantees the order). A single ascending prefix scan over roll/<tier>.
func (c *Cortex) Rollups(tier RollupTier, since, until int64) ([]RollupRecord, error) {
	prefix := keys.RollupTierPrefix(uint8(tier))
	out := make([]RollupRecord, 0, 8)
	iterErr := c.s.PrefixIter(prefix, func(k, v []byte) error {
		_, start, perr := keys.ParseRollupKey(k)
		if perr != nil {
			return fmt.Errorf("parse rollup key: %w", perr)
		}
		s := int64(start)
		if s < since || s > until {
			return nil
		}
		var rec RollupRecord
		if derr := DecodeRollupRecord(v, &rec); derr != nil {
			return fmt.Errorf("decode rollup record start=%d: %w", s, derr)
		}
		out = append(out, rec)
		return nil
	})
	if iterErr != nil {
		return nil, fmt.Errorf("cortex.Rollups: %w", iterErr)
	}
	return out, nil
}
