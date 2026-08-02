// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// prediction.go — Mechanism 3 of the epistemic core (epistemic-core req.6):
// prediction-carrying dispatch. Probe-class tool calls (network/endpoint/
// search/exec probes) carry a one-line stated expectation of outcome shape
// (the `expect` argument); mismatch is detected deterministically where
// possible (error, HTTP status, empty/nonempty, JSON parse) and by a cheap
// model otherwise, and is a first-class belief-update event: the hypothesis
// premise that licensed the probe takes the hit, structured guidance renders,
// and a per-strategy meter accumulates across differently-argued probes of
// one strategy — five guessed paths on one host are ONE strategy with five
// mismatches, not five fresh attempts.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"matrix/neo/internal/llm"
)

// expectParamDescription is the schema-level advertisement of the `expect`
// argument. Living in every tool's parameter schema (not just prose in the
// charter) is what makes the model actually include it call after call.
const expectParamDescription = "REQUIRED on every call: one short line predicting this call's outcome shape (e.g. \"200 with a JSON array of candles\", \"exit 0 listing the config files\"). The system lifts it off the call before the tool runs — it is your stated prediction, not a tool input."

// injectExpectParam returns a copy of the advertised schemas with an `expect`
// string property added to every tool's parameters (req.6.1 made universal:
// every call carries a prediction, not just probe-class calls). Copy-on-write:
// the Manager's own schema maps are never mutated — Schemas() hands out the
// SAME underlying parameter maps on every call, so an in-place edit here
// would poison every later consumer (sub-agents, the capability surface).
func injectExpectParam(schemas []llm.Tool) []llm.Tool {
	out := make([]llm.Tool, len(schemas))
	for i, t := range schemas {
		params := make(map[string]interface{}, len(t.Function.Parameters)+1)
		for k, v := range t.Function.Parameters {
			params[k] = v
		}
		props := map[string]interface{}{}
		if raw, ok := params["properties"].(map[string]interface{}); ok {
			for k, v := range raw {
				props[k] = v
			}
		}
		if _, exists := props["expect"]; !exists {
			props["expect"] = map[string]interface{}{
				"type":        "string",
				"description": expectParamDescription,
			}
		}
		params["properties"] = props
		if params["type"] == nil {
			params["type"] = "object"
		}
		t.Function.Parameters = params
		out[i] = t
	}
	return out
}

// popExpect removes and returns the `expect` argument (the stated expectation)
// from a tool call's args, so the underlying tool never sees it.
func popExpect(args map[string]interface{}) string {
	if args == nil {
		return ""
	}
	raw, ok := args["expect"]
	if !ok {
		return ""
	}
	delete(args, "expect")
	s, _ := raw.(string)
	return strings.TrimSpace(s)
}

// probeStrategy classifies a tool call as probe-class and returns its strategy
// key. The key deliberately IGNORES the varying argument (the guessed path,
// the reworded query) and keeps the invariant part (tool + host / command
// head), so differently-argued guesses of one strategy meter together.
func probeStrategy(name string, args map[string]interface{}) (string, bool) {
	n := strings.ToLower(name)
	argStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	switch {
	case strings.Contains(n, "search"):
		return name, true
	case strings.Contains(n, "fetch") || strings.Contains(n, "http") || strings.Contains(n, "request") ||
		strings.Contains(n, "navigate") || strings.Contains(n, "download") || n == "web_read":
		key := name
		if raw := argStr("url", "uri", "address", "endpoint"); raw != "" {
			if u, err := url.Parse(raw); err == nil && u.Host != "" {
				key = name + "@" + u.Host
			}
		}
		return key, true
	case strings.Contains(n, "exec") || strings.Contains(n, "shell") || strings.Contains(n, "command") ||
		strings.Contains(n, "terminal") || strings.HasPrefix(n, "run_"):
		key := name
		if cmd := argStr("command", "cmd", "script"); cmd != "" {
			if fields := strings.Fields(cmd); len(fields) > 0 {
				key = name + ":" + fields[0]
			}
		}
		return key, true
	}
	return "", false
}

