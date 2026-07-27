// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"strings"

	neoidentity "matrix/neo/internal/identity"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

const (
	heartbeatWakeMarker  = "HEARTBEAT"
	heartbeatOK          = "HEARTBEAT_OK"
	automatrixWakeMarker = "AUTOMATRIX"
	automatrixIdle       = "AUTOMATRIX_IDLE"
)

type DeliveryResult struct {
	Content          string
	Suppressed       bool
	IdentityScrubbed bool
}

type DeliveryChoke struct {
	AgentName          string
	Reporter           DeliveryReporter
	Recorder           TurnRecorder
	Consolidator       Consolidator
	SuppressIncomplete bool
}

func (choke *DeliveryChoke) Deliver(
	_ context.Context,
	userInput string,
	checkpoint turnstate.Checkpoint,
	content string,
	honestPartial bool,
) DeliveryResult {
	name := strings.TrimSpace(choke.AgentName)
	if name == "" {
		name = "Neo"
	}
	scrubbed, leaked := neoidentity.Scrub(name, content)
	if choke.Recorder != nil {
		choke.Recorder.RecordDelivery(scrubbed)
	}
	suppressed := shouldSuppressDelivery(userInput, scrubbed)
	if !suppressed && choke.Reporter != nil {
		if honestPartial {
			if reporter, ok := choke.Reporter.(HonestPartialReporter); ok {
				reporter.SayHonestPartial(scrubbed)
			} else {
				choke.Reporter.Say(scrubbed, false)
			}
		} else {
			choke.Reporter.Say(scrubbed, false)
		}
	}
	choke.consolidate(checkpoint, scrubbed)
	return DeliveryResult{
		Content: scrubbed, Suppressed: suppressed,
		IdentityScrubbed: leaked,
	}
}

func (choke *DeliveryChoke) FinalizeIncomplete(
	_ context.Context,
	checkpoint turnstate.Checkpoint,
	incomplete *Incomplete,
) {
	if !choke.SuppressIncomplete && choke.Reporter != nil {
		content := incompleteStatus(incomplete)
		if choke.Recorder != nil {
			choke.Recorder.RecordDelivery(content)
		}
		choke.Reporter.Say(content, false)
	}
	choke.consolidate(checkpoint, "")
}

func incompleteStatus(incomplete *Incomplete) string {
	status := "I couldn't finish this yet, but the completed work is saved. " +
		"I can resume from the last completed step without repeating it."
	if incomplete != nil && incomplete.Phase == "effect_reconciliation" {
		status = "I couldn't safely confirm whether the last action completed. " +
			"The completed work is saved, and I need to verify that action " +
			"before continuing so I don't repeat it."
	}
	return status
}

func (choke *DeliveryChoke) consolidate(
	checkpoint turnstate.Checkpoint,
	finalContent string,
) {
	if choke.Consolidator == nil {
		return
	}
	messages := cloneMessages(checkpoint.Messages)
	if finalContent != "" && len(messages) > 0 {
		last := len(messages) - 1
		if messages[last].Role == protocol.RoleAssistant {
			messages[last].Content = finalContent
		}
	}
	conversationID := ""
	var sequenceLow, sequenceHigh uint64
	if choke.Recorder != nil {
		conversationID, sequenceLow, sequenceHigh =
			choke.Recorder.ProvenanceRange()
	}
	choke.Consolidator.Consolidate(
		renderTranscript(messages),
		conversationID,
		sequenceLow,
		sequenceHigh,
	)
}

func shouldSuppressDelivery(userInput, content string) bool {
	answer := strings.TrimSpace(content)
	return strings.Contains(userInput, heartbeatWakeMarker) &&
		answer == heartbeatOK ||
		strings.Contains(userInput, automatrixWakeMarker) &&
			answer == automatrixIdle
}

func renderTranscript(messages []protocol.Message) string {
	var builder strings.Builder
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content != "" {
			builder.WriteString(string(message.Role))
			builder.WriteString(": ")
			builder.WriteString(content)
			builder.WriteByte('\n')
		}
		for _, call := range message.ToolCalls {
			builder.WriteString("assistant tool call: ")
			builder.WriteString(call.Name)
			builder.WriteByte(' ')
			builder.Write(call.Arguments)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}
