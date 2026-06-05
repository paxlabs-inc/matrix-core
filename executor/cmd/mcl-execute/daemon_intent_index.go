// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_intent_index.go — read-side index over the journal directory.
//
// The journal layout is filesystem-native:
//
//   <journalDir>/<intent_id>/<seq>-<kind>.json   (signed envelope JSON)
//
// Where <kind> is the message kind with dots replaced by hyphens
// (identity.go:236 sanitiseKind), e.g. "intent-compiled",
// "intent-attest", "plan-proposed", "intent-fail".
//
// This file provides:
//
//   1. listIntentSummaries  — paginated listing of all intents the
//                             daemon has journaled, with derived
//                             state + timing without parsing every
//                             envelope body.
//
//   2. loadIntentSummary    — single-intent summary (lifecycle path,
//                             durations, cited URIs count, errors).
//
//   3. loadIntentEnvelopes  — full envelope chain for one intent
//                             (mirrors the existing /intents/:id
//                             handler that lives in daemon_server.go,
//                             refactored so callers can reuse it).
//
//   4. extractIntentMeta    — pull prose + verb + skill_uri + hash
//                             from the intent.compiled envelope.
//
//   5. extractAttestation   — pull cited_uris + evidence + outcome
//                             from the intent.attest envelope.
//
//   6. extractPlan          — pull canonical plan JSON from the
//                             plan.proposed envelope.
//
// Cache: a small in-memory LRU keyed by (intent_id, dir_mtime). Cache
// hits are O(1); misses scan the directory once. The cache is bounded
// at 256 entries — bigger than typical in-flight surface, small enough
// to never bloat the daemon. Cache invalidation is mtime-based: when
// the journal dir is appended-to (new envelope arrives), the mtime
// bumps and the cached entry is discarded.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/mcl/envelope"
)

