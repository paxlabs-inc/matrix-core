// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command cortex-shell is the smoke-test CLI for the cortex implementation.
//
// Phase 1 commands (journal-only): head, dump, append.
// Phase 2 commands (typed memory):  write, resolve, update, tombstone, list.
//
// Usage:
//
//	cortex-shell -root <dir> -actor <name> <cmd> [args]
//
// Not for production. Once the §12 Find/Context surface lands, this CLI will
// gain query subcommands; for now it covers the §11 mutating + §12 Resolve
// surface plus journal introspection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

func main() {
	root := flag.String("root", "", "cortex data root directory (required)")
	actor := flag.String("actor", "", "actor name (required)")
	flag.Usage = usage
	flag.Parse()

	if *root == "" || *actor == "" {
		usage()
		os.Exit(2)
	}
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	s, err := store.Open(*root, *actor, nil)
	if err != nil {
		die("open: %v", err)
	}
	defer s.Close()
	c := cortex.New(s)

	switch args[0] {
	case "head":
		fmt.Printf("next_seq=%d count=%d\n", s.NextSeq(), s.JournalCount())

	case "append":
		if len(args) != 3 {
			die("append: usage append <kind> <payload>")
		}
		// raw journal append for smoke testing; bypasses typed Write.
		seq, err := s.AppendJournal(&journal.Entry{
			Kind:      journal.Kind(args[1]),
			CreatedAt: time.Now().UnixNano(),
			Payload:   []byte(args[2]),
		})
		if err != nil {
			die("append: %v", err)
		}
		fmt.Printf("appended seq=%d\n", seq)

	case "dump":
		err := s.IterJournal(func(e *journal.Entry) error {
			fmt.Printf("seq=%d kind=%s at=%d by=%q payload=%x\n",
				e.Seq, e.Kind, e.CreatedAt, e.CreatedBy, e.Payload)
			return nil
		})
		if err != nil {
			die("dump: %v", err)
		}

	case "write":
		if len(args) != 3 {
			die("write: usage write <Type> <json-body>")
		}
		typeName, body := args[1], args[2]
		data, err := parseTypedJSON(typeName, body)
		if err != nil {
			die("write: %v", err)
		}
		uri, err := c.Write(memory.Head{ActorScope: *actor},
			data,
			cortex.WriteMeta{
				CreatedBy:  *actor,
				Forms:      memory.Forms{Short: typeName + ":" + summarize(body)},
				Provenance: memory.Provenance{Source: memory.SourceUserInput},
			})
		if err != nil {
			die("write: %v", err)
		}
		fmt.Printf("uri=%s\n", uri)

	case "resolve":
		if len(args) != 2 {
			die("resolve: usage resolve <uri>")
		}
		m, err := c.Resolve(memory.URI(args[1]))
		if err != nil {
			die("resolve: %v", err)
		}
		printMemory(m)

	case "update":
		if len(args) != 3 {
			die("update: usage update <uri> <json-body>")
		}
		uri := memory.URI(args[1])
		t, _, _, err := cortex.ParseURI(uri)
		if err != nil {
			die("update: %v", err)
		}
		data, err := parseTypedJSON(t.String(), args[2])
		if err != nil {
			die("update: %v", err)
		}
		newURI, err := c.Update(uri, data, cortex.WriteMeta{
			CreatedBy:  *actor,
			Forms:      memory.Forms{Short: t.String() + ":" + summarize(args[2])},
			Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
		if err != nil {
			die("update: %v", err)
		}
		fmt.Printf("uri=%s\n", newURI)

	case "tombstone":
		if len(args) != 3 {
			die("tombstone: usage tombstone <uri> <reason>")
		}
		if err := c.Tombstone(memory.URI(args[1]), args[2], *actor); err != nil {
			die("tombstone: %v", err)
		}
		fmt.Println("ok")

	case "list":
		if len(args) != 2 {
			die("list: usage list <Type>")
		}
		t := parseTypeName(args[1])
		if !t.Valid() {
			die("list: unknown type %q", args[1])
		}
		ids, err := c.ListByType(t, 0)
		if err != nil {
			die("list: %v", err)
		}
		for _, id := range ids {
			fmt.Println(cortex.BuildURI(t, id, 1)) // version 1 by default; resolve to learn current
		}

	case "find":
		runFind(c, *root, *actor, args[1:])

	case "context":
		runContext(c, args[1:])

	case "write-frame":
		// write-frame <Type> <json-body> <verb:kind:ref> [verb:kind:ref ...]
		// Variant of `write` that stamps Head.Frames at Write time, used
		// to seed idx/frame and idx/actor_obj for `context` smoke tests.
		// Plain `write` stays positional-args-only for backwards compat.
		if len(args) < 4 {
			die("write-frame: usage write-frame <Type> <json-body> <verb:kind:ref> [<verb:kind:ref>...]")
		}
		typeName, body := args[1], args[2]
		data, err := parseTypedJSON(typeName, body)
		if err != nil {
			die("write-frame: %v", err)
		}
		var frames []memory.FrameRef
		for _, spec := range args[3:] {
			fr, err := parseFrameSpec(spec)
			if err != nil {
				die("write-frame: parse %q: %v", spec, err)
			}
			frames = append(frames, fr)
		}
		uri, err := c.Write(
			memory.Head{ActorScope: *actor, Frames: frames},
			data,
			cortex.WriteMeta{
				CreatedBy:  *actor,
				Forms:      memory.Forms{Short: typeName + ":" + summarize(body)},
				Provenance: memory.Provenance{Source: memory.SourceUserInput},
			})
		if err != nil {
			die("write-frame: %v", err)
		}
		fmt.Printf("uri=%s frames=%d\n", uri, len(frames))

	case "add-edge":
		if len(args) != 4 {
			die("add-edge: usage add-edge <src-uri> <edge-type> <dst-uri>")
		}
		src, srcOK := edgeURIToID(args[1])
		dst, dstOK := edgeURIToID(args[3])
		if !srcOK || !dstOK {
			die("add-edge: bad URI src=%v dst=%v", srcOK, dstOK)
		}
		t, ok := memory.ParseEdgeType(args[2])
		if !ok {
			die("add-edge: unknown edge type %q", args[2])
		}
		if err := c.AddEdge(src, t, dst, cortex.AddEdgeMeta{CreatedBy: *actor}); err != nil {
			die("add-edge: %v", err)
		}
		fmt.Println("ok")

	case "remove-edge":
		if len(args) != 5 {
			die("remove-edge: usage remove-edge <src-uri> <edge-type> <dst-uri> <reason>")
		}
		src, srcOK := edgeURIToID(args[1])
		dst, dstOK := edgeURIToID(args[3])
		if !srcOK || !dstOK {
			die("remove-edge: bad URI")
		}
		t, ok := memory.ParseEdgeType(args[2])
		if !ok {
			die("remove-edge: unknown edge type %q", args[2])
		}
		if err := c.RemoveEdge(src, t, dst, args[4], *actor); err != nil {
			die("remove-edge: %v", err)
		}
		fmt.Println("ok")

	case "list-edges":
		if len(args) < 2 || len(args) > 3 {
			die("list-edges: usage list-edges <src-uri> [in|out|both] (default out)")
		}
		src, ok := edgeURIToID(args[1])
		if !ok {
			die("list-edges: bad URI")
		}
		dir := "out"
		if len(args) == 3 {
			dir = args[2]
		}
		printEdge := func(rec *memory.EdgeRecord) error {
			tomb := ""
			if rec.Tombstoned {
				tomb = " tombstoned"
			}
			fmt.Printf("%s %s -> %s weight=%.3f%s\n",
				rec.Type, rec.Src, rec.Dst, rec.Weight, tomb)
			return nil
		}
		opts := cortex.IterEdgesOptions{IncludeTombstoned: true}
		switch dir {
		case "out":
			if err := c.IterEdgesOut(src, opts, printEdge); err != nil {
				die("list-edges: %v", err)
			}
		case "in":
			if err := c.IterEdgesIn(src, opts, printEdge); err != nil {
				die("list-edges: %v", err)
			}
		case "both":
			if err := c.IterEdgesOut(src, opts, printEdge); err != nil {
				die("list-edges: %v", err)
			}
			if err := c.IterEdgesIn(src, opts, printEdge); err != nil {
				die("list-edges: %v", err)
			}
		default:
			die("list-edges: bad direction %q", dir)
		}

	case "snapshot":
		// snapshot [reason]
		reason := "explicit"
		if len(args) >= 2 {
			reason = args[1]
		}
		m, err := c.Snapshot(reason)
		if err != nil {
			die("snapshot: %v", err)
		}
		fmt.Printf("snap_seq=%d journal_seq=%d trigger=%s\n",
			m.SeqAtSnapshot, m.JournalSeq, m.Trigger)
		fmt.Printf("journal_root=%x\n", m.JournalRoot[:])
		mem := m.StateRoots["memories"]
		edge := m.StateRoots["edges"]
		fmt.Printf("memories_root=%x\n", mem[:])
		fmt.Printf("edges_root=%x\n", edge[:])
		fmt.Printf("overall_root=%x\n", m.OverallRoot[:])
		fmt.Printf("counters: memories=%d edges=%d tombstoned=%d\n",
			m.Counters.Memories, m.Counters.Edges, m.Counters.Tombstoned)

	case "dump-snapshot":
		// dump-snapshot <seq>
		if len(args) != 2 {
			die("dump-snapshot: usage dump-snapshot <seq>")
		}
		var seq uint64
		if _, err := fmt.Sscanf(args[1], "%d", &seq); err != nil {
			die("dump-snapshot: bad seq %q", args[1])
		}
		m, err := c.Snap().LoadSnapshot(seq)
		if err != nil {
			die("dump-snapshot: %v", err)
		}
		fmt.Printf("snap_seq=%d journal_seq=%d actor=%s trigger=%s\n",
			m.SeqAtSnapshot, m.JournalSeq, m.Actor, m.Trigger)
		fmt.Printf("created_at=%d\n", m.CreatedAt)
		fmt.Printf("schema_version=%d\n", m.SchemaVersion)
		fmt.Printf("journal_root=%x\n", m.JournalRoot[:])
		for _, ns := range []string{"memories", "edges"} {
			r := m.StateRoots[ns]
			fmt.Printf("%s_root=%x\n", ns, r[:])
		}
		fmt.Printf("overall_root=%x\n", m.OverallRoot[:])
		fmt.Printf("counters: memories=%d edges=%d tombstoned=%d\n",
			m.Counters.Memories, m.Counters.Edges, m.Counters.Tombstoned)

	case "overall-root":
		// overall-root — pull-driven OverallRoot without persisting a snap/.
		root, err := c.OverallRoot()
		if err != nil {
			die("overall-root: %v", err)
		}
		fmt.Printf("%x\n", root[:])

	case "prove":
		// prove <uri> — emit an SMT membership proof for a memory URI
		// against the most recently persisted snap/. Useful for sub-agent
		// dispatch smoke tests (Phase 10) and as a debug surface today.
		if len(args) != 2 {
			die("prove: usage prove <uri>")
		}
		_, id, _, err := cortex.ParseURI(memory.URI(args[1]))
		if err != nil {
			die("prove: bad URI: %v", err)
		}
		var rawID [16]byte
		copy(rawID[:], id[:])
		smt := c.Snap().SMT("memories")
		// Read the canonical Head bytes for the value-hash component.
		var ulid keys.ULID
		copy(ulid[:], id[:])
		headBytes, ok, err := s.Get(keys.MemoryHeadKey(ulid))
		if err != nil {
			die("prove: head read: %v", err)
		}
		if !ok {
			// Non-membership proof.
			pf, err := smt.Prove(snapshot.HashMemoryKey(rawID))
			if err != nil {
				die("prove: %v", err)
			}
			root, _ := smt.Root()
			if err := snapshot.VerifyMembership(root, pf); err != nil {
				die("prove: self-verify failed: %v", err)
			}
			fmt.Printf("non-membership proof verified against current memories root %x\n", root[:8])
			return
		}
		pf, err := smt.ProveWithValue(snapshot.HashMemoryKey(rawID), headBytes)
		if err != nil {
			die("prove: %v", err)
		}
		root, _ := smt.Root()
		if err := snapshot.VerifyMembership(root, pf); err != nil {
			die("prove: self-verify failed: %v", err)
		}
		fmt.Printf("membership proof verified against current memories root %x\n", root[:8])

	case "compact":
		// compact -intent ID -step ID [-budget N] [-dir DIR] [-load URI]... <uri>...
		runCompact(c, args[1:])

	case "dump-checkpoint":
		// dump-checkpoint <intent_id> <step_id>
		if len(args) != 3 {
			die("dump-checkpoint: usage dump-checkpoint <intent_id> <step_id>")
		}
		rec, err := c.LoadCheckpoint(args[1], args[2])
		if err != nil {
			die("dump-checkpoint: %v", err)
		}
		out, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			die("dump-checkpoint: marshal: %v", err)
		}
		fmt.Println(string(out))

	case "update-head":
		// update-head <uri> [-tag T]... [-frame V:K:R]... [-importance N]
		//                  [-visibility private|scoped|actor_public]
		runUpdateHead(c, *actor, args[1:])

	case "dump-scope":
		// dump-scope <file>
		// Reads canonical CBOR Scope bytes from <file>, decodes and prints
		// a JSON view. Pure offline tool — does NOT verify the scope (no
		// keyresolver wired here).
		if len(args) != 2 {
			die("dump-scope: usage dump-scope <file>")
		}
		runDumpScope(args[1])

	case "rebuild":
		// rebuild [-verify-only]
		// Drops every key under spec's `indexes/` namespace and rebuilds
		// from canonical state. Implements research/04-cortex.md §13.4.
		// With -verify-only, captures pre-drop OverallRoot and asserts
		// post-rebuild equality (the strongest form of the §13.4
		// invariant); without it, prints the rebuild counters and roots.
		runRebuild(c, args[1:])

	case "attest":
		// attest -intent ID -outcome success|failure [-reason R]
		//        [-by CREATOR] <uri>...
		// Phase 11.5 cortex.Attest primitive. Bumps salience.Citations
		// (and .AccessCount) on cited memories per research/04-cortex.md
		// §8.3. Commits one KindAttest journal entry.
		runAttest(c, *actor, args[1:])

	case "dump-attest":
		// dump-attest <seq>
		// Reads j/<seq> and prints the decoded AttestPayload as JSON.
		// Inverse of `attest` for human verification / debugging.
		if len(args) != 2 {
			die("dump-attest: usage dump-attest <seq>")
		}
		runDumpAttest(c, args[1])

	case "dump-salience":
		// dump-salience <uri>
		// Reads salience/<id> and prints the cached Score as JSON.
		// Useful for verifying Phase 11.5 AccessCount/Citations bumps
		// landed (smoke: write -> find -late -> dump-salience).
		if len(args) != 2 {
			die("dump-salience: usage dump-salience <uri>")
		}
		runDumpSalience(c, args[1])

	case "dump-weights":
		// dump-weights
		// Reads meta/salience_weights and prints the per-actor learned
		// salience.Weights as JSON. Returns DefaultWeights() when the
		// key is absent (cold start). Useful for verifying Phase 12 EMA
		// updates landed (smoke: write -> find -late -> attest -> dump-
		// weights shows WC pulled up after a success citation).
		runDumpWeights(c)

	default:
		die("unknown command %q", args[0])
	}
}

// runRebuild parses CLI flags and invokes cortex.Rebuild. With
// -verify-only, the post-rebuild root MUST equal the pre-drop root or
// the command exits non-zero — useful in CI / disaster-recovery scripts
// to assert the §13.4 invariant on a real actor's data.
func runRebuild(c *cortex.Cortex, args []string) {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	verifyOnly := fs.Bool("verify-only", false, "exit non-zero if post-rebuild OverallRoot != pre-drop OverallRoot")
	if err := fs.Parse(args); err != nil {
		die("rebuild: %v", err)
	}
	res, err := c.Rebuild(cortex.RebuildOptions{})
	if err != nil {
		die("rebuild: %v", err)
	}
	fmt.Printf("rebuild: memories=%d edges=%d journal_leaves=%d journal_seq=%d\n",
		res.MemoriesScanned, res.EdgesScanned,
		res.JournalLeavesAppended, res.JournalSeq)
	fmt.Printf("rebuild: pre_overall_root  %x\n", res.PreOverallRoot[:])
	fmt.Printf("rebuild: post_overall_root %x\n", res.PostOverallRoot[:])
	if res.PreOverallRoot == res.PostOverallRoot {
		fmt.Println("rebuild: OverallRoot preserved — §13.4 invariant holds")
	} else {
		fmt.Println("rebuild: WARNING — OverallRoot drifted post-rebuild")
		if *verifyOnly {
			die("rebuild: -verify-only assertion failed")
		}
	}
}

// runAttest parses CLI flags and invokes cortex.Attest. Positional
// <uri>... args are the cited memories. Mirrors research/04-cortex.md §8.3
// "intent.attest" but the cortex-side primitive only — the MCL envelope
// + agent-runtime wiring is out of scope for this CLI.
func runAttest(c *cortex.Cortex, actor string, args []string) {
	fs := flag.NewFlagSet("attest", flag.ContinueOnError)
	intent := fs.String("intent", "", "intent_id (required)")
	outcome := fs.String("outcome", "success", "success|failure")
	reason := fs.String("reason", "", "failure reason (factual_error|wrong_assumption trigger Citations decrement)")
	by := fs.String("by", "", "CreatedBy ref recorded in journal (defaults to actor)")
	if err := fs.Parse(args); err != nil {
		die("attest: %v", err)
	}
	if *intent == "" {
		die("attest: -intent required")
	}
	uris := fs.Args()
	if len(uris) == 0 {
		die("attest: at least one cited <uri> required")
	}
	var oc cortex.AttestOutcome
	switch *outcome {
	case "success":
		oc = cortex.AttestOutcomeSuccess
	case "failure":
		oc = cortex.AttestOutcomeFailure
	default:
		die("attest: -outcome must be success|failure, got %q", *outcome)
	}
	cited := make([]memory.URI, 0, len(uris))
	for _, u := range uris {
		cited = append(cited, memory.URI(u))
	}
	creator := *by
	if creator == "" {
		creator = actor
	}
	res, err := c.Attest(cortex.AttestOpts{
		IntentID:  *intent,
		Outcome:   oc,
		Reason:    *reason,
		Cited:     cited,
		CreatedBy: creator,
	})
	if err != nil {
		die("attest: %v", err)
	}
	fmt.Printf("attest: seq=%d learn_seq=%d affected=%d skipped=%d delta_citations=%+d\n",
		res.Seq, res.LearnSeq, len(res.AffectedIDs), len(res.SkippedURIs), res.CitationsDelta)
	fmt.Printf("attest: weights_updated=%t\n", res.WeightsUpdated)
	fmt.Printf("attest: prev_w=(%.4f,%.4f,%.4f,%.4f,%.4f) new_w=(%.4f,%.4f,%.4f,%.4f,%.4f)\n",
		res.PrevWeights.WR, res.PrevWeights.WA, res.PrevWeights.WC, res.PrevWeights.WD, res.PrevWeights.WV,
		res.NewWeights.WR, res.NewWeights.WA, res.NewWeights.WC, res.NewWeights.WD, res.NewWeights.WV)
	if len(res.SkippedURIs) > 0 {
		for _, u := range res.SkippedURIs {
			fmt.Fprintf(os.Stderr, "attest: skipped %s (missing/tombstoned/malformed)\n", u)
		}
	}
}

// runDumpAttest reads j/<seq> and prints the decoded AttestPayload as
// indented JSON. Fails if the entry at seq is not KindAttest.
func runDumpAttest(c *cortex.Cortex, seqStr string) {
	var seq uint64
	if _, err := fmt.Sscanf(seqStr, "%d", &seq); err != nil {
		die("dump-attest: bad seq %q: %v", seqStr, err)
	}
	raw, ok, err := c.Store().Get(keys.JournalKey(seq))
	if err != nil {
		die("dump-attest: get j/%d: %v", seq, err)
	}
	if !ok {
		die("dump-attest: j/%d not found", seq)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		die("dump-attest: decode entry: %v", err)
	}
	if entry.Kind != journal.KindAttest {
		die("dump-attest: j/%d kind=%q, expected %q", seq, entry.Kind, journal.KindAttest)
	}
	var pl journal.AttestPayload
	if err := journal.DecodeAttestPayload(entry.Payload, &pl); err != nil {
		die("dump-attest: decode payload: %v", err)
	}
	type view struct {
		Seq           uint64   `json:"seq"`
		Kind          string   `json:"kind"`
		CreatedAt     int64    `json:"created_at"`
		CreatedBy     string   `json:"created_by,omitempty"`
		SchemaVersion uint8    `json:"schema_version"`
		IntentID      string   `json:"intent_id"`
		Outcome       string   `json:"outcome"`
		Reason        string   `json:"reason,omitempty"`
		CitedHex      []string `json:"cited_ids_hex"`
	}
	v := view{
		Seq:           entry.Seq,
		Kind:          string(entry.Kind),
		CreatedAt:     entry.CreatedAt,
		CreatedBy:     string(entry.CreatedBy),
		SchemaVersion: pl.SchemaVersion,
		IntentID:      pl.IntentID,
		Outcome:       outcomeString(pl.Outcome),
		Reason:        pl.Reason,
	}
	for _, id := range pl.CitedIDs {
		v.CitedHex = append(v.CitedHex, fmt.Sprintf("%x", id[:]))
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		die("dump-attest: marshal: %v", err)
	}
	fmt.Println(string(out))
}

// runDumpSalience reads salience/<id> for the memory referenced by uri
// and prints it as JSON. ID is parsed from the URI; the cached Score is
// read raw (no live recompute).
func runDumpSalience(c *cortex.Cortex, uri string) {
	_, id, _, err := cortex.ParseURI(memory.URI(uri))
	if err != nil {
		die("dump-salience: parse uri: %v", err)
	}
	var ulid keys.ULID
	copy(ulid[:], id[:])
	raw, ok, err := c.Store().Get(keys.SalienceKey(ulid))
	if err != nil {
		die("dump-salience: get: %v", err)
	}
	if !ok {
		die("dump-salience: salience/<id> not found for %s", uri)
	}
	var sc salience.Score
	if err := salience.Decode(raw, &sc); err != nil {
		die("dump-salience: decode: %v", err)
	}
	out, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		die("dump-salience: marshal: %v", err)
	}
	fmt.Println(string(out))
}

