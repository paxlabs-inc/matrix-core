// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"fmt"
	"strings"

	"matrix/cortex/memory"
)

// failureModeMarkerPrefix / failureModeMarkerSuffix bracket the canonical
// failure-mode marker embedded at the head of a self-authored failure-pattern
// statement, e.g. "[failure-mode:no_progress_stall]". It is the stable dedup
// identity (self-model task 3.2, req.5.2): the consolidation pass matches a
// recurrence of the SAME mode against the ONE belief it already wrote, so
// repeated deaths reinforce a single memory rather than piling up duplicates.
const (
	failureModeMarkerPrefix = "[failure-mode:"
	failureModeMarkerSuffix = "]"
)

// ConsolidateDeathJournal is the self-authoring consolidation pass (self-model
// task 3.2, req.5.1/5.2/5.3): the agent reads its accumulated death-journal
// Events (the durable path first-classed in task 3.1), groups them by failure
// MODE, and writes ONE durable how-I-fail failure-pattern belief per mode —
// reinforcing (refreshing recency + merging evidence on) the existing belief for
// a recurring mode instead of writing a duplicate. It returns the number of
// patterns written or reinforced.
//
// It is a PURE observability + reasoning side-channel (req.5.4): it only reads
// the death journal and writes/updates self-model Belief memories. It never
// signs an envelope, never mutates plan/walk, and cannot perturb the MCL D11
// replay byte-identity invariant — the same guarantee the raw death record and
// the structural self already hold. Best-effort: a store error surfaces to the
// caller, which treats consolidation as advisory and never blocks a respawn on
// it.
func (p *Pager) ConsolidateDeathJournal(ctx context.Context) (int, error) {
	if p == nil || p.cortex == nil {
		return 0, nil
	}
	journal, err := p.DeathJournal(ctx, deathJournalScanCap)
	if err != nil {
		return 0, err
	}
	if len(journal) == 0 {
		return 0, nil
	}

	// Group the journal by failure mode. The journal is newest-first, so the
	// first digest seen for a mode is the freshest example of it.
	type modeAgg struct {
		count    int
		evidence []string
		digest   string
	}
	groups := map[string]*modeAgg{}
	order := make([]string, 0)
	for _, d := range journal {
		mode := deathMode(d.Summary)
		g := groups[mode]
		if g == nil {
			g = &modeAgg{}
			groups[mode] = g
			order = append(order, mode)
		}
		g.count++
		g.evidence = append(g.evidence, d.URI)
		if g.digest == "" {
			g.digest = deathStuckDigest(d.Summary)
		}
	}

	// Index the failure-pattern beliefs already authored, by mode, for dedup.
	model, err := p.SelfModel(ctx)
	if err != nil {
		return 0, err
	}
	existing := map[string]FailurePattern{}
	for _, fp := range model.FailurePatterns {
		if m := patternMode(fp.Statement); m != "" {
			existing[m] = fp
		}
	}

	written := 0
	for _, mode := range order {
		g := groups[mode]
		statement := failurePatternStatement(mode, g.count, g.digest)
		if fp, ok := existing[mode]; ok {
			// Recurrence of a known mode: reinforce the ONE belief (refresh its
			// recency so salience rises, merge the new supporting deaths) rather
			// than write a duplicate (req.5.2).
			if _, uerr := p.reinforceFailurePattern(fp.URI, statement, mergeUnique(fp.DerivedFrom, g.evidence)); uerr == nil {
				written++
			}
			continue
		}
		if _, werr := p.WriteFailurePattern(ctx, statement, g.evidence); werr == nil {
			written++
		}
	}
	return written, nil
}

// reinforceFailurePattern updates an existing failure-pattern belief in place —
// a new cortex Version with refreshed recency (so the recurring mode's salience
// rises) and the merged supporting-death evidence — WITHOUT creating a second
// memory. The head (importance, subject) is preserved by cortex.Update.
func (p *Pager) reinforceFailurePattern(uri, statement string, evidence []string) (string, error) {
	updated, err := p.cortex.Update(
		memory.URI(uri),
		memory.BeliefData{
			SchemaVersion: 1,
			Statement:     statement,
			Subject:       selfModelSubject,
			Stance:        memory.StanceBelieve,
			EvidenceFor:   cleanStrings(evidence),
		},
		p.writeMeta(),
	)
	return string(updated), err
}