// failureEvidence are the deterministic failure shapes readable off a tool
// result string (probe outcomes are text on this seam).
var (
	httpErrorRe = regexp.MustCompile(`(?i)\b(HTTP[ /]?\d\.\d )?(status(?:\scode)?[:= ]+|\b)(4\d\d|5\d\d)\b`)
	exitCodeRe  = regexp.MustCompile(`(?i)\bexit (?:code|status)[:= ]+([1-9]\d*)\b`)
	errPrefixRe = regexp.MustCompile(`(?i)^\s*(error|failed|failure|not found|no such|cannot|could not|unable to)\b`)
)

// expectPredictsFailure reports whether the stated expectation ALREADY
// predicts a failure shape (probing FOR a 404 is a valid hypothesis: the 404
// then matches).
func expectPredictsFailure(expect string) bool {
	e := strings.ToLower(expect)
	if httpErrorRe.MatchString(e) || exitCodeRe.MatchString(e) || errPrefixRe.MatchString(e) {
		return true
	}
	for _, marker := range []string{"404", "403", "401", "500", "error", "fail", "not found", "refus", "non-zero", "nonzero", "empty"} {
		if strings.Contains(e, marker) {
			return true
		}
	}
	return false
}

// detectMismatch is the deterministic detector (req.6.2): error/exit-code/HTTP-
// status/empty/JSON-parse shapes decide without a model call. Returns
// (mismatch, decided); undecided outcomes fall to the cheap-model comparator.
func detectMismatch(expect, content string, isErr bool) (bool, bool) {
	failed := isErr ||
		httpErrorRe.MatchString(content) ||
		exitCodeRe.MatchString(content) ||
		errPrefixRe.MatchString(content)
	if failed {
		return !expectPredictsFailure(expect), true
	}
	if strings.TrimSpace(content) == "" {
		return !expectPredictsFailure(expect), true
	}
	e := strings.ToLower(expect)
	if strings.Contains(e, "json") {
		trimmed := strings.TrimSpace(content)
		if json.Valid([]byte(trimmed)) {
			return false, true
		}
		// Not raw JSON — could be JSON embedded in a tool envelope; undecided.
		return false, false
	}
	return false, false
}

// predictionComparatorSystem is the cheap-model fallback comparator (req.6.2).
const predictionComparatorSystem = `You compare a stated expectation against an actual tool outcome. Answer with EXACTLY one word: "match" if the outcome has the expected shape, "mismatch" if it does not.`

// compareExpectationModel is the model-assisted comparison for outcomes the
// deterministic detector cannot decide. Fails toward "match" (no belief-update
// event on comparator failure — never fabricate a mismatch).
func (a *Agent) compareExpectationModel(ctx context.Context, expect, content string) bool {
	client := a.cheap
	if client == nil {
		client = a.main
	}
	if client == nil {
		return false
	}
	res, err := client.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			llm.SystemMessage(predictionComparatorSystem),
			llm.UserMessage("Expectation: " + expect + "\n\nActual outcome (may be truncated):\n" + truncate(content, 2000)),
		},
	})
	if err != nil || res == nil {
		return false
	}
	return strings.Contains(strings.ToLower(res.Message.Content), "mismatch")
}

// epistemicPredictionsOn reports whether Mechanism 3 is active.
func (a *Agent) epistemicPredictionsOn() bool {
	return a.cfg.EpistemicPredictions && a.executionPosture()
}