// runDumpWeights prints the per-actor learned salience.Weights from
// meta/salience_weights as JSON. Cold start (key absent) prints the
// DefaultWeights set with a `cold_start: true` annotation so the caller
// can tell apart "no EMA updates yet" from "learned but happens to
// match cold weights".
func runDumpWeights(c *cortex.Cortex) {
	w, found, err := salience.ReadWeights(c.Store())
	if err != nil {
		die("dump-weights: read: %v", err)
	}
	type dump struct {
		ColdStart bool             `json:"cold_start"`
		Weights   salience.Weights `json:"weights"`
	}
	out, err := json.MarshalIndent(dump{ColdStart: !found, Weights: w}, "", "  ")
	if err != nil {
		die("dump-weights: marshal: %v", err)
	}
	fmt.Println(string(out))
}

// outcomeString returns the canonical CLI string form of an AttestOutcome.
func outcomeString(o cortex.AttestOutcome) string {
	switch o {
	case cortex.AttestOutcomeSuccess:
		return "success"
	case cortex.AttestOutcomeFailure:
		return "failure"
	default:
		return fmt.Sprintf("unknown(%d)", o)
	}
}

// runCompact parses CLI flags and invokes cortex.Compact. The positional
// <uri>... args form the InContext list; -load URI (repeatable) becomes
// LoadBearing; -dir sets CheckpointDir (filesystem mirror). Mirrors the
// research/03 §5.3 primitive signature.
func runCompact(c *cortex.Cortex, args []string) {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	intent := fs.String("intent", "", "intent_id (required)")
	step := fs.String("step", "", "step_id (required)")
	budget := fs.Int("budget", cortex.DefaultCompactBudgetTokens, "budget tokens")
	dir := fs.String("dir", "", "CheckpointDir for human-readable JSON mirror (optional)")
	var loadFlags multiFlag
	fs.Var(&loadFlags, "load", "URI marked as load-bearing (repeatable)")
	if err := fs.Parse(args); err != nil {
		die("compact: %v", err)
	}
	if *intent == "" || *step == "" {
		die("compact: -intent and -step required")
	}
	uris := fs.Args()
	if len(uris) == 0 {
		die("compact: at least one in_context <uri> required")
	}
	inCtx := make([]*memory.Memory, 0, len(uris))
	for _, u := range uris {
		m, err := c.Resolve(memory.URI(u))
		if err != nil {
			die("compact: resolve %s: %v", u, err)
		}
		inCtx = append(inCtx, m)
	}
	loadBearing := make([]memory.URI, 0, len(loadFlags))
	for _, u := range loadFlags {
		loadBearing = append(loadBearing, memory.URI(u))
	}
	res, err := c.Compact(cortex.CompactOpts{
		InContext:     inCtx,
		LoadBearing:   loadBearing,
		BudgetTokens:  *budget,
		IntentID:      *intent,
		StepID:        *step,
		CheckpointDir: *dir,
	})
	if err != nil {
		die("compact: %v", err)
	}
	fmt.Printf("kept=%d compacted=%d\n", len(res.Kept), len(res.Compacted))
	fmt.Printf("snapshot_uri=%s\n", res.SnapshotURI)
	if res.SnapshotPath != "" {
		fmt.Printf("snapshot_path=%s\n", res.SnapshotPath)
	}
	for _, ci := range res.Compacted {
		fmt.Printf("  compacted ref=%s salience=%.3f short=%q\n",
			ci.Ref, ci.Salience, ci.ShortForm)
	}
}

