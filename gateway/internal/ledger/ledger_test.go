// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ledger

import (
	"context"
	"testing"
	"time"

	"matrix/gateway/internal/rates"
)

func TestMemoryRecordAndDailySpend(t *testing.T) {
	now := time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC)
	m := NewMemory("10")
	m.SetClock(func() time.Time { return now })

	if err := m.Record(context.Background(), Entry{
		ActorDID: "did:pax:a",
		Model:    rates.ModelGPTOSS20B,
		CostPax:  "0.000050000000",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := m.Record(context.Background(), Entry{
		ActorDID: "did:pax:a",
		Model:    rates.ModelGPTOSS20B,
		CostPax:  "0.000150000000",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := m.DailySpend(context.Background(), "did:pax:a", now)
	if err != nil {
		t.Fatalf("DailySpend: %v", err)
	}
	if got != "0.000200000000" {
		t.Fatalf("want 0.000200000000 got %q", got)
	}
}

func TestMemoryDailySpendIsolatesActorAndDay(t *testing.T) {
	day1 := time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 28, 14, 0, 0, 0, time.UTC)
	m := NewMemory("")

	m.SetClock(func() time.Time { return day1 })
	_ = m.Record(context.Background(), Entry{ActorDID: "did:pax:a", CostPax: "0.5"})
	_ = m.Record(context.Background(), Entry{ActorDID: "did:pax:b", CostPax: "0.7"})

	m.SetClock(func() time.Time { return day2 })
	_ = m.Record(context.Background(), Entry{ActorDID: "did:pax:a", CostPax: "0.3"})

	if v, _ := m.DailySpend(context.Background(), "did:pax:a", day1); v != "0.500000000000" {
		t.Fatalf("a/day1 got %q", v)
	}
	if v, _ := m.DailySpend(context.Background(), "did:pax:a", day2); v != "0.300000000000" {
		t.Fatalf("a/day2 got %q", v)
	}
	if v, _ := m.DailySpend(context.Background(), "did:pax:b", day1); v != "0.700000000000" {
		t.Fatalf("b/day1 got %q", v)
	}
}

func TestMemoryDailyCapDefault(t *testing.T) {
	m := NewMemory("")
	cap, err := m.DailyCap(context.Background(), "did:pax:x")
	if err != nil {
		t.Fatalf("DailyCap: %v", err)
	}
	if cap != DefaultDailyPaxCap {
		t.Fatalf("got %q want %q", cap, DefaultDailyPaxCap)
	}
}

func TestMemoryDailyCapOverride(t *testing.T) {
	m := NewMemory("10")
	m.SetCap("did:pax:x", "1.5")
	cap, _ := m.DailyCap(context.Background(), "did:pax:x")
	if cap != "1.5" {
		t.Fatalf("override: got %q", cap)
	}
}

func TestCheckBudget(t *testing.T) {
	rem, exhausted, err := CheckBudget("3", "1.5", "5")
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if exhausted {
		t.Fatalf("3+1.5 < 5; should not be exhausted")
	}
	if rem != "2.000000000000" {
		t.Fatalf("remaining: %q", rem)
	}
	rem, exhausted, err = CheckBudget("4", "1.5", "5")
	if err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if !exhausted {
		t.Fatalf("4+1.5 > 5; should be exhausted")
	}
	if rem != "1.000000000000" {
		t.Fatalf("rem at boundary: %q", rem)
	}
}

func TestRecordRejectsEmptyActor(t *testing.T) {
	m := NewMemory("")
	err := m.Record(context.Background(), Entry{CostPax: "0.1"})
	if err == nil {
		t.Fatalf("expected error on empty actor")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
