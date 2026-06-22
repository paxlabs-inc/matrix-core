// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package task is Neo's durable task-supervision ledger.
//
// A dispatched user task must run to completion no matter what — the user
// closing the app, the model erroring, a tool failing, the loop ending early,
// or the daemon itself restarting / being suspended by Fly. The in-memory
// supervisor (internal/server) handles failures WITHIN a live process; this
// store is what makes a task survive the PROCESS dying: it records, on the
// machine volume, that a given conversation has an unfinished objective, so a
// boot-time reaper can pick it back up and finish it.
//
// One record per conversation_id (turns in a conversation are serialized and a
// new message supersedes the previous task — barge-in — so a conversation has
// at most one active task). Writes are compare-and-set on the run id so a
// superseded older run can never clobber the record of the run that replaced
// it. Persistence is atomic (tmp + rename) and best-effort: an IO error is
// logged, never fatal, and a disabled store (empty dir) is a safe no-op so
// dev/CLI runs are unchanged.
//
// It is a pure side-channel: it never touches cortex, signs anything, or
// perturbs replay — task continuity is independent storage, exactly like the
// conversation store it mirrors.
package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Status is a task's lifecycle state.
type Status string

const (
	// StatusRunning marks a task that has been dispatched and is not yet
	// finished. The reaper resumes exactly these after a restart.
	StatusRunning Status = "running"
	// StatusDone marks a task the completion gate accepted.
	StatusDone Status = "done"
	// StatusCeiling marks a task that hit the supervisor's hard ceiling
	// (wall-clock or max respawns) and was delivered as an honest partial.
	StatusCeiling Status = "ceiling"
	// StatusInterrupted marks a task the user explicitly stopped or superseded
	// with a new message. The reaper must NOT resume these.
	StatusInterrupted Status = "interrupted"
)

// Task is the persisted supervision record for one conversation's active (or
// last) task.
type Task struct {
	ConvID    string    `json:"conversation_id"`
	RunID     string    `json:"run_id"`   // the supervising run/intent id (the CAS token)
	Objective string    `json:"objective"` // the original user request the task must fulfil
	Status    Status    `json:"status"`
	Attempt   int        `json:"attempt"`        // how many supervised attempts have run
	Note      string    `json:"note,omitempty"` // last checkpoint note (diagnostic)
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Store is Neo's durable task ledger. One mutex guards all access; a per-user
// daemon has at most a handful of conversations, so this is plenty fast.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// Enabled reports whether persistence is on (a non-empty directory).
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

func (s *Store) pathLocked(convID string) string {
	if !s.Enabled() || convID == "" {
		return ""
	}
	return filepath.Join(s.dir, sanitize(convID)+".json")
}

// sanitize keeps a conversation id safe as a filename (it is server-minted as
// conv_<hex>, but guard against path separators regardless).
func sanitize(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "\\", "_")
	return strings.TrimSpace(id)
}

// loadLocked reads a record; a missing/corrupt file yields (zero, false).
// Caller MUST hold s.mu.
func (s *Store) loadLocked(convID string) (Task, bool) {
	path := s.pathLocked(convID)
	if path == "" {
		return Task{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Task{}, false
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return Task{}, false
	}
	return t, true
}

// writeLocked persists t atomically (tmp + rename). Caller MUST hold s.mu.
func (s *Store) writeLocked(t Task) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: mkdir %s: %v\n", s.dir, err)
		return
	}
	data, err := json.Marshal(t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: marshal %s: %v\n", t.ConvID, err)
		return
	}
	path := s.pathLocked(t.ConvID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: rename %s: %v\n", path, err)
		_ = os.Remove(tmp)
	}
}

// Begin records (or replaces) a conversation's task as running, owned by runID.
// A new dispatch for a conversation supersedes any previous task — the new
// runID becomes the CAS token, so a superseded older run's later Finish is a
// no-op. Created is preserved across a same-conversation supersession only if
// the objective is unchanged (a genuine resume); otherwise it resets.
func (s *Store) Begin(convID, runID, objective string) {
	if !s.Enabled() || convID == "" || runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	created := now
	if prev, ok := s.loadLocked(convID); ok && prev.Objective == strings.TrimSpace(objective) {
		created = prev.Created
	}
	s.writeLocked(Task{
		ConvID:    convID,
		RunID:     runID,
		Objective: strings.TrimSpace(objective),
		Status:    StatusRunning,
		Attempt:   1,
		Created:   created,
		Updated:   now,
	})
}

// Checkpoint records progress for runID's task (the attempt counter + a short
// note). Compare-and-set on runID: a superseded run's checkpoint is ignored.
func (s *Store) Checkpoint(convID, runID string, attempt int, note string) {
	if !s.Enabled() || convID == "" || runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.loadLocked(convID)
	if !ok || t.RunID != runID {
		return
	}
	t.Attempt = attempt
	t.Note = strings.TrimSpace(note)
	t.Updated = time.Now().UTC()
	s.writeLocked(t)
}

// Finish marks runID's task terminal. Compare-and-set on runID: a superseded
// older run can never clobber the record of the run that replaced it.
func (s *Store) Finish(convID, runID string, status Status) {
	if !s.Enabled() || convID == "" || runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.loadLocked(convID)
	if !ok || t.RunID != runID {
		return
	}
	t.Status = status
	t.Updated = time.Now().UTC()
	s.writeLocked(t)
}

// Running returns every conversation whose task is still running — the set the
// boot reaper must resume. Best-effort: unreadable files are skipped.
func (s *Store) Running() []Task {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Task
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		convID := strings.TrimSuffix(name, ".json")
		t, ok := s.loadLocked(convID)
		if !ok || t.Status != StatusRunning || strings.TrimSpace(t.Objective) == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Get returns a conversation's task record, if any.
func (s *Store) Get(convID string) (Task, bool) {
	if !s.Enabled() || convID == "" {
		return Task{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(convID)
}

// Dir resolves Neo's task directory. An explicit override wins; else it is
// derived from the cortex root's parent (matching the conversation store, so
// tasks live beside history under /data in prod and survive suspend/redeploy).
// Returns "" when neither is available (persistence disabled).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "tasks")
	}
	return ""
}
