// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store persists sheets and turn-in reports as JSON files under a root
// directory (the plan directory on the volume). Writes are atomic
// (tmp + rename) so a crash never leaves a torn record; a crashed worker is
// replaced by a fresh one dispatched from the same persisted sheet.
type Store struct {
	root string
}

// OpenStore creates (if needed) and opens a durable contract store rooted at
// dir.
func OpenStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("contract store: empty root dir")
	}
	for _, sub := range []string{"sheets", "reports"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("contract store: %w", err)
		}
	}
	return &Store{root: dir}, nil
}

// Root reports the store's root directory.
func (st *Store) Root() string { return st.root }

// SaveSheet validates and durably persists a task sheet.
func (st *Store) SaveSheet(s *TaskSheet) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return writeJSON(filepath.Join(st.root, "sheets", s.TaskID+".json"), s)
}

// LoadSheet reads the persisted sheet for a task id.
func (st *Store) LoadSheet(taskID string) (*TaskSheet, error) {
	if err := validateID(taskID); err != nil {
		return nil, err
	}
	var s TaskSheet
	if err := readJSON(filepath.Join(st.root, "sheets", taskID+".json"), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListSheets returns the task ids of all persisted sheets, sorted.
func (st *Store) ListSheets() ([]string, error) {
	return listIDs(filepath.Join(st.root, "sheets"))
}

// SaveReport validates and durably persists a turn-in report. Reports are
// keyed by task id + attempt so a re-dispatch never overwrites the history it
// was rejected over.
func (st *Store) SaveReport(r *TurnInReport) error {
	if err := r.Validate(); err != nil {
		return err
	}
	name := fmt.Sprintf("%s.attempt-%d.json", r.TaskID, r.Attempt)
	return writeJSON(filepath.Join(st.root, "reports", name), r)
}

// LoadReports returns every persisted report for a task id in attempt order.
func (st *Store) LoadReports(taskID string) ([]*TurnInReport, error) {
	if err := validateID(taskID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(st.root, "reports"))
	if err != nil {
		return nil, err
	}
	var out []*TurnInReport
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), taskID+".attempt-") {
			continue
		}
		var r TurnInReport
		if err := readJSON(filepath.Join(st.root, "reports", e.Name()), &r); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}

func listIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
