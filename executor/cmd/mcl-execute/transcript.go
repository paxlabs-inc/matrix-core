// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// transcript writes JSONL events to a file + mirrors to stderr for live
// human observation. Implements runtime.EventSink so it can plug
// straight into the walker.
//
// JSONL shape mirrors cmd/mcl-e2e/Transcript:
//
//	{"seq": N, "ts": "...", "phase": "compile|synth|walk|attest|correct",
//	 "type": "event_name", "fields": {...}}
//
// Field-mode is map[string]interface{} so callers can stuff arbitrary
// structured payloads. Sequence is atomic so concurrent walker phases
// (parallel-node dispatch) don't race.
type transcript struct {
	mu  sync.Mutex
	enc *json.Encoder
	out io.WriteCloser

	mirror io.Writer
	seq    uint64

	// Optional live tap. When set, every Event is also published to the
	// SSE broker for live web-client streaming. nil in CLI mode.
	broker *sseBroker

	// Optional per-route latency accumulator (Session 31d · P4).
	// When non-nil, routed-LLM call sites push (slot, kind, model,
	// streamed, ms, err) observations through it; the accumulator
	// then surfaces aggregates via router.histogram events + the
	// daemon's /metrics endpoint. nil in tests + non-daemon CLI
	// flows where the per-event audit fields are sufficient.
	metrics *routerMetrics

	// Optional per-message intent scope. When non-empty, Event auto-
	// stamps fields["intent_id"] on every emitted record (preserving
	// any caller-supplied value). This closes the SSE-filter drop bug:
	// the broker's per-subscriber sseFilter filters by
	// fields["intent_id"] (daemon_sse.go:54) but most call sites in the
	// pipeline (walk.start, lifecycle.transition, step.text, gate.*,
	// envelope.signed, synth.* …) don't redundantly include intent_id
	// in their payload. Without this stamp, those events silently
	// dropped for every browser subscribed with ?intent_id=…, leaving
	// the live-transcript pane stuck on "Connected — waiting for
	// activity" even after the run had completed. Idempotent and
	// caller-safe; legacy CLI flows that never set IntentID retain
	// their existing emission shape exactly.
	intentID string
}

// AttachBroker installs an SSE broker so every subsequent Event() call
// is mirrored to live subscribers in addition to the JSONL file. Idempotent.
func (t *transcript) AttachBroker(b *sseBroker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.broker = b
}

// AttachMetrics installs a routerMetrics accumulator so subsequent
// routed-LLM call sites can record latency observations. Idempotent;
// replacing an existing accumulator silently drops the prior
// counters (caller is responsible for flushing first if needed).
func (t *transcript) AttachMetrics(m *routerMetrics) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.metrics = m
}

// SetIntentID binds this transcript to a single intent so every
// subsequent Event auto-stamps fields["intent_id"]=id when the caller
// did not already include it. See the intentID field comment for the
// motivation (SSE filter drop fix). Idempotent. Empty id clears the
// scope (CLI / multi-intent test transcripts that should NOT auto-
// stamp call SetIntentID("") explicitly).
func (t *transcript) SetIntentID(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.intentID = id
}

// Metrics returns the attached routerMetrics accumulator (or nil).
// Read under t.mu so concurrent AttachMetrics calls don't race.
func (t *transcript) Metrics() *routerMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.metrics
}

// openTranscript opens path for append; "" or "-" writes only to stderr.
func openTranscript(path string) (*transcript, error) {
	t := &transcript{mirror: os.Stderr}
	if path == "" || path == "-" {
		t.enc = json.NewEncoder(os.Stderr)
		return t, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	t.out = f
	t.enc = json.NewEncoder(f)
	return t, nil
}

// Close flushes and closes the file if any. Safe to call on stderr-only
// transcripts.
func (t *transcript) Close() error {
	if t.out != nil {
		return t.out.Close()
	}
	return nil
}

// Event implements runtime.EventSink.
func (t *transcript) Event(eventType, phase string, fields map[string]interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Auto-stamp the per-transcript intent_id on every event when
	// the caller did not already include one. Stamping in-place on the
	// caller's map is intentional: Event is the terminal sink, the map
	// is single-use per call site, and stamping here means the JSONL
	// record + the broker copy + (any future audit hook) all see the
	// same intent_id without each call site having to remember to
	// duplicate it. See intentID field for the SSE-filter motivation.
	if t.intentID != "" {
		if fields == nil {
			fields = map[string]interface{}{}
		}
		if got, _ := fields["intent_id"].(string); got == "" {
			fields["intent_id"] = t.intentID
		}
	}
	rec := struct {
		Seq    uint64                 `json:"seq"`
		TS     string                 `json:"ts"`
		Phase  string                 `json:"phase"`
		Type   string                 `json:"type"`
		Fields map[string]interface{} `json:"fields,omitempty"`
	}{
		Seq:    atomic.AddUint64(&t.seq, 1),
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Phase:  phase,
		Type:   eventType,
		Fields: fields,
	}
	if err := t.enc.Encode(rec); err != nil {
		fmt.Fprintf(os.Stderr, "transcript: encode: %v\n", err)
	}
	// Mirror a one-liner to stderr for live tail; only when t.out != stderr.
	if t.out != nil {
		fmt.Fprintf(t.mirror, "[%s] %s.%s %v\n", rec.TS, phase, eventType, fields)
	}
	// Tap to SSE broker for live web clients. Defensive copy of fields
	// so subscribers can't mutate the upstream caller's map. Non-blocking
	// per broker.Publish semantics.
	if t.broker != nil {
		var fcopy map[string]interface{}
		if len(fields) > 0 {
			fcopy = make(map[string]interface{}, len(fields))
			for k, v := range fields {
				fcopy[k] = v
			}
		}
		t.broker.Publish(sseEvent{
			Seq:    rec.Seq,
			TS:     rec.TS,
			Phase:  phase,
			Type:   eventType,
			Fields: fcopy,
		})
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
