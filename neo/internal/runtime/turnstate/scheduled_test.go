// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package turnstate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChronosWakeConventionsProduceDeterministicOccurrences(t *testing.T) {
	scheduledFor := time.Date(2026, 7, 27, 7, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		message string
		kind    WakeKind
	}{
		{
			message: "HEARTBEAT: review active goals",
			kind:    WakeHeartbeat,
		},
		{
			message: "AUTOMATRIX: review pending opportunities",
			kind:    WakeAutomatrix,
		},
		{
			message: "MORNING_BRIEF: compose the user's brief",
			kind:    WakeMorningBrief,
		},
	} {
		kind, err := ClassifyWake(test.message)
		if err != nil || kind != test.kind {
			t.Fatalf("ClassifyWake(%q) = %q, %v", test.message, kind, err)
		}
		first, err := OccurrenceID(kind, "alarm-1", scheduledFor)
		if err != nil {
			t.Fatal(err)
		}
		second, err := OccurrenceID(
			kind, "alarm-1", scheduledFor.In(time.FixedZone("offset", 3600)),
		)
		if err != nil || first != second {
			t.Fatalf("occurrence IDs differ by timezone: %q %q, %v", first, second, err)
		}
		next, err := OccurrenceID(kind, "alarm-1", scheduledFor.Add(time.Hour))
		if err != nil || next == first {
			t.Fatalf("next occurrence = %q, first = %q, %v", next, first, err)
		}
	}
}

func TestScheduledWakeRedeliveryCreatesOneTurnAndOneMessage(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	defer store.Close(ctx)
	wake := ScheduledWake{
		AlarmID: "alarm-brief-1", ActorID: testActorID,
		SessionID: testSession,
		Message:   "MORNING_BRIEF: compose the user's brief",
		ScheduledFor: time.Date(
			2026, 7, 27, 7, 0, 0, 0, time.UTC,
		),
	}
	const deliveries = 16
	var created atomic.Int32
	turnIDs := make(chan string, deliveries)
	errs := make(chan error, deliveries)
	var group sync.WaitGroup
	for delivery := 0; delivery < deliveries; delivery++ {
		group.Add(1)
		go func() {
			defer group.Done()
			state, wasCreated, err := store.CreateScheduledTurn(ctx, wake)
			if wasCreated {
				created.Add(1)
			}
			turnIDs <- state.TurnID
			errs <- err
		}()
	}
	group.Wait()
	close(turnIDs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d", created.Load())
	}
	turnID := ""
	for current := range turnIDs {
		if turnID == "" {
			turnID = current
		}
		if current != turnID {
			t.Fatalf("turn IDs differ: %q != %q", current, turnID)
		}
	}
	messages, err := store.ListTurnMessages(ctx, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 ||
		messages[0].Content != wake.Message ||
		messages[0].TurnID != turnID {
		t.Fatalf("messages = %+v", messages)
	}
	var turns, occurrences, transcriptRows int
	if err := store.readDB.QueryRow(
		`SELECT COUNT(*) FROM turn_state WHERE turn_id = ?`, turnID,
	).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err := store.readDB.QueryRow(
		`SELECT COUNT(*) FROM scheduled_occurrences WHERE turn_id = ?`, turnID,
	).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := store.readDB.QueryRow(
		`SELECT COUNT(*) FROM turn_messages WHERE turn_id = ?`, turnID,
	).Scan(&transcriptRows); err != nil {
		t.Fatal(err)
	}
	if turns != 1 || occurrences != 1 || transcriptRows != 1 {
		t.Fatalf("rows turn=%d occurrence=%d message=%d",
			turns, occurrences, transcriptRows)
	}
}
