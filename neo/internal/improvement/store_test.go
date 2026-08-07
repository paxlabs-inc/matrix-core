// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package improvement

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"matrix/neo/internal/memory"
	"matrix/vault"
)

func evidencedMemoryDraft() Draft {
	return Draft{
		Kind: KindMemory, Summary: "Correct the preferred deployment region",
		Rationale: "The user explicitly corrected the earlier region.", Confidence: 1,
		Evidence: []Evidence{{ConversationID: "conversation-1", RunID: "run-1", Role: "user", Quote: "Use Frankfurt, not Virginia."}},
		Payload: Payload{Memory: &memory.MutationItem{
			Operation: memory.MutationSupersede,
			Target:    &memory.MutationTarget{URI: "memory://fact/old#1"},
			Value:     &memory.MutationValue{Type: "user_fact", Content: "Preferred deployment region is Frankfurt."},
			Reason:    "explicit user correction",
		}},
	}
}

func TestStoreRestartIdempotencyAndVersionedRollback(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, nil, "did:matrix:test")
	if err != nil {
		t.Fatal(err)
	}
	observation, fresh, err := store.Schedule("conversation-1", "run-1", time.Now().Add(time.Minute))
	if err != nil || !fresh {
		t.Fatalf("schedule: fresh=%v err=%v", fresh, err)
	}
	if err := store.SetAlarm(observation.Key, "alarm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(observation.Key); err != nil {
		t.Fatal(err)
	}
	created, err := store.Finish(observation.Key, []Draft{evidencedMemoryDraft()})
	if err != nil || len(created) != 1 {
		t.Fatalf("finish: proposals=%d err=%v", len(created), err)
	}

	reopened, err := Open(dir, nil, "did:matrix:test")
	if err != nil {
		t.Fatal(err)
	}
	_, fresh, err = reopened.Schedule("conversation-1", "run-1", time.Now().Add(time.Hour))
	if err != nil || fresh {
		t.Fatalf("restart schedule must deduplicate: fresh=%v err=%v", fresh, err)
	}
	repeated, err := reopened.Finish(observation.Key, []Draft{evidencedMemoryDraft()})
	if err != nil || len(repeated) != 1 || len(reopened.List("")) != 1 {
		t.Fatalf("replayed observer duplicated proposal: repeated=%d total=%d err=%v", len(repeated), len(reopened.List("")), err)
	}

	proposal := created[0]
	approved, err := reopened.Transition(proposal.ID, []ProposalStatus{StatusPending}, StatusApproved, "owner", "reviewed", "", "")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := reopened.Transition(proposal.ID, []ProposalStatus{StatusApproved}, StatusApplied, "system", "owner applied", "memory://fact/new#2", "memory://fact/old#1")
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := reopened.Transition(proposal.ID, []ProposalStatus{StatusApplied}, StatusRolledBack, "owner", "rollback verified", "", "memory://fact/rollback#3")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Version != 2 || applied.Version != 3 || rolledBack.Version != 4 || len(rolledBack.History) != 4 {
		t.Fatalf("version history not preserved: approved=%d applied=%d rollback=%d history=%d", approved.Version, applied.Version, rolledBack.Version, len(rolledBack.History))
	}
}

func TestProposalSnapshotsDoNotExposeMutableStoreState(t *testing.T) {
	store, err := Open(t.TempDir(), nil, "did:matrix:test")
	if err != nil {
		t.Fatal(err)
	}
	observation, _, _ := store.Schedule("conversation-copy", "run-copy", time.Now())
	draft := evidencedMemoryDraft()
	created, err := store.Finish(observation.Key, []Draft{draft})
	if err != nil {
		t.Fatal(err)
	}
	draft.Payload.Memory.Value.Content = "caller mutation"
	created[0].Draft.Payload.Memory.Value.Content = "returned mutation"
	listed := store.List("")
	listed[0].Draft.Payload.Memory.Value.Content = "list mutation"
	got, _ := store.Get(created[0].ID)
	if got.Draft.Payload.Memory.Value.Content != "Preferred deployment region is Frankfurt." {
		t.Fatalf("caller mutated live proposal state: %+v", got.Draft.Payload.Memory.Value)
	}
}

func TestDraftValidationRequiresEvidenceAndGovernedPayload(t *testing.T) {
	draft := evidencedMemoryDraft()
	draft.Evidence = nil
	if err := ValidateDraft(draft); err == nil {
		t.Fatal("proposal without evidence must fail")
	}
	draft = evidencedMemoryDraft()
	draft.Payload.Memory.Operation = memory.MutationUpdate
	if err := ValidateDraft(draft); err == nil {
		t.Fatal("memory update must use typed supersession")
	}
	draft = evidencedMemoryDraft()
	draft.Kind = KindAuthority
	if err := ValidateDraft(draft); err == nil {
		t.Fatal("authority change must enter the Capability Hub candidate lifecycle")
	}
}

func TestNoopObservationIsSilentAndDurable(t *testing.T) {
	store, err := Open(t.TempDir(), nil, "did:matrix:test")
	if err != nil {
		t.Fatal(err)
	}
	observation, _, _ := store.Schedule("conversation-empty", "run-empty", time.Now())
	if _, err := store.Begin(observation.Key); err != nil {
		t.Fatal(err)
	}
	proposals, err := store.Finish(observation.Key, nil)
	if err != nil || len(proposals) != 0 || len(store.List("")) != 0 {
		t.Fatalf("no-op must create nothing: proposals=%d list=%d err=%v", len(proposals), len(store.List("")), err)
	}
	got, ok := store.Observation(observation.Key)
	if !ok || got.Status != ObservationNoop {
		t.Fatalf("no-op observation state = %+v, ok=%v", got, ok)
	}
}

func TestStoreSealsProposalEvidenceAndReloads(t *testing.T) {
	user := "did:matrix:improvement-vault-test"
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := Open(dir, session, user)
	if err != nil {
		t.Fatal(err)
	}
	observation, _, err := store.Schedule("conversation-secret", "run-secret", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	draft := evidencedMemoryDraft()
	draft.Evidence[0].Quote = "The private correction must not be plaintext on disk."
	if _, err := store.Finish(observation.Key, []Draft{draft}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(raw) || bytes.Contains(raw, []byte("private correction")) {
		t.Fatalf("proposal state was not sealed: %q", raw)
	}
	reopened, err := Open(dir, session, user)
	if err != nil {
		t.Fatal(err)
	}
	proposals := reopened.List("")
	if len(proposals) != 1 || proposals[0].Draft.Evidence[0].Quote != draft.Evidence[0].Quote {
		t.Fatalf("sealed proposal did not round-trip: %+v", proposals)
	}
}
