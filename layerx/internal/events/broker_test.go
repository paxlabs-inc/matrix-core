package events

import (
	"encoding/json"
	"testing"
)

func TestBrokerFanOut(t *testing.T) {
	b := NewBroker(8)
	ch1, cancel1 := b.Subscribe()
	defer cancel1()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("Subscribers = %d, want 2", got)
	}

	ev := Event{Type: TypeTransfer, Seq: 7, Data: json.RawMessage(`{"seq":7}`)}
	b.Publish(ev)

	for i, ch := range []<-chan Event{ch1, ch2} {
		got := <-ch
		if got.Seq != 7 || got.Type != TypeTransfer {
			t.Fatalf("sub %d got %+v, want seq 7 transfer", i, got)
		}
	}
}

func TestBrokerCancelClosesChannel(t *testing.T) {
	b := NewBroker(4)
	ch, cancel := b.Subscribe()
	cancel()
	if _, open := <-ch; open {
		t.Fatal("channel should be closed after cancel")
	}
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("Subscribers = %d, want 0 after cancel", got)
	}
	// Cancel is idempotent (no double-close panic).
	cancel()
}

// A subscriber that never drains is dropped (channel closed) once its buffer
// fills, rather than blocking the publisher or silently losing events.
func TestBrokerDropsSlowConsumer(t *testing.T) {
	const buf = 4
	b := NewBroker(buf)
	ch, cancel := b.Subscribe()
	defer cancel()

	for i := 0; i < buf+5; i++ {
		b.Publish(Event{Type: TypeTransfer, Seq: int64(i + 1)})
	}

	// Drain whatever buffered, then expect a closed channel (dropped).
	closed := false
	for range make([]struct{}, buf+5) {
		if _, open := <-ch; !open {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("slow consumer should have been dropped (channel closed)")
	}
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("Subscribers = %d, want 0 after drop", got)
	}
}