// predictionObserve is the belief-update seam, called from the single-threaded
// result-assembly loops for every dispatched call. For a probe-class call it
// maintains the strategy's hypothesis premise on the ledger (the expectation
// IS a premise: assumption at birth, cited on a match, refuted on a mismatch),
// advances the per-strategy consecutive-mismatch meter, and appends structured
// guidance to the working transcript on a mismatch so the model must read the
// belief update, not just another failure string. Returns whether the call was
// a probe that MISSED its expectation (a missed probe discharges no evidence
// on the task graph).
func (a *Agent) predictionObserve(ctx context.Context, name string, args map[string]interface{}, expect, content string, isErr bool) bool {
	if !a.epistemicPredictionsOn() {
		return false
	}
	strategy, isProbe := probeStrategy(name, args)
	if !isProbe {
		// Non-probe calls join the discipline the moment they state an
		// expectation (every call is asked to — the universal `expect`
		// schema param): same ledger, same meter, same guidance, keyed by
		// tool name. Without one there is nothing to update — refusal at
		// the dispatch seam stays probe-only.
		if expect == "" {
			return false
		}
		strategy = name
	}
	if expect == "" {
		// No stated expectation: nothing to update (the refusal-to-dispatch
		// enforcement lives at the gate, task 3.2). A bare failed probe still
		// counts as missed for evidence purposes.
		return isErr
	}
	if a.turn.mismatchMeter == nil {
		a.turn.mismatchMeter = map[string]int{}
	}
	if a.turn.hypotheses == nil {
		a.turn.hypotheses = map[string]*Premise{}
	}

	// The expectation is a premise with provenance (req.6.2 "a mismatch SHALL
	// update the premise ledger"): seed or refresh the strategy's hypothesis.
	hyp := a.turn.hypotheses[strategy]
	if hyp == nil || hyp.Status == premiseRefuted {
		if hyp != nil {
			// A fresh expectation on a refuted hypothesis IS the revision of
			// that hypothesis — the standing refutation is answered.
			hyp.Revised = true
		}
		if a.turn.ledger == nil {
			a.turn.ledger = &premiseLedger{}
		}
		hyp = a.turn.ledger.add(Premise{
			Statement:  fmt.Sprintf("expectation for %s: %s", strategy, expect),
			Status:     premiseAssumption,
			Hypothesis: true,
		})
		a.turn.hypotheses[strategy] = hyp
	}

	mismatch, decided := detectMismatch(expect, content, isErr)
	if !decided {
		mismatch = a.compareExpectationModel(ctx, expect, content)
	}
	if !mismatch {
		a.turn.mismatchMeter[strategy] = 0
		a.premiseDischarge(hyp, "tool-evidence", truncate(strings.TrimSpace(content), 160))
		return false
	}

	a.turn.mismatchMeter[strategy]++
	count := a.turn.mismatchMeter[strategy]
	a.premiseRefute(hyp, "observed: "+truncate(strings.TrimSpace(content), 160))
	// Bound (req.6.3): N consecutive mismatches on ONE strategy force the
	// tools-stripped revision step before any further dispatch — the epistemic
	// layer fires BEFORE the outer stall failsafes.
	if limit := a.cfg.EpistemicMismatchLimit; limit > 0 && count >= limit &&
		a.turn.revisionPending == "" && a.turn.revisionsThisTurn < maxRevisionsPerTurn {
		a.turn.revisionPending = fmt.Sprintf(
			"You have %d consecutive prediction mismatches on strategy %q — the strategy itself is wrong, not its arguments. This step has NO tools on purpose: diagnose WHY every probe of this strategy misses (what premise does the pattern of failures refute?) and write a revised plan built on a different, grounded approach.",
			count, strategy)
	}
	// Structured guidance (req.6.2): the mismatch is a belief-update event the
	// model must answer, not a failure string it can autocomplete past.
	a.pushGuidance(fmt.Sprintf(
		"Prediction mismatch on strategy %q (%d consecutive): you expected %q, the probe did not return that shape. Update your beliefs — what does this failure imply about your premise? Do not fire another variation of the same probe without a revised hypothesis.",
		strategy, count, expect))
	return true
}
