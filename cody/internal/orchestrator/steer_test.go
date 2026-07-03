// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"matrix/cody/internal/contract"
)

// TestSteerFoldsIntoNextSheetWithoutKillingWorker proves req 12.2: a steer that
// arrives WHILE t1's worker is alive is folded at the next orchestrator
// boundary (onto t2's sheet), never interrupts the live worker, and surfaces as
// a steer.folded event. Real orchestrator, real worker func, real workspace.
func TestSteerFoldsIntoNextSheetWithoutKillingWorker(t *testing.T) {
	root := t.TempDir()
	plan := twoTaskPlan()
	st := openStore(t)

	var mu sync.Mutex
	var steers []string
	provider := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, steers...)
	}

	const steerText = "prefer tabs over spaces"
	t1Started := make(chan struct{})
	steerLanded := make(chan struct{})
	sheetSteers := map[string][]string{}
	var folded []string

	o, err := New(Options{
		Root:   root,
		Plan:   plan,
		Store:  st,
		Steers: provider,
		Observer: func(event string, fields map[string]interface{}) {
			if event == "steer.folded" {
				folded = append(folded, fields["text"].(string))
			}
		},
		Worker: func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
			sheetSteers[sheet.TaskID] = append([]string{}, sheet.Steers...)
			file, content := "greet.txt", "hello cody\n"
			if sheet.TaskID == "t1" {
				close(t1Started)
				<-steerLanded // the steer arrives while THIS worker is alive
			} else {
				file, content = "reply.txt", "hello back\n"
			}
			if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0o644); err != nil {
				return nil, err
			}
			return &contract.TurnInReport{
				TaskID: sheet.TaskID, Status: contract.StatusDone, Summary: "wrote " + file,
				Verification: []contract.Evidence{{Command: sheet.Verify.Commands[0], Exit: 0}},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Submit the steer mid-t1: after t1's worker is alive, before it returns.
	go func() {
		<-t1Started
		mu.Lock()
		steers = append(steers, steerText)
		mu.Unlock()
		close(steerLanded)
	}()

	res, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The live t1 worker ran to completion — the steer never interrupted it.
	if len(res.Done) != 2 || res.Done[0] != "t1" || res.Done[1] != "t2" {
		t.Fatalf("Done = %v, Failed = %v (steer must not kill the live worker)", res.Done, res.Failed)
	}
	// t1's sheet was authored before the steer landed: no direction.
	if len(sheetSteers["t1"]) != 0 {
		t.Fatalf("t1 sheet carried steers %v, want none (authored pre-steer)", sheetSteers["t1"])
	}
	// t2's sheet — authored at the boundary after the steer — carries it.
	if len(sheetSteers["t2"]) != 1 || sheetSteers["t2"][0] != steerText {
		t.Fatalf("t2 sheet steers = %v, want [%q]", sheetSteers["t2"], steerText)
	}
	// The fold surfaced exactly once as an event.
	if len(folded) != 1 || folded[0] != steerText {
		t.Fatalf("steer.folded events = %v, want one %q", folded, steerText)
	}
}
