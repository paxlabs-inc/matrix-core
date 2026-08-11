// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import "strings"

// Automatrix — idle wake with AUTOMATRIX_IDLE suppression (Chronos-driven
// proactive surprise-task convention).
//
// A Chronos recurring alarm whose wake_message tells Neo it has idle time to
// review its pending non-financial opportunities, complete one end-to-end, and
// notify the user with the result. When nothing is worth doing right now, Neo
// replies with the sentinel AUTOMATRIX_IDLE and SUPPRESSES the turn (no
// output/surface). Anything else is delivered normally.
//
// Infra already exists (Chronos durable recurring alarms with
// wake_message+payload+conversation_id); this file defines the convention +
// the suppression logic on the Neo side, as a sibling of heartbeat.go.

// AutomatrixIdle is the sentinel the agent emits when an Automatrix wake found
// nothing worth doing right now. When the agent's final answer is exactly this
// token, the turn is suppressed — a.out.Say is never called, so AUTOMATRIX_IDLE
// is never surfaced to the user.
const AutomatrixIdle = "AUTOMATRIX_IDLE"

// AutomatrixWakeMarker is the token a Chronos AUTOMATRIX alarm's wake_message
// carries so Neo can detect it as an Automatrix (proactive surprise-task) turn
// rather than a normal user message. Distinct from AutomatrixIdle so a wake
// never accidentally self-suppresses.
const AutomatrixWakeMarker = "AUTOMATRIX"

// AutomatrixWakeMessage is the canonical wake_message a Chronos AUTOMATRIX alarm
// delivers. It instructs the agent to review its pending non-financial
// opportunities, complete one if worthwhile and notify the user, or reply with
// exactly AUTOMATRIX_IDLE when nothing is worth doing right now.
//
// This MUST stay byte-identical to the Chronos-side WakeMessage
// (chronos/internal/automatrix) — they share the AUTOMATRIX marker that the
// agent uses to detect an Automatrix wake.
const AutomatrixWakeMessage = "AUTOMATRIX: you have idle time. Review your pending non-financial opportunities and, if a worthwhile one exists, pick ONE and complete it end-to-end, then notify the user with the result. If nothing is worth doing right now, reply with exactly AUTOMATRIX_IDLE."

// isAutomatrixWake reports whether the user input is a Chronos Automatrix wake —
// i.e. it carries the AUTOMATRIX marker that distinguishes a proactive surprise
// turn from a normal user message.
func isAutomatrixWake(userInput string) bool {
	return strings.Contains(userInput, AutomatrixWakeMarker)
}

// shouldSuppressAutomatrix reports whether the agent's final answer is the
// AUTOMATRIX_IDLE sentinel (after trimming whitespace). Only the exact sentinel
// suppresses — a message that merely contains the token does not, so a real
// answer that happens to mention it still reaches the user.
func shouldSuppressAutomatrix(answer string) bool {
	return strings.TrimSpace(answer) == AutomatrixIdle
}
