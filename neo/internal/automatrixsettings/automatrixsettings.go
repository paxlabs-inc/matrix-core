// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package automatrixsettings is the durable per-user Automatrix opt-in store —
// the authoritative home of the `automatrix_enabled` setting (default false,
// req 7.1) that governs whether Neo wakes to do proactive work. It sits beside
// the existing per-user daemon settings + onboarding profile and is read by the
// production AutomatrixGovernor on EVERY wake, so a toggle takes effect without
// a process restart (req 7.5): the engine treats this setting as authoritative.
//
// Alongside the opt-in it persists the small amount of bookkeeping the alarm
// lifecycle needs to be DID-scoped and reversible without a restart:
//   - alarm_id   — the id of the single AUTOMATRIX Chronos alarm this agent
//     created, so disable can cancel exactly its own alarm (req 8.1: an agent
//     only creates/cancels its own alarm) and re-enabling never creates a second;
//   - tasks_day / tasks_today — the per-day proactive-task counter (req 4.7),
//     which resets at UTC midnight.
//
// It is a pure SIDECAR mirroring neo/internal/automatrixlog: an in-memory state
// kept hot for the wake's live read, persisted to a single crash-atomic JSON
// file on the machine volume (tmp+rename) so it survives reload / suspend /
// redeploy. An empty dir yields an in-memory-only store (dev/CLI/tests still
// toggle correctly, just without disk durability). It never touches Neocortex,
// signs anything, or perturbs the plan/walk, and stores NO secrets, tokens, or
// keys — only the opt-in flag, the alarm id, and the daily counter.
package automatrixsettings

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

// settingsFile is the single per-daemon settings file. The daemon is per-user,
// so one file holds that user/agent's Automatrix opt-in state.
const settingsFile = "automatrix.settings.json"

// Store type and schema bound into the settings file's associated data, so a
// sealed file cannot be read across users.
const (
	storeSettings   = "neo.automatrix.settings"
	schemaSettings1 = "settings.v1"
)

// State is the durable Automatrix opt-in state. Its shape is the whole
// no-secrets guarantee: these are the only fields that exist.
type State struct {
	// Enabled is the per-user opt-in (default false). The engine treats it as
	// authoritative on every wake (req 7.1).
	Enabled bool `json:"enabled"`
	// AlarmID is the id of the single AUTOMATRIX Chronos alarm this agent
	// created (req 4.1 / 8.1). Empty when disabled / no alarm exists.
	AlarmID string `json:"alarm_id,omitempty"`
	// TasksDay is the UTC date (YYYY-MM-DD) the counter belongs to; a different
	// current date resets the count to zero (req 4.7).
	TasksDay string `json:"tasks_day,omitempty"`
	// TasksToday is the number of proactive tasks started on TasksDay.
	TasksToday int `json:"tasks_today,omitempty"`
	// Timezone is the user's IANA timezone for the daily proactive-work budget.
	Timezone string `json:"timezone,omitempty"`
}

// Store is the durable Automatrix opt-in store. State is held in memory for the
// wake's hot live read and mirrored to disk on every mutation; all access is
// serialised by mu.
type Store struct {
	dir string
	mu  sync.Mutex
	st  State

	// vault seals the settings file (whole-file AEAD, tmp+rename) when
	// encrypting; nil = plaintext dev/CLI. user is the DID bound into the
	// file's associated data.
	vault *vault.Session
	user  string
}

// Open builds a store rooted at dir. State is loaded lazily on the first
// operation (Load), so SetVault can be wired between Open and first use. An
// empty dir yields an in-memory-only store (a safe, durable-less fallback) so
// dev/CLI runs and tests still toggle the opt-in correctly.
func Open(dir string) *Store {
	s := &Store{dir: strings.TrimSpace(dir)}
	if s.dir != "" && s.vault == nil {
		// Best-effort eager load for the no-vault path (backward compatible);
		// the sealing path re-loads under SetVault via loadLocked.
		if b, err := os.ReadFile(s.path()); err == nil && !vault.IsVault(b) {
			var st State
			if json.Unmarshal(b, &st) == nil {
				s.st = st
			}
		}
	}
	return s
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store and (re)loads persisted state under it. Called once at engine
// assembly before any mutation; a nil session leaves the store on plaintext.
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vault = sess
	s.user = user
	s.loadLocked()
}