// edgeURIToID extracts the memory ID from a canonical cortex URI; returns
// (id, true) on success and (zero, false) on parse failure. The CLI uses
// this for edge subcommands where Type and version are irrelevant.
func edgeURIToID(s string) (memory.ID, bool) {
	_, id, _, err := cortex.ParseURI(memory.URI(s))
	if err != nil {
		return memory.ID{}, false
	}
	return id, true
}

// runFind drives the §12 typed Find surface from the CLI. Supports a tiny
// subset of the predicate AST so smoke tests can exercise the engine end
// to end:
//
//	find <Type|*> [-tag T]... [-eq field=value]... [-limit N] [-late]
//	              [-near "text"|-near-uri <uri>]
//	              [-from <uri> [-follow <type>:<dir>:<min..max>]]
//
// More expressive querying is intentionally NOT exposed here; complex
// predicates belong in a real authoring layer, not a smoke shell.
func runFind(c *cortex.Cortex, root, actor string, args []string) {
	if len(args) < 1 {
		die("find: usage find <Type|*> [-tag T]... [-eq field=value]... [-limit N] [-near \"text\"|-near-uri <uri>] [-late]")
	}
	q := query.Query{Limit: 10}
	if args[0] != "*" {
		t := parseTypeName(args[0])
		if !t.Valid() {
			die("find: unknown type %q", args[0])
		}
		q.Type = []memory.Type{t}
	}
	var (
		conjuncts []query.Predicate
		near      string
		nearURI   string
		fromURI   string
		follow    string
	)
	i := 1
	for i < len(args) {
		switch args[i] {
		case "-tag":
			if i+1 >= len(args) {
				die("find: -tag requires value")
			}
			conjuncts = append(conjuncts, query.HasTag{Tag: args[i+1]})
			i += 2
		case "-eq":
			if i+1 >= len(args) {
				die("find: -eq requires field=value")
			}
			eq := strings.SplitN(args[i+1], "=", 2)
			if len(eq) != 2 {
				die("find: -eq value must be field=value, got %q", args[i+1])
			}
			conjuncts = append(conjuncts, query.Eq{Field: query.FieldRef(eq[0]), Value: eq[1]})
			i += 2
		case "-limit":
			if i+1 >= len(args) {
				die("find: -limit requires N")
			}
			var n int
			if _, err := fmt.Sscanf(args[i+1], "%d", &n); err != nil || n <= 0 {
				die("find: bad limit %q", args[i+1])
			}
			q.Limit = n
			i += 2
		case "-late":
			q.LateBinding = true
			i++
		case "-near":
			if i+1 >= len(args) {
				die("find: -near requires text")
			}
			near = args[i+1]
			i += 2
		case "-near-uri":
			if i+1 >= len(args) {
				die("find: -near-uri requires URI")
			}
			nearURI = args[i+1]
			i += 2
		case "-from":
			if i+1 >= len(args) {
				die("find: -from requires URI")
			}
			fromURI = args[i+1]
			i += 2
		case "-follow":
			if i+1 >= len(args) {
				die("find: -follow requires <type>:<dir>:<min..max>")
			}
			follow = args[i+1]
			i += 2
		default:
			die("find: unknown flag %q", args[i])
		}
	}
	if len(conjuncts) == 1 {
		q.Where = conjuncts[0]
	} else if len(conjuncts) > 1 {
		q.Where = query.And{Children: conjuncts}
	}
	// If vector recall was requested, boot the embedder, drain it so
	// every existing memory has a vec/meta entry, and populate Near /
	// NearURI on the query.
	if fromURI != "" {
		u := memory.URI(fromURI)
		q.From = &u
		if follow != "" {
			expr, err := parseFollowExpr(follow)
			if err != nil {
				die("find: parse -follow: %v", err)
			}
			q.Follow = expr
		}
	}
	if near != "" || nearURI != "" {
		ensureEmbedder(c, root, actor)
		defer func() {
			if err := c.StopEmbedder(); err != nil {
				fmt.Fprintf(os.Stderr, "warn: StopEmbedder: %v\n", err)
			}
		}()
		if near != "" {
			q.Near = near
		}
		if nearURI != "" {
			u := memory.URI(nearURI)
			q.NearURI = &u
		}
	}
	res, err := c.Find(q)
	if err != nil {
		die("find: %v", err)
	}
	fmt.Printf("# scanned=%d total=%d returned=%d trimmed=%d form=%s\n",
		res.CandidatesScanned, res.Total, len(res.Memories), res.TrimmedByBudget, res.Form)
	for _, m := range res.Memories {
		uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
		switch {
		case len(res.Hops) > 0:
			fmt.Printf("uri=%s hop=%d salience=%.3f short=%q\n",
				uri, res.Hops[m.Head.ID], res.Scores[m.Head.ID], m.Version.Forms.Short)
		case len(res.Distances) > 0:
			fmt.Printf("uri=%s distance=%.4f salience=%.3f short=%q\n",
				uri, res.Distances[m.Head.ID], res.Scores[m.Head.ID], m.Version.Forms.Short)
		default:
			fmt.Printf("uri=%s salience=%.3f short=%q\n",
				uri, res.Scores[m.Head.ID], m.Version.Forms.Short)
		}
	}
}

