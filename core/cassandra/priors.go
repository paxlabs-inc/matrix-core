// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import "strings"

// priors.go — deterministic completeness priors ([adjudicator].priors).
//
// These extractors run first and free: they are pure string/structural signals
// over the ground-truth evidence digest that flag the obvious incompleteness
// shapes an LLM auditor would otherwise have to notice. They never decide the
// verdict on their own (that would change the MCL re-home's behaviour); they
// short-circuit the obvious, feed the auditor a hint, and inform the
// escalation tier on high-stakes turns.

// PriorInput is the raw material the deterministic priors scan. RequestedDeliverables
// and ProducedResults are optional caller hints; 0 means "unknown" and that
// signal is skipped.
type PriorInput struct {
	// Evidence is the ground-truth execution digest (real tool calls + real
	// results) the agent claims completion against.
	Evidence string
	// FinishReason is the provider stop reason of the agent's final turn, when
	// known (e.g. "length" means the answer was cut off mid-stream).
	FinishReason string
	// RequestedDeliverables / ProducedResults let a caller that already counts
	// these (the MCL plan tree, Neo's tool calls) surface an under-production
	// signal. 0 = unknown.
	RequestedDeliverables int
	ProducedResults       int
}

// PriorSignal is the structured output of the deterministic pre-pass.
type PriorSignal struct {
	// TruncationSuspect — the evidence contains truncation markers, so a
	// result the agent treated as complete may have been cut off
	// ([scope].in_scope "truncated_or_partial_reads_treated_as_complete").
	TruncationSuspect bool
	// DegenerateEvidence — the evidence is empty or a "nothing ran" sentinel,
	// so a completion claim has no ground truth behind it at all.
	DegenerateEvidence bool
	// SelfReportedGaps — explicit "could not" / "unable" / TODO-style strings
	// the agent itself emitted, which betray an unfinished task.
	SelfReportedGaps []string
	// FinishedByLength — the final turn stopped on a length cap (answer cut off).
	FinishedByLength bool
	// UnderProduced — fewer results were produced than deliverables requested
	// (only set when both counts are known/positive).
	UnderProduced bool
}

// FlagsCompletionRisk reports whether any prior fired. A true value means the
// auditor should look hard (and, on a high-stakes turn, may warrant escalation);
// it is NOT itself a verdict.
func (p PriorSignal) FlagsCompletionRisk() bool {
	return p.TruncationSuspect ||
		p.DegenerateEvidence ||
		p.FinishedByLength ||
		p.UnderProduced ||
		len(p.SelfReportedGaps) > 0
}

// truncationMarkers are substrings that strongly suggest a tool result or read
// was cut off before completion — i.e. the AGENT itself saw only part of the
// data (a partial file/list read, a capped result). Kept conservative and
// keyed on explicit truncation LANGUAGE to avoid false positives; we
// deliberately do NOT key on the digest builder's own "…" compression marker,
// because that truncates the auditor's copy, not what the agent perceived.
var truncationMarkers = []string{
	"truncated",
	"output cut off",
	"output too long",
	"response was truncated",
	"see full output",
	"+n more",
	"showing lines",
}

// degenerateSentinels are the exact "nothing of substance ran" digests the MCL
// digest builder emits, plus the empty string.
var degenerateSentinels = []string{
	"(no plan executed)",
	"(plan executed but produced no recorded output)",
}

// selfReportGapPhrases are agent-emitted admissions of an unfinished step.
var selfReportGapPhrases = []string{
	"could not",
	"couldn't",
	"unable to",
	"was not able to",
	"were not able to",
	"failed to",
	"not implemented",
	"todo",
	"fixme",
	"i don't have access",
	"do not have access",
	"no access to",
}

// ScanPriors runs the deterministic pre-pass over the evidence and returns the
// signals that fired. It is pure and allocation-light.
func ScanPriors(in PriorInput) PriorSignal {
	var sig PriorSignal
	ev := strings.ToLower(in.Evidence)
	trimmed := strings.TrimSpace(ev)

	if trimmed == "" {
		sig.DegenerateEvidence = true
	} else {
		for _, s := range degenerateSentinels {
			if strings.Contains(trimmed, s) {
				sig.DegenerateEvidence = true
				break
			}
		}
	}

	if !sig.DegenerateEvidence {
		for _, m := range truncationMarkers {
			if strings.Contains(ev, m) {
				sig.TruncationSuspect = true
				break
			}
		}
	}

	// selfReportGapPhrases is a distinct set, so each match contributes at
	// most one entry — no dedup needed.
	for _, phrase := range selfReportGapPhrases {
		if strings.Contains(ev, phrase) {
			sig.SelfReportedGaps = append(sig.SelfReportedGaps, phrase)
		}
	}

	sig.FinishedByLength = strings.EqualFold(strings.TrimSpace(in.FinishReason), "length")

	if in.RequestedDeliverables > 0 && in.ProducedResults > 0 &&
		in.ProducedResults < in.RequestedDeliverables {
		sig.UnderProduced = true
	}

	return sig
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