// ad reconstructs the settings file's associated data (this user's single
// settings file). Never stored, so a file moved between users fails auth.
func (s *Store) ad() vault.AD {
	return vault.AD{User: s.user, Store: storeSettings, Schema: schemaSettings1}
}

// loadLocked reads persisted state, decrypting a sealed file under the
// reconstructed AD (legacy plaintext parses as today). A wrong-key/tampered
// sealed file leaves the in-memory state at its zero value. Caller holds mu.
func (s *Store) loadLocked() {
	if s.dir == "" {
		return
	}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	if vault.IsVault(b) {
		uv := s.vault.UserVault()
		if uv == nil {
			return
		}
		plain, oerr := uv.OpenFile(s.ad(), b)
		if oerr != nil {
			return
		}
		b = plain
	}
	var st State
	if json.Unmarshal(b, &st) == nil {
		s.st = st
	}
}

// Persistent reports whether state is mirrored to disk (a non-empty directory).
func (s *Store) Persistent() bool { return s != nil && s.dir != "" }

// View returns a copy of the current state (safe to read without holding mu).
func (s *Store) View() State {
	if s == nil {
		return State{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// Enabled reports the authoritative per-user opt-in, re-read live on every
// wake (req 7.1). Default false.
func (s *Store) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Enabled
}

// SetEnabled records the opt-in flag and the alarm id together (one atomic
// mutation), so the live setting and the alarm bookkeeping never diverge.
// Disabling clears the alarm id. Persists immediately when durable.
func (s *Store) SetEnabled(enabled bool, alarmID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Enabled = enabled
	s.st.AlarmID = strings.TrimSpace(alarmID)
	if !enabled {
		s.st.AlarmID = ""
	}
	return s.persistLocked()
}

// AlarmID returns the id of this agent's AUTOMATRIX alarm, or "" when none
// exists. Used by the cancel path so an agent only ever cancels its own alarm
// (req 8.1).
func (s *Store) AlarmID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.AlarmID
}

// TasksToday returns the proactive-task count for the current UTC date,
// treating a stale date as zero (the daily reset, req 4.7). Pure read — it does
// not persist the reset; RecordTaskStarted writes the fresh date.
func (s *Store) TasksToday() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.TasksDay != today(s.st.Timezone) {
		return 0
	}
	return s.st.TasksToday
}

// RecordTaskStarted increments the per-day proactive-task counter, rolling the
// window over at UTC midnight (a new date resets the count before incrementing).
// Persists immediately when durable.
func (s *Store) RecordTaskStarted() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := today(s.st.Timezone)
	if s.st.TasksDay != d {
		s.st.TasksDay = d
		s.st.TasksToday = 0
	}
	s.st.TasksToday++
	return s.persistLocked()
}

// persistLocked writes the current state to disk crash-atomically (tmp+rename).
// A no-op for an in-memory-only store. Caller holds mu.
func (s *Store) persistLocked() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("automatrixsettings: mkdir %s: %w", s.dir, err)
	}
	b, err := json.Marshal(s.st)
	if err != nil {
		return fmt.Errorf("automatrixsettings: marshal: %w", err)
	}
	// Seal the whole file at rest (fail-closed when the vault is required);
	// nil session = legacy plaintext for dev/CLI. tmp+rename preserved below.
	b, err = s.vault.MaybeSealFile(s.ad(), b)
	if err != nil {
		return fmt.Errorf("automatrixsettings: seal: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("automatrixsettings: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("automatrixsettings: rename %s: %w", s.path(), err)
	}
	return nil
}

func (s *Store) path() string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, settingsFile)
}

// today is the current UTC date as YYYY-MM-DD (the per-day counter window key).
func today(timezone string) string {
	location := time.Local
	if strings.TrimSpace(timezone) != "" {
		if parsed, err := time.LoadLocation(timezone); err == nil {
			location = parsed
		}
	}
	return time.Now().In(location).Format("2006-01-02")
}

func (s *Store) SetTimezone(timezone string) error {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = time.Local.String()
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("automatrixsettings: invalid timezone %q: %w", timezone, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Timezone = timezone
	return s.persistLocked()
}

// Dir resolves Neo's Automatrix settings directory. An explicit override wins;
// else it derives from the Neocortex root's parent (matching the conversation /
// task / trace / automatrix stores, so it shares /data with them and survives
// suspend / redeploy). Returns "" when neither is available (in-memory only).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "automatrix")
	}
	return ""
}
