// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package checkpoint gives Cody two survival primitives: workspace snapshots
// (taken before risky multi-file changes, restorable on request) and durable
// per-task progress checkpoints written to cortex through the
// continuous-memory session surface — so an accepted task survives process
// restarts and the orchestrator resumes at the correct next task.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cortex "matrix/cortex"
)

// snapshotsDir lives under the workspace's .cody state dir.
const snapshotsDir = ".cody/snapshots"

// skipDirs are never snapshotted nor deleted by a restore.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	".cody": true,
}

// Snapshotter snapshots and restores a workspace tree.
type Snapshotter struct {
	root string
}

// Info describes one stored snapshot.
type Info struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	At    time.Time `json:"at"`
	Files int       `json:"files"`
}

// NewSnapshotter creates a snapshotter for a workspace root.
func NewSnapshotter(root string) (*Snapshotter, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("snapshotter root %q is not a directory", root)
	}
	return &Snapshotter{root: abs}, nil
}

// Snapshot copies the workspace tree (skip dirs excluded) into a new
// snapshot and returns its id.
func (s *Snapshotter) Snapshot(label string) (string, error) {
	at := time.Now().UTC()
	id := at.Format("20060102T150405.000000000Z")
	if label != "" {
		id += "-" + sanitizeLabel(label)
	}
	dest := filepath.Join(s.root, snapshotsDir, id)
	if err := os.MkdirAll(filepath.Join(dest, "tree"), 0o755); err != nil {
		return "", err
	}
	files := 0
	err := s.walkTree(s.root, func(rel string, info fs.FileInfo) error {
		src := filepath.Join(s.root, rel)
		dst := filepath.Join(dest, "tree", rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return "", err
	}
	meta := Info{ID: id, Label: label, At: at, Files: files}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dest, "meta.json"), append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// List returns the stored snapshots, oldest first.
func (s *Snapshotter) List() ([]Info, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, snapshotsDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, snapshotsDir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var info Info
		if json.Unmarshal(data, &info) == nil {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Restore rolls the workspace tree back to a snapshot: snapshot files are
// copied back, and files created since (outside the skip dirs) are removed.
func (s *Snapshotter) Restore(id string) error {
	tree := filepath.Join(s.root, snapshotsDir, id, "tree")
	if fi, err := os.Stat(tree); err != nil || !fi.IsDir() {
		return fmt.Errorf("snapshot %q not found", id)
	}
	// Files in the snapshot come back exactly as captured.
	snapFiles := map[string]bool{}
	err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(tree, path)
		if err != nil {
			return err
		}
		snapFiles[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		dst := filepath.Join(s.root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst, info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	// Files that did not exist at snapshot time are removed.
	return s.walkTree(s.root, func(rel string, info fs.FileInfo) error {
		if !snapFiles[rel] {
			return os.Remove(filepath.Join(s.root, rel))
		}
		return nil
	})
}

// walkTree visits every regular file under root outside the skip dirs.
func (s *Snapshotter) walkTree(root string, fn func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(rel, info)
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sanitizeLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	const max = 48
	out := b.String()
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// checkpointPrefix tags progress checkpoints in the cortex transcript so
// they filter cleanly out of other session messages.
const checkpointPrefix = "cody.checkpoint.v1:"

// Checkpoint is one durable progress record: a task accepted (or honestly
// surfaced as partial/blocked) with its adjudicated outcome.
type Checkpoint struct {
	TaskID  string    `json:"task_id"`
	Attempt int       `json:"attempt"`
	Status  string    `json:"status"`
	Summary string    `json:"summary,omitempty"`
	At      time.Time `json:"at"`
}

// Progress writes and reads per-task progress checkpoints through the cortex
// continuous-memory session surface (AppendMessage / Transcript) — the
// derived, journaled lane, so checkpoints are durable, ordered, and
// rebuildable without touching anchored world-state.
type Progress struct {
	c    *cortex.Cortex
	conv string
}

// NewProgress binds a progress writer to a cortex instance and a per-plan
// conversation id.
func NewProgress(c *cortex.Cortex, conv string) *Progress {
	return &Progress{c: c, conv: conv}
}

// Record durably appends a checkpoint.
func (p *Progress) Record(cp Checkpoint) error {
	if cp.TaskID == "" {
		return fmt.Errorf("checkpoint: empty task_id")
	}
	if cp.At.IsZero() {
		cp.At = time.Now().UTC()
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	_, err = p.c.AppendMessage(cortex.Message{
		ConversationID: p.conv,
		Role:           cortex.RoleSystem,
		Content:        checkpointPrefix + string(data),
	})
	return err
}

// All returns every checkpoint of the plan conversation in append order.
func (p *Progress) All() ([]Checkpoint, error) {
	msgs, err := p.c.Transcript(p.conv, 0, 0)
	if err != nil {
		return nil, err
	}
	var out []Checkpoint
	for _, m := range msgs {
		if m.Role != cortex.RoleSystem || !strings.HasPrefix(m.Content, checkpointPrefix) {
			continue
		}
		var cp Checkpoint
		if json.Unmarshal([]byte(strings.TrimPrefix(m.Content, checkpointPrefix)), &cp) == nil {
			out = append(out, cp)
		}
	}
	return out, nil
}

// Latest returns the most recent checkpoint, or nil when none exist.
func (p *Progress) Latest() (*Checkpoint, error) {
	all, err := p.All()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	return &all[len(all)-1], nil
}

// Done returns the set of task ids checkpointed with an accepted (done)
// status — the resume computation's input.
func (p *Progress) Done() (map[string]bool, error) {
	all, err := p.All()
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, cp := range all {
		if cp.Status == "done" {
			done[cp.TaskID] = true
		}
	}
	return done, nil
}

// projectMemoryPrefix tags project-memory records in the cortex transcript.
const projectMemoryPrefix = "cody.project.v1:"

// ProjectRecord is one durable piece of project memory: a resolved Stack
// Decision Record, a resolved Design Language Record, or an accepted task's
// turn-in summary. Together they are what the project IS — recalled as
// grounding for every later conversation in the same project, so Cody plans
// from accumulated memory instead of re-deriving from a file scan.
type ProjectRecord struct {
	Kind    string    `json:"kind"` // sdr | dlr | task
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

// ProjectMemory reads and writes project-scoped memory through the cortex
// continuous-memory session surface, keyed by project id (NOT by
// conversation) so records accumulate across conversations.
type ProjectMemory struct {
	c    *cortex.Cortex
	conv string
}

// NewProjectMemory binds project memory to a cortex instance and a project id.
func NewProjectMemory(c *cortex.Cortex, projectID string) *ProjectMemory {
	if strings.TrimSpace(projectID) == "" {
		projectID = "default"
	}
	return &ProjectMemory{c: c, conv: "cody-project-" + projectID}
}

// projectSummaryCap bounds one stored record so a verbose design language can
// never bloat the recall bundle.
const projectSummaryCap = 1500

// Record durably appends one project-memory record.
func (p *ProjectMemory) Record(kind, summary string) error {
	summary = strings.TrimSpace(summary)
	if kind == "" || summary == "" {
		return fmt.Errorf("project memory: empty kind or summary")
	}
	if len(summary) > projectSummaryCap {
		summary = summary[:projectSummaryCap] + "..."
	}
	data, err := json.Marshal(ProjectRecord{Kind: kind, Summary: summary, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = p.c.AppendMessage(cortex.Message{
		ConversationID: p.conv,
		Role:           cortex.RoleSystem,
		Content:        projectMemoryPrefix + string(data),
	})
	return err
}

// Recent returns the newest n project records, oldest first.
func (p *ProjectMemory) Recent(n int) ([]ProjectRecord, error) {
	msgs, err := p.c.Transcript(p.conv, 0, 0)
	if err != nil {
		return nil, err
	}
	var out []ProjectRecord
	for _, m := range msgs {
		if m.Role != cortex.RoleSystem || !strings.HasPrefix(m.Content, projectMemoryPrefix) {
			continue
		}
		var rec ProjectRecord
		if json.Unmarshal([]byte(strings.TrimPrefix(m.Content, projectMemoryPrefix)), &rec) == nil {
			out = append(out, rec)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// RenderProjectMemory renders records as the grounding section planners and
// task sheets carry. Decisions (SDR/DLR) render before task summaries because
// they bind everything after them.
func RenderProjectMemory(records []ProjectRecord) string {
	if len(records) == 0 {
		return ""
	}
	var decisions, tasks []ProjectRecord
	for _, r := range records {
		switch r.Kind {
		case "sdr", "dlr":
			decisions = append(decisions, r)
		default:
			tasks = append(tasks, r)
		}
	}
	var b strings.Builder
	for _, r := range decisions {
		label := "Stack decision"
		if r.Kind == "dlr" {
			label = "Design language"
		}
		fmt.Fprintf(&b, "- %s: %s\n", label, r.Summary)
	}
	for _, r := range tasks {
		fmt.Fprintf(&b, "- Delivered: %s\n", r.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}
