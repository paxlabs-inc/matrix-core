// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"sync/atomic"
	"testing"
	"time"
)

// sleepReader returns data after a fixed per-Read delay, modelling a stream
// whose chunks arrive with a gap. It does not observe context: the watchdog
// firing is what the test asserts (in production onIdle cancels the request
// context, which is what actually unblocks a genuinely stalled read).
type sleepReader struct {
	delay time.Duration
	data  []byte
}

func (s *sleepReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	return copy(p, s.data), nil
}

// TestIdleTimeoutReaderFiresOnStall proves a Read that blocks longer than the
// idle budget trips the watchdog — the stalled-stream case.
func TestIdleTimeoutReaderFiresOnStall(t *testing.T) {
	var fired int32
	sr := &sleepReader{delay: 200 * time.Millisecond, data: []byte("x")}
	ir := newIdleTimeoutReader(sr, 25*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	defer ir.stop()

	buf := make([]byte, 8)
	_, _ = ir.Read(buf)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("watchdog did not fire on a stalled read")
	}
}

// TestIdleTimeoutReaderQuietOnProgress proves a stream that keeps producing
// chunks faster than the idle budget never trips — the long-but-healthy
// generation case that the old total http.Client.Timeout wrongly killed.
func TestIdleTimeoutReaderQuietOnProgress(t *testing.T) {
	var fired int32
	sr := &sleepReader{delay: 5 * time.Millisecond, data: []byte("chunk")}
	ir := newIdleTimeoutReader(sr, 100*time.Millisecond, func() { atomic.StoreInt32(&fired, 1) })
	defer ir.stop()

	buf := make([]byte, 8)
	// Total elapsed (~150ms over 30 reads) far exceeds the 100ms idle budget,
	// but no single gap does — so a total-request timeout would have failed
	// here while the idle watchdog stays quiet.
	for i := 0; i < 30; i++ {
		if _, err := ir.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatal("watchdog fired despite steady progress")
	}
}