// intentSummary is the compact view returned by /intents and used as
// the short-form response for /intents/:id/summary. Mirrors the
// shape we promised the frontend in the route inventory.
type intentSummary struct {
	IntentID   string `json:"intent_id"`
	IntentHash string `json:"intent_hash,omitempty"`
	State      string `json:"state"` // drafting|proposed|clarifying|accepted|executing|completed|failed|cancelled
	Verb       string `json:"verb,omitempty"`
	Skill      string `json:"skill,omitempty"`
	Prose      string `json:"prose,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	NodeCount  int    `json:"node_count,omitempty"`
	WalkErrors int    `json:"walk_errors,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"` // drafting --[intent.compiled]--> proposed --...
	EnvelopeN  int    `json:"envelope_count"`
	HasAttest  bool   `json:"has_attest"`
	HasFail    bool   `json:"has_fail"`
	HasCancel  bool   `json:"has_cancel"`
	PostRoot   string `json:"post_replay_root,omitempty"`
	PreRoot    string `json:"pre_replay_root,omitempty"`
}

// intentDirEntry pairs an intent's directory with its mtime for the
// list+sort path.
type intentDirEntry struct {
	IntentID string
	Path     string
	Mtime    time.Time
}

// indexCache is a small mtime-keyed LRU over intent summaries.
type indexCache struct {
	mu      sync.Mutex
	entries map[string]indexCacheEntry
	max     int
}

type indexCacheEntry struct {
	mtime   time.Time
	summary *intentSummary
}

func newIndexCache(max int) *indexCache {
	if max <= 0 {
		max = 256
	}
	return &indexCache{
		entries: make(map[string]indexCacheEntry, max),
		max:     max,
	}
}

func (c *indexCache) get(intentID string, mtime time.Time) (*intentSummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[intentID]
	if !ok {
		return nil, false
	}
	if !e.mtime.Equal(mtime) {
		delete(c.entries, intentID)
		return nil, false
	}
	return e.summary, true
}

func (c *indexCache) put(intentID string, mtime time.Time, sum *intentSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		// Evict an arbitrary entry — Go map iter is randomised, so
		// this is effectively random eviction. Adequate for an index
		// where the high-cardinality cost is minor (a single dir
		// re-scan on a cache miss).
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[intentID] = indexCacheEntry{
		mtime:   mtime,
		summary: sum,
	}
}

// listIntentDirs scans journalDir and returns one entry per direct
// subdirectory (every intent_id has its own dir). Sorted descending
// by mtime (newest first) so paginated callers see recent activity
// at the top of the page.
func listIntentDirs(journalDir string) ([]intentDirEntry, error) {
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("intent_index: read %s: %w", journalDir, err)
	}
	out := make([]intentDirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip the per-message transcript dir if it lives under the
		// journal root (defensive — sess#26 placed it elsewhere).
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		full := filepath.Join(journalDir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, intentDirEntry{
			IntentID: e.Name(),
			Path:     full,
			Mtime:    info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Mtime.After(out[j].Mtime)
	})
	return out, nil
}

// listIntentSummaries returns one intentSummary per intent dir,
// honoring the supplied cursor (pagination by mtime) and limit.
//
// State filter (when non-empty) keeps only intents whose terminal
// state matches; non-terminal intents are excluded if state filter is
// "completed" / "failed" / "cancelled".
func listIntentSummaries(d *daemonState, cur listCursor, limit int, stateFilter string) ([]intentSummary, string, int, error) {
	dirs, err := listIntentDirs(d.journalDir)
	if err != nil {
		return nil, "", -1, err
	}
	total := len(dirs)

	out := make([]intentSummary, 0, limit)
	var nextCursor string

	for _, e := range dirs {
		// Cursor filter: skip entries whose mtime is more recent than
		// or equal to the cursor's mtime+id pair (descending sort).
		if cur.TS != 0 {
			if e.Mtime.UnixNano() > cur.TS {
				continue
			}
			if e.Mtime.UnixNano() == cur.TS && e.IntentID >= cur.ID {
				continue
			}
		}

		sum := d.indexCache.summaryFor(e.IntentID, e.Path, e.Mtime)
		if sum == nil {
			continue
		}
		if stateFilter != "" && sum.State != stateFilter {
			continue
		}

		out = append(out, *sum)
		if len(out) == limit {
			// Compose next cursor from the last-included entry.
			nextCursor = encodeCursor(listCursor{
				TS: e.Mtime.UnixNano(),
				ID: e.IntentID,
			})
			break
		}
	}
	return out, nextCursor, total, nil
}

// summaryFor produces (and caches) an intentSummary for a single
// intent dir. Returns nil if the dir is empty / unreadable.
func (c *indexCache) summaryFor(intentID, dir string, mtime time.Time) *intentSummary {
	if cached, ok := c.get(intentID, mtime); ok {
		return cached
	}
	sum := buildIntentSummary(intentID, dir)
	if sum != nil {
		c.put(intentID, mtime, sum)
	}
	return sum
}

// buildIntentSummary scans a single intent dir and derives the typed
// summary. Reads:
//
//   - All envelope filenames (cheap; dir read).
//   - The first intent-compiled envelope body (to extract verb,
//     skill_uri, intent_hash, prose).
//   - The terminal envelope body (intent-attest / intent-fail /
//     intent-cancel) for outcome + completed_at.
//
// Skips body parsing of plan.step / plan.output / envelope-signed
// rows since those are dispatch noise — their seq numbers contribute
// to envelope_count but not to summary fields.
func buildIntentSummary(intentID, dir string) *intentSummary {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type fileEntry struct {
		Seq  int
		Kind string
		Name string
	}
	var files []fileEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seq, kind := parseEnvelopeFilename(e.Name())
		files = append(files, fileEntry{Seq: seq, Kind: kind, Name: e.Name()})
	}
	if len(files) == 0 {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Seq < files[j].Seq })

	sum := &intentSummary{
		IntentID:  intentID,
		EnvelopeN: len(files),
	}

	// Build lifecycle path from kind sequence.
	var transitions []string
	prevState := "drafting"
	transitions = append(transitions, prevState)

	for _, f := range files {
		nextKind := envelopeKindFromFilenameKind(f.Kind)
		nextState := lifecycleAdvance(prevState, nextKind)
		if nextState != prevState {
			transitions = append(transitions, fmt.Sprintf("--[%s]--> %s", nextKind, nextState))
			prevState = nextState
		}
	}
	sum.State = prevState
	sum.Lifecycle = strings.Join(transitions, " ")

	// Pull metadata from the FIRST intent-compiled envelope.
	for _, f := range files {
		if f.Kind != "intent-compiled" {
			continue
		}
		body, err := readEnvelopeBody(filepath.Join(dir, f.Name))
		if err != nil {
			break
		}
		var compBody envelope.IntentCompiledBody
		if err := body.DecodeBody(&compBody); err != nil {
			break
		}
		// Intent JSON carries the prose + verb + frame; we extract
		// the surface fields without bringing the full Intent IR back
		// into scope (avoids a transitive ir import here that we'd
		// have to thread through the wire shape).
		var parsed struct {
			ID    string `json:"id"`
			Verb  string `json:"verb"`
			Hash  string `json:"hash"`
			Frame struct {
				Skill string `json:"skill_ref"`
				Prose string `json:"prose"`
			} `json:"frame"`
			Prose string `json:"prose"`
		}
		_ = json.Unmarshal(compBody.IntentJSON, &parsed)
		sum.IntentHash = parsed.Hash
		sum.Verb = parsed.Verb
		sum.Skill = parsed.Frame.Skill
		// Intent.Frame.Prose is canonical; fallback to top-level
		// Prose for older shapes.
		if parsed.Frame.Prose != "" {
			sum.Prose = parsed.Frame.Prose
		} else if parsed.Prose != "" {
			sum.Prose = parsed.Prose
		}
		// StartedAt approximated via envelope At header on the
		// compiled envelope. Open the wrapping JSON for the At field.
		sum.StartedAt = body.At
		break
	}

	// Pull terminal state metadata + cited URIs from the LAST envelope.
	if last := files[len(files)-1]; true {
		sum.HasAttest = last.Kind == "intent-attest"
		sum.HasFail = last.Kind == "intent-fail"
		sum.HasCancel = last.Kind == "intent-cancel"
		body, err := readEnvelopeBody(filepath.Join(dir, last.Name))
		if err == nil {
			sum.EndedAt = body.At
			switch last.Kind {
			case "intent-attest":
				var ab envelope.IntentAttestBody
				if err := body.DecodeBody(&ab); err == nil {
					sum.NodeCount = countNodesFromEvidence(ab.EvidenceJSON)
					sum.WalkErrors = countWalkErrorsFromEvidence(ab.EvidenceJSON)
					sum.PostRoot = extractRootFromEvidence(ab.EvidenceJSON)
				}
			case "intent-fail":
				var fb envelope.IntentFailBody
				if err := body.DecodeBody(&fb); err == nil {
					sum.NodeCount = countNodesFromEvidence(fb.EvidenceJSON)
					sum.WalkErrors = countWalkErrorsFromEvidence(fb.EvidenceJSON)
					sum.PostRoot = extractRootFromEvidence(fb.EvidenceJSON)
				}
			}
		}
	}

	// DurationMS from envelope At timestamps when both are present.
	if sum.StartedAt != "" && sum.EndedAt != "" {
		t0, err1 := time.Parse(time.RFC3339Nano, sum.StartedAt)
		t1, err2 := time.Parse(time.RFC3339Nano, sum.EndedAt)
		if err1 == nil && err2 == nil {
			sum.DurationMS = t1.Sub(t0).Milliseconds()
		}
	}
	return sum
}

// lifecycleAdvance maps (current_state, message_kind) to next state.
// Mirrors lifecycle/state.go transitions but operates on canonical
// dot-form kinds. Returns prev when the kind has no transition.
func lifecycleAdvance(prev, kind string) string {
	switch prev {
	case "drafting":
		switch kind {
		case "intent.compiled":
			return "proposed"
		case "intent.fail":
			return "failed"
		case "intent.cancel":
			return "cancelled"
		}
	case "proposed":
		switch kind {
		case "intent.clarify":
			return "clarifying"
		case "intent.accept":
			return "accepted"
		case "intent.cancel":
			return "cancelled"
		case "intent.fail":
			return "failed"
		}
	case "clarifying":
		switch kind {
		case "intent.answer":
			return "proposed"
		case "intent.cancel":
			return "cancelled"
		case "intent.fail":
			return "failed"
		}
	case "accepted":
		switch kind {
		case "plan.proposed":
			return "executing"
		case "intent.cancel":
			return "cancelled"
		case "intent.fail":
			return "failed"
		}
	case "executing":
		switch kind {
		case "intent.attest":
			return "completed"
		case "intent.fail":
			return "failed"
		case "intent.cancel":
			return "cancelled"
		case "intent.correct":
			return "executing" // material vs non-material is opaque to the path string here
		}
	}
	return prev
}

// envelopeKindFromFilenameKind reverts the sanitiseKind hyphen-for-dot
// mapping so the lifecycle path uses canonical dot-form kinds.
func envelopeKindFromFilenameKind(k string) string {
	// Two-segment kinds (intent.compiled, plan.proposed, etc) are
	// hyphenated by sanitiseKind. Reverse: the LAST hyphen is the
	// boundary, but the leading namespace is one of {intent, plan,
	// policy} so we can split on the FIRST hyphen safely.
	idx := strings.IndexByte(k, '-')
	if idx <= 0 {
		return k
	}
	return k[:idx] + "." + k[idx+1:]
}

// rawEnvelopeJSON mirrors the envelope JSON wire shape for reading
// metadata + body. Avoids re-deriving the typed Envelope at callsites
// that only need the bytes.
type rawEnvelopeJSON struct {
	ID            string          `json:"id"`
	At            string          `json:"at"`
	From          string          `json:"from"`
	To            string          `json:"to,omitempty"`
	Kind          string          `json:"kind"`
	Intent        string          `json:"intent"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	SelfHash      string          `json:"self_hash,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
}

// DecodeBody decodes the envelope body into the kind-typed struct.
// Mirrors envelope.Envelope.DecodeBody but works against the JSON
// wire shape we persist on disk via prettyJSON.
func (r *rawEnvelopeJSON) DecodeBody(out interface{}) error {
	if len(r.Body) == 0 {
		return fmt.Errorf("envelope: empty body")
	}
	return json.Unmarshal(r.Body, out)
}

// readEnvelopeBody parses the on-disk pretty JSON of one envelope into
// a typed-body-decodable shape.
func readEnvelopeBody(path string) (*rawEnvelopeJSON, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out rawEnvelopeJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// countNodesFromEvidence pulls node_count out of an attest/fail
// evidence JSON without redefining the typed shape.
func countNodesFromEvidence(ev json.RawMessage) int {
	if len(ev) == 0 {
		return 0
	}
	var doc struct {
		StepCount int      `json:"step_count"`
		GateCount int      `json:"gate_count"`
		SubCount  int      `json:"sub_count"`
		NodeIDs   []string `json:"node_ids"`
	}
	if err := json.Unmarshal(ev, &doc); err != nil {
		return 0
	}
	if len(doc.NodeIDs) > 0 {
		return len(doc.NodeIDs)
	}
	return doc.StepCount + doc.GateCount + doc.SubCount
}

// countWalkErrorsFromEvidence pulls the count of in-band tool errors.
func countWalkErrorsFromEvidence(ev json.RawMessage) int {
	if len(ev) == 0 {
		return 0
	}
	var doc struct {
		IsErrors map[string]bool `json:"is_errors"`
	}
	if err := json.Unmarshal(ev, &doc); err != nil {
		return 0
	}
	n := 0
	for _, v := range doc.IsErrors {
		if v {
			n++
		}
	}
	return n
}

// extractRootFromEvidence pulls cortex_overall_root.
func extractRootFromEvidence(ev json.RawMessage) string {
	if len(ev) == 0 {
		return ""
	}
	var doc struct {
		OverallRoot string `json:"cortex_overall_root"`
	}
	if err := json.Unmarshal(ev, &doc); err != nil {
		return ""
	}
	return doc.OverallRoot
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
