// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

// Status values for a TodoItem. These are the only legal Status strings; the
// agent loop and the todo tool emit them, and TodoList enforces the
// one-in_progress invariant against them.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoDone       = "done"
)

// TodoTool is defined in neo/internal/tools (tools.TodoTool) alongside the
// other synthetic tool constants. It is intercepted in the agent loop, not
// routed through Manager.Dispatch.

// TodoItem is one entry in the live task-list.
type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"` // "pending" | "in_progress" | "done"
}

// TodoList is the live task-list state for a conversation. It is owned by the
// agent loop goroutine and accessed only from there (single-threaded): no
// mutex is required. The list is an ordered plan with per-item status that
// drives the client's live task-list surface; it is snapshot/restore-able so
// it survives a respawn over the durable trace.
type TodoList struct {
	items []TodoItem
}

// NewTodoList creates an empty todo list.
func NewTodoList() *TodoList {
	return &TodoList{}
}

// Set replaces the entire list with a new plan. The first item is set to
// in_progress (if any), all others pending. This is the "create plan" path:
// item IDs and content are taken from the input, but statuses are normalized
// (the model never authoritatively sets its own starting status). The input is
// copied so the caller's slice cannot alias internal state.
func (t *TodoList) Set(items []TodoItem) {
	t.items = make([]TodoItem, len(items))
	for i, it := range items {
		it.Status = TodoPending
		t.items[i] = it
	}
	if len(t.items) > 0 {
		t.items[0].Status = TodoInProgress
	}
}

// Update modifies the status of a single item by ID. Enforces:
//   - At most one in_progress at a time (setting a new in_progress marks
//     the previous in_progress as done immediately).
//   - Marking an item done is immediate (not batched).
//   - Unknown IDs are ignored.
//
// Setting an item back to pending clears the in_progress slot only when that
// item was itself the in_progress one (zero in_progress is legal; the rule is
// "at most one", not "exactly one").
func (t *TodoList) Update(id, status string) {
	// Find the target item; ignore unknown IDs.
	idx := -1
	for i := range t.items {
		if t.items[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	// Enforce the one-in_progress invariant: promoting a new item to
	// in_progress auto-completes the previous in_progress immediately. The
	// target item itself is skipped so re-asserting in_progress on the
	// current item is a no-op rather than a self-completion.
	if status == TodoInProgress {
		for i := range t.items {
			if i != idx && t.items[i].Status == TodoInProgress {
				t.items[i].Status = TodoDone
			}
		}
	}
	t.items[idx].Status = status
}

// Snapshot returns a copy of the current items for client rendering. The
// returned slice is safe for the caller to retain and mutate. It is nil when
// the list is empty.
func (t *TodoList) Snapshot() []TodoItem {
	if len(t.items) == 0 {
		return nil
	}
	out := make([]TodoItem, len(t.items))
	copy(out, t.items)
	return out
}

// Empty reports whether the list has no items.
func (t *TodoList) Empty() bool {
	return len(t.items) == 0
}

// Restore replaces the list from a prior snapshot (durable trace / respawn).
// Unlike Set, it preserves the items' statuses verbatim — a snapshot already
// carries the authoritative per-item status, so restoring must not re-derive
// it. The input is copied so the caller's slice cannot alias internal state.
func (t *TodoList) Restore(items []TodoItem) {
	t.items = make([]TodoItem, len(items))
	copy(t.items, items)
}

// TodoEvent is a todo state-change event for the observer/reporter. It carries
// the full post-change item list so a surface can render the complete list
// from a single event without reconciling diffs.
type TodoEvent struct {
	Items []TodoItem `json:"items"`
}
