// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"
	"time"
)

// TestMustDeliverBlocksBriefly proves the two-tier delivery: with a
// subscriber's buffer full, a streaming-grade event drops instantly while a
// must-deliver terminal waits (bounded) for the consumer to free a slot.
func TestMustDeliverBlocksBriefly(t *testing.T) {
	b := newBroker()
	b.ensure("run-1")
	_, ch, cancel := b.subscribe("run-1", 0)
	defer cancel()

	// Fill the subscriber's buffer without draining.
	for i := 0; i < subBuffer; i++ {
		b.publish("run-1", "run.activity", "cody", map[string]interface{}{"i": i})
	}

	// A lossy event over a full buffer drops silently (publish returns fast).
	start := time.Now()
	b.publish("run-1", "run.activity", "cody", map[string]interface{}{"dropped": true})
	if elapsed := time.Since(start); elapsed > mustDeliverWait/2 {
		t.Fatalf("lossy publish blocked %v; must be instant", elapsed)
	}

	// A consumer frees one slot shortly after the must-deliver publish starts.
	go func() {
		time.Sleep(10 * time.Millisecond)
		<-ch
	}()
	done := b.publish("run-1", "plan.completed", "cody", map[string]interface{}{"done": []string{"t1"}})

	// Drain everything: the terminal must be there; the dropped tick must not.
	sawTerminal, sawDropped := false, false
	for {
		select {
		case ev := <-ch:
			if ev.Type == "plan.completed" && ev.Seq == done.Seq {
				sawTerminal = true
			}
			if ev.Fields != nil {
				if v, ok := ev.Fields["dropped"].(bool); ok && v {
					sawDropped = true
				}
			}
			continue
		default:
		}
		break
	}
	if !sawTerminal {
		t.Fatal("must-deliver terminal was lost to a full buffer")
	}
	if sawDropped {
		t.Fatal("lossy event survived a full buffer (should have dropped)")
	}
}
