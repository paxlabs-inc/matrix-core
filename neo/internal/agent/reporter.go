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
	// Say emits a user-facing assistant message (the answer / the narration
	// the user is meant to read).
	Say(text string)
	// Status emits ephemeral progress (a tool starting, an interim preamble).
	Status(text string)
	// Notice emits a deliberate, visible notice — surfaced prominently because
	// it is a spoken promise (compaction, escalation to the secure path).
	Notice(text string)
}

// nopReporter discards everything. Default when none is supplied.
type nopReporter struct{}

func (nopReporter) Say(string)    {}
func (nopReporter) Status(string) {}
func (nopReporter) Notice(string) {}
