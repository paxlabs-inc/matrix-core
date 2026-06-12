package schedule

import (
	"testing"
	"time"
)

func TestNextOnceDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := NextOnce(600, "", now)
	if err != nil {
		t.Fatalf("NextOnce delay: %v", err)
	}
	want := now.Add(10 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextOnceAbsolute(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := NextOnce(0, "2026-01-01T18:30:00Z", now)
	if err != nil {
		t.Fatalf("NextOnce abs: %v", err)
	}
	if got.Hour() != 18 || got.Minute() != 30 {
		t.Fatalf("unexpected time %v", got)
	}
}

func TestNextOnceRejectsPast(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := NextOnce(0, "2025-01-01T00:00:00Z", now); err == nil {
		t.Fatal("expected error for past fire_at")
	}
}

func TestNextOnceRejectsBoth(t *testing.T) {
	now := time.Now()
	if _, err := NextOnce(60, "2030-01-01T00:00:00Z", now); err == nil {
		t.Fatal("expected error when both delay and fire_at set")
	}
}

func TestNextOnceRejectsNeither(t *testing.T) {
	if _, err := NextOnce(0, "", time.Now()); err == nil {
		t.Fatal("expected error when neither delay nor fire_at set")
	}
}

func TestNextCronDaily(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// every day at 09:00 UTC -> next is 2026-01-02 09:00.
	got, err := NextCron("0 9 * * *", "UTC", now)
	if err != nil {
		t.Fatalf("NextCron: %v", err)
	}
	want := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextCronTimezone(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 09:00 New York on Jan 1 = 14:00 UTC (EST, UTC-5).
	got, err := NextCron("0 9 * * *", "America/New_York", now)
	if err != nil {
		t.Fatalf("NextCron tz: %v", err)
	}
	want := time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextCronDescriptor(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 30, 0, 0, time.UTC)
	got, err := NextCron("@hourly", "UTC", now)
	if err != nil {
		t.Fatalf("NextCron @hourly: %v", err)
	}
	want := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseCronRejectsGarbage(t *testing.T) {
	if _, err := ParseCron("not a cron"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadLocationDefaultsUTC(t *testing.T) {
	loc, err := LoadLocation("")
	if err != nil || loc != time.UTC {
		t.Fatalf("expected UTC default, got %v err %v", loc, err)
	}
}
