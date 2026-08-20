// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/memory"
)

type episodicTrigger struct {
	Fired bool
	Class string
}

type episodicLexeme struct {
	class string
	re    *regexp.Regexp
}

var episodicNegativeGuards = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bremember\s+(?:to|how\s+to)\b`),
	regexp.MustCompile(`(?i)\bremind\s+me\s+to\b`),
}

var episodicLexicon = []episodicLexeme{
	{class: "remembrance", re: regexp.MustCompile(`(?i)\bremember\s+(?:when|how|that)\b`)},
	{class: "remembrance", re: regexp.MustCompile(`(?i)\brecall\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\byou\s+(?:said|mentioned|told\s+me)\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\bwe\s+(?:discuss(?:ed)?|talked\s+about|decided)\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\blast\s+time\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\bthat\s+(?:thing|bug|plan)\s+we\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\bback\s+when\b`)},
	{class: "shared_past", re: regexp.MustCompile(`(?i)\bearlier\s+you\b`)},
	{class: "temporal", re: regexp.MustCompile(`(?i)\byesterday\b`)},
	{class: "temporal", re: regexp.MustCompile(`(?i)\blast\s+(?:week|month|year)\b`)},
	{class: "temporal", re: regexp.MustCompile(`(?i)\b(?:back\s+)?in\s+(?:january|february|march|april|may|june|july|august|september|october|november|december)\b`)},
}

func classifyEpisodicUserMessage(userMsg string) episodicTrigger {
	if strings.TrimSpace(userMsg) == "" || isHeartbeatWake(userMsg) || isAutomatrixWake(userMsg) || IsMorningBriefWake(userMsg) {
		return episodicTrigger{}
	}
	for _, guard := range episodicNegativeGuards {
		if guard.MatchString(userMsg) {
			return episodicTrigger{}
		}
	}
	for _, lexeme := range episodicLexicon {
		if lexeme.re.MatchString(userMsg) {
			return episodicTrigger{Fired: true, Class: lexeme.class}
		}
	}
	return episodicTrigger{}
}

func classifyEpisodicMessage(msg llm.Message) episodicTrigger {
	if msg.Role != llm.RoleUser || msg.IsGuidance() {
		return episodicTrigger{}
	}
	return classifyEpisodicUserMessage(msg.Content)
}

type episodicTimeWindow struct {
	From  time.Time
	Until time.Time
}

func (w episodicTimeWindow) bounded() bool {
	return !w.From.IsZero() && !w.Until.IsZero() && w.From.Before(w.Until)
}

type episodicExtraction struct {
	Referent  string
	TimeHint  string
	ScopeHint string
	Window    episodicTimeWindow
}

const episodicExtractionTimeout = 2 * time.Second

const episodicExtractionSystem = `Extract the past-exchange search target from the user's message.
Return ONLY one JSON object (no prose, no fences):
{"referent":"short search query","time_hint":"time phrase or empty","scope_hint":"conversation scope or empty"}
Do not answer the user. Preserve distinctive names and exact terms in referent.`

func (a *Agent) extractEpisodic(ctx context.Context, userMsg string) episodicExtraction {
	return a.extractEpisodicAt(ctx, userMsg, time.Now().UTC())
}

func (a *Agent) extractEpisodicAt(ctx context.Context, userMsg string, now time.Time) episodicExtraction {
	fallback := episodicExtraction{Referent: strings.TrimSpace(userMsg)}
	client := a.cheap
	if client == nil {
		client = a.main
	}
	if client == nil || fallback.Referent == "" {
		return fallback
	}

	callCtx, cancel := context.WithTimeout(ctx, episodicExtractionTimeout)
	defer cancel()
	res, err := client.Chat(callCtx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage(episodicExtractionSystem),
		llm.UserMessage(userMsg),
	}})
	if err != nil || res == nil {
		return fallback
	}

	raw := strings.TrimSpace(res.Message.Content)
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(raw, "```json"), "```"), "```"))
	var row struct {
		Referent  string `json:"referent"`
		TimeHint  string `json:"time_hint"`
		ScopeHint string `json:"scope_hint"`
	}
	if json.Unmarshal([]byte(raw), &row) != nil {
		return fallback
	}
	out := episodicExtraction{
		Referent:  strings.TrimSpace(row.Referent),
		TimeHint:  strings.TrimSpace(row.TimeHint),
		ScopeHint: strings.TrimSpace(row.ScopeHint),
	}
	if out.Referent == "" {
		out.Referent = fallback.Referent
	}
	out.Window = parseEpisodicTimeHint(out.TimeHint, now)
	return out
}

