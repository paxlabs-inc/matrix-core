// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
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
	if a.pager == nil || a.turn == nil {
		return nil
	}
	if a.turn.memorySeam != nil {
		return a.turn.memorySeam
	}
	seam, err := a.pager.NewNeocortexLoopSeam(a.cmConvID(), a.cfg.ActivationBudget())
	if err != nil {
		return nil
	}
	a.turn.memorySeam = seam
	return a.turn.memorySeam
}

func (a *Agent) captureMemoryProvenance(seam *cortexclient.LoopSeam) {
	if a.turn == nil || seam == nil {
		return
	}
	_, lo, hi := seam.ProvenanceRange()
	if lo != 0 {
		a.turn.noteSessionSeq(lo)
	}
	if hi != 0 {
		a.turn.noteSessionSeq(hi)
	}
}

func (a *Agent) cmRecordUser(content string) {
	if seam := a.memorySeam(); seam != nil {
		seam.RecordUser(strings.TrimSpace(content))
		a.captureMemoryProvenance(seam)
	}
}

func (a *Agent) cmRecordDelivery(content string) {
	if seam := a.memorySeam(); seam != nil {
		seam.RecordDelivery(strings.TrimSpace(content))
		a.captureMemoryProvenance(seam)
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
	if a.turn == nil {
		return "", 0, 0
	}
	a.captureMemoryProvenance(a.turn.memorySeam)
	if !a.turn.haveSeq {
		return "", 0, 0
	}
	return a.cmConvID(), a.turn.seqLo, a.turn.seqHi
}

func (a *Agent) cmActivate(query string) (*cortexclient.Bundle, error) {
	if a.pager == nil {
		return nil, nil
	}
	bundle, err := a.pager.Activate(context.Background(), a.cmConvID(), query, a.cfg.ActivationBudget())
	if err != nil {
		return nil, err
	}
	return bundle, nil
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
	Timeline        []cortexclient.MemoryProjection
	TriggerClass    string
	SelectionReason string
	Excerpts        []EpisodicMemoryEvent
}

type EpisodicMemoryEvent struct {
	ConversationID  string  `json:"conversation_id"`
	Date            string  `json:"date"`
	SeqLo           uint64  `json:"seq_lo"`
	SeqHi           uint64  `json:"seq_hi"`
	Exact           bool    `json:"exact"`
	SourceType      string  `json:"source_type"`
	Confidence      float64 `json:"confidence"`
	SelectionReason string  `json:"selection_reason"`
	Text            string  `json:"text"`
}

type MemoryObserver func(MemoryEvent)

func (a *Agent) emitMemory(bundle *cortexclient.Bundle, triggerClass string, excerpts []memory.EpisodicExcerpt) {
	if a.memObserver == nil {
		return
	}
	event := MemoryEvent{TriggerClass: triggerClass}
	if bundle != nil {
		event.Timeline = cortexclient.ProjectBundle(bundle)
	}
	for _, excerpt := range excerpts {
		confidence := 0.5
		sourceType := "semantic_recall"
		if excerpt.Exact {
			confidence = 1
			sourceType = "exact_transcript"
		}
		date := ""
		if !excerpt.Date.IsZero() {
			date = excerpt.Date.UTC().Format("2006-01-02")
		} else {
			date = "unknown"
		}
		event.Excerpts = append(event.Excerpts, EpisodicMemoryEvent{ConversationID: excerpt.ConversationID, Date: date, SeqLo: excerpt.SeqLo, SeqHi: excerpt.SeqHi, Exact: excerpt.Exact, SourceType: sourceType, Confidence: confidence, SelectionReason: triggerClass + " relevance", Text: excerpt.Text})
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
		if rendered := strings.TrimSpace(cortexclient.RenderBundle(bundle, nil)); rendered != "" {
			out.WriteString("\nDurable Neocortex activation; the live exchange wins on conflict:\n")
			out.WriteString(rendered)
			out.WriteByte('\n')
		}
	}
	return out.String()
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
