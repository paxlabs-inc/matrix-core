// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrix/vault"
)

// Store type and schema bound into each transcript record's associated data, so
// a sealed line cannot be replayed across users, intents, or positions. The
// reader (readTranscriptLines) reconstructs the SAME AD from where the record
// lives, so a record moved between users/intents/positions fails authentication.
const (
	storeTranscript    = "executor.transcript"
	schemaTranscriptV1 = "event.v1"
)

// transcript writes JSONL events to a file + mirrors redacted structured logs
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

	mirror      io.Writer
	errorMirror io.Writer
	seq         uint64

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

	// vault seals each JSONL record line when encrypting; nil = plaintext
	// (dev/CLI). user is the DID bound into each record's associated data.
	// encSeq is the on-disk line position (seeded from any existing lines at
	// open) so each sealed record is bound to its position and appends stay
	// O(1). Only file-backed transcripts seal; the stderr mirror is untouched.
	vault  *vault.Session
	user   string
	encSeq uint64
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the transcript. Called once right after openTranscript (before any Event) so
// every emitted line is sealed; a nil session leaves the file writing legacy
// plaintext (dev/CLI).
func (t *transcript) SetVault(sess *vault.Session, user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.vault = sess
	t.user = user
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

// openTranscript opens path for append. Blank keeps only the redacted live log;
// "-" emits the full JSONL stream to stdout for explicit CLI use.
func openTranscript(path string) (*transcript, error) {
	t := &transcript{mirror: os.Stdout, errorMirror: os.Stderr}
	if path == "" {
		return t, nil
	}
	if path == "-" {
		t.enc = json.NewEncoder(os.Stdout)
		return t, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	t.out = f
	t.enc = json.NewEncoder(f)
	// Seed the on-disk line position from any existing lines so records
	// appended after a restart stay bound to their true position.
	t.encSeq = countTranscriptLines(path)
	return t, nil
}

// countTranscriptLines counts non-empty lines in a JSONL transcript (0 when
// absent) so the sealed-record position counter resumes correctly on reopen.
func countTranscriptLines(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var n uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		n++
	}
	return n
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
	if t.out != nil {
		// File-backed transcript: seal each record as one newline-free line
		// (sealed base64, or legacy plaintext JSON when no vault), preserving
		// O_APPEND single-line crash-atomicity. The AD binds the record to its
		// user, store, intent, and position so a line moved between users,
		// intents, or positions fails authentication.
		if b, err := json.Marshal(rec); err != nil {
			fmt.Fprintf(os.Stderr, "transcript: marshal: %v\n", err)
		} else {
			ad := vault.AD{User: t.user, Store: storeTranscript, Stream: t.intentID, Seq: t.encSeq, Schema: schemaTranscriptV1}
			line, err := t.vault.EncodeLine(ad, b)
			if err != nil {
				fmt.Fprintf(os.Stderr, "transcript: seal: %v\n", err)
			} else {
				line = append(line, '\n')
				if _, err := t.out.Write(line); err != nil {
					fmt.Fprintf(os.Stderr, "transcript: write: %v\n", err)
				} else {
					t.encSeq++
				}
			}
		}
	} else if t.enc != nil {
		if err := t.enc.Encode(rec); err != nil {
			fmt.Fprintf(os.Stderr, "transcript: encode: %v\n", err)
		}
	}
	// A file-backed or daemon transcript gets one redacted structured live log.
	// Explicit path="-" is already the caller-requested full JSONL stream.
	if t.enc == nil || t.out != nil {
		t.logEvent(rec.TS, phase, eventType, fields)
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

func (t *transcript) logEvent(ts, phase, eventType string, fields map[string]interface{}) {
	row := map[string]interface{}{
		"ts":       ts,
		"severity": "info",
		"event":    phase + "." + eventType,
	}
	for _, key := range []string{"intent_id", "conversation_id", "request_id", "tenant_correlation", "method", "path", "status", "duration_ms", "outcome", "terminal_outcome"} {
		if value, ok := fields[key]; ok {
			row[key] = value
		}
	}
	failure := strings.Contains(strings.ToLower(eventType), "fail") || strings.Contains(strings.ToLower(eventType), "error")
	if status, ok := fields["status"].(int); ok && status >= 500 {
		failure = true
	}
	if failure {
		row["severity"] = "error"
	}
	w := t.mirror
	if failure && t.errorMirror != nil {
		w = t.errorMirror
	}
	if w == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(row); err != nil && t.errorMirror != nil {
		fmt.Fprintf(t.errorMirror, "transcript: log encode: %v\n", err)
	}
}

// transcriptAD reconstructs the associated data for a transcript record from
// where it lives (this user, the intent's file, the line position). It is never
// stored, so a record moved between users, intents, or positions fails auth.
func (d *daemonState) transcriptAD(intentID string, seq uint64) vault.AD {
	return vault.AD{User: d.vaultUser, Store: storeTranscript, Stream: intentID, Seq: seq, Schema: schemaTranscriptV1}
}

// readTranscriptLines opens an intent's transcript file and returns each
// record as plaintext JSON, decrypting sealed lines under the reconstructed AD
// and passing legacy plaintext lines through unchanged (so a store mid-migration
// reads both shapes). A wrong-key, tampered, or crash-truncated line is skipped;
// the position counter still advances so surviving records stay position-bound.
// The os.Open error (incl. os.IsNotExist) is returned to the caller so the HTTP
// handlers keep their 404/500 behavior.
func (d *daemonState) readTranscriptLines(intentID string) ([][]byte, error) {
	path := filepath.Join(d.transcriptsDir, intentID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var seq uint64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		plain, derr := d.vault.DecodeLine(d.transcriptAD(intentID, seq), append([]byte(nil), line...))
		seq++
		if derr != nil {
			continue
		}
		out = append(out, append([]byte(nil), plain...))
	}
	return out, sc.Err()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
