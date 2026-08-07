// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"matrix/neo/internal/capabilityhub"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/improvement"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/task"
	"matrix/neo/internal/tools"
)

const (
	improvementWakeMarker = "NEO_IMPROVEMENT_OBSERVER"
	improvementMaxTurns   = 24
	improvementMaxInput   = 48 * 1024
)

type improvementWake struct {
	Key string `json:"key"`
}

type improvementAlarmController interface {
	Set(context.Context, improvement.Observation, int) (string, error)
}

type mcpImprovementAlarmController struct {
	tools *tools.Manager
}

// improvementCompletionObserver attaches only to the existing server-side
// terminal notification seam. Its callback is non-blocking; scheduling happens
// in a separately drained postlude after a genuinely completed fresh run.
func (e *Engine) improvementCompletionObserver(conversationID string) (func(task.Status), func(string, bool)) {
	if e == nil || e.improvement == nil || !e.cfg.ImprovementEnabled {
		return nil, func(string, bool) {}
	}
	completed := make(chan task.Status, 1)
	observe := func(status task.Status) {
		select {
		case completed <- status:
		default:
		}
	}
	arm := func(runID string, fresh bool) {
		if !fresh || strings.TrimSpace(runID) == "" {
			return
		}
		e.improvementWG.Add(1)
		go func() {
			defer e.improvementWG.Done()
			if status := <-completed; status == task.StatusDone {
				e.scheduleImprovementObservation(conversationID, runID)
			}
		}()
	}
	return observe, arm
}

func (controller *mcpImprovementAlarmController) Set(ctx context.Context, observation improvement.Observation, retry int) (string, error) {
	if controller == nil || controller.tools == nil {
		return "", errors.New("improvement: Chronos tool manager unavailable")
	}
	fn, ok := newMCPAlarmController(controller.tools).resolveTool(alarmSetToolSuffix)
	if !ok {
		return "", errors.New("improvement: local Chronos alarm_set unavailable")
	}
	wake, _ := json.Marshal(improvementWake{Key: observation.Key})
	fireAt := observation.FireAt
	if retry > 0 {
		fireAt = time.Now().UTC().Add(2 * time.Minute)
	}
	out, isErr, err := controller.tools.Dispatch(ctx, fn, map[string]interface{}{
		"kind":            "once",
		"fire_at":         fireAt.UTC().Format(time.RFC3339Nano),
		"conversation_id": "improvement-observer-wake",
		"wake_message":    improvementWakeMarker + " " + string(wake),
		"idempotency_key": fmt.Sprintf("neo-improvement-%s-%d", observation.Key, retry),
		"label":           "Neo verified improvement observer",
	})
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("improvement: Chronos rejected alarm: %s", strings.TrimSpace(out))
	}
	id := parseAlarmID(out)
	if id == "" {
		return "", errors.New("improvement: Chronos returned no alarm id")
	}
	return id, nil
}

func (e *Engine) scheduleImprovementObservation(conversationID, runID string) {
	if e == nil || e.improvement == nil || !e.cfg.ImprovementEnabled || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	delay := time.Duration(e.cfg.ImprovementIdleDelayMinutes) * time.Minute
	if delay <= 0 {
		delay = 10 * time.Minute
	}
	observation, fresh, err := e.improvement.Schedule(conversationID, runID, time.Now().UTC().Add(delay))
	if err != nil || !fresh {
		return
	}
	e.improvementWG.Add(1)
	go func() {
		defer e.improvementWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		alarmID, err := e.scheduleImprovementAlarm(ctx, observation, 0)
		if err != nil {
			_ = e.improvement.Fail(observation.Key, err.Error())
			return
		}
		_ = e.improvement.SetAlarm(observation.Key, alarmID)
	}()
}

func (e *Engine) scheduleImprovementAlarm(ctx context.Context, observation improvement.Observation, retry int) (string, error) {
	if e.improvementAlarms == nil {
		return "", errors.New("improvement: Chronos alarm controller unavailable")
	}
	return e.improvementAlarms.Set(ctx, observation, retry)
}

