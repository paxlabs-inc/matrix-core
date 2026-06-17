package ratelimit

import (
	"testing"
	"time"
)

func TestAllowConsumesBurstThenDenies(t *testing.T) {
	l := New(1, 3) // 1 rps, burst 3
	now := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		ok, _ := l.allowAt("ip", now)
		if !ok {
			t.Fatalf("request %d within burst should be allowed", i+1)
		}
	}
	ok, retry := l.allowAt("ip", now)
	if ok {
		t.Fatal("4th request in the same instant should be denied (burst exhausted)")
	}
	if retry <= 0 || retry > time.Second {
		t.Fatalf("retry-after = %v, want (0, 1s] at 1rps", retry)
	}
}

func TestRefillOverTime(t *testing.T) {
	l := New(2, 2) // 2 rps
	now := time.Unix(100, 0)
	// drain
	l.allowAt("ip", now)
	l.allowAt("ip", now)
	if ok, _ := l.allowAt("ip", now); ok {
		t.Fatal("expected drained bucket to deny")
	}
	// 1s later -> 2 tokens refilled
	later := now.Add(time.Second)
	if ok, _ := l.allowAt("ip", later); !ok {
		t.Fatal("expected refill after 1s to allow")
	}
	if ok, _ := l.allowAt("ip", later); !ok {
		t.Fatal("expected second refilled token to allow")
	}
	if ok, _ := l.allowAt("ip", later); ok {
		t.Fatal("expected third request to deny (only 2 refilled)")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, 1)
	now := time.Unix(0, 0)
	if ok, _ := l.allowAt("a", now); !ok {
		t.Fatal("key a first request should allow")
	}
	if ok, _ := l.allowAt("b", now); !ok {
		t.Fatal("key b must not be limited by key a's usage")
	}
	if ok, _ := l.allowAt("a", now); ok {
		t.Fatal("key a second request should deny")
	}
}

func TestPurgeDropsIdleRefilledKeys(t *testing.T) {
	l := New(1, 2)
	now := time.Unix(0, 0)
	l.allowAt("idle", now)   // partially drained, will refill
	l.allowAt("active", now) // drained then kept active below
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want 2", l.Len())
	}
	// 10 minutes later both are fully refilled; purge idle-> drops them.
	l.purgeAt(now.Add(20*time.Minute), 10*time.Minute)
	if l.Len() != 0 {
		t.Fatalf("Len after purge = %d, want 0 (both idle + refilled)", l.Len())
	}
}

func TestPurgeKeepsRecentlyActive(t *testing.T) {
	l := New(1, 2)
	now := time.Unix(0, 0)
	l.allowAt("k", now)
	// purge with a window larger than idle -> key stays.
	l.purgeAt(now.Add(time.Minute), 10*time.Minute)
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (recently active key kept)", l.Len())
	}
}
