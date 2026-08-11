// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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
// It is a pure side-channel: it never touches Neocortex, signs anything, or
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

	"matrix/vault"
)

// Store type and schema bound into each task file's associated data, so a
// sealed record cannot be read across users or conversations.
const (
	storeTask   = "neo.task"
	schemaTask1 = "task.v1"
)

// Status is a task's lifecycle state.
type Status string

const (
	// KindBrief marks a task whose objective is an autonomous MORNING_BRIEF run
	// (ORACLE task 5.4). The normal boot reaper (Running) EXCLUDES these — a
	// brief must never resume on the full interactive/money surface — and the
	// dedicated brief-resume path (RunningKind) picks them up on the restricted
	// brief surface instead. An empty Kind is an ordinary interactive task.
	KindBrief = "brief"
)

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
	RunID     string    `json:"run_id"`         // the supervising run/intent id (the CAS token)
	Objective string    `json:"objective"`      // the original user request the task must fulfil
	Kind      string    `json:"kind,omitempty"` // "" = interactive task; KindBrief = autonomous morning brief
	Status    Status    `json:"status"`
	Attempt   int       `json:"attempt"`        // how many supervised attempts have run
	Note      string    `json:"note,omitempty"` // last checkpoint note (diagnostic)
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Store is Neo's durable task ledger. One mutex guards all access; a per-user
// daemon has at most a handful of conversations, so this is plenty fast.
type Store struct {
	mu  sync.Mutex
	dir string

	// vault seals each task file (whole-file AEAD, tmp+rename) when encrypting;
	// nil = plaintext dev/CLI. user is the DID bound into the file's associated
	// data so a record cannot be read across users.
	vault *vault.Session
	user  string
}

// Open builds a store rooted at dir. An empty dir yields a disabled store
// (every method is a safe no-op) so dev/CLI runs work unchanged.
func Open(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store. Called once at engine assembly before any write; a nil session
// leaves the store writing legacy plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = sess
	s.user = user
	s.mu.Unlock()
}

// ad reconstructs the associated data for a task file from where it lives (this
// user, this conversation) — never stored, so a file moved between users or
// conversations fails authentication.
func (s *Store) ad(convID string) vault.AD {
	// Bind to the sanitized id so the write side (original conv id) and the
	// Running() read side (id derived from the file name) reconstruct the same AD.
	return vault.AD{User: s.user, Store: storeTask, Stream: sanitize(convID), Schema: schemaTask1}
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
	// Sniff the header: a vault-sealed file decrypts under the reconstructed AD;
	// a legacy plaintext file parses as today (until migrated). A wrong-key /
	// tampered sealed file yields (zero, false) rather than leaking bytes.
	if vault.IsVault(data) {
		uv := s.vault.UserVault()
		if uv == nil {
			return Task{}, false
		}
		plain, oerr := uv.OpenFile(s.ad(convID), data)
		if oerr != nil {
			return Task{}, false
		}
		data = plain
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return Task{}, false
	}
	return t, true
}

// writeLocked persists t atomically (tmp + rename). Caller MUST hold s.mu.
func (s *Store) writeLocked(t Task) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: mkdir %s: %v\n", s.dir, err)
		return
	}
	data, err := json.Marshal(t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: marshal %s: %v\n", t.ConvID, err)
		return
	}
	// Seal the whole file at rest (fail-closed when the vault is required);
	// nil session = legacy plaintext for dev/CLI. tmp+rename preserved below.
	data, err = s.vault.MaybeSealFile(s.ad(t.ConvID), data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/task: seal %s: %v\n", t.ConvID, err)
		return
	}
	path := s.pathLocked(t.ConvID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
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
	s.BeginKind(convID, runID, objective, "")
}

// BeginKind is Begin with an explicit task kind ("" = interactive; KindBrief =
// autonomous morning brief). The kind steers the boot reaper: an interactive
// task resumes on the full surface (Running), a brief on the restricted brief
// surface (RunningKind) — a brief must never resume on the money surface.
func (s *Store) BeginKind(convID, runID, objective, kind string) {
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
		Kind:      strings.TrimSpace(kind),
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

// Running returns every INTERACTIVE conversation whose task is still running —
// the set the normal boot reaper must resume on the full surface. Brief tasks
// (KindBrief) are deliberately EXCLUDED: they must resume only on the
// restricted brief surface via RunningKind, never on the money surface.
// Best-effort: unreadable files are skipped.
func (s *Store) Running() []Task {
	return s.running(func(t Task) bool { return t.Kind == "" })
}

// RunningKind returns every still-running task of the given kind — the dedicated
// resume set for a specialized surface (e.g. KindBrief for the restricted
// morning-brief runner). Best-effort: unreadable files are skipped.
func (s *Store) RunningKind(kind string) []Task {
	kind = strings.TrimSpace(kind)
	return s.running(func(t Task) bool { return t.Kind == kind })
}

// running collects every still-running task whose record satisfies keep.
func (s *Store) running(keep func(Task) bool) []Task {
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
		if keep != nil && !keep(t) {
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
// derived from the Neocortex root's parent (matching the conversation store, so
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
