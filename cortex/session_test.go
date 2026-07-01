// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"strings"
	"testing"
)

// TestSessionAppendAndTranscript exercises the real AppendMessage ->
// Transcript path: mixed roles (incl a tool_call with ToolArgs and a
// tool_result), gap-free 0,1,2,... seqs, and ordered read-back with fields
// intact.
func TestSessionAppendAndTranscript(t *testing.T) {
	c := openCortex(t)
	const conv = "conv-A"

	msgs := []Message{
		{ConversationID: conv, Role: RoleSystem, Content: "you are helpful"},
		{ConversationID: conv, Role: RoleUser, Content: "launch the token"},
		{ConversationID: conv, Role: RoleToolCall, ToolName: "core_execute",
			ToolArgs: []byte(`{"verb":"launch","args":{"symbol":"PAX"}}`)},
		{ConversationID: conv, Role: RoleToolResult, ToolName: "core_execute",
			Content: "ok: launched at 0xabc"},
		{ConversationID: conv, Role: RoleAssistant, Content: "done — launched."},
	}

	for i, m := range msgs {
		uri, err := c.AppendMessage(m)
		if err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
		wantURI := BuildSessionURI(conv, uint64(i))
		if uri != wantURI {
			t.Fatalf("AppendMessage[%d] uri = %q, want %q", i, uri, wantURI)
		}
	}

	got, err := c.Transcript(conv, 0, 100)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("Transcript len = %d, want %d", len(got), len(msgs))
	}
	for i := range got {
		if got[i].Seq != uint64(i) {
			t.Fatalf("msg[%d] seq = %d, want %d (gaps/out-of-order)", i, got[i].Seq, i)
		}
		if got[i].Role != msgs[i].Role {
			t.Fatalf("msg[%d] role = %v, want %v", i, got[i].Role, msgs[i].Role)
		}
		if got[i].Content != msgs[i].Content {
			t.Fatalf("msg[%d] content = %q, want %q", i, got[i].Content, msgs[i].Content)
		}
		if got[i].ConversationID != conv {
			t.Fatalf("msg[%d] conv = %q, want %q", i, got[i].ConversationID, conv)
		}
		if got[i].TS == 0 {
			t.Fatalf("msg[%d] TS not stamped", i)
		}
	}
	// Tool call fields intact.
	if got[2].ToolName != "core_execute" || !bytes.Equal(got[2].ToolArgs, msgs[2].ToolArgs) {
		t.Fatalf("tool_call fields lost: name=%q args=%q", got[2].ToolName, got[2].ToolArgs)
	}
}

