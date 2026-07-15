// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

// The ORACLE brief content policy (task 5.5, req 16/17.1) — two halves:
//
//  1. the POLICY PROMPT (briefPolicyRules, folded into the runner's objective):
//     at most five news items, every news claim from fresh in-run retrieval
//     with a publication date + source link + one-line why-relevant, one or
//     two ROTATING recommendations (not every category daily), a small "Neo
//     can help today" section from the stated day-to-day goals, current facts
//     clearly distinguished from recommendations, and no sponsored/affiliate
//     content without clear disclosure;
//
//  2. the DETERMINISTIC POST-CHECK (briefPolicyViolation, run before delivery):
//     the checks that are mechanically decidable on the composed text — the
//     source-link budget (a brief citing more distinct links than five news
//     items + two recommendations could carry has violated the item caps) and
//     the no-repeat property (req 17.1: a source URL already delivered in a
//     recent brief may not be re-delivered). A violating brief is NOT
//     delivered and the date is NOT marked, so the next wake retries.

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// briefMaxNewsItems is the news-item cap (req 16.1).
	briefMaxNewsItems = 5
	// briefMaxRecommendations is the rotating-recommendation cap (req 16.2).
	briefMaxRecommendations = 2
	// briefMaxLinks is the deterministic source-link budget: five news items
	// (one source link each) plus two recommendations.
	briefMaxLinks = briefMaxNewsItems + briefMaxRecommendations
	// briefNoRepeatWindow is how many recent deliveries the no-repeat set spans.
	briefNoRepeatWindow = 14
)

// briefURLPattern extracts http(s) source links from a composed brief.
var briefURLPattern = regexp.MustCompile(`https?://[^\s)\]>"']+`)

// extractBriefURLs returns the distinct source links in a composed brief, in
// order of first appearance, with trailing punctuation trimmed.
func extractBriefURLs(text string) []string {
	raw := briefURLPattern.FindAllString(text, -1)
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, u := range raw {
		u = strings.TrimRight(u, ".,;:!?")
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// briefRecMarker is the line prefix the policy prompt requires on every
// recommendation. It does double duty: it keeps recommendations mechanically
// distinguishable from reported facts (req 16.3), and it is the durable
// recommendation IDENTIFIER the history records and the no-repeat set matches
// on (req 17.1).
const briefRecMarker = "Recommendation:"

// extractBriefRecs returns the recommendation identifiers in a composed brief —
// the text of each marked recommendation line, deduped, in order.
func extractBriefRecs(text string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*• "))
		if !strings.HasPrefix(line, briefRecMarker) {
			continue
		}
		rec := strings.TrimSpace(strings.TrimPrefix(line, briefRecMarker))
		if rec == "" {
			continue
		}
		key := strings.ToLower(rec)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, rec)
	}
	return out
}

// briefPolicyViolation runs the deterministic post-check over a composed brief
// against the recently-delivered recommendations + source URLs. Returns ""
// when the brief passes, else a one-line reason (logged; the brief is not
// delivered).
func briefPolicyViolation(text string, recentRecs, recentURLs []string) string {
	urls := extractBriefURLs(text)
	if len(urls) > briefMaxLinks {
		return fmt.Sprintf("cites %d distinct source links (budget %d: %d news items + %d recommendations)", len(urls), briefMaxLinks, briefMaxNewsItems, briefMaxRecommendations)
	}
	recs := extractBriefRecs(text)
	if len(recs) > briefMaxRecommendations {
		return fmt.Sprintf("carries %d recommendations (max %d)", len(recs), briefMaxRecommendations)
	}
	recentURLSet := make(map[string]struct{}, len(recentURLs))
	for _, u := range recentURLs {
		recentURLSet[u] = struct{}{}
	}
	for _, u := range urls {
		if _, dup := recentURLSet[u]; dup {
			return fmt.Sprintf("repeats an already-delivered source (%s) — news sources must not repeat without new context", u)
		}
	}
	recentRecSet := make(map[string]struct{}, len(recentRecs))
	for _, r := range recentRecs {
		recentRecSet[strings.ToLower(strings.TrimSpace(r))] = struct{}{}
	}
	for _, r := range recs {
		if _, dup := recentRecSet[strings.ToLower(r)]; dup {
			return fmt.Sprintf("repeats an already-delivered recommendation (%q)", r)
		}
	}
	return ""
}

// briefPolicyRules renders the content-policy block of the runner's objective
// (req 16), including the no-repeat set from the durable history (req 17.1).
func briefPolicyRules(noRepeatRecs, noRepeatURLs []string) string {
	var b strings.Builder
	b.WriteString("Content policy for the brief:\n")
	fmt.Fprintf(&b, "- At most %d news items across their interests. EVERY news claim must come from a retrieval you ran in THIS session — never your own memory — and each item carries its publication date, its source link, and one short line on why it's relevant to them.\n", briefMaxNewsItems)
	fmt.Fprintf(&b, "- One or two rotating recommendations at most — pick %d or fewer categories they care about and vary which ones day to day; never every category every day. Match their stated adventurousness. Begin each recommendation's line with \"%s\" — that is how the reader tells your suggestions from reported news.\n", briefMaxRecommendations, briefRecMarker)
	b.WriteString("- A small \"Neo can help today\" section drawn from their stated day-to-day goals: one or two concrete things you could take off their plate today.\n")
	b.WriteString("- Keep current facts clearly separate from recommendations — a reader must never mistake your suggestion for reported news.\n")
	b.WriteString("- No sponsored or affiliate recommendations of any kind without clear disclosure.\n")
	if len(noRepeatRecs) > 0 || len(noRepeatURLs) > 0 {
		b.WriteString("- Do NOT repeat anything already delivered in recent briefs unless genuinely new context exists:\n")
		if len(noRepeatRecs) > 0 {
			fmt.Fprintf(&b, "  Recently recommended: %s\n", strings.Join(noRepeatRecs, "; "))
		}
		if len(noRepeatURLs) > 0 {
			fmt.Fprintf(&b, "  Recently cited sources: %s\n", strings.Join(noRepeatURLs, " "))
		}
	}
	return b.String()
}
