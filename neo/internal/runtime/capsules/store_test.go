// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capsules

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"matrix/neo/internal/runtime/records"
	"matrix/vault"
)

func capsuleVault(t *testing.T, root string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: filepath.Join(root, "vault"),
		UserDID: "did:matrix:capsule-test", KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func capsuleTurn() records.TurnRecord {
	return records.TurnRecord{
		LogicalTurnID: "turn-capsules", ConversationID: "conversation-capsules",
		RequestIdentity: "request-1", Objective: "qualify a multi-hour causal run",
		LatestGenuineMessageID: "message-1", CurrentState: records.StateGenerating,
	}
}

func TestOneHundredDeltaCapsulesPreserveClaimsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "capsules")
	session := capsuleVault(t, root)
	store, err := Open(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	turn := capsuleTurn()
	wantClaims := make([]Claim, 0, 100)
	for cycle := 1; cycle <= 100; cycle++ {
		claim := Claim{
			Statement: fmt.Sprintf("claim-%03d remained exact", cycle),
			Status:    "observed", Evidence: []string{fmt.Sprintf("artifact-%03d#L%d", cycle, cycle)},
		}
		wantClaims = append(wantClaims, claim)
		_, err := store.Append(context.Background(), turn, Capsule{
			CapsuleID: fmt.Sprintf("capsule-%03d", cycle), LogicalTurnID: turn.LogicalTurnID,
			CycleStart: uint64(cycle), CycleEnd: uint64(cycle), Objective: turn.Objective,
			OperationsAttempted: []string{fmt.Sprintf("operation-%03d", cycle)},
			Observations:        []string{fmt.Sprintf("hour-minute-%03d", cycle)},
			EvidenceRefs:        claim.Evidence, Claims: []Claim{claim},
			SourceIdentities: []string{fmt.Sprintf("result-%03d", cycle)},
			Temperature:      Warm,
		})
		if err != nil {
			t.Fatalf("append cycle %d: %v", cycle, err)
		}
	}
	restarted, err := Open(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	capsules, err := restarted.List(context.Background())
	if err != nil || len(capsules) != 100 {
		t.Fatalf("restart list count=%d err=%v", len(capsules), err)
	}
	for index, capsule := range capsules {
		if len(capsule.Claims) != 1 || !reflect.DeepEqual(capsule.Claims[0], wantClaims[index]) || capsule.ContentHash == "" {
			t.Fatalf("claim/provenance drift at %d: %#v", index, capsule)
		}
	}
}

func TestConsumptionFenceSurvivesTurnReload(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "capsules"), capsuleVault(t, root))
	if err != nil {
		t.Fatal(err)
	}
	turn := capsuleTurn()
	turn.CurrentState = records.StateSynthesisOwed
	turn.SynthesisDebt = records.SynthesisDebt{Owed: true, UnconsumedEvidence: []string{"result-critical"}}
	capsule := Capsule{
		CapsuleID: "capsule-blocked", LogicalTurnID: turn.LogicalTurnID,
		CycleStart: 1, CycleEnd: 1, Objective: turn.Objective,
		SourceIdentities: []string{"result-critical"},
	}
	if _, err := store.Append(context.Background(), turn, capsule); err == nil {
		t.Fatal("unconsumed evidence crossed the compaction fence")
	}
	turn.CurrentState = records.StateGenerating
	turn.SynthesisDebt = records.SynthesisDebt{}
	if _, err := store.Append(context.Background(), turn, capsule); err != nil {
		t.Fatalf("acknowledged evidence did not compact: %v", err)
	}
}
