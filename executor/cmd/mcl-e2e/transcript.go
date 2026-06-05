// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Transcript writes one JSON object per line to a file (and optionally stderr
// for live tailing). Every event includes a monotonic seq + wall-clock + the
// caller-supplied phase tag so post-run greps can slice by phase.
type Transcript struct {
	mu     sync.Mutex
	file   *os.File
	enc    *json.Encoder
	mirror bool
	seq    uint64
	run    string
}

// NewTranscript opens path for writing; truncates any existing file.
// run is a short tag (e.g. "A", "B", "C") embedded in every event so a
// merged transcript can interleave runs without ambiguity.
// mirrorStderr controls whether each event is also printed as a one-line
// summary to stderr.
func NewTranscript(path, run string, mirrorStderr bool) (*Transcript, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &Transcript{
		file:   f,
		enc:    enc,
		mirror: mirrorStderr,
		run:    run,
	}, nil
}

// Event writes a single typed event to the transcript.
// fields are merged into the envelope; "type", "seq", "at", "run" are
// reserved.
func (t *Transcript) Event(eventType, phase string, fields map[string]interface{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.seq++
	rec := map[string]interface{}{
		"seq":   t.seq,
		"at":    time.Now().UTC().Format(time.RFC3339Nano),
		"run":   t.run,
		"phase": phase,
		"type":  eventType,
	}
	for k, v := range fields {
		if _, reserved := rec[k]; reserved {
			continue
		}
		rec[k] = v
	}
	if err := t.enc.Encode(rec); err != nil {
		fmt.Fprintf(os.Stderr, "transcript: encode failed: %v\n", err)
	}
	if t.mirror {
		fmt.Fprintf(os.Stderr, "[%s/%s/%s] %s\n", t.run, phase, eventType, summarize(fields))
	}
}

// Close flushes and closes the underlying file.
func (t *Transcript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.file.Close()
}

func summarize(fields map[string]interface{}) string {
	if fields == nil {
		return ""
	}
	// Compact one-liner of selected hint fields.
	out := ""
	for _, k := range []string{"label", "tool", "verb", "uri", "state", "kind", "ok", "error", "overall_root", "intent_hash", "plan_hash", "ms", "phase_label", "ext"} {
		if v, ok := fields[k]; ok {
			out += fmt.Sprintf("%s=%v ", k, truncate(fmt.Sprint(v), 80))
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
