// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"centra/core/cortexclient"
	"centra/agents/neo/internal/runtime/protocol"
	"centra/agents/neo/internal/runtime/turnstate"
)

func (loop *Loop) commitEvidence(
	ctx context.Context,
	execution *ToolExecution,
	checkpoint turnstate.Checkpoint,
	response Response,
	started time.Time,
) error {
	if loop.evidenceJournal != nil {
		citation, err := loop.evidenceJournal.CommitToolExecution(
			ctx, *execution,
		)
		if err != nil {
			return loop.incomplete(
				context.WithoutCancel(ctx), "evidence_commit",
				checkpoint, response, started,
				"reconcile_effect_then_commit_evidence", err,
			)
		}
		execution.Citation = &citation
	}
	if loop.evidenceObserver != nil {
		if err := loop.evidenceObserver.ObserveToolExecution(
			ctx, *execution,
		); err != nil {
			return loop.incomplete(
				context.WithoutCancel(ctx), "belief_commit",
				checkpoint, response, started,
				"verify_citation_then_resume_belief_update", err,
			)
		}
	}
	return nil
}

// observeDoubt hands one committed execution to the silent voice and returns
// the line to fold into the next request-time clone. Silence when healthy:
// no controller, or no trigger, means the next request is byte-identical to
// the controller absent.
func (loop *Loop) observeDoubt(
	ctx context.Context,
	step int,
	execution ToolExecution,
) (string, bool) {
	if loop.doubt == nil {
		return "", false
	}
	return loop.doubt.ObserveMismatch(ctx, step, execution)
}

// foldDoubt layers the controller line after the content of the LAST
// assistant message in a request-time clone. Content-only by construction:
// it mutates nothing but .Content, never a tool call, role, or a
// tool/user/system message, and the original text is always preserved.
// Returns messages unchanged when there is no assistant message to fold into.
func foldDoubt(
	messages []protocol.Message,
	line string,
) []protocol.Message {
	if strings.TrimSpace(line) == "" {
		return messages
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != protocol.RoleAssistant {
			continue
		}
		folded := make([]protocol.Message, len(messages))
		copy(folded, messages)
		original := folded[index].Content
		if strings.TrimSpace(original) == "" {
			folded[index].Content = line
		} else {
			folded[index].Content = original + "\n\n" + line
		}
		return folded
	}
	return messages
}

func (loop *Loop) subgoalFor(
	call protocol.NormalizedToolCall,
) string {
	if loop.subgoals != nil {
		if id := strings.TrimSpace(loop.subgoals.SubgoalFor(call)); id != "" {
			return id
		}
	}
	return "root"
}

// executionError derives the evidence error string from a tool result. Both
// the dispatch path and the pending-call reconciliation path go through it so
// a resumed turn commits byte-identical evidence to an uninterrupted one — a
// result carrying only a failure class must not lose its error on resume.
func executionError(result ToolResult) string {
	if !result.IsError {
		return ""
	}
	if result.FailureMessage != "" {
		return result.FailureMessage
	}
	return result.FailureClass
}

var evidenceWord = regexp.MustCompile(`[a-z0-9][a-z0-9._:/-]*`)

func matchToolExpectation(expect string, result ToolResult) string {
	expected := strings.ToLower(strings.TrimSpace(expect))
	if expected == "" {
		return cortexclient.ToolMatchUnknown
	}
	actual := strings.ToLower(strings.TrimSpace(string(result.Content)))
	predictsFailure := strings.Contains(expected, "error") ||
		strings.Contains(expected, "fail") ||
		strings.Contains(expected, "non-zero") ||
		strings.Contains(expected, "404") ||
		strings.Contains(expected, "500")
	if result.IsError {
		if predictsFailure {
			return cortexclient.ToolMatchMatched
		}
		return cortexclient.ToolMatchMismatched
	}
	var envelope map[string]interface{}
	if json.Unmarshal(result.Content, &envelope) == nil {
		if timedOut, _ := envelope["timed_out"].(bool); timedOut {
			return cortexclient.ToolMatchMismatched
		}
		if ok, exists := envelope["ok"].(bool); exists && !ok {
			return cortexclient.ToolMatchMismatched
		}
		if exit, exists := numberAsInt(envelope["exit_code"]); exists {
			if strings.Contains(expected, "exit 0") {
				if exit == 0 {
					return cortexclient.ToolMatchMatched
				}
				return cortexclient.ToolMatchMismatched
			}
			if exit != 0 && !predictsFailure {
				return cortexclient.ToolMatchMismatched
			}
		}
	}
	for _, word := range evidenceWord.FindAllString(expected, -1) {
		switch word {
		case "a", "an", "and", "with", "returns", "return", "prints",
			"print", "output", "result", "json", "http", "status", "the",
			"one", "report", "evidence", "marker":
			continue
		}
		if len(word) >= 3 && strings.Contains(actual, word) {
			return cortexclient.ToolMatchMatched
		}
	}
	return cortexclient.ToolMatchUnknown
}

func numberAsInt(value interface{}) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), number == float64(int(number))
	case json.Number:
		parsed, err := strconv.Atoi(number.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}