func parseImprovementWake(message string) (improvementWake, bool) {
	message = strings.TrimSpace(message)
	if !strings.HasPrefix(message, improvementWakeMarker) {
		return improvementWake{}, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(message, improvementWakeMarker))
	var wake improvementWake
	if json.Unmarshal([]byte(raw), &wake) != nil || strings.TrimSpace(wake.Key) == "" {
		return improvementWake{}, false
	}
	return wake, true
}

func (e *Engine) MaybeHandleImprovementWake(ctx context.Context, message string) bool {
	wake, ok := parseImprovementWake(message)
	if !ok {
		return false
	}
	if e == nil || e.improvement == nil || !e.cfg.ImprovementEnabled {
		return true
	}
	observation, exists := e.improvement.Observation(wake.Key)
	if !exists || observation.Status == improvement.ObservationDone || observation.Status == improvement.ObservationNoop {
		return true
	}
	e.mu.Lock()
	busy := len(e.runs) > 0
	e.mu.Unlock()
	if busy {
		alarmID, err := e.scheduleImprovementAlarm(ctx, observation, observation.Attempts+1)
		if err != nil {
			_ = e.improvement.Fail(wake.Key, err.Error())
		} else {
			_ = e.improvement.SetAlarm(wake.Key, alarmID)
		}
		return true
	}
	e.improvementWG.Add(1)
	go func() {
		defer e.improvementWG.Done()
		e.runImprovementObservation(wake.Key)
	}()
	return true
}

func (e *Engine) runImprovementObservation(key string) {
	observation, err := e.improvement.Begin(key)
	if err != nil || observation.Status == improvement.ObservationDone || observation.Status == improvement.ObservationNoop {
		return
	}
	turns := e.conv.Recent(observation.ConversationID, improvementMaxTurns)
	input := renderImprovementInput(observation, turns)
	if input == "" {
		_, _ = e.improvement.Finish(key, nil)
		return
	}
	client := e.subMain
	if client == nil {
		client = e.cheap
	}
	if client == nil {
		_ = e.improvement.Fail(key, "observer model unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(improvementObserverPrompt),
		llm.UserMessage(input),
	}, ToolChoice: "none", MaxTokens: 4096})
	if err != nil {
		_ = e.improvement.Fail(key, err.Error())
		return
	}
	drafts, err := decodeImprovementDrafts(result.Message.Content, observation)
	if err != nil {
		_ = e.improvement.Fail(key, err.Error())
		return
	}
	if _, err := e.improvement.Finish(key, drafts); err != nil {
		_ = e.improvement.Fail(key, err.Error())
	}
}

const improvementObserverPrompt = `You are an isolated read-only quality observer. You cannot act, edit, schedule, write memory, or change configuration. Review only the supplied completed conversation and return strict JSON: {"proposals":[...]}. Most sessions should return an empty array.

Every proposal needs kind, summary, rationale, confidence, evidence, and exactly one matching payload. Evidence entries contain role and an exact quote; the server stamps conversation and run identity. Never infer beyond quoted evidence.

Allowed kinds and payloads:
- memory: payload.memory is one operation="supersede" with target.uri and value containing type/content. Only explicit corrections to an existing URI.
- knowledge: payload.knowledge is a bounded sourced KnowledgeImportRequest.
- capability, skill, rule, configuration, authority: payload.capability has manifest, prose, provenance. These only create quarantined Capability Hub candidates and never gain authority here.
- unfinished_work: payload.opportunity has summary, rationale, confidence. It becomes a reviewable Automatrix opportunity.

Do not propose style tweaks, speculative preferences, direct self-editing, deployment, money movement, or changes without exact evidence. Return JSON only.`

func renderImprovementInput(observation improvement.Observation, turns []conversation.Turn) string {
	if len(turns) == 0 {
		return ""
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "conversation_id=%s\nrun_id=%s\ncompleted transcript:\n", observation.ConversationID, observation.RunID)
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&builder, "%s: %s\n", turn.Role, text)
		if builder.Len() >= improvementMaxInput {
			break
		}
	}
	value := builder.String()
	if len(value) > improvementMaxInput {
		value = value[:improvementMaxInput]
	}
	return strings.TrimSpace(value)
}

