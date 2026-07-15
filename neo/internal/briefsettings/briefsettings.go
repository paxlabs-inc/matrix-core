// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

// Package briefsettings is the durable per-user schedule state for the ORACLE
// morning brief (req 14.1) — a pure SIDECAR mirroring neo/internal/
// automatrixsettings. It holds the operational schedule (enabled, timezone,
// local delivery time, selected days, brief length, sections, paused, the single
// MORNING_BRIEF alarm id, the destination conversation, and the last-delivered
// local date) SEPARATE from the personalization profile (which lives as a cortex
// record; req 13/14.1). The brief governor re-reads it live on every wake, so a
// toggle takes effect without a process restart (req 14.3).
//
// Like automatrixsettings it keeps state hot in memory, mirrors it to a single
// crash-atomic JSON file on the machine volume (tmp+rename) sealed at rest under
// the per-user vault, carries a legacy plaintext file until migrated, and stores
// NO secrets/keys — only the schedule and the alarm id. An empty dir yields an
// in-memory-only store (dev/CLI/tests still toggle correctly).
package briefsettings

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

const settingsFile = "brief.settings.json"

const (
	storeSettings   = "neo.brief.settings"
	schemaSettings1 = "brief.settings.v1"
)

// State is the durable morning-brief schedule state (req 14.1). Default zero
// value is the OFF state (Enabled=false), so an absent file means the brief is
// off — the default (req 11.1).
type State struct {
	// Enabled is the per-user brief opt-in (default false). Authoritative,
	// re-read live on every wake (req 14.3).
	Enabled bool `json:"enabled"`
	// Timezone is the IANA timezone the delivery time is evaluated in.
	Timezone string `json:"timezone,omitempty"`
	// DeliveryTime is the local delivery time "HH:MM" (24h).
	DeliveryTime string `json:"delivery_time,omitempty"`
	// Days restricts delivery to these weekdays (0=Sunday..6=Saturday).
	// Empty = every day.
	Days []int `json:"days,omitempty"`
	// Length is the brief length preference (short|standard|deep).
	Length string `json:"length,omitempty"`
	// Sections selects which content sections appear in the brief.
	Sections []string `json:"sections,omitempty"`
	// Paused skips delivery without disabling (keeps the alarm; the wake
	// handler skips while paused, req 14.2).
	Paused bool `json:"paused,omitempty"`
	// AlarmID is the id of the single MORNING_BRIEF Chronos alarm (req 14.2).
	// Empty when disabled / no alarm exists.
	AlarmID string `json:"alarm_id,omitempty"`
	// ConversationID is the destination conversation the brief is delivered
	// into.
	ConversationID string `json:"conversation_id,omitempty"`
	// LastDeliveredDate is the last LOCAL date (YYYY-MM-DD) a brief was
	// delivered, so the handler never delivers twice for one local date
	// (req 14.3).
	LastDeliveredDate string `json:"last_delivered_date,omitempty"`
}

// Store is the durable brief-settings store. State is held in memory for the
// wake's hot live read and mirrored to disk on every mutation; access is
// serialised by mu.
type Store struct {
	dir string
	mu  sync.Mutex
	st  State

	vault *vault.Session
	user  string
}

// Open builds a store rooted at dir. State is loaded lazily on first use so
// SetVault can be wired between Open and first use. An empty dir yields an
// in-memory-only store.
func Open(dir string) *Store {
	s := &Store{dir: strings.TrimSpace(dir)}
	if s.dir != "" && s.vault == nil {
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
// the store and (re)loads persisted state under it. A nil session leaves the
// store on plaintext.
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

// Persistent reports whether state is mirrored to disk.
func (s *Store) Persistent() bool { return s != nil && s.dir != "" }

// View returns a copy of the current state.
func (s *Store) View() State {
	if s == nil {
		return State{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.clone()
}

// Enabled reports the authoritative per-user opt-in, re-read live (req 14.3).
func (s *Store) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Enabled
}

// Paused reports whether delivery is currently paused.
func (s *Store) Paused() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Paused
}

// AlarmID returns the id of this agent's MORNING_BRIEF alarm, or "".
func (s *Store) AlarmID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.AlarmID
}

// Schedule is the mutable schedule shape the control surface writes.
type Schedule struct {
	Timezone       string
	DeliveryTime   string
	Days           []int
	Length         string
	Sections       []string
	ConversationID string
}

// SetSchedule persists the schedule fields (tz/time/days/length/sections/
// destination) without touching Enabled/Paused/AlarmID/LastDelivered.
func (s *Store) SetSchedule(sc Schedule) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Timezone = sc.Timezone
	s.st.DeliveryTime = sc.DeliveryTime
	s.st.Days = append([]int(nil), sc.Days...)
	s.st.Length = sc.Length
	s.st.Sections = append([]string(nil), sc.Sections...)
	if strings.TrimSpace(sc.ConversationID) != "" {
		s.st.ConversationID = sc.ConversationID
	}
	return s.persistLocked()
}

// SetEnabled records the opt-in flag and the alarm id together (one atomic
// mutation). Disabling clears the alarm id. Persists immediately when durable.
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
		s.st.Paused = false
	}
	return s.persistLocked()
}

// SetAlarmID records the alarm id (used when a reschedule recreates the alarm).
func (s *Store) SetAlarmID(alarmID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.AlarmID = strings.TrimSpace(alarmID)
	return s.persistLocked()
}

// SetPaused records the paused flag (the alarm is retained; the wake handler
// skips while paused, req 14.2).
func (s *Store) SetPaused(paused bool) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Paused = paused
	return s.persistLocked()
}

// MarkDelivered records the local date a brief was delivered, so the handler
// never delivers twice for one local date (req 14.3).
func (s *Store) MarkDelivered(localDate string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastDeliveredDate = strings.TrimSpace(localDate)
	return s.persistLocked()
}

// AlreadyDelivered reports whether a brief was already delivered for localDate.
func (s *Store) AlreadyDelivered(localDate string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.LastDeliveredDate != "" && s.st.LastDeliveredDate == strings.TrimSpace(localDate)
}

func (s *Store) persistLocked() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("briefsettings: mkdir %s: %w", s.dir, err)
	}
	b, err := json.Marshal(s.st)
	if err != nil {
		return fmt.Errorf("briefsettings: marshal: %w", err)
	}
	b, err = s.vault.MaybeSealFile(s.ad(), b)
	if err != nil {
		return fmt.Errorf("briefsettings: seal: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("briefsettings: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("briefsettings: rename %s: %w", s.path(), err)
	}
	return nil
}

func (s *Store) path() string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, settingsFile)
}

func (st State) clone() State {
	out := st
	out.Days = append([]int(nil), st.Days...)
	out.Sections = append([]string(nil), st.Sections...)
	return out
}

// today is the current UTC date as YYYY-MM-DD.
func today() string { return time.Now().UTC().Format("2006-01-02") }

// Dir resolves Neo's brief-settings directory. An explicit override wins; else
// it derives from the cortex root's parent (matching the automatrix/conversation
// /task/trace stores, so it shares /data with them and survives suspend /
// redeploy). Returns "" when neither is available (in-memory only).
func Dir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "brief")
	}
	return ""
}