// runUpdateHead drives Phase 10 cortex.UpdateHead from the CLI:
//
//	update-head <uri> [-tag T]... [-frame V:K:R]...
//	            [-importance N] [-visibility private|scoped|actor_public]
//
// Empty -tag/-frame flag sets are NOT a "remove all" — to clear, pass
// the explicit -clear-tags / -clear-frames flag. Pointer-vs-zero
// semantics mirrors the HeadPatch struct.
func runUpdateHead(c *cortex.Cortex, actor string, args []string) {
	if len(args) == 0 {
		die("update-head: usage update-head <uri> [flags]")
	}
	uri := memory.URI(args[0])
	args = args[1:]

	fs := flag.NewFlagSet("update-head", flag.ContinueOnError)
	var tagFlags multiFlag
	var frameFlags multiFlag
	clearTags := fs.Bool("clear-tags", false, "set Tags to empty (removes all)")
	clearFrames := fs.Bool("clear-frames", false, "set Frames to empty (removes all)")
	fs.Var(&tagFlags, "tag", "tag to set (repeatable; replaces all tags)")
	fs.Var(&frameFlags, "frame", "frame spec verb:kind:ref (repeatable; replaces all frames)")
	importance := fs.Int("importance", -1, "DeclaredImportance 0..10 (negative = no change)")
	visibility := fs.String("visibility", "", "private|scoped|actor_public (empty = no change)")
	if err := fs.Parse(args); err != nil {
		die("update-head: %v", err)
	}

	patch := cortex.HeadPatch{}
	if *clearTags {
		empty := []memory.Tag{}
		patch.Tags = &empty
	} else if len(tagFlags) > 0 {
		tags := make([]memory.Tag, 0, len(tagFlags))
		for _, t := range tagFlags {
			tags = append(tags, memory.Tag(t))
		}
		patch.Tags = &tags
	}
	if *clearFrames {
		empty := []memory.FrameRef{}
		patch.Frames = &empty
	} else if len(frameFlags) > 0 {
		frames := make([]memory.FrameRef, 0, len(frameFlags))
		for _, spec := range frameFlags {
			fr, err := parseFrameSpec(spec)
			if err != nil {
				die("update-head: parse frame %q: %v", spec, err)
			}
			frames = append(frames, fr)
		}
		patch.Frames = &frames
	}
	if *importance >= 0 {
		if *importance > 10 {
			die("update-head: importance must be 0..10")
		}
		v := uint8(*importance)
		patch.DeclaredImportance = &v
	}
	if *visibility != "" {
		var vis memory.Visibility
		switch *visibility {
		case "private":
			vis = memory.VisPrivate
		case "scoped":
			vis = memory.VisScoped
		case "actor_public":
			vis = memory.VisActorPublic
		default:
			die("update-head: unknown visibility %q (want private|scoped|actor_public)", *visibility)
		}
		patch.Visibility = &vis
	}

	got, err := c.UpdateHead(uri, patch, cortex.UpdateHeadMeta{CreatedBy: actor})
	if err != nil {
		die("update-head: %v", err)
	}
	fmt.Printf("uri=%s (head rewritten; data version unchanged)\n", got)
}

