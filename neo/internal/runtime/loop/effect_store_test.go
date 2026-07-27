// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"matrix/neo/internal/runtime/protocol"
)

func TestDurableEffectJournalReconcilesCompletedAndRetrySafeRealDispatches(
	t *testing.T,
) {
	store := realTurnStore(
		t, "effect-journal-turn", "Exercise effect reconciliation.",
	)
	journal := &DurableEffectJournal{Store: store}
	manager := realExecManager(t)
	adapter, err := NewToolManagerAdapter(manager, journal)
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	completed, err := adapter.Execute(
		t.Context(),
		protocol.NormalizedToolCall{
			ID: "effect-completed", Name: "exec__shell",
			Arguments: json.RawMessage(
				`{"command":"printf durable-effect","cwd":"` +
					filepath.ToSlash(workdir) +
					`","expect":"prints durable-effect"}`,
			),
		},
		"effect-completed-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := journal.ReconcileEffect(
		t.Context(), "effect-completed-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileCompleted ||
		string(reconciled.Result.Content) != string(completed.Content) ||
		reconciled.Result.IsError != completed.IsError {
		t.Fatalf("completed reconciliation = %+v want %+v", reconciled, completed)
	}

	if _, err := adapter.Execute(
		t.Context(),
		protocol.NormalizedToolCall{
			ID: "effect-read-dispatch", Name: "exec__service_list",
			Arguments: json.RawMessage(`{"expect":"lists managed services"}`),
		},
		"effect-read-dispatch-key",
	); err != nil {
		t.Fatal(err)
	}
	readRecord, err := store.LoadEffect(
		t.Context(), "effect-read-dispatch-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !readRecord.RetrySafe {
		t.Fatal("real read-only dispatch was not persisted as retry-safe")
	}

	if err := journal.BeginEffect(
		t.Context(),
		"effect-read-key",
		"exec__service_list",
		json.RawMessage(`{"expect":"lists services"}`),
		true,
	); err != nil {
		t.Fatal(err)
	}
	reconciled, err = journal.ReconcileEffect(
		t.Context(), "effect-read-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileRetrySafe {
		t.Fatalf("read-only started effect = %+v", reconciled)
	}

	if err := journal.BeginEffect(
		t.Context(),
		"effect-write-key",
		"exec__shell",
		json.RawMessage(`{"command":"true","expect":"exit 0"}`),
		false,
	); err != nil {
		t.Fatal(err)
	}
	reconciled, err = journal.ReconcileEffect(
		t.Context(), "effect-write-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileUnknown {
		t.Fatalf("uncertain write effect = %+v", reconciled)
	}

	err = journal.BeginEffect(
		t.Context(),
		"effect-write-key",
		"exec__shell",
		json.RawMessage(`{"command":"printf changed","expect":"changed"}`),
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("effect identity conflict = %v", err)
	}
}

func TestDurableEffectJournalUnknownForMissingKey(t *testing.T) {
	store := realTurnStore(
		t, "effect-missing-turn", "Check a missing effect.",
	)
	journal := &DurableEffectJournal{Store: store}
	result, err := journal.ReconcileEffect(t.Context(), "never-recorded")
	if err != nil || result.Status != ReconcileUnknown {
		t.Fatalf("missing effect = %+v, %v", result, err)
	}
}
