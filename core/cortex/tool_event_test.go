// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"centra/core/cortex/journal"
	"centra/core/cortex/keys"
	"centra/packages/vault"
)

func TestToolEventCitationRoundTripAndHistoricalRoot(t *testing.T) {
	cortex := openSealedCortex(t)
	before, err := cortex.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	event := ToolEvent{
		CallID: "call-1", ToolName: "exec__shell",
		Arguments:      json.RawMessage(`{"command":"printf verified"}`),
		Result:         json.RawMessage(`{"ok":true,"stdout":"verified"}`),
		Expect:         "exit 0 with verified output",
		MatchVerdict:   ToolMatchMatched,
		SubgoalID:      "report-evidence",
		IdempotencyKey: "idem-1",
		CreatedBy:      "did:matrix:test",
	}
	citation, err := cortex.RecordToolEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	after, err := cortex.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	if before == after || citation.LeafCount != 1 {
		t.Fatalf("tool leaf did not move OverallRoot: before=%x after=%x citation=%+v",
			before, after, citation)
	}
	payload, err := cortex.VerifyToolEventCitation(citation)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ToolName != event.ToolName ||
		payload.ResultDigest != sha256Bytes(event.Result) ||
		payload.MatchVerdict != ToolMatchMatched ||
		payload.SubgoalID != event.SubgoalID {
		t.Fatalf("verified payload=%+v", payload)
	}
	replayed, err := cortex.RecordToolEvent(event)
	if err != nil || replayed != citation {
		t.Fatalf("idempotent replay citation=%+v err=%v", replayed, err)
	}
	if count, err := cortex.Snap().MMR().LeafCount(); err != nil || count != 1 {
		t.Fatalf("idempotent replay leaf count=%d err=%v", count, err)
	}
	conflict := event
	conflict.Result = json.RawMessage(`{"ok":true,"stdout":"different"}`)
	if _, err := cortex.RecordToolEvent(conflict); !errors.Is(err, ErrToolEventConflict) {
		t.Fatalf("idempotency conflict err=%v", err)
	}

	if _, err := cortex.RecordToolEvent(ToolEvent{
		CallID: "call-2", ToolName: "fetch__fetch",
		Arguments:    json.RawMessage(`{"url":"https://example.test"}`),
		Result:       json.RawMessage(`{"status":503}`),
		Error:        "HTTP 503",
		Expect:       "HTTP 200 JSON",
		MatchVerdict: ToolMatchMismatched,
		SubgoalID:    "source-check",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cortex.VerifyToolEventCitation(citation); err != nil {
		t.Fatalf("historical citation failed after later append: %v", err)
	}
	rebuild, err := cortex.Rebuild(RebuildOptions{})
	if err != nil || rebuild.PreOverallRoot != rebuild.PostOverallRoot {
		t.Fatalf("tool-event MMR rebuild=%+v err=%v", rebuild, err)
	}
	if _, err := cortex.VerifyToolEventCitation(citation); err != nil {
		t.Fatalf("citation failed after MMR rebuild: %v", err)
	}
	tampered := citation
	tampered.LeafHash[0] ^= 0xff
	if _, err := cortex.VerifyToolEventCitation(tampered); !errors.Is(err, ErrToolCitationMismatch) {
		t.Fatalf("tampered citation err=%v", err)
	}
}

func TestToolEventJournalValueSealedBelowPlaintextHash(t *testing.T) {
	cortex := openSealedCortex(t)
	citation, err := cortex.RecordToolEvent(ToolEvent{
		CallID: "sealed-call", ToolName: "exec__shell",
		Arguments:    json.RawMessage(`{"command":"printf private-evidence"}`),
		Result:       json.RawMessage(`{"ok":true}`),
		Expect:       "exit 0",
		MatchVerdict: ToolMatchMatched,
		SubgoalID:    "sealed-proof",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, closer, err := cortex.Store().DB().Get(
		keys.JournalKey(citation.Seq),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if !vault.IsVault(raw) ||
		bytes.Contains(raw, []byte("private-evidence")) {
		t.Fatal("tool-event journal entry was not sealed at rest")
	}
	payload, err := cortex.VerifyToolEventCitation(citation)
	if err != nil || payload.CallID != "sealed-call" {
		t.Fatalf("sealed citation payload=%+v err=%v", payload, err)
	}
	var observed journal.Entry
	if err := cortex.Store().IterJournal(
		func(entry *journal.Entry) error {
			observed = *entry
			return nil
		},
	); err != nil || observed.Kind != journal.KindToolEvent {
		t.Fatalf("plaintext read seam entry=%+v err=%v", observed, err)
	}
}

// Citations are minted after Commit releases the store write lock, so a
// concurrent journal writer (the async embedder, a session append, a sibling
// tool execution) can land a leaf before the root is read. Every citation
// must still pin the prefix its own commit produced.
func TestConcurrentToolEventCitationsPinTheirOwnPrefix(t *testing.T) {
	cortex := openSealedCortex(t)
	const executions = 12
	citations := make([]ToolEventCitation, executions)
	errs := make([]error, executions)
	var wg sync.WaitGroup
	for index := 0; index < executions; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			citations[index], errs[index] = cortex.RecordToolEvent(ToolEvent{
				CallID:   fmt.Sprintf("concurrent-call-%d", index),
				ToolName: "exec__shell",
				Arguments: json.RawMessage(fmt.Sprintf(
					`{"command":"printf leg-%d"}`, index,
				)),
				Result: json.RawMessage(fmt.Sprintf(
					`{"ok":true,"stdout":"leg-%d"}`, index,
				)),
				Expect:         fmt.Sprintf("exit 0 printing leg-%d", index),
				MatchVerdict:   ToolMatchMatched,
				SubgoalID:      "concurrent-evidence",
				IdempotencyKey: fmt.Sprintf("concurrent-idem-%d", index),
			})
		}(index)
	}
	wg.Wait()
	seen := make(map[uint64]bool, executions)
	for index, citation := range citations {
		if errs[index] != nil {
			t.Fatalf("execution %d: %v", index, errs[index])
		}
		if seen[citation.Seq] {
			t.Fatalf("execution %d reused seq %d", index, citation.Seq)
		}
		seen[citation.Seq] = true
		payload, err := cortex.VerifyToolEventCitation(citation)
		if err != nil {
			t.Fatalf("execution %d citation %+v did not verify: %v",
				index, citation, err)
		}
		if payload.CallID != fmt.Sprintf("concurrent-call-%d", index) {
			t.Fatalf("execution %d verified the wrong leaf: %+v",
				index, payload)
		}
	}
	count, err := cortex.Snap().MMR().LeafCount()
	if err != nil || count != executions {
		t.Fatalf("leaf count=%d err=%v want=%d", count, err, executions)
	}
}

// The idempotency index is written in the same atomic batch as the leaf it
// points at, so a replay resolves without walking the journal and a rebuild
// (which re-derives the accumulator) leaves both intact.
func TestToolEventIdempotencyIndexResolvesWithoutJournalScan(t *testing.T) {
	cortex := openSealedCortex(t)
	event := ToolEvent{
		CallID: "indexed-call", ToolName: "fetch__fetch",
		Arguments:      json.RawMessage(`{"url":"https://example.test/a"}`),
		Result:         json.RawMessage(`{"status":200}`),
		Expect:         "HTTP 200 JSON",
		MatchVerdict:   ToolMatchMatched,
		SubgoalID:      "source-check",
		IdempotencyKey: "indexed-idem",
	}
	citation, err := cortex.RecordToolEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok, err := cortex.Store().Get(keys.ToolEventIdemKey("indexed-idem"))
	if err != nil || !ok || len(raw) != 8 ||
		binary.BigEndian.Uint64(raw) != citation.Seq {
		t.Fatalf("idempotency index raw=%x ok=%v err=%v seq=%d",
			raw, ok, err, citation.Seq)
	}
	for filler := 0; filler < 5; filler++ {
		if _, err := cortex.RecordToolEvent(ToolEvent{
			CallID:   fmt.Sprintf("filler-%d", filler),
			ToolName: "exec__shell",
			Arguments: json.RawMessage(
				fmt.Sprintf(`{"command":"printf %d"}`, filler),
			),
			Result:       json.RawMessage(`{"ok":true}`),
			Expect:       "exit 0",
			MatchVerdict: ToolMatchMatched,
			SubgoalID:    "filler",
		}); err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := cortex.RecordToolEvent(event)
	if err != nil || replayed != citation {
		t.Fatalf("replayed=%+v want=%+v err=%v", replayed, citation, err)
	}
	rebuild, err := cortex.Rebuild(RebuildOptions{})
	if err != nil || rebuild.PreOverallRoot != rebuild.PostOverallRoot {
		t.Fatalf("rebuild=%+v err=%v", rebuild, err)
	}
	afterRebuild, err := cortex.RecordToolEvent(event)
	if err != nil || afterRebuild != citation {
		t.Fatalf("post-rebuild replay=%+v want=%+v err=%v",
			afterRebuild, citation, err)
	}
	if _, err := cortex.VerifyToolEventCitation(citation); err != nil {
		t.Fatalf("post-rebuild citation: %v", err)
	}
}

func sha256Bytes(content []byte) [32]byte {
	return sha256.Sum256(content)
}