func decodeImprovementDrafts(content string, observation improvement.Observation) ([]improvement.Draft, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	var envelope struct {
		Proposals []improvement.Draft `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &envelope); err != nil {
		return nil, fmt.Errorf("improvement: decode observer response: %w", err)
	}
	if len(envelope.Proposals) > 8 {
		return nil, errors.New("improvement: observer returned more than eight proposals")
	}
	for index := range envelope.Proposals {
		for evidenceIndex := range envelope.Proposals[index].Evidence {
			evidence := &envelope.Proposals[index].Evidence[evidenceIndex]
			evidence.ConversationID = observation.ConversationID
			evidence.RunID = observation.RunID
		}
		if err := improvement.ValidateDraft(envelope.Proposals[index]); err != nil {
			return nil, err
		}
	}
	return envelope.Proposals, nil
}

type improvementActionRequest struct {
	Verification string `json:"verification"`
	Reason       string `json:"reason"`
}

func (s *Server) handleImprovement(w http.ResponseWriter, r *http.Request) {
	if s.engine.improvement == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "verified improvement is disabled"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/improvement")
	if path == "/proposals" || path == "/proposals/" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		status := improvement.ProposalStatus(strings.TrimSpace(r.URL.Query().Get("status")))
		writeJSON(w, http.StatusOK, map[string]interface{}{"proposals": s.engine.improvement.List(status)})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "proposals" || r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, action := parts[1], parts[2]
	var request improvementActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	var (
		proposal improvement.Proposal
		err      error
	)
	switch action {
	case "approve":
		if len(strings.TrimSpace(request.Verification)) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "concrete verification evidence is required"})
			return
		}
		proposal, err = s.engine.improvement.Transition(id, []improvement.ProposalStatus{improvement.StatusPending}, improvement.StatusApproved, "owner", request.Verification, "", "")
		if err == nil {
			proposal, err = s.engine.applyImprovementProposal(r.Context(), proposal)
		}
	case "apply":
		proposal, _ = s.engine.improvement.Get(id)
		if proposal.Status != improvement.StatusApproved {
			err = errors.New("proposal is not approved")
		} else {
			proposal, err = s.engine.applyImprovementProposal(r.Context(), proposal)
		}
	case "reject":
		proposal, err = s.engine.improvement.Transition(id, []improvement.ProposalStatus{improvement.StatusPending, improvement.StatusApproved}, improvement.StatusRejected, "owner", request.Reason, "", "")
	case "rollback":
		proposal, _ = s.engine.improvement.Get(id)
		if proposal.Status != improvement.StatusApplied {
			err = errors.New("proposal is not applied")
		} else {
			proposal, err = s.engine.rollbackImprovementProposal(r.Context(), proposal, request.Reason)
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		_ = s.engine.improvement.SetProposalError(id, err.Error())
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (e *Engine) applyImprovementProposal(ctx context.Context, proposal improvement.Proposal) (improvement.Proposal, error) {
	var appliedRef, rollbackRef string
	switch proposal.Draft.Kind {
	case improvement.KindMemory:
		if e.pager == nil {
			return proposal, errors.New("memory owner unavailable")
		}
		previous, err := e.memoryValueForURI(proposal.Draft.Payload.Memory.Target.URI)
		if err != nil {
			return proposal, err
		}
		result, err := e.pager.Mutate(ctx, memory.MutationRequest{Items: []memory.MutationItem{*proposal.Draft.Payload.Memory}})
		if err != nil {
			return proposal, fmt.Errorf("memory supersession failed: %w", err)
		}
		if len(result.Results) != 1 {
			return proposal, errors.New("memory supersession returned no durable result")
		}
		appliedRef = result.Results[0].URI
		raw, _ := json.Marshal(previous)
		rollbackRef = string(raw)
	case improvement.KindKnowledge:
		if e.pager == nil {
			return proposal, errors.New("knowledge owner unavailable")
		}
		document, err := e.pager.ImportKnowledge(ctx, *proposal.Draft.Payload.Knowledge)
		if err != nil {
			return proposal, err
		}
		appliedRef = document.ID
	case improvement.KindUnfinishedWork:
		if e.pager == nil {
			return proposal, errors.New("Automatrix opportunity owner unavailable")
		}
		opportunity := *proposal.Draft.Payload.Opportunity
		opportunity.Status = memory.OpportunityPending
		opportunity.OriginConversationID = proposal.Draft.Evidence[0].ConversationID
		opportunity.EligibleAutonomous = memory.EligibleForAutonomy(opportunity.Summary, false)
		uri, err := e.pager.RememberOpportunity(ctx, opportunity)
		if err != nil {
			return proposal, err
		}
		appliedRef = uri
	case improvement.KindCapability, improvement.KindSkill, improvement.KindRule, improvement.KindConfiguration, improvement.KindAuthority:
		if e.capabilities == nil {
			return proposal, errors.New("Capability Hub owner unavailable")
		}
		payload := proposal.Draft.Payload.Capability
		candidate, err := e.capabilities.ImportAuthored(ctx, capabilityhub.AuthoredRequest{Manifest: payload.Manifest, Prose: payload.Prose, SourceRef: payload.Provenance})
		if err != nil {
			return proposal, err
		}
		raw, _ := json.Marshal(map[string]string{"slug": candidate.Slug, "version": candidate.Version})
		appliedRef = string(raw)
	default:
		return proposal, errors.New("unsupported proposal owner")
	}
	return e.improvement.Transition(proposal.ID, []improvement.ProposalStatus{improvement.StatusApproved}, improvement.StatusApplied, "owner-router", "applied by bounded owner", appliedRef, rollbackRef)
}

func (e *Engine) rollbackImprovementProposal(ctx context.Context, proposal improvement.Proposal, reason string) (improvement.Proposal, error) {
	rollbackResult := ""
	switch proposal.Draft.Kind {
	case improvement.KindMemory:
		var previous memory.MutationValue
		if json.Unmarshal([]byte(proposal.RollbackRef), &previous) != nil || previous.Content == "" {
			return proposal, errors.New("memory rollback material unavailable")
		}
		result, err := e.pager.Mutate(ctx, memory.MutationRequest{Items: []memory.MutationItem{{Operation: memory.MutationSupersede, Target: &memory.MutationTarget{URI: proposal.AppliedRef}, Value: &previous, Reason: "owner rollback: " + reason}}})
		if err != nil {
			return proposal, fmt.Errorf("memory rollback failed: %w", err)
		}
		if len(result.Results) != 1 {
			return proposal, errors.New("memory rollback returned no durable result")
		}
		rollbackResult = result.Results[0].URI
	case improvement.KindKnowledge:
		archived := true
		document, err := e.pager.UpdateKnowledgeDocument(ctx, proposal.AppliedRef, memory.KnowledgeDocumentUpdate{Archived: &archived})
		if err != nil {
			return proposal, err
		}
		rollbackResult = document.ID
	case improvement.KindUnfinishedWork:
		if err := e.pager.SetOpportunityStatus(ctx, proposal.AppliedRef, memory.OpportunityDismissed); err != nil {
			return proposal, err
		}
		rollbackResult = proposal.AppliedRef
	default:
		var ref map[string]string
		if json.Unmarshal([]byte(proposal.AppliedRef), &ref) != nil {
			return proposal, errors.New("Capability Hub reference unavailable")
		}
		if err := e.capabilities.Uninstall(ctx, ref["slug"], ref["version"]); err != nil {
			return proposal, err
		}
		rollbackResult = proposal.AppliedRef
	}
	return e.improvement.Transition(proposal.ID, []improvement.ProposalStatus{improvement.StatusApplied}, improvement.StatusRolledBack, "owner", reason, "", rollbackResult)
}

func (e *Engine) memoryValueForURI(uri string) (memory.MutationValue, error) {
	entries, _, err := e.pager.Timeline(memory.TimelineQuery{Limit: 200})
	if err != nil {
		return memory.MutationValue{}, err
	}
	for _, entry := range entries {
		if entry.URI == uri {
			return memory.MutationValue{Type: entry.Type, Content: entry.FormMedium}, nil
		}
	}
	return memory.MutationValue{}, errors.New("memory target not found")
}