func parseEpisodicTimeHint(hint string, now time.Time) episodicTimeWindow {
	now = now.UTC()
	hint = strings.ToLower(strings.TrimSpace(hint))
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch hint {
	case "yesterday":
		return episodicTimeWindow{From: day.AddDate(0, 0, -1), Until: day}
	case "last week":
		thisMonday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
		return episodicTimeWindow{From: thisMonday.AddDate(0, 0, -7), Until: thisMonday}
	case "last month":
		thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return episodicTimeWindow{From: thisMonth.AddDate(0, -1, 0), Until: thisMonth}
	case "last year":
		thisYear := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		return episodicTimeWindow{From: thisYear.AddDate(-1, 0, 0), Until: thisYear}
	}

	monthText := strings.TrimSpace(strings.TrimPrefix(hint, "back "))
	monthText = strings.TrimSpace(strings.TrimPrefix(monthText, "in "))
	months := map[string]time.Month{
		"january": time.January, "february": time.February, "march": time.March,
		"april": time.April, "may": time.May, "june": time.June,
		"july": time.July, "august": time.August, "september": time.September,
		"october": time.October, "november": time.November, "december": time.December,
	}
	month, ok := months[monthText]
	if !ok {
		return episodicTimeWindow{}
	}
	year := now.Year()
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	if from.After(now) {
		from = from.AddDate(-1, 0, 0)
	}
	return episodicTimeWindow{From: from, Until: from.AddDate(0, 1, 0)}
}

func (a *Agent) filterNewEpisodic(in []memory.EpisodicExcerpt) []memory.EpisodicExcerpt {
	out := in[:0]
	for _, ex := range in {
		if _, ok := a.episodicSeen(ex.Ref); ok {
			continue
		}
		a.episodicRemember(ex.Ref)
		out = append(out, ex)
	}
	return out
}

func renderEpisodicBlock(excerpts []memory.EpisodicExcerpt) string {
	var b strings.Builder
	b.WriteString("\nAuto-recalled past exchange the user referenced (ground your answer in this material; if it does not match what they meant, say so plainly):\n")
	for _, ex := range excerpts {
		exactness := "exact"
		sourceType := "exact_transcript"
		confidence := 1.0
		if !ex.Exact {
			exactness = "approximate"
			sourceType = "semantic_recall"
			confidence = 0.5
		}
		date := "date unavailable"
		if !ex.Date.IsZero() {
			date = ex.Date.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "Past exchange [%s, conversation %s, %s, source %s, confidence %.2f, selected because the user explicitly referenced an older exchange, seq %d-%d]:\n%s\n", exactness, ex.ConversationID, date, sourceType, confidence, ex.SeqLo, ex.SeqHi, strings.TrimSpace(ex.Text))
		for _, related := range ex.RelatedMemories {
			if text := strings.TrimSpace(related.Text); text != "" {
				fmt.Fprintf(&b, "Related memory: %s\n", text)
			}
		}
	}
	return b.String()
}

func (a *Agent) episodicSeen(ref string) (struct{}, bool) {
	v, ok := a.episodicSurfaced[ref]
	return v, ok
}

func (a *Agent) episodicRemember(ref string) { a.episodicSurfaced[ref] = struct{}{} }
func (a *Agent) episodicReset()              { a.episodicSurfaced = map[string]struct{}{} }

func (t *turn) episodicMark(class string) {
	t.episodicDetected = true
	t.episodicPending = true
	t.episodicTrigger = class
}

func (t *turn) episodicGrounded() { t.episodicPending = false }

func logEpisodic(trigger string, hits, tokens int, started time.Time) {
	log.Printf("neo episodic trigger=%s lanes=semantic,provenance,lexical,current hits=%d tokens=%d latency_ms=%d", trigger, hits, tokens, time.Since(started).Milliseconds())
}