// runDumpScope reads canonical CBOR Scope bytes from path, decodes,
// and prints a JSON-pretty view. Pure offline tool — does NOT verify
// the scope (no key resolver wired here). Useful for inspecting a
// scope envelope received from a parent agent before plumbing it into
// a sub-agent's Cortex.
func runDumpScope(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("dump-scope: read: %v", err)
	}
	var s scope.Scope
	if err := scope.DecodeScope(raw, &s); err != nil {
		die("dump-scope: decode: %v", err)
	}
	out, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		die("dump-scope: marshal: %v", err)
	}
	fmt.Println(string(out))
}

// parseFrameSpec parses one `verb:kind:ref` spec into a FrameRef. Verb
// and kind use the lower-case enum names from memory.ParseVerb /
// memory.ParseObjKind. Ref is anything up to MaxObjRefLen; colons in
// the ref are permitted by splitting on the first two ':'.
func parseFrameSpec(s string) (memory.FrameRef, error) {
	first := strings.IndexByte(s, ':')
	if first < 0 {
		return memory.FrameRef{}, fmt.Errorf("frame %q needs verb:kind:ref", s)
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, ':')
	if second < 0 {
		return memory.FrameRef{}, fmt.Errorf("frame %q needs verb:kind:ref", s)
	}
	verbName := s[:first]
	kindName := rest[:second]
	ref := rest[second+1:]
	verb, ok := memory.ParseVerb(verbName)
	if !ok {
		return memory.FrameRef{}, fmt.Errorf("unknown verb %q", verbName)
	}
	kind, ok := memory.ParseObjKind(kindName)
	if !ok {
		return memory.FrameRef{}, fmt.Errorf("unknown obj kind %q", kindName)
	}
	return memory.FrameRef{Verb: verb, ObjKind: kind, ObjRef: ref}, nil
}

