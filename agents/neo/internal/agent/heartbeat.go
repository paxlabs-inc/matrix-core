// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import "strings"

// P1-4 — Heartbeat with HEARTBEAT_OK suppression (Chronos-driven proactive
// turn convention).
//
// A Chronos recurring alarm whose wake_message tells Neo to review active
// goals/constraints and reply with the sentinel HEARTBEAT_OK when nothing
// needs attention. Neo recognizes HEARTBEAT_OK and SUPPRESSES the turn (no
// output/surface). Anything else is delivered normally.
//
// Infra already exists (Chronos durable recurring alarms with
// wake_message+payload+conversation_id); this file defines the convention +
// the suppression logic on the Neo side.

// HeartbeatOK is the sentinel the agent emits when a heartbeat wake reviewed
// active goals/constraints and found nothing needing attention. When the
// agent's final answer is exactly this token, the turn is suppressed —
// a.out.Say is never called, so HEARTBEAT_OK is never surfaced to the user.
const HeartbeatOK = "HEARTBEAT_OK"

// HeartbeatWakeMarker is the token a Chronos heartbeat alarm's wake_message
// carries so Neo can detect it as a heartbeat (proactive review) turn rather
// than a normal user message. Distinct from HeartbeatOK so a wake never
// accidentally self-suppresses.
const HeartbeatWakeMarker = "HEARTBEAT"

// HeartbeatWakeMessage is the canonical wake_message a Chronos heartbeat alarm
// delivers. It instructs the agent to review active goals/constraints and reply
// with HEARTBEAT_OK when nothing needs attention, or surface real content when
// something does.
const HeartbeatWakeMessage = "HEARTBEAT: review your active goals and constraints. If nothing needs attention, reply with exactly HEARTBEAT_OK. If something needs attention, surface it."

// isHeartbeatWake reports whether the user input is a Chronos heartbeat wake —
// i.e. it carries the HEARTBEAT marker that distinguishes a proactive review
// turn from a normal user message.
func isHeartbeatWake(userInput string) bool {
	return strings.Contains(userInput, HeartbeatWakeMarker)
}

// shouldSuppressHeartbeat reports whether the agent's final answer is the
// HEARTBEAT_OK sentinel (after trimming whitespace). Only the exact sentinel
// suppresses — a message that merely contains the token does not, so a real
// answer that happens to mention it still reaches the user.
func shouldSuppressHeartbeat(answer string) bool {
	return strings.TrimSpace(answer) == HeartbeatOK
}
