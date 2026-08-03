// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"fmt"
	"regexp"
	"strings"

	"matrix/neo/internal/runtime/liveness"
	"matrix/neo/internal/runtime/protocol"
)

var finalAnswerTokenPattern = regexp.MustCompile(`[a-z0-9][a-z0-9.-]*`)

func finalAnswerRepairPrompt(reason string) string {
	return "The previous candidate answer was rejected because " +
		strings.TrimSpace(reason) +
		". Answer the original user request directly from the available evidence. " +
		"Do not discuss system messages, internal planning, revisions, retries, or a supposedly earlier answer. " +
		"Use the restored tools if more evidence is required."
}

func finalAnswerAddressesRequest(
	userContent string,
	content string,
	events []ToolExecution,
) (bool, string) {
	return finalAnswerAddressesRequestWithHistory(
		userContent, content, events, nil,
	)
}

func finalAnswerAddressesRequestWithHistory(
	userContent string,
	content string,
	events []ToolExecution,
	history []protocol.Message,
) (bool, string) {
	answer := strings.TrimSpace(content)
	lower := strings.ToLower(answer)
	for _, marker := range []string{
		"internal plan revision",
		"system message says",
		"looking at the conversation flow",
		"already provided the answer",
		"already gave you the",
		"the report is above",
		"my response was already delivered",
		"wait for your next",
		"wait for the next user",
		"there's nothing more to do",
		"there is nothing more to do",
	} {
		if strings.Contains(lower, marker) {
			return false,
				"it exposed internal control flow or referred to an answer that was not delivered"
		}
	}
	if RequestRequiresToolEvidence(userContent) &&
		!hasSuccessfulToolEvidence(events) {
		return false,
			"the request requires real inspection or action but no citation-verified successful tool result exists"
	}
	if isThreadFollowUp(userContent) && len(history) > 0 &&
		!answerLeadSharesThreadSubject(content, history) {
		return false,
			"the answer's leading subject is not grounded in the immediately preceding conversation"
	}
	if !hasExplorationEvidence(events) ||
		!requestsResearchAnswer(userContent) {
		return true, ""
	}
	if !sharesRequestSubject(userContent, answer) {
		return false,
			"the answer did not address the subject of the original request"
	}
	if len(strings.Fields(answer)) < 20 {
		return false,
			"a research turn ended without a substantive evidence-backed answer"
	}
	if !strings.Contains(lower, "http://") &&
		!strings.Contains(lower, "https://") &&
		!strings.Contains(lower, "](") &&
		!statesResearchLimitation(lower) {
		return false,
			"a completed research answer omitted sources or an explicit evidence limitation"
	}
	return true, ""
}