// runContext drives the §12.1 Context bundle composer from the CLI:
//
//	context [-verb V] [-obj kind:ref]... [-include-tier t]...
//	        [-budget N] [-outcome-limit N] [-form short|medium|full]
//
// Prints one line per memory across the three tiers, plus a header
// with bundle compile_metadata for latency/budget audit.
func runContext(c *cortex.Cortex, args []string) {
	opts := cortex.ContextOpts{}
	objs := map[string]string{}
	i := 0
	for i < len(args) {
		switch args[i] {
		case "-verb":
			if i+1 >= len(args) {
				die("context: -verb requires a value")
			}
			v, ok := memory.ParseVerb(args[i+1])
			if !ok {
				die("context: unknown verb %q", args[i+1])
			}
			opts.Verb = v
			i += 2
		case "-obj":
			if i+1 >= len(args) {
				die("context: -obj requires kind:ref")
			}
			parts := strings.SplitN(args[i+1], ":", 2)
			if len(parts) != 2 {
				die("context: -obj must be kind:ref, got %q", args[i+1])
			}
			objs[parts[0]] = parts[1]
			i += 2
		case "-include-tier":
			if i+1 >= len(args) {
				die("context: -include-tier requires a value")
			}
			t, ok := cortex.ParseTier(args[i+1])
			if !ok {
				die("context: unknown tier %q", args[i+1])
			}
			opts.IncludeTiers = append(opts.IncludeTiers, t)
			i += 2
		case "-budget":
			if i+1 >= len(args) {
				die("context: -budget requires N")
			}
			var n int
			if _, err := fmt.Sscanf(args[i+1], "%d", &n); err != nil || n <= 0 {
				die("context: bad budget %q", args[i+1])
			}
			opts.BudgetTokens = n
			i += 2
		case "-outcome-limit":
			if i+1 >= len(args) {
				die("context: -outcome-limit requires N")
			}
			var n int
			if _, err := fmt.Sscanf(args[i+1], "%d", &n); err != nil || n <= 0 {
				die("context: bad outcome-limit %q", args[i+1])
			}
			opts.OutcomeLimit = n
			i += 2
		case "-form":
			if i+1 >= len(args) {
				die("context: -form requires short|medium|full")
			}
			switch args[i+1] {
			case "short":
				opts.Form = query.FormShort
			case "medium":
				opts.Form = query.FormMedium
			case "full":
				opts.Form = query.FormFull
			default:
				die("context: unknown form %q", args[i+1])
			}
			i += 2
		default:
			die("context: unknown flag %q", args[i])
		}
	}
	if len(objs) > 0 {
		opts.Objects = objs
	}
	bundle, err := c.Context(opts)
	if err != nil {
		die("context: %v", err)
	}
	fmt.Printf("# tokens=%d trimmed=%d latency_ms=%d form=%s\n",
		bundle.TotalTokens, bundle.Trimmed, bundle.LatencyMS, bundle.Form)
	printTier := func(label string, mems []*memory.Memory) {
		fmt.Printf("[%s] (%d memories)\n", label, len(mems))
		for _, m := range mems {
			uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
			score := bundle.Scores[m.Head.ID]
			fmt.Printf("  uri=%s salience=%.3f text=%q\n", uri, score, bundle.Rendered[m.Head.ID])
		}
	}
	printTier("pinned", bundle.Pinned)
	printTier("frame_relevant", bundle.FrameRelevant)
	printTier("outcomes", bundle.Outcomes)
	if len(bundle.ReachableURIs) > 0 {
		fmt.Printf("[reachable] (%d uris not in bundle)\n", len(bundle.ReachableURIs))
		for _, u := range bundle.ReachableURIs {
			fmt.Printf("  uri=%s\n", u)
		}
	}
}

