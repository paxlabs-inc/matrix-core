// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestAllowDisabledByDefault(t *testing.T) {
	l := New(0, 0)
	for i := 0; i < 100; i++ {
		if !l.Allow("k") {
			t.Fatalf("rate=0 should allow all; failed at i=%d", i)
		}
	}
}

func TestAllowConsumesAndRefills(t *testing.T) {
	l := New(2, 5) // 2 tokens/sec, burst 5
	now := time.Unix(1700_000_000, 0)
	l.SetClock(func() time.Time { return now })

	// Initial bucket = 5; allow 5, then deny.
	for i := 0; i < 5; i++ {
		if !l.Allow("a") {
			t.Fatalf("expected allow at i=%d", i)
		}
	}
	if l.Allow("a") {
		t.Fatalf("expected deny after burst exhausted")
	}
	// Advance 2s → +4 tokens but capped at burst=5.
	now = now.Add(2 * time.Second)
	for i := 0; i < 4; i++ {
		if !l.Allow("a") {
			t.Fatalf("expected allow after refill, i=%d", i)
		}
	}
}

func TestAllowPerKeyIndependent(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(1700_000_000, 0)
	l.SetClock(func() time.Time { return now })
	if !l.Allow("a") {
		t.Fatalf("a first call")
	}
	if !l.Allow("b") {
		t.Fatalf("b first call")
	}
	if l.Allow("a") {
		t.Fatalf("a second call should be denied")
	}
}

func TestAllowConcurrent(t *testing.T) {
	// Burst=1000, rate=0 — pure burst exhaustion test.
	l := New(0.0001, 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 1500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Burst is 1000; rate is 0.0001 → effectively no refill within
	// the test window. Allow should be exactly 1000.
	if allowed > 1000 || allowed < 990 {
		t.Fatalf("allowed=%d expected ~1000", allowed)
	}
}

func TestSnapshotMissingKeyEqualsBurst(t *testing.T) {
	l := New(1, 7)
	if got := l.Snapshot("ghost"); got != 7 {
		t.Fatalf("missing key snapshot=%g, want 7", got)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
