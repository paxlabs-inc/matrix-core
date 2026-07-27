// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package belief

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	runtimeloop "matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
	"matrix/vault"
)

func TestVerifiedCitationsGatePremisesSubgoalsAndCapabilities(
	t *testing.T,
) {
	ctx := t.Context()
	root := t.TempDir()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:belief-test",
		KEKHex:  hex.EncodeToString(bytes.Repeat([]byte{0x44}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	journalStore, err := cortexstore.Open(
		filepath.Join(root, "cortex"), "belief-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalStore.SetVault(session, "did:matrix:belief-test")
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)
	turns, err := turnstate.Open(
		ctx, filepath.Join(root, "turnstate.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := turns.Close(closeCtx); err != nil {
			t.Errorf("close turnstate: %v", err)
		}
	})
	state, err := New("belief-session", cx, turns)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddPremise(
		ctx, "p-supported", "the command returns verified output",
	); err != nil {
		t.Fatal(err)
	}
	if err := state.AddPremise(
		ctx, "p-refuted", "the missing endpoint exists",
	); err != nil {
		t.Fatal(err)
	}

	matched := runtimeloop.ToolExecution{
		Call: protocol.NormalizedToolCall{
			ID: "matched-call", Name: "exec__shell",
			Arguments: json.RawMessage(`{"command":"printf verified"}`),
		},
		Result:         json.RawMessage(`{"ok":true,"exit_code":0,"stdout":"verified"}`),
		Expect:         "exit 0 with verified output",
		IdempotencyKey: "matched-idem",
		MatchVerdict:   cortex.ToolMatchMatched,
		SubgoalID:      "report",
	}
	matched.Citation = recordExecution(t, cx, matched)
	if err := state.ObserveToolExecution(ctx, matched); err != nil {
		t.Fatal(err)
	}
	if err := state.CitePremise(
		ctx, "p-supported", *matched.Citation,
	); err != nil {
		t.Fatal(err)
	}
	if err := state.CanAct("p-supported"); err != nil {
		t.Fatalf("verified cited premise blocked action: %v", err)
	}

	mismatched := runtimeloop.ToolExecution{
		Call: protocol.NormalizedToolCall{
			ID: "mismatch-call", Name: "exec__shell",
			Arguments: json.RawMessage(`{"command":"exit 7"}`),
		},
		Result:         json.RawMessage(`{"ok":false,"exit_code":7}`),
		Error:          "exit 7",
		Expect:         "exit 0 proving the endpoint exists",
		IdempotencyKey: "mismatch-idem",
		MatchVerdict:   cortex.ToolMatchMismatched,
		SubgoalID:      "endpoint-check",
	}
	mismatched.Citation = recordExecution(t, cx, mismatched)
	if err := state.ObserveToolExecution(ctx, mismatched); err != nil {
		t.Fatal(err)
	}
	if err := state.RefutePremise(
		ctx, "p-refuted", *mismatched.Citation,
	); err != nil {
		t.Fatal(err)
	}
	if err := state.CanAct("p-refuted"); !errors.Is(err, ErrPremiseBlocked) {
		t.Fatalf("refuted premise did not block action: %v", err)
	}

	snapshot := state.Snapshot()
	if !snapshot.Subgoals["report"].Completed ||
		snapshot.Subgoals["endpoint-check"].Completed ||
		snapshot.Capabilities["exec__shell"].VerifiedSuccesses != 1 ||
		snapshot.Premises["prediction:mismatch-call"].Status !=
			PremiseRefuted {
		t.Fatalf("citation-gated state=%+v", snapshot)
	}
	tampered := matched
	tampered.Citation = cloneCitation(matched.Citation)
	tampered.Citation.LeafHash[0] ^= 0xff
	if err := state.ObserveToolExecution(ctx, tampered); !errors.Is(err, ErrCitationRequired) {
		t.Fatalf("tampered evidence accepted: %v", err)
	}
	if state.Snapshot().Capabilities["exec__shell"].VerifiedSuccesses != 1 {
		t.Fatal("tampered citation updated self-model capability")
	}

	restored, err := New("belief-session", cx, turns)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	restoredState := restored.Snapshot()
	if !restoredState.Subgoals["report"].Completed ||
		restoredState.Premises["p-refuted"].Status != PremiseRefuted ||
		restoredState.Capabilities["exec__shell"].VerifiedSuccesses != 1 {
		t.Fatalf("restored cognition state=%+v", restoredState)
	}
}

func recordExecution(
	t *testing.T,
	cx *cortex.Cortex,
	execution runtimeloop.ToolExecution,
) *cortex.ToolEventCitation {
	t.Helper()
	citation, err := cx.RecordToolEvent(cortex.ToolEvent{
		CallID:         execution.Call.ID,
		ToolName:       execution.Call.Name,
		Arguments:      execution.Call.Arguments,
		Result:         execution.Result,
		Error:          execution.Error,
		Expect:         execution.Expect,
		MatchVerdict:   execution.MatchVerdict,
		SubgoalID:      execution.SubgoalID,
		IdempotencyKey: execution.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &citation
}

func cloneCitation(
	citation *cortex.ToolEventCitation,
) *cortex.ToolEventCitation {
	cloned := *citation
	return &cloned
}
