// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// corpus.go — the priority ops-knowledge pass: RUNBOOK.md and the 9 issue/fix
// chat logs under knowledge/core_chats/. Plus the shared, deterministic
// markdown/text helpers used across passes.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrix/cortex/memory"
)

// --- text helpers (deterministic, zero-dependency) ------------------------

// slugify lowercases s and reduces it to a stable [a-z0-9-] token usable in an
// ingest key.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// condense collapses all runs of whitespace (including newlines) into single
// spaces and trims the result.
func condense(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate clamps s to at most n bytes on a rune boundary, appending an
// ellipsis marker when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " …"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// section is one markdown heading and its body.
type section struct {
	title string
	body  string
}

// splitSections splits md into sections headed by exactly `level` hashes
// followed by a space. Deeper or shallower headings are kept inside the body
// (the prefix check naturally excludes them: "### " does not start with "## ").
func splitSections(md string, level int) []section {
	prefix := strings.Repeat("#", level) + " "
	var (
		secs []section
		cur  *section
		buf  []string
	)
	flush := func() {
		if cur != nil {
			cur.body = strings.TrimSpace(strings.Join(buf, "\n"))
			secs = append(secs, *cur)
		}
		buf = nil
	}
	for _, ln := range strings.Split(md, "\n") {
		if strings.HasPrefix(ln, prefix) {
			flush()
			cur = &section{title: strings.TrimSpace(strings.TrimPrefix(ln, prefix))}
			continue
		}
		if cur != nil {
			buf = append(buf, ln)
		}
	}
	flush()
	return secs
}

// headings returns the titles of every level-2 and level-3 heading in md, in
// document order — a deterministic outline of a chat transcript.
func headings(md string) []string {
	var out []string
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "### ") {
			out = append(out, strings.TrimSpace(t[4:]))
		} else if strings.HasPrefix(t, "## ") {
			out = append(out, strings.TrimSpace(t[3:]))
		}
	}
	return out
}

// tokenSet returns the set of lowercased alphanumeric tokens in s.
func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		out[f] = struct{}{}
	}
	return out
}

// significantKeywords returns lowercased tokens of length >= 4 from s, used to
// link incidents/chats back to failure-mode patterns.
func significantKeywords(s string) []string {
	seen := map[string]struct{}{}
	var out []string
	for tok := range tokenSet(s) {
		if len(tok) >= 4 {
			if _, dup := seen[tok]; !dup {
				seen[tok] = struct{}{}
				out = append(out, tok)
			}
		}
	}
	return out
}

// --- RUNBOOK pass ---------------------------------------------------------

