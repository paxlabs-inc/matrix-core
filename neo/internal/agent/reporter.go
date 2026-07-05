// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

// Reporter is Neo's user-facing output channel. The agent never writes to a
// terminal directly; it speaks through a Reporter so the same loop can drive a
// CLI, a daemon SSE stream, or a test sink.
//
// The split mirrors the transparency rule: Say is the substance, Status is
// ephemeral progress, and Notice is a deliberate, visible promise (e.g. the
// compaction announcement or a money-escalation heads-up).
type Reporter interface {
	// Say emits a user-facing assistant closing message. completion=true marks
	// the validated task_complete summary — a redundant recap layered on top of
	// the narration the user already read, so the surface keeps it out of the
	// thread; it is still sent (and closes the run) so an accepted completion
	// deterministically ends the task. completion=false is a conversational or
	// ceiling answer that IS the content the user reads.
	Say(text string, completion bool)
	// Status emits DURABLE narration — genuine model-authored "thinking out
	// loud" the model wrote alongside its tool calls. It becomes a permanent
	// thread bubble and marks the turn as having delivered content the user
	// read, so an accepted task_complete summary can be hidden as a redundant
	// recap.
	Status(text string)
	// Progress emits an EPHEMERAL "about to act" intent line — the synthesized
	// narrate-before-act stub used when the model gave NO preamble of its own
	// (e.g. it went straight to a tool). It is surfaced live so the user can
	// follow along, but it is NEVER persisted to the durable thread and NEVER
	// counts as the delivered answer. This is load-bearing: a synthetic stub
	// (e.g. "Layerx deposit.") must not mark the turn "already narrated" and so
	// suppress the real task_complete summary — the bug where the model batched
	// straight to task_complete and the actual content silently vanished.
	Progress(text string)
	// Notice emits a deliberate, visible notice — surfaced prominently because
	// it is a spoken promise (compaction, escalation to the secure path).
	Notice(text string)
	// Think emits a glimpse of the model's chain-of-thought (reasoning channel)
	// as the loop works. It is NEVER the answer and NEVER persisted to the
	// thread — a secondary, dismissible "thinking" channel the surface may show
	// so the user sees SOME of how Neo is reasoning, not the full monologue.
	Think(text string)
	// Delta streams an incremental fragment of the CURRENT model turn as it is
	// generated, before the turn settles. channel is "content" (the visible
	// answer being typed out) or "reasoning" (the live thinking). turn is the
	// agent-loop step index so a consumer can segment the stream per turn
	// (resetting its buffer when turn advances). It is best-effort — fragments
	// may be coalesced or dropped — and NEVER persisted: the durable thread is
	// still written from Say. A consumer that does not stream may ignore it.
	Delta(turn int, channel, text string)
}

// nopReporter discards everything. Default when none is supplied.
type nopReporter struct{}

func (nopReporter) Say(string, bool)          {}
func (nopReporter) Status(string)             {}
func (nopReporter) Progress(string)           {}
func (nopReporter) Notice(string)             {}
func (nopReporter) Think(string)              {}
func (nopReporter) Delta(int, string, string) {}
