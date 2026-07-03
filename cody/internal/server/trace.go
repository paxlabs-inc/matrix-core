// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// traceWorkspaceTypes whitelists the event families persisted to the durable
// trace — the workspace timeline the client rebuilds on reopen. Transients
// (deltas) and terminals are excluded; final assistant messages live in the
// conversation record, not the trace.
var traceWorkspaceTypes = map[string]bool{
	"plan.created":   true,
	"task.started":   true,
	"sheet.authored": true,
	"task.turnin":    true,
	"task.accepted":  true,
	"task.rejected":  true,
	"task.failed":    true,
	"plan.completed": true,
	// Answer/steer channel (req 12.3): the needs_input stall, its resolution,
	// and folded steers are durable so the client rebuilds them on reopen.
	"run.needs_input": true,
	"run.answered":    true,
	"steer.folded":    true,
	// Decision gates (req 3.4, 8, 9): the authored Stack and Design Language
	// Records and their resolution are durable so the client rebuilds the
	// decision cards on reopen.
	"decision.stack":    true,
	"decision.design":   true,
	"decision.resolved": true,
	// Spec ingestion (req 11): the plan was adopted from a spec document; the
	// provenance is durable so the client rebuilds the adoption banner on reopen.
	"plan.adopted": true,
	// Undo (req 14): named snapshots and restores are durable trace events so the
	// client rebuilds the checkpoint history on reopen.
	"snapshot.created":  true,
	"snapshot.restored": true,
}

// defaultTraceRetain caps retained events per run.
const defaultTraceRetain = 2000

// traceStore persists per-run event timelines as append-only JSONL — one file
// per run under dir. Writes are mutex-serialized (plan-level event volume is
// a few events per task, not per token); a torn final line from a crash is
// skipped on read.
type traceStore struct {
	dir    string
	retain int
	mu     sync.Mutex
}

func newTraceStore(dir string) (*traceStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &traceStore{dir: dir, retain: defaultTraceRetain}, nil
}

func (s *traceStore) path(runID string) string {
	return filepath.Join(s.dir, sanitizeRunID(runID)+".trace.jsonl")
}

// record appends one event to the run's timeline.
func (s *traceStore) record(runID string, ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(runID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// load returns the newest retain events of a run, oldest first. A missing
// trace is an empty timeline, not an error.
func (s *traceStore) load(runID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path(runID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) > s.retain {
		out = out[len(out)-s.retain:]
	}
	return out, nil
}

func sanitizeRunID(id string) string {
	out := make([]rune, 0, len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
