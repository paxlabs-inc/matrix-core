// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"
	"strings"

	"centra/agents/neo/internal/consolidation"
	neoidentity "centra/agents/neo/internal/identity"
	"centra/agents/neo/internal/runtime/protocol"
	"centra/agents/neo/internal/runtime/turnstate"
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
	Acknowledged     bool
	Error            string
}

type DeliveryChoke struct {
	AgentName          string
	Reporter           DeliveryReporter
	Recorder           TurnRecorder
	Consolidator       Consolidator
	SuppressIncomplete bool
	IntentID           string
	Attempt            int
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
	suppressed := shouldSuppressDelivery(userInput, scrubbed)
	deliveryError := ""
	if !suppressed && choke.Reporter != nil {
		if reliable, ok := choke.Reporter.(ReliableDeliveryReporter); ok {
			if err := reliable.SayResult(scrubbed, honestPartial); err != nil {
				deliveryError = strings.TrimSpace(err.Error())
			}
		} else {
			choke.Reporter.Say(scrubbed, false)
		}
	}
	if deliveryError == "" && choke.Recorder != nil {
		choke.Recorder.RecordDelivery(scrubbed)
	}
	if deliveryError == "" {
		choke.consolidate(checkpoint, userInput, scrubbed, true)
	}
	return DeliveryResult{
		Content: scrubbed, Suppressed: suppressed,
		IdentityScrubbed: leaked, Acknowledged: deliveryError == "",
		Error: deliveryError,
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
			if source, ok := choke.Recorder.(interface{ RecordError() error }); ok && source.RecordError() != nil {
				return
			}
		}
		choke.Reporter.Say(content, false)
	}
	choke.consolidate(checkpoint, firstUserContent(checkpoint.Messages), "", false)
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
	objective string,
	finalContent string,
	terminal bool,
) {
	if choke.Consolidator == nil {
		return
	}
	conversationID := ""
	var sequenceLow, sequenceHigh uint64
	if choke.Recorder != nil {
		conversationID, sequenceLow, sequenceHigh =
			choke.Recorder.ProvenanceRange()
	}
	job := consolidation.Job{
		IntentID: strings.TrimSpace(choke.IntentID),
		Attempt:  choke.Attempt, Terminal: terminal,
		Objective: strings.TrimSpace(objective),
		Outcome:   strings.TrimSpace(finalContent),
		Conv:      conversationID, HaveSeq: conversationID != "",
		SeqLo: sequenceLow, SeqHi: sequenceHigh,
	}
	if job.Attempt <= 0 {
		job.Attempt = 1
	}
	if job.Objective != "" {
		job.Evidence = append(job.Evidence, consolidation.Evidence{
			Kind: consolidation.EvidenceUser, Content: job.Objective,
		})
	}
	for _, message := range checkpoint.Messages {
		switch message.Role {
		case protocol.RoleTool:
			job.Evidence = append(job.Evidence, consolidation.Evidence{
				Kind: consolidation.EvidenceTool, ToolName: message.Name,
				AttemptID: message.ToolCallID, Content: message.Content,
			})
		case protocol.RoleAssistant:
			for _, call := range message.ToolCalls {
				encoded, err := json.Marshal(call)
				if err == nil {
					job.Evidence = append(job.Evidence, consolidation.Evidence{
						Kind: consolidation.EvidenceTool, ToolName: call.Name,
						AttemptID: call.ID, Content: string(encoded),
					})
				}
			}
		}
	}
	if job.Outcome != "" {
		job.Evidence = append(job.Evidence, consolidation.Evidence{
			Kind: consolidation.EvidenceAssistant, Content: job.Outcome,
		})
	}
	choke.Consolidator.Consolidate(job)
}

func shouldSuppressDelivery(userInput, content string) bool {
	answer := strings.TrimSpace(content)
	return strings.Contains(userInput, heartbeatWakeMarker) &&
		answer == heartbeatOK ||
		strings.Contains(userInput, automatrixWakeMarker) &&
			answer == automatrixIdle
}

func firstUserContent(messages []protocol.Message) string {
	for _, message := range messages {
		if message.Role == protocol.RoleUser && strings.TrimSpace(message.Content) != "" {
			return message.Content
		}
	}
	return ""
}
