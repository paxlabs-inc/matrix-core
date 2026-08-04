// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"matrix/cortexclient"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/o1"
)

func (a *Agent) cmConvID() string {
	if a.convID != "" {
		return a.convID
	}
	return "cli"
}

func (a *Agent) memorySeam() *cortexclient.LoopSeam {
	if a.pager == nil {
		return nil
	}
	seam, err := a.pager.NewNeocortexLoopSeam(a.cmConvID(), a.cfg.ActivationBudget())
	if err != nil {
		return nil
	}
	return seam
}

func (a *Agent) cmRecordUser(content string) {
	if seam := a.memorySeam(); seam != nil {
		seam.RecordUser(strings.TrimSpace(content))
	}
}

func (a *Agent) cmRecordAssistant(msg llm.Message) {
	if msg.IsGuidance() {
		return
	}
	seam := a.memorySeam()
	if seam == nil {
		return
	}
	if content := strings.TrimSpace(msg.Content); content != "" {
		seam.RecordAssistantWorking(content)
	}
	for _, call := range msg.ToolCalls {
		payload, _ := json.Marshal(map[string]any{"tool": call.Function.Name, "arguments": json.RawMessage(call.Function.Arguments)})
		seam.RecordAssistantWorking(string(payload))
	}
}

func (a *Agent) cmRecordToolResult(name, content string) {
	if seam := a.memorySeam(); seam != nil {
		payload, _ := json.Marshal(map[string]string{"tool": name, "result": content})
		seam.RecordAssistantWorking(string(payload))
	}
}

func (a *Agent) persistO1State() {
	if a.pager == nil || a.turn == nil || a.turn.runLedger == nil {
		return
	}
	snapshot, err := a.turn.runLedger.SnapshotJSON()
	if err != nil {
		a.turn.runLedger.AddHazard("o1-ledger-snapshot")
		return
	}
	if _, err := a.pager.NeocortexClient().WriteCheckpoint(context.Background(), "neo.o1."+a.cmConvID(), snapshot); err != nil {
		a.turn.runLedger.AddHazard("o1-ledger-persistence")
	}
}

func (a *Agent) restoreO1State(contractID string) *o1.RunLedger {
	if a.pager == nil {
		return nil
	}
	blob, _, err := a.pager.NeocortexClient().LatestCheckpoint(context.Background(), "neo.o1."+a.cmConvID())
	if err != nil {
		return nil
	}
	ledger, err := o1.RestoreRunLedger(blob)
	if err == nil && ledger.ContractID == contractID && ledger.TerminalProof == nil {
		return ledger
	}
	return nil
}

func (a *Agent) provenanceRange() (string, uint64, uint64) {
	if a.turn == nil || !a.turn.haveSeq {
		return "", 0, 0
	}
	return a.cmConvID(), a.turn.seqLo, a.turn.seqHi
}

func (a *Agent) cmActivate(query string) *cortexclient.Bundle {
	if a.pager == nil {
		return nil
	}
	bundle, err := a.pager.Activate(context.Background(), a.cmConvID(), query, a.cfg.ActivationBudget())
	if err != nil {
		return nil
	}
	return bundle
}

func (a *Agent) cmRelevancePush(ctx context.Context, query string) ([]memory.Snippet, string) {
	if !a.cfg.FirstTurnRelevancePush || a.turnSeq != 1 || a.pager == nil {
		return nil, ""
	}
	snippets, err := a.pager.Retrieve(ctx, query)
	if err != nil || len(snippets) == 0 {
		return nil, ""
	}
	var out strings.Builder
	out.WriteString("\nRelevant to your message; the current conversation has authority over older material:\n")
	for _, snippet := range snippets {
		if line := strings.TrimSpace(snippet.Text); line != "" {
			fmt.Fprintf(&out, "- %s\n", line)
		}
	}
	return snippets, out.String()
}

type MemoryEvent struct {
	StorySoFar      string
	Timeline        []string
	TriggerClass    string
	SelectionReason string
	Excerpts        []EpisodicMemoryEvent
}

type EpisodicMemoryEvent struct {
	ConversationID string `json:"conversation_id"`
	Date           string `json:"date"`
	SeqLo          uint64 `json:"seq_lo"`
	SeqHi          uint64 `json:"seq_hi"`
	Exact          bool   `json:"exact"`
	Text           string `json:"text"`
}

type MemoryObserver func(MemoryEvent)

func (a *Agent) emitMemory(bundle *cortexclient.Bundle, triggerClass string, excerpts []memory.EpisodicExcerpt) {
	if a.memObserver == nil {
		return
	}
	event := MemoryEvent{TriggerClass: triggerClass}
	if bundle != nil {
		for _, section := range bundle.Sections {
			for _, item := range section.Items {
				line := strings.TrimSpace(string(item.Content))
				if line != "" {
					event.Timeline = append(event.Timeline, line)
				}
			}
		}
	}
	for _, excerpt := range excerpts {
		event.Excerpts = append(event.Excerpts, EpisodicMemoryEvent{ConversationID: excerpt.ConversationID, Date: excerpt.Date.UTC().Format("2006-01-02"), SeqLo: excerpt.SeqLo, SeqHi: excerpt.SeqHi, Exact: excerpt.Exact, Text: excerpt.Text})
	}
	if len(event.Timeline) > 0 || len(event.Excerpts) > 0 {
		a.memObserver(event)
	}
}

func (a *Agent) renderActivationBundle(bundle *cortexclient.Bundle) string {
	var out strings.Builder
	out.WriteString("(Reference notes, not a new message — this conversation is already in progress. Continue from the live exchange above; do NOT restart it or re-answer an earlier request.)\n")
	if !a.cfg.SessionCurrentIntent {
		if goal := strings.TrimSpace(a.activeGoal); goal != "" {
			fmt.Fprintf(&out, "Standing objective for this conversation: %s\n", goal)
		}
	} else if objective := a.currentObjective(); objective != "" {
		fmt.Fprintf(&out, "Current user objective (authoritative; newer than any historical task): %s\n", objective)
	}
	if bundle != nil {
		if a.cfg.SessionExactProjection {
			out.WriteString("\nExact Neocortex transcript slice recovered after live-window trimming:\n")
		}
		if rendered := strings.TrimSpace(cortexclient.RenderBundle(bundle, nil)); rendered != "" {
			out.WriteString("\nDurable Neocortex activation; the live exchange wins on conflict:\n")
			out.WriteString(rendered)
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func assembleWindowUserTail(stableSystem string, transcript []llm.Message, tail string) []llm.Message {
	return append(append([]llm.Message{llm.SystemMessage(stableSystem)}, transcript...), llm.UserMessage(tail))
}

func assembleWindowContextSidecar(stableSystem string, transcript []llm.Message, contextProjection string) []llm.Message {
	window := append([]llm.Message{llm.SystemMessage(stableSystem)}, transcript...)
	if sidecar := llm.ContextMessage(contextProjection); sidecar.Role != "" {
		window = append(window, sidecar)
	}
	return window
}

func (a *Agent) cmTrimWorking() {
	if a.cfg.SessionExactProjection {
		a.compactExactWorking()
		return
	}
	if len(a.working) <= 2 {
		return
	}
	older, recent := splitForCompaction(a.working, keepRecentUserTurns)
	if older == nil {
		a.stripOldImages()
		a.working = safeTail(a.working)
		return
	}
	a.working = recent
}