// failurePatternStatement renders the durable how-I-fail statement for one mode,
// leading with the canonical mode marker (the dedup identity) and a first-person
// pattern the agent can act on at reasoning time.
func failurePatternStatement(mode string, count int, digest string) string {
	var b strings.Builder
	b.WriteString(failureModeMarkerPrefix)
	b.WriteString(mode)
	b.WriteString(failureModeMarkerSuffix)
	fmt.Fprintf(&b, " I tend to die by %s", humanMode(mode))
	if count > 1 {
		fmt.Fprintf(&b, " (seen %d times)", count)
	}
	b.WriteString(". ")
	b.WriteString(modeLesson(mode))
	if digest = strings.TrimSpace(digest); digest != "" {
		b.WriteString(" Most recent example: ")
		b.WriteString(clip(digest, 200))
	}
	return b.String()
}

// deathMode extracts the failure MODE from a durable death summary: the loop
// death reason from the rich loop-state suffix ("reason=<mode>") when present,
// else the failure class from the prefix ("class=<class>"), else "unknown".
func deathMode(summary string) string {
	if r := extractTagValue(summary, "reason="); r != "" {
		return r
	}
	if c := extractTagValue(summary, "class="); c != "" {
		return "class:" + c
	}
	return "unknown"
}

// deathStuckDigest pulls the where-it-got-stuck digest out of a durable death
// summary — the text after "Where it got stuck: " and before the rich
// loop-state suffix (if any).
func deathStuckDigest(summary string) string {
	const marker = "Where it got stuck: "
	i := strings.Index(summary, marker)
	if i < 0 {
		return ""
	}
	rest := summary[i+len(marker):]
	if j := strings.Index(rest, " [loop-state:"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// patternMode extracts the mode marker from a failure-pattern statement, or ""
// when the statement carries no marker (a hand-authored belief, not one this
// pass wrote — never a dedup target).
func patternMode(statement string) string {
	return extractBracketMarker(statement, failureModeMarkerPrefix, failureModeMarkerSuffix)
}

// extractTagValue returns the value of a "key=" token in s, read up to the next
// space or "]" (the loop-state suffix delimiters). "" when the key is absent.
func extractTagValue(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	end := strings.IndexAny(rest, " ]")
	if end < 0 {
		end = len(rest)
	}
	return strings.TrimSpace(rest[:end])
}

// extractBracketMarker returns the inner value of a "<prefix>value<suffix>"
// marker at the start of s, or "" when absent.
func extractBracketMarker(s, prefix, suffix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// humanMode / modeLesson turn a machine mode id into readable prose + a concrete
// lesson the agent can apply at reasoning time to avoid the pattern (req.5.3).
func humanMode(mode string) string {
	switch mode {
	case "no_progress_stall":
		return "repeating the same step without making progress"
	case "step_budget":
		return "running out of my step budget before finishing"
	case "unproductive_cap":
		return "burning attempts without moving the task forward"
	default:
		if v := strings.TrimPrefix(mode, "class:"); v != mode {
			return "a " + v + "-class failure"
		}
		return mode
	}
}

// clip trims s to at most max runes, appending an ellipsis when truncated, so a
// long where-it-got-stuck example never bloats a failure-pattern statement.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func modeLesson(mode string) string {
	switch mode {
	case "no_progress_stall":
		return "When I catch myself re-running an operation whose result I already have, I should change tactic — read the existing result, decompose the work, or ask — instead of repeating the call."
	case "step_budget":
		return "When a task is large, I should decompose it early (spawn_subagents) or narrow scope, rather than grinding step-by-step until the budget runs out."
	case "unproductive_cap":
		return "When several attempts in a row produce nothing new, I should step back and rethink the approach rather than keep pushing the same one."
	default:
		return "I should recognize this shape early and change approach before it recurs."
	}
}
