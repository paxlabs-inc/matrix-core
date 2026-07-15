// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package agent

import "strings"

// MorningBrief — the ORACLE personalized-brief wake convention on the Neo side
// (a sibling of automatrix.go). A Chronos MORNING_BRIEF alarm fires at the
// user's chosen local time on their chosen days; its wake_message carries the
// MORNING_BRIEF marker so Neo routes the turn to the restricted, supervised
// brief runner rather than a normal conversational dispatch.

// MorningBriefWakeMarker is the token a Chronos MORNING_BRIEF alarm's
// wake_message carries so Neo can detect a brief wake.
const MorningBriefWakeMarker = "MORNING_BRIEF"

// MorningBriefWakeMessage is the canonical wake_message a Chronos MORNING_BRIEF
// alarm delivers. It MUST stay byte-identical to the Chronos-side WakeMessage
// (chronos/internal/morningbrief) — they share the MORNING_BRIEF marker the
// agent uses to detect a brief wake.
const MorningBriefWakeMessage = "MORNING_BRIEF: it is time to compose the user's personalized brief. Gather fresh, source-backed items across their stated interests and deliver a short brief. If personalization or the brief is disabled, do nothing."

// IsMorningBriefWake reports whether the user input is a Chronos MORNING_BRIEF
// wake — i.e. it carries the MORNING_BRIEF marker that distinguishes a brief
// turn from a normal user message.
func IsMorningBriefWake(userInput string) bool {
	return strings.Contains(userInput, MorningBriefWakeMarker)
}

// NewMorningBrief builds an Agent for an autonomous morning-brief run on the
// TIGHT read-only information surface (Manager.BriefSchemas): web_news /
// web_search / fetch + memory_recall only. Financial, signing, filesystem,
// deployment, and arbitrary-execution tools (including core_execute) are
// structurally absent (req 15.1). It also forces Automatrix so the run gets the
// autonomous-mode charter (user away, work self-contained, no screen surfaces,
// no questions back). The restriction is a property of the constructor, so the
// engine cannot accidentally dispatch a brief on a wider surface.
func NewMorningBrief(o Options) *Agent {
	o.Brief = true
	o.Automatrix = true
	return New(o)
}