func (ig *ingester) ingestRunbook(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	md := string(raw)
	source := "knowledge/core_chats/RUNBOOK.md"

	byTitle := map[string]string{}
	for _, s := range splitSections(md, 2) {
		byTitle[s.title] = s.body
	}

	// Recovery procedure -> authoritative Capability (the fix-procedure).
	var recoveryID memory.ID
	if body, ok := byTitle["Standard Recovery Procedure"]; ok {
		params := []byte(truncate(body, 8000))
		cap := memory.CapabilityData{
			SchemaVersion: 1,
			Subject:       "matrix://hyperpax/network",
			Capability:    "standard-recovery-procedure: stop containers, back up, copy validator2->validator1 data, reset priv_validator_state.json, restart, verify heights",
			Parameters:    params,
			Verified:      true,
			LastObserved:  stableObservedAt,
		}
		recoveryID, err = ig.upsertNode("gideon:runbook:recovery:standard", cap, 10, 1.0,
			"runbook", "hyperpax", "recovery", "fix-procedure")
		if err != nil {
			return err
		}
	}

	// Hard rules -> a single authoritative prohibitions Pattern.
	if body, ok := byTitle["Hard Rules (NEVER Violate)"]; ok {
		pat := memory.PatternData{
			SchemaVersion: 1,
			Statement:     "HyperPax hard rules (NEVER violate): " + condense(body),
			DerivedFrom:   []string{source},
			Strength:      1.0,
			Coverage:      1,
		}
		if _, err := ig.upsertNode("gideon:runbook:hard-rules", pat, 10, 1.0,
			"runbook", "hyperpax", "hard-rules"); err != nil {
			return err
		}
	}

	// Known failure modes -> Pattern per ### subsection.
	if body, ok := byTitle["Known Failure Modes"]; ok {
		for _, fm := range splitSections(body, 3) {
			title := fm.title
			key := "gideon:runbook:failure:" + slugify(title)
			pat := memory.PatternData{
				SchemaVersion: 1,
				Statement:     "Failure mode — " + title + ". " + truncate(condense(fm.body), 1200),
				DerivedFrom:   []string{source},
				Strength:      1.0,
				Coverage:      1,
			}
			id, err := ig.upsertNode(key, pat, 10, 1.0,
				"runbook", "hyperpax", "failure-mode")
			if err != nil {
				return err
			}
			ig.failurePatterns = append(ig.failurePatterns, keywordRef{
				id:       id,
				keywords: significantKeywords(title),
			})
			ig.fixPatterns = append(ig.fixPatterns, patternRef{id: id, text: title + " " + fm.body})

			// resolved-by edge to the standard recovery capability when the
			// section points at it.
			if !recoveryID.IsZero() && strings.Contains(strings.ToLower(fm.body), "standard recovery") {
				if err := ig.linkEdge(id, memory.EdgeReferences, recoveryID, "resolved-by"); err != nil {
					return err
				}
			}
		}
	}

	// Incident history table -> Event per row, linked caused-by to the
	// failure-mode pattern it matches.
	if body, ok := byTitle["Incident History"]; ok {
		for i, row := range parseTableRows(body) {
			if len(row) < 4 {
				continue
			}
			date, block, issue, fix := row[0], row[1], row[2], row[3]
			summary := fmt.Sprintf("%s (block %s): %s — fix: %s", date, block, issue, fix)
			key := fmt.Sprintf("gideon:runbook:incident:%02d:%s", i, slugify(date+"-"+block))
			ev := memory.EventData{
				SchemaVersion: 1,
				Kind:          memory.EventObservation,
				OutcomeVal:    memory.OutcomeSuccess,
				Summary:       condense(summary),
			}
			id, err := ig.upsertNode(key, ev, 7, 1.0, "runbook", "hyperpax", "incident")
			if err != nil {
				return err
			}
			for _, fid := range ig.matchFailures(issue) {
				if err := ig.linkEdge(id, memory.EdgeCausedBy, fid, "caused-by"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// parseTableRows extracts the data rows of the first markdown pipe-table in
// body, dropping the header and the separator row. Each row is a slice of
// trimmed cell strings.
func parseTableRows(body string) [][]string {
	var rows [][]string
	header := false
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "|") {
			if len(rows) > 0 || header {
				break // table ended
			}
			continue
		}
		cells := splitTableRow(t)
		// separator row like |---|---|
		if isSeparatorRow(cells) {
			continue
		}
		if !header {
			header = true // first pipe row is the header; skip it
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

func splitTableRow(t string) []string {
	t = strings.Trim(t, "|")
	parts := strings.Split(t, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func isSeparatorRow(cells []string) bool {
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// --- chat-log pass --------------------------------------------------------

func (ig *ingester) ingestChats(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "RUNBOOK.md" {
			continue
		}
		if err := ig.ingestChat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("chat %s: %w", name, err)
		}
	}
	return nil
}

func (ig *ingester) ingestChat(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	md := string(raw)
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	slug := slugify(base)
	source := "knowledge/core_chats/" + filepath.Base(path)

	// Outline of the transcript: the first handful of headings act as a
	// deterministic diagnosis->fix skeleton (no LLM summarisation).
	outline := headings(md)
	if len(outline) > 12 {
		outline = outline[:12]
	}

	// Event — the incident/session occurrence.
	evKey := "gideon:chat:" + slug + ":event"
	ev := memory.EventData{
		SchemaVersion: 1,
		Kind:          memory.EventObservation,
		OutcomeVal:    memory.OutcomeSuccess,
		Artifacts:     []string{source},
		Summary:       "Ops session — " + base,
	}
	evID, err := ig.upsertNode(evKey, ev, 7, 0.9, "core-chat", "hyperpax", "incident")
	if err != nil {
		return err
	}

	// Pattern — the symptom->diagnosis->fix lesson distilled from the log.
	stmt := "Ops fix pattern from session '" + base + "' (symptom→diagnosis→fix)."
	if len(outline) > 0 {
		stmt += " Sections: " + strings.Join(outline, "; ") + "."
	}
	patKey := "gideon:chat:" + slug + ":pattern"
	pat := memory.PatternData{
		SchemaVersion: 1,
		Statement:     truncate(condense(stmt), 1400),
		DerivedFrom:   []string{source},
		Strength:      0.9,
		Coverage:      1,
	}
	patID, err := ig.upsertNode(patKey, pat, 8, 0.9, "core-chat", "hyperpax", "fix-pattern")
	if err != nil {
		return err
	}
	ig.fixPatterns = append(ig.fixPatterns, patternRef{id: patID, text: base + " " + strings.Join(outline, " ")})

	// derived-from: the lesson came from the incident.
	if err := ig.linkEdge(patID, memory.EdgeDerivedFrom, evID, "derived-from"); err != nil {
		return err
	}
	// caused-by: link the incident to any RUNBOOK failure mode its title names.
	for _, fid := range ig.matchFailures(base + " " + strings.Join(outline, " ")) {
		if err := ig.linkEdge(evID, memory.EdgeCausedBy, fid, "caused-by"); err != nil {
			return err
		}
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