// parseFollowExpr accepts <type>[,<type>...]:<dir>:<min..max> e.g.
//
//	references:out:1..2
//	references,corroborates:both:1..3
//
// Empty type list (":out:1..2") means any type. Direction defaults to "out";
// hop range defaults to "1..1".
func parseFollowExpr(s string) (*query.EdgeExpr, error) {
	parts := strings.SplitN(s, ":", 3)
	expr := &query.EdgeExpr{Direction: query.DirOut, MinHops: 1, MaxHops: 1}
	if len(parts) >= 1 && parts[0] != "" {
		for _, name := range strings.Split(parts[0], ",") {
			t, ok := memory.ParseEdgeType(name)
			if !ok {
				return nil, fmt.Errorf("unknown edge type %q", name)
			}
			expr.Types = append(expr.Types, t)
		}
	}
	if len(parts) >= 2 && parts[1] != "" {
		switch parts[1] {
		case "out":
			expr.Direction = query.DirOut
		case "in":
			expr.Direction = query.DirIn
		case "both":
			expr.Direction = query.DirBoth
		default:
			return nil, fmt.Errorf("bad direction %q", parts[1])
		}
	}
	if len(parts) >= 3 && parts[2] != "" {
		hopParts := strings.SplitN(parts[2], "..", 2)
		if len(hopParts) != 2 {
			return nil, fmt.Errorf("hop range %q must be MIN..MAX", parts[2])
		}
		var minHops, maxHops int
		if _, err := fmt.Sscanf(hopParts[0], "%d", &minHops); err != nil {
			return nil, fmt.Errorf("bad min hops %q", hopParts[0])
		}
		if _, err := fmt.Sscanf(hopParts[1], "%d", &maxHops); err != nil {
			return nil, fmt.Errorf("bad max hops %q", hopParts[1])
		}
		expr.MinHops = minHops
		expr.MaxHops = maxHops
	}
	return expr, nil
}

// ensureEmbedder boots the deterministic HashEmbedder + persisted HNSW
// index for this actor, then drains the journal backlog. Called only from
// the find subcommand when -near / -near-uri is set. The index file lives
// alongside the actor's Pebble DB so subsequent CLI invocations reuse it.
func ensureEmbedder(c *cortex.Cortex, root, actor string) {
	idxPath := filepath.Join(root, actor, "indexes", "vector", "index.hnsw")
	if err := c.StartEmbedder(cortex.EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 100 * time.Millisecond,
	}); err != nil {
		die("StartEmbedder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.DrainEmbedder(ctx); err != nil {
		die("DrainEmbedder: %v", err)
	}
}

// parseTypedJSON converts a JSON body into the corresponding TypedData.
//
// Note: this uses encoding/json (the convenient ergonomic wrapper), not
// canonical CBOR. The cortex stores the canonical CBOR form internally; the
// JSON here is only the CLI surface.
func parseTypedJSON(typeName, body string) (memory.TypedData, error) {
	switch typeName {
	case "Identity":
		var d memory.IdentityData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Fact":
		var d memory.FactData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Preference":
		var d memory.PreferenceData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Belief":
		var d memory.BeliefData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Event":
		var d memory.EventData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Goal":
		var d memory.GoalData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Constraint":
		var d memory.ConstraintData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Capability":
		var d memory.CapabilityData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Pattern":
		var d memory.PatternData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	}
	return nil, fmt.Errorf("unknown type %q", typeName)
}

