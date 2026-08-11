// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"strings"
	"testing"

	"matrix/neo/internal/llm"
)

// dataURI builds an inline base64 image data URI with a blob of n payload
// chars (well above minImageBlob when n is large).
func dataURI(n int) string {
	return "data:image/png;base64," + strings.Repeat("A", n)
}

// A large inline image must be charged the FLAT per-image token cost, NOT its
// base64 length — otherwise one thumbnail trips spurious compactions.
func TestImageAwareTokensFlatCost(t *testing.T) {
	const blob = 40000 // ~10k tokens under bytes/4, but should bill ~flatImageTokens
	withImg := "here is a screenshot " + dataURI(blob)

	naive := (len(withImg) + 3) / 4 // the old bytes/4 estimate
	got := imageAwareTokens(withImg)

	if got >= naive {
		t.Errorf("image-aware tokens (%d) must be far below the bytes/4 estimate (%d)", got, naive)
	}
	// The flat charge dominates: within a small multiple of flatImageTokens.
	if got > flatImageTokens*2 {
		t.Errorf("a single image should bill ~%d tokens, got %d", flatImageTokens, got)
	}
	if got < flatImageTokens {
		t.Errorf("an image must bill at least the flat cost %d, got %d", flatImageTokens, got)
	}
}

// stripDataURIs replaces a large blob with the placeholder, keeps surrounding
// text, and leaves a tiny data URI (below minImageBlob) untouched.
func TestStripDataURIs(t *testing.T) {
	body := "before " + dataURI(2000) + " after"
	out, n := stripDataURIs(body)
	if n != 1 {
		t.Fatalf("expected 1 stripped image, got %d", n)
	}
	if !strings.Contains(out, "before ") || !strings.Contains(out, " after") {
		t.Errorf("surrounding text must survive: %q", out)
	}
	if strings.Contains(out, strings.Repeat("A", 2000)) {
		t.Error("the base64 blob must be removed")
	}
	if !strings.Contains(out, imagePlaceholder) {
		t.Error("a placeholder must replace the stripped image")
	}

	// A short data URI in prose is left verbatim.
	tiny := "icon data:image/png;base64,QUJD end"
	out2, n2 := stripDataURIs(tiny)
	if n2 != 0 || out2 != tiny {
		t.Errorf("a sub-threshold data URI must be left untouched: n=%d out=%q", n2, out2)
	}
}

// splitForCompaction keeps the most recent N user turns verbatim and carves the
// older prefix for summarization, never splitting on a tool-result boundary.
func TestSplitForCompactionKeepsRecentUserTurns(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("u1"),
		llm.AssistantMessage("a1"),
		llm.UserMessage("u2"),
		llm.AssistantMessage("a2"),
		llm.UserMessage("u3"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc("f", "{}")}},
		llm.ToolResult("f", "f", "r3"),
		llm.UserMessage("u4"),
		llm.AssistantMessage("a4"),
	}
	older, recent := splitForCompaction(msgs, 2)
	// keep last 2 user turns (u3, u4) → recent starts at u3.
	if len(recent) == 0 || recent[0].Role != llm.RoleUser || recent[0].Content != "u3" {
		t.Fatalf("recent tail should start at u3, got %+v", recent)
	}
	if len(older) == 0 || older[len(older)-1].Content != "a2" {
		t.Fatalf("older section should end before u3, got %+v", older)
	}
	// Every recent message must be self-consistent (no orphan tool result at head).
	if recent[0].Role == llm.RoleTool {
		t.Error("recent tail must not start on a tool result")
	}
}

// Too few user turns to split → older is nil so the caller degrades instead of
// summarizing recent verbatim context away.
func TestSplitForCompactionTooShort(t *testing.T) {
	msgs := []llm.Message{llm.UserMessage("only"), llm.AssistantMessage("reply")}
	older, recent := splitForCompaction(msgs, 3)
	if older != nil {
		t.Errorf("with fewer than keep user turns, older must be nil; got %+v", older)
	}
	if len(recent) != len(msgs) {
		t.Errorf("recent must be the whole transcript; got %+v", recent)
	}
}

// stripOldImages drops image payloads from OLDER turns while preserving images
// in the most recent turns the model may still be working with.
func TestStripOldImagesPreservesRecent(t *testing.T) {
	a := New(Options{})
	a.working = []llm.Message{
		llm.UserMessage("old turn " + dataURI(2000)),
		llm.AssistantMessage("a1"),
		llm.UserMessage("u2"),
		llm.AssistantMessage("a2"),
		llm.UserMessage("u3"),
		llm.AssistantMessage("a3"),
		llm.UserMessage("recent turn " + dataURI(2000)),
	}
	n := a.stripOldImages()
	if n != 1 {
		t.Fatalf("expected exactly the OLD image stripped, got %d", n)
	}
	if strings.Contains(a.working[0].Content, strings.Repeat("A", 2000)) {
		t.Error("the old image payload must be stripped")
	}
	if !strings.Contains(a.working[len(a.working)-1].Content, strings.Repeat("A", 2000)) {
		t.Error("the most recent image must be preserved")
	}
}
