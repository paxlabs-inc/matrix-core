// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"io"
	"time"
)

// idleTimeoutReader wraps a streaming response body and fires onIdle if a
// single Read makes no progress within idle. It is the stream-aware
// replacement for a blunt http.Client.Timeout: a long but healthy generation
// streams tokens continuously and never trips, while a provider that stalls
// mid-stream (or never produces a first token) is cancelled promptly. onIdle is
// expected to cancel the request context, which unblocks the in-flight Read.
type idleTimeoutReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

// newIdleTimeoutReader arms a watchdog around r. onIdle runs (once per stall)
// if any Read blocks longer than idle without returning.
func newIdleTimeoutReader(r io.Reader, idle time.Duration, onIdle func()) *idleTimeoutReader {
	if idle <= 0 {
		idle = 120 * time.Second
	}
	t := time.AfterFunc(idle, onIdle)
	t.Stop()
	return &idleTimeoutReader{r: r, idle: idle, timer: t}
}

// Read arms the watchdog for the duration of the underlying Read and disarms it
// as soon as the Read returns (data or error), so the deadline governs silence
// on the wire, never the caller's processing time between reads.
func (ir *idleTimeoutReader) Read(p []byte) (int, error) {
	ir.timer.Reset(ir.idle)
	n, err := ir.r.Read(p)
	ir.timer.Stop()
	return n, err
}

// stop disarms the watchdog; call it once the stream is fully consumed.
func (ir *idleTimeoutReader) stop() { ir.timer.Stop() }