func parseTypeName(name string) memory.Type {
	switch name {
	case "Identity":
		return memory.TypeIdentity
	case "Fact":
		return memory.TypeFact
	case "Preference":
		return memory.TypePreference
	case "Belief":
		return memory.TypeBelief
	case "Event":
		return memory.TypeEvent
	case "Goal":
		return memory.TypeGoal
	case "Constraint":
		return memory.TypeConstraint
	case "Capability":
		return memory.TypeCapability
	case "Pattern":
		return memory.TypePattern
	}
	return 0
}

func printMemory(m *memory.Memory) {
	fmt.Printf("type=%s id=%s version=%d created=%s by=%s confidence=%.2f hash=%x\n",
		m.Head.Type, m.Head.ID, m.Version.Version,
		m.Version.CreatedAt.Format(time.RFC3339Nano), m.Version.CreatedBy,
		m.Version.Confidence, m.Version.Hash)
	if m.Head.Tombstoned != nil {
		fmt.Printf("tombstoned: reason=%q at=%s by=%s\n",
			m.Head.Tombstoned.Reason,
			m.Head.Tombstoned.At.Format(time.RFC3339Nano),
			m.Head.Tombstoned.By)
	}
	if m.Version.Forms.Short != "" {
		fmt.Printf("forms.short=%q\n", m.Version.Forms.Short)
	}
	if m.Version.Forms.Medium != "" {
		fmt.Printf("forms.medium=%q\n", m.Version.Forms.Medium)
	}
	dec, err := memory.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		fmt.Printf("data decode error: %v\n", err)
		return
	}
	pretty, err := json.MarshalIndent(dec, "  ", "  ")
	if err == nil {
		fmt.Printf("data:\n  %s\n", pretty)
	}
}

func summarize(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: cortex-shell -root DIR -actor NAME <cmd> [args]\n")
	fmt.Fprintf(os.Stderr, "  commands:\n")
	fmt.Fprintf(os.Stderr, "    head                          journal next-seq + count\n")
	fmt.Fprintf(os.Stderr, "    dump                          dump journal entries (raw)\n")
	fmt.Fprintf(os.Stderr, "    append <kind> <payload>       raw journal append (smoke)\n")
	fmt.Fprintf(os.Stderr, "    write <Type> <json-body>      typed memory write\n")
	fmt.Fprintf(os.Stderr, "    resolve <uri>                 fetch + print a memory\n")
	fmt.Fprintf(os.Stderr, "    update <uri> <json-body>      replace data, bump version\n")
	fmt.Fprintf(os.Stderr, "    tombstone <uri> <reason>      soft-delete a memory\n")
	fmt.Fprintf(os.Stderr, "    list <Type>                   list URIs of memories of Type\n")
	fmt.Fprintf(os.Stderr, "    find <Type|*> [-tag T]... [-eq f=v]... [-limit N] [-late]\n")
	fmt.Fprintf(os.Stderr, "         [-near \"text\"|-near-uri <uri>]\n")
	fmt.Fprintf(os.Stderr, "         [-from <uri> [-follow <type>:<dir>:<min..max>]]\n")
	fmt.Fprintf(os.Stderr, "                                  typed Find + vector + graph (Phases 3 + 5 + 6)\n")
	fmt.Fprintf(os.Stderr, "    add-edge <src> <type> <dst>   write a typed edge (Phase 6)\n")
	fmt.Fprintf(os.Stderr, "    remove-edge <src> <type> <dst> <reason>\n")
	fmt.Fprintf(os.Stderr, "    list-edges <uri> [out|in|both]\n")
	fmt.Fprintf(os.Stderr, "    snapshot [reason]             persist a SnapshotManifest at snap/<seq>\n")
	fmt.Fprintf(os.Stderr, "    dump-snapshot <seq>           print a persisted manifest by seq\n")
	fmt.Fprintf(os.Stderr, "    overall-root                  print current OverallRoot (D11 seed input)\n")
	fmt.Fprintf(os.Stderr, "    prove <uri>                   SMT membership/non-membership proof for a memory\n")
	fmt.Fprintf(os.Stderr, "    write-frame <Type> <json-body> <verb:kind:ref>...\n")
	fmt.Fprintf(os.Stderr, "                                  Write with Head.Frames (seeds idx/frame + idx/actor_obj) (Phase 8)\n")
	fmt.Fprintf(os.Stderr, "    context [-verb V] [-obj kind:ref]... [-include-tier T]... [-budget N] [-outcome-limit N] [-form F]\n")
	fmt.Fprintf(os.Stderr, "                                  Phase 1 cold-start bundle composer (Phase 8)\n")
	fmt.Fprintf(os.Stderr, "    compact -intent ID -step ID [-budget N] [-dir DIR] [-load URI]... <uri>...\n")
	fmt.Fprintf(os.Stderr, "                                  Phase 4 budget-aware compaction (Phase 9)\n")
	fmt.Fprintf(os.Stderr, "    dump-checkpoint <intent_id> <step_id>\n")
	fmt.Fprintf(os.Stderr, "                                  print a persisted Phase 9 checkpoint as JSON\n")
	fmt.Fprintf(os.Stderr, "    update-head <uri> [flags]     mutate Head fields without bumping Data version (Phase 10)\n")
	fmt.Fprintf(os.Stderr, "    dump-scope <file>             decode + print a CortexScope CBOR file (Phase 10)\n")
	fmt.Fprintf(os.Stderr, "    rebuild [-verify-only]        drop indexes/, rebuild from canonical, verify (Phase 11)\n")
	fmt.Fprintf(os.Stderr, "    attest -intent ID -outcome (success|failure) [-reason R] [-by CREATOR] <uri>...\n")
	fmt.Fprintf(os.Stderr, "                                  bump salience.Citations on cited memories (Phase 11.5)\n")
	fmt.Fprintf(os.Stderr, "    dump-attest <seq>             print KindAttest payload at j/<seq> as JSON\n")
	fmt.Fprintf(os.Stderr, "    dump-salience <uri>           print salience/<id> Score as JSON (debug AccessCount/Citations)\n")
	fmt.Fprintf(os.Stderr, "    dump-weights                  print meta/salience_weights as JSON (Phase 12 EMA-learned weights; cold-start fallback when absent)\n")
	flag.PrintDefaults()
}

// multiFlag implements flag.Value for repeated string flags (e.g.
// `-load uri1 -load uri2`). Used by runCompact.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cortex-shell: "+format+"\n", a...)
	os.Exit(1)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
