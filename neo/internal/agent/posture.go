// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"fmt"
	"strings"
	"unicode"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/o1"
)

type InteractionPosture string

const (
	PostureConversation InteractionPosture = "conversation"
	PostureExploration  InteractionPosture = "exploration"
	PostureExecution    InteractionPosture = "execution"
)

var executionIntentPhrases = []string{
	"create ", "write ", "edit ", "update ", "change ", "modify ", "delete ",
	"remove ", "install ", "deploy ", "restart ", "run ", "execute ", "send ",
	"publish ", "submit ", "book ", "buy ", "sell ", "transfer ", "pay ",
	"approve ", "authorize ", "sign ", "commit ", "push ", "merge ", "fix ",
	"implement ", "build ", "migrate ", "provision ", "configure ", "schedule ",
	"upload ", "download ", "rename ", "move ", "copy ", "fill ", "click ",
	"log in", "login", "sign in", "place an order", "complete the operation",
}

var explorationIntentPhrases = []string{
	"research ", "search ", "look up", "lookup ", "find out", "investigate ",
	"inspect ", "check ", "verify ", "compare ", "analyze ", "analyse ",
	"read ", "show me", "what is the status", "what's the status", "list ",
	"summarize ", "summarise ", "browse ", "fetch ",
}

func ClassifyInteractionPosture(cfg config.Config, objective string) InteractionPosture {
	if !cfg.InteractionPosture {
		return PostureExecution
	}
	text := normalizedIntent(objective)
	if containsIntentPhrase(text, executionIntentPhrases) {
		return PostureExecution
	}
	if containsIntentPhrase(text, explorationIntentPhrases) {
		return PostureExploration
	}
	return PostureConversation
}

func normalizedIntent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return " " + strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return r
		}
		return ' '
	}, value) + " "
}

func containsIntentPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, " "+strings.TrimSpace(phrase)+" ") || strings.Contains(text, strings.TrimSpace(phrase)+" ") {
			return true
		}
	}
	return false
}

func (a *Agent) TurnPosture() InteractionPosture {
	if a == nil || a.turn == nil {
		return PostureExecution
	}
	return a.turn.posture
}

func (a *Agent) executionPosture() bool {
	return !a.cfg.InteractionPosture || a.turn == nil || a.turn.posture == PostureExecution
}

func readOnlyToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || mutatingToolName(name) {
		return false
	}
	for _, marker := range []string{
		"read", "search", "fetch", "get", "list", "status", "check", "inspect",
		"query", "find", "stat", "snapshot", "screenshot", "recall", "history",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func mutatingToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{
		"write", "edit", "create", "delete", "remove", "move", "copy", "rename",
		"mkdir", "touch", "patch", "exec", "shell", "terminal", "command", "run",
		"send", "publish", "submit", "upload", "deploy", "restart", "install",
		"purchase", "buy", "sell", "transfer", "payment", "approve", "authorize",
		"sign", "commit", "push", "merge", "click", "fill", "navigate", "login",
		"schedule", "book", "cancel", "set_", "update", "mutate", "spawn",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func callsRequireExecution(calls []llm.ToolCall) bool {
	for _, call := range calls {
		if !readOnlyToolName(call.Function.Name) {
			return true
		}
	}
	return false
}

func (a *Agent) preparePosture() error {
	if a.executionPosture() {
		return a.prepareExecutionFrame()
	}
	var selected []llm.Tool
	if a.turn.posture == PostureExploration {
		for _, schema := range a.allSchemas {
			if readOnlyToolName(schema.Function.Name) {
				selected = append(selected, schema)
			}
		}
	}
	return a.installPostureSchemas(selected)
}

func (a *Agent) installPostureSchemas(selected []llm.Tool) error {
	if a.tools != nil && len(selected) > 0 {
		names := make([]string, 0, len(selected))
		for _, schema := range selected {
			if schema.Function.Name != readOverflowTool {
				names = append(names, schema.Function.Name)
			}
		}
		if err := a.tools.Preflight(names); err != nil {
			return fmt.Errorf("posture live capability preflight: %w", err)
		}
	}
	a.schemas = append([]llm.Tool(nil), selected...)
	a.advertised = make(map[string]struct{}, len(a.schemas))
	for _, schema := range a.schemas {
		a.advertised[schema.Function.Name] = struct{}{}
	}
	a.schemaTokens = estimateToolTokens(a.schemas)
	a.schemaBytes = estimateToolBytes(a.schemas)
	return nil
}

func (a *Agent) prepareExecutionFrame() error {
	a.turn.posture = PostureExecution
	a.turn.contract = o1.Compile(o1.CompileInput{
		Request: a.turn.objective,
		RepositoryRules: []string{
			"Preserve unrelated workspace changes.",
			"Do not use fake, mock, stub, canned, or placeholder implementations or verification.",
		},
	})
	var selected o1.RuntimeCapabilities
	var err error
	if a.cfg.O1ConstrainTools {
		selected, err = o1.CompileRuntimeCapabilities(a.turn.contract, a.allSchemas)
	} else {
		selected, err = o1.CompileAllCapabilities(a.allSchemas)
	}
	if err != nil {
		return fmt.Errorf("o1 capability preflight: %w", err)
	}
	if a.tools != nil {
		names := make([]string, 0, len(selected.Tools))
		for _, schema := range selected.Tools {
			if schema.Function.Name != readOverflowTool {
				names = append(names, schema.Function.Name)
			}
		}
		if err := a.tools.Preflight(names); err != nil {
			return fmt.Errorf("o1 live capability preflight: %w", err)
		}
	}
	a.turn.runLedger = o1.NewRunLedger(a.turnIntentID(), a.turn.contract.ID, a.turn.contract.Version)
	if restored := a.restoreO1State(a.turn.contract.ID); restored != nil {
		a.turn.runLedger = restored
	}
	a.turn.verifiers = o1.CompileVerifiers(a.turn.contract.Criteria)
	for _, manifest := range selected.Manifests {
		a.turn.manifests[manifest.Operation] = manifest
	}
	return a.installPostureSchemas(selected.Tools)
}

func (a *Agent) promoteToExecution() error {
	if a.executionPosture() {
		return nil
	}
	a.turn.promoted = true
	return a.prepareExecutionFrame()
}

func (a *Agent) promoteForIntent(objective string) error {
	if !a.cfg.InteractionPosture || a.turn == nil || a.turn.posture == PostureExecution {
		return nil
	}
	next := ClassifyInteractionPosture(a.cfg, objective)
	if next == PostureExecution {
		return a.promoteToExecution()
	}
	if next == PostureExploration && a.turn.posture == PostureConversation {
		a.turn.posture = PostureExploration
		return a.preparePosture()
	}
	return nil
}