// RequestRequiresToolEvidence is deliberately conservative and fail-closed
// for verbs that ask Neo to inspect or alter real state. Pure explanations are
// excluded; action phrasing requires a verified tool result before completion.
func RequestRequiresToolEvidence(request string) bool {
	normalized := normalizedFinalRequest(request)
	explanationOnly := false
	for _, marker := range []string{
		" how do i ", " how can i ", " explain ", " describe ",
		" what does ", " why does ", " example of ",
	} {
		if strings.Contains(normalized, marker) {
			explanationOnly = true
			break
		}
	}
	if explanationOnly {
		explicitAction := false
		for _, marker := range []string{
			" then ", " go ahead ", " for me ", " actually ",
			" and run ", " and check ", " and inspect ", " please run ",
		} {
			if strings.Contains(normalized, marker) {
				explicitAction = true
				break
			}
		}
		if !explicitAction {
			return false
		}
	}
	for _, marker := range []string{
		" read ", " list ", " inspect ", " check ", " verify ",
		" look up ", " search ", " fetch ", " browse ", " open ",
		" run ", " execute ", " test ", " write ", " edit ",
		" create ", " delete ", " remove ", " move ", " copy ",
		" restart ", " deploy ", " install ", " send ", " publish ",
		" upload ", " download ", " click ", " log in ", " login ",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func hasSuccessfulToolEvidence(events []ToolExecution) bool {
	for _, event := range events {
		if event.Error == "" {
			return true
		}
	}
	return false
}

func isThreadFollowUp(request string) bool {
	normalized := normalizedFinalRequest(request)
	for _, marker := range []string{
		" what happened ", " what went wrong ", " why did that ",
		" why did it ", " continue ", " pick it back up ",
		" try again ", " resume ", " what were we ", " where were we ",
		" what do you mean ", " did it work ", " is it done ",
		" how so ", " why ",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	if len(strings.Fields(normalized)) <= 14 {
		for _, marker := range []string{
			" it ", " that ", " this ", " those ", " them ",
			" previous ", " earlier ", " last one ",
		} {
			if strings.Contains(normalized, marker) {
				return true
			}
		}
	}
	return false
}

func normalizedFinalRequest(request string) string {
	tokens := finalAnswerTokenPattern.FindAllString(
		strings.ToLower(strings.TrimSpace(request)), -1,
	)
	return " " + strings.Join(tokens, " ") + " "
}

func answerLeadSharesThreadSubject(answer string, history []protocol.Message) bool {
	lastUser := -1
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Role == protocol.RoleUser {
			lastUser = index
			break
		}
	}
	prior := history
	if lastUser >= 0 {
		prior = history[:lastUser]
	}
	if len(prior) == 0 {
		return true
	}
	start := len(prior) - 6
	if start < 0 {
		start = 0
	}
	subject := make(map[string]struct{})
	for _, message := range prior[start:] {
		for _, token := range finalAnswerTokenPattern.FindAllString(
			strings.ToLower(message.Content), -1,
		) {
			if len(token) >= 4 && !threadSubjectStopword(token) {
				subject[token] = struct{}{}
			}
		}
	}
	if len(subject) == 0 {
		return true
	}
	lead := []rune(strings.TrimSpace(answer))
	if len(lead) > 320 {
		lead = lead[:320]
	}
	for _, token := range finalAnswerTokenPattern.FindAllString(
		strings.ToLower(string(lead)), -1,
	) {
		if _, ok := subject[token]; ok {
			return true
		}
	}
	return false
}

func threadSubjectStopword(token string) bool {
	if finalAnswerStopword(token) {
		return true
	}
	switch token {
	case "conversation", "transcript", "message", "assistant", "working",
		"happened", "continue", "started", "ready", "actual", "really":
		return true
	default:
		return false
	}
}

func finalAnswerHonestFallback(events []ToolExecution) string {
	completed := 0
	needsAttention := 0
	for _, event := range events {
		if event.Error == "" {
			completed++
		} else {
			needsAttention++
		}
	}
	if completed == 0 && needsAttention == 0 {
		return "I could not produce a reliable answer to this request, so I will not claim it was completed."
	}
	summary := fmt.Sprintf(
		"I could not produce a reliable final summary, so I will not claim the request is complete. Work details preserve %d completed action",
		completed,
	)
	if completed != 1 {
		summary += "s"
	}
	if needsAttention > 0 {
		summary += fmt.Sprintf(" and %d action", needsAttention)
		if needsAttention != 1 {
			summary += "s"
		}
		summary += " that need attention"
	}
	return summary + "."
}

// The acceptance gate and the enforced exploration budget read the SAME
// classifiers, so a request that earns an exploration budget is exactly the one
// whose answer is held to the source requirement. Two lists would drift.
func requestsResearchAnswer(request string) bool {
	return liveness.ClassifyShape(request) == liveness.ShapeResearch
}

func hasExplorationEvidence(events []ToolExecution) bool {
	for _, event := range events {
		if liveness.IsExplorationTool(event.Call.Name) {
			return true
		}
	}
	return false
}

func sharesRequestSubject(request, answer string) bool {
	answerTokens := make(map[string]struct{})
	for _, token := range finalAnswerTokenPattern.FindAllString(
		strings.ToLower(answer), -1,
	) {
		answerTokens[token] = struct{}{}
	}
	for _, token := range finalAnswerTokenPattern.FindAllString(
		strings.ToLower(request), -1,
	) {
		token = strings.Trim(token, ".-")
		if len(token) < 4 || finalAnswerStopword(token) {
			continue
		}
		if _, exists := answerTokens[token]; exists {
			return true
		}
	}
	return false
}

func finalAnswerStopword(token string) bool {
	switch token {
	case "about", "after", "again", "could", "current", "find", "give",
		"into", "look", "please", "report", "research", "thanks", "that",
		"their", "there", "these", "thing", "this", "those", "what",
		"when", "where", "which", "with", "would":
		return true
	default:
		return false
	}
}

func statesResearchLimitation(answer string) bool {
	for _, marker := range []string{
		"could not verify", "couldn't verify", "unable to verify",
		"could not access", "couldn't access", "no reliable source",
		"no relevant result",
	} {
		if strings.Contains(answer, marker) {
			return true
		}
	}
	return false
}