// TestSessionTranscriptSinceSeqAndLimit checks the tail slice + bound.
func TestSessionTranscriptSinceSeqAndLimit(t *testing.T) {
	c := openCortex(t)
	const conv = "conv-tail"
	for i := 0; i < 6; i++ {
		if _, err := c.AppendMessage(Message{ConversationID: conv, Role: RoleUser,
			Content: strings.Repeat("x", i+1)}); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	// sinceSeq=2 → seqs 2..5.
	tail, err := c.Transcript(conv, 2, 100)
	if err != nil {
		t.Fatalf("Transcript tail: %v", err)
	}
	if len(tail) != 4 {
		t.Fatalf("tail len = %d, want 4", len(tail))
	}
	if tail[0].Seq != 2 || tail[3].Seq != 5 {
		t.Fatalf("tail seqs = [%d..%d], want [2..5]", tail[0].Seq, tail[3].Seq)
	}

	// sinceSeq=2, limit=2 → seqs 2,3.
	bounded, err := c.Transcript(conv, 2, 2)
	if err != nil {
		t.Fatalf("Transcript bounded: %v", err)
	}
	if len(bounded) != 2 || bounded[0].Seq != 2 || bounded[1].Seq != 3 {
		t.Fatalf("bounded = %+v, want seqs [2,3]", bounded)
	}
}

// TestSessionConversationIsolation proves two conversations have independent
// seq spaces.
func TestSessionConversationIsolation(t *testing.T) {
	c := openCortex(t)

	if _, err := c.AppendMessage(Message{ConversationID: "A", Role: RoleUser, Content: "a0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppendMessage(Message{ConversationID: "B", Role: RoleUser, Content: "b0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AppendMessage(Message{ConversationID: "A", Role: RoleUser, Content: "a1"}); err != nil {
		t.Fatal(err)
	}

	a, err := c.Transcript("A", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Transcript("B", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a[0].Seq != 0 || a[1].Seq != 1 || a[0].Content != "a0" || a[1].Content != "a1" {
		t.Fatalf("conv A = %+v, want seqs [0,1] contents [a0,a1]", a)
	}
	if len(b) != 1 || b[0].Seq != 0 || b[0].Content != "b0" {
		t.Fatalf("conv B = %+v, want seq [0] content [b0]", b)
	}
}

// TestSessionToolResultSpill checks overflow discipline: a large tool_result
// stores empty Content + a resolvable ToolResultRef, and ResolveToolResult
// returns the exact original bytes.
func TestSessionToolResultSpill(t *testing.T) {
	c := openCortex(t)
	const conv = "conv-spill"

	big := strings.Repeat("Z", SessionToolResultSpillBytes+1)
	if _, err := c.AppendMessage(Message{ConversationID: conv, Role: RoleToolResult,
		ToolName: "grep", Content: big}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	got, err := c.Transcript(conv, 0, 100)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Content != "" {
		t.Fatalf("spilled record Content should be empty, got %d bytes", len(got[0].Content))
	}
	wantRef := BuildToolResultURI(conv, 0)
	if got[0].ToolResultRef != wantRef {
		t.Fatalf("ToolResultRef = %q, want %q", got[0].ToolResultRef, wantRef)
	}

	resolved, err := c.ResolveToolResult(conv, 0)
	if err != nil {
		t.Fatalf("ResolveToolResult: %v", err)
	}
	if string(resolved) != big {
		t.Fatalf("resolved bytes differ: got %d bytes, want %d", len(resolved), len(big))
	}

	// A small tool_result must NOT spill.
	if _, err := c.AppendMessage(Message{ConversationID: conv, Role: RoleToolResult,
		ToolName: "grep", Content: "tiny"}); err != nil {
		t.Fatalf("AppendMessage small: %v", err)
	}
	got2, err := c.Transcript(conv, 1, 100)
	if err != nil {
		t.Fatalf("Transcript small: %v", err)
	}
	if len(got2) != 1 || got2[0].Content != "tiny" || got2[0].ToolResultRef != "" {
		t.Fatalf("small tool_result unexpectedly spilled: %+v", got2)
	}
}

// TestSessionDerivedLaneNoAnchoredWrite proves AppendMessage rides the
// derived lane: the anchored namespace SMT roots ("memories", "edges") are
// byte-identical before and after an append (no snap.StageMemoryUpdate).
func TestSessionDerivedLaneNoAnchoredWrite(t *testing.T) {
	c := openCortex(t)

	memBefore, err := c.Snap().SMT("memories").Root()
	if err != nil {
		t.Fatalf("memories root before: %v", err)
	}
	edgeBefore, err := c.Snap().SMT("edges").Root()
	if err != nil {
		t.Fatalf("edges root before: %v", err)
	}

	if _, err := c.AppendMessage(Message{ConversationID: "lane", Role: RoleUser, Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	memAfter, err := c.Snap().SMT("memories").Root()
	if err != nil {
		t.Fatalf("memories root after: %v", err)
	}
	edgeAfter, err := c.Snap().SMT("edges").Root()
	if err != nil {
		t.Fatalf("edges root after: %v", err)
	}

	if memBefore != memAfter {
		t.Fatalf("memories SMT root changed by AppendMessage: %x -> %x", memBefore, memAfter)
	}
	if edgeBefore != edgeAfter {
		t.Fatalf("edges SMT root changed by AppendMessage: %x -> %x", edgeBefore, edgeAfter)
	}
}

// TestSessionProviderAgnosticRoundTrip proves a tool_call with ToolName +
// canonical-JSON ToolArgs survives AppendMessage -> Transcript losslessly.
func TestSessionProviderAgnosticRoundTrip(t *testing.T) {
	c := openCortex(t)
	const conv = "conv-agnostic"

	args := []byte(`{"a":1,"b":["x","y"],"nested":{"k":true}}`)
	in := Message{
		ConversationID: conv,
		Role:           RoleToolCall,
		ToolName:       "search_web",
		ToolArgs:       args,
		Content:        "searching",
		MediaRefs:      []string{"matrix://media/img/1", "matrix://media/img/2"},
	}
	if _, err := c.AppendMessage(in); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	got, err := c.Transcript(conv, 0, 100)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0]
	if m.Role != RoleToolCall || m.ToolName != "search_web" || m.Content != "searching" {
		t.Fatalf("role/name/content lost: %+v", m)
	}
	if !bytes.Equal(m.ToolArgs, args) {
		t.Fatalf("ToolArgs not byte-identical: got %q want %q", m.ToolArgs, args)
	}
	if len(m.MediaRefs) != 2 || m.MediaRefs[0] != "matrix://media/img/1" || m.MediaRefs[1] != "matrix://media/img/2" {
		t.Fatalf("MediaRefs lost: %+v", m.MediaRefs)
	}
}

// TestSessionAppendValidation covers the error boundaries.
func TestSessionAppendValidation(t *testing.T) {
	c := openCortex(t)
	if _, err := c.AppendMessage(Message{Role: RoleUser}); err != ErrEmptyConversationID {
		t.Fatalf("empty conv err = %v, want ErrEmptyConversationID", err)
	}
	if _, err := c.AppendMessage(Message{ConversationID: "x", Role: Role(99)}); err != ErrInvalidRole {
		t.Fatalf("bad role err = %v, want ErrInvalidRole", err)
	}
	if _, err := c.AppendMessage(Message{ConversationID: "a/b", Role: RoleUser}); err == nil {
		t.Fatalf("conv with '/' should error")
	}
}
