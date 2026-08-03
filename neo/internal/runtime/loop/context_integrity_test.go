// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"errors"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

func TestRequestMessagesKeepsCurrentUserAuthoritativeOverForeignMemory(t *testing.T) {
	history := []protocol.Message{
		{Role: protocol.RoleUser, Content: "hi neo"},
		{Role: protocol.RoleAssistant, Content: "Hey Andrew. What's up?"},
		{Role: protocol.RoleUser, Content: "test the native workspace file tools"},
		{Role: protocol.RoleAssistant, Content: "The native tool attempt failed before dispatch."},
		{Role: protocol.RoleUser, Content: "what happened?"},
	}
	messages := requestMessages(
		"stable system",
		history,
		"",
		"Foreign cross-thread memory: Machine Mail neo-o1@machinemail.org",
	)
	if len(messages) != len(history)+2 {
		t.Fatalf("messages = %+v", messages)
	}
	if messages[1].Role != protocol.RoleSystem ||
		messages[1].Content == "" {
		t.Fatalf("activation was not kept in the system prefix: %+v", messages)
	}
	last := messages[len(messages)-1]
	if last.Role != protocol.RoleUser || last.Content != "what happened?" {
		t.Fatalf("latest user request lost authority: %+v", last)
	}
}

func TestIncompletePreservesPhaseRepairsAndProviderError(t *testing.T) {
	const turnID = "diagnostic-incomplete"
	store := realTurnStore(t, turnID, "inspect the workspace")
	runtimeLoop := &Loop{
		store:  store,
		config: Config{TurnID: turnID, ConversationID: "diagnostic-conversation"},
	}
	checkpoint := turnstate.Checkpoint{
		ProviderAttempts: 3,
		Runtime:          []byte(`{"textual_repairs":1,"expectation_repairs":2,"final_repairs":1,"completion_defers":2}`),
	}
	incomplete := runtimeLoop.incomplete(
		t.Context(), "provider", checkpoint, Response{}, time.Now(),
		"resume_from_checkpoint", errors.New("invalid structured native tool call"),
	)
	if incomplete.Phase != "provider" || incomplete.Repairs.Textual != 1 ||
		incomplete.Repairs.Expectation != 2 || incomplete.Repairs.FinalAnswer != 1 ||
		incomplete.Repairs.CompletionDeferrals != 2 ||
		!strings.Contains(incomplete.ProviderError, "invalid structured native tool call") {
		t.Fatalf("incomplete diagnostics = %+v", incomplete)
	}
}

func TestFollowUpRejectsForeignLeadingSubject(t *testing.T) {
	history := []protocol.Message{
		{Role: protocol.RoleUser, Content: "can you try to read a workspace file and list dirs with the native tools"},
		{Role: protocol.RoleAssistant, Content: "Let me run through a few native workspace tools right now."},
		{Role: protocol.RoleAssistant, Content: "The attempt ended incomplete."},
		{Role: protocol.RoleUser, Content: "what happened?"},
	}
	foreign := "Based on the conversation, you were setting up Machine Mail for neo-o1@machinemail.org to register web accounts."
	if accepted, _ := finalAnswerAddressesRequestWithHistory(
		"what happened?", foreign, nil, history,
	); accepted {
		t.Fatal("foreign Machine Mail context was accepted for a native-tool follow-up")
	}
	grounded := "The native workspace-tool attempt failed before it dispatched a file or directory read."
	if accepted, reason := finalAnswerAddressesRequestWithHistory(
		"what happened?", grounded, nil, history,
	); !accepted {
		t.Fatalf("same-thread answer rejected: %s", reason)
	}
}

func TestInspectionRequestCannotCompleteWithoutToolEvidence(t *testing.T) {
	request := "can you try to read a workspace file and list dirs"
	if accepted, _ := finalAnswerAddressesRequest(request, "I will inspect it now.", nil); accepted {
		t.Fatal("inspection request completed with zero tool evidence")
	}
	if accepted, reason := finalAnswerAddressesRequest(
		request,
		"The workspace directory contains the requested file.",
		[]ToolExecution{{Call: protocol.NormalizedToolCall{Name: "list_directory"}}},
	); !accepted {
		t.Fatalf("successful inspection evidence rejected: %s", reason)
	}
}

func TestExplanationDoesNotEraseExplicitActionEvidenceRequirement(t *testing.T) {
	if RequestRequiresToolEvidence("explain the command") {
		t.Fatal("pure explanation incorrectly required execution evidence")
	}
	if !RequestRequiresToolEvidence("explain the command and then run it for me") {
		t.Fatal("explicit action was hidden behind explanatory wording")
	}
}

func TestEvidenceCompletionGateFailsClosedUntilVerifiedExecution(t *testing.T) {
	gate := NewEvidenceCompletionGate(true)
	if decision, _ := gate.CheckCompletion(t.Context()); decision.Ready {
		t.Fatal("required evidence gate started ready")
	}
	_ = gate.ObserveToolExecution(t.Context(), ToolExecution{
		Error: "permission denied", Citation: &cortex.ToolEventCitation{},
		MatchVerdict: string(cortex.ToolMatchMismatched),
	})
	if decision, _ := gate.CheckCompletion(t.Context()); decision.Ready {
		t.Fatal("failed or mismatched evidence opened completion")
	}
	_ = gate.ObserveToolExecution(t.Context(), ToolExecution{
		Citation:     &cortex.ToolEventCitation{},
		MatchVerdict: string(cortex.ToolMatchMatched),
	})
	if decision, _ := gate.CheckCompletion(t.Context()); !decision.Ready {
		t.Fatal("verified matched evidence did not open completion")
	}
}
