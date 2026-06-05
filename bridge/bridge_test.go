// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package bridge

import (
	"context"
	"strings"
	"testing"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/parser"
)

// ----- test helpers -------------------------------------------------------

func mustParse(t *testing.T, src []byte) *ast.File {
	t.Helper()
	file, errs := parser.New(src).Parse()
	if len(errs) > 0 {
		for _, e := range errs {
			t.Logf("parse error: %s", e)
		}
		t.Fatalf("parser returned %d errors", len(errs))
	}
	return file
}

func embedHashStub(t *testing.T) *embed.HashEmbedder {
	t.Helper()
	return embed.NewHashEmbedder()
}

func toTags(ss []string) []memory.Tag {
	if len(ss) == 0 {
		return nil
	}
	out := make([]memory.Tag, len(ss))
	for i, s := range ss {
		out[i] = memory.Tag(s)
	}
	return out
}

func newAdapter(t *testing.T, opts ...Option) (*Adapter, *cortex.Cortex) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := cortex.New(s)
	return New(c, opts...), c
}

func writePref(t *testing.T, c *cortex.Cortex, topic, body string, tags ...string) memory.URI {
	t.Helper()
	h := memory.Head{ActorScope: "andrew", Tags: toTags(tags)}
	d := memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   0.7,
		Rationale:     body,
	}
	uri, err := c.Write(h, d, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: topic + ":short", Medium: topic + ": " + body},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write Preference: %v", err)
	}
	return uri
}

func writeFact(t *testing.T, c *cortex.Cortex, subject, predicate, statement string, tags ...string) memory.URI {
	t.Helper()
	h := memory.Head{ActorScope: "andrew", Tags: toTags(tags)}
	d := memory.FactData{
		SchemaVersion: 1,
		Subject:       subject,
		Predicate:     predicate,
		Statement:     statement,
	}
	uri, err := c.Write(h, d, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: predicate + "/" + subject, Medium: predicate + "(" + subject + ")=" + statement},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write Fact: %v", err)
	}
	return uri
}

// ----- Adapter interface conformance --------------------------------------

func TestAdapterImplementsInterpreterCortex(t *testing.T) {
	a, _ := newAdapter(t)
	var _ interpreter.Cortex = a
}

// ----- Find ---------------------------------------------------------------

func TestFindByType(t *testing.T) {
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse over verbose")
	writePref(t, c, "tempo", "fast over slow")
	writeFact(t, c, "andrew", "knows", "go")

	res, err := a.Find(context.Background(), map[string]string{"type": "Preference"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 preferences, got %d", len(res))
	}
	for _, r := range res {
		if r.Type != "Preference" {
			t.Fatalf("non-Preference in result: %q", r.Type)
		}
		if !strings.HasPrefix(r.URI, "matrix://cortex/Preference/") {
			t.Fatalf("bad URI: %q", r.URI)
		}
		if !strings.HasSuffix(r.URI, "#1") {
			t.Fatalf("missing version pin: %q", r.URI)
		}
		if r.Summary == "" {
			t.Fatalf("empty summary for %q", r.URI)
		}
	}
}

func TestFindByTypeAndTag(t *testing.T) {
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse", "ui", "style")
	writePref(t, c, "tempo", "fast", "ui")
	writePref(t, c, "color", "dark") // no tag match

	res, err := a.Find(context.Background(), map[string]string{
		"type": "Preference",
		"tag":  "ui",
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 tagged prefs, got %d", len(res))
	}
}

func TestFindLimit(t *testing.T) {
	a, c := newAdapter(t)
	for i := 0; i < 5; i++ {
		writePref(t, c, "topic"+string(rune('A'+i)), "body")
	}

	res, err := a.Find(context.Background(), map[string]string{
		"type":  "Preference",
		"limit": "3",
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("expected limit=3, got %d", len(res))
	}
}

func TestFindDefaultLimit(t *testing.T) {
	a, c := newAdapter(t, WithDefaultLimit(2))
	for i := 0; i < 5; i++ {
		writePref(t, c, "topic"+string(rune('A'+i)), "body")
	}
	res, err := a.Find(context.Background(), map[string]string{"type": "Preference"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected default limit=2, got %d", len(res))
	}
}

func TestFindUnknownType(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Find(context.Background(), map[string]string{"type": "NotAType"})
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected unknown-type error, got %v", err)
	}
}

func TestFindUnknownArg(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Find(context.Background(), map[string]string{
		"type":     "Preference",
		"bogusKey": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown find arg") {
		t.Fatalf("expected unknown-arg error, got %v", err)
	}
}

func TestFindBadLimit(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Find(context.Background(), map[string]string{
		"type":  "Preference",
		"limit": "abc",
	})
	if err == nil || !strings.Contains(err.Error(), "bad limit") {
		t.Fatalf("expected bad-limit error, got %v", err)
	}
}

func TestFindCompileTimeDoesNotJournal(t *testing.T) {
	// Phase 3 + Phase 11.5 invariant: compile-time Find (LateBinding=false)
	// MUST NOT journal a KindFind entry. The bridge defaults to
	// LateBinding=false, so two Finds should leave OverallRoot unchanged.
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse")

	before := rootHex(t, c)
	for i := 0; i < 3; i++ {
		_, err := a.Find(context.Background(), map[string]string{"type": "Preference"})
		if err != nil {
			t.Fatalf("Find #%d: %v", i, err)
		}
	}
	after := rootHex(t, c)
	if before != after {
		t.Fatalf("compile-time Find altered OverallRoot: %s → %s", before, after)
	}
}

func rootHex(t *testing.T, c *cortex.Cortex) string {
	t.Helper()
	root, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot: %v", err)
	}
	return string(root[:])
}

func TestFindLateBindingJournals(t *testing.T) {
	a, c := newAdapter(t, WithLateBinding(true))
	writePref(t, c, "tone", "terse")

	before := rootHex(t, c)
	_, err := a.Find(context.Background(), map[string]string{"type": "Preference"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	after := rootHex(t, c)
	if before == after {
		t.Fatalf("late-binding Find did not journal a KindFind entry")
	}
}

func TestFindNoLateBindingOverrideViaArgs(t *testing.T) {
	// The "late" arg per-call overrides the Adapter default.
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse")

	before := rootHex(t, c)
	_, err := a.Find(context.Background(), map[string]string{
		"type": "Preference",
		"late": "true",
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rootHex(t, c) == before {
		t.Fatalf("late=true arg did not journal")
	}
}

func TestFindNearOnEmptyIndexReturnsNoResults(t *testing.T) {
	// Fresh cortex + running embedder + no memories: HNSW index is
	// empty, cortex returns vector.ErrEmptyIndex. The bridge maps that
	// to "no candidates" so SKILL.mtx unknown blocks can fire instead
	// of crashing the whole compile.
	a, c := newAdapter(t)

	if err := c.StartEmbedder(cortex.EmbedderOptions{
		Embedder: embedHashStub(t),
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}
	defer c.StopEmbedder()

	res, err := a.Find(context.Background(), map[string]string{
		"near":  "anything semantic",
		"limit": "5",
	})
	if err != nil {
		t.Fatalf("Find on empty index: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("expected no results, got %d", len(res))
	}
}

func TestFindFormShortPicksUpShortSummary(t *testing.T) {
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse")
	res, err := a.Find(context.Background(), map[string]string{
		"type": "Preference",
		"form": "short",
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1, got %d", len(res))
	}
	// FormShort renders into Forms.Short = "tone:short" via the test helper.
	if !strings.Contains(res[0].Summary, "tone") {
		t.Fatalf("expected short summary, got %q", res[0].Summary)
	}
}

// ----- Resolve ------------------------------------------------------------

func TestResolveURI(t *testing.T) {
	a, c := newAdapter(t)
	uri := writePref(t, c, "tone", "terse")

	r, err := a.Resolve(context.Background(), string(uri))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r == nil {
		t.Fatalf("expected non-nil result")
	}
	if r.URI != string(uri) {
		t.Fatalf("URI mismatch: got %q want %q", r.URI, uri)
	}
	if r.Type != "Preference" {
		t.Fatalf("type mismatch: %q", r.Type)
	}
	if r.Summary == "" {
		t.Fatalf("empty summary")
	}
}

func TestResolveURINotFound(t *testing.T) {
	a, _ := newAdapter(t)
	// Valid-shape URI for a memory that doesn't exist.
	bogus := "matrix://cortex/Preference/01ARZ3NDEKTSV4RRFFQ69G5FAV#1"
	r, err := a.Resolve(context.Background(), bogus)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r != nil {
		t.Fatalf("expected nil result for not-found, got %+v", r)
	}
}

func TestResolveEmpty(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Resolve(context.Background(), "   ")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-expr error, got %v", err)
	}
}

func TestResolveNLWithoutEmbedderFailsClean(t *testing.T) {
	// No embedder running → cortex.Find with Near returns
	// "requires StartEmbedder". The bridge surfaces that verbatim.
	a, _ := newAdapter(t)
	_, err := a.Resolve(context.Background(), "some natural language phrase")
	if err == nil {
		t.Fatalf("expected error from NL resolve without embedder")
	}
	if !strings.Contains(err.Error(), "StartEmbedder") &&
		!strings.Contains(err.Error(), "embedder") {
		t.Fatalf("expected embedder-not-running error, got %v", err)
	}
}

// ----- Context ------------------------------------------------------------

func TestContextVerbOnlyPinnedTier(t *testing.T) {
	a, c := newAdapter(t)
	// Write an Identity → Pinned tier should pick it up.
	_, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.IdentityData{
		SchemaVersion: 1,
		Name:          "Andrew",
		DID:           "did:pax:owner",
	}, cortex.WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "Andrew", Medium: "Andrew (did:pax:owner)"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write Identity: %v", err)
	}

	out, err := a.Context(context.Background(), map[string]string{
		"verb": "build",
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if !strings.Contains(out, "## Pinned") {
		t.Fatalf("expected Pinned section in bundle:\n%s", out)
	}
	if !strings.Contains(out, "Andrew") {
		t.Fatalf("expected Identity summary in bundle:\n%s", out)
	}
	if !strings.Contains(out, "---") {
		t.Fatalf("expected trailer in bundle:\n%s", out)
	}
}

func TestContextEmpty(t *testing.T) {
	a, _ := newAdapter(t)
	out, err := a.Context(context.Background(), nil)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if !strings.Contains(out, "empty cortex bundle") {
		t.Fatalf("expected empty-bundle marker:\n%s", out)
	}
}

func TestContextUnknownVerb(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Context(context.Background(), map[string]string{"verb": "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("expected unknown-verb error, got %v", err)
	}
}

func TestContextUnknownArg(t *testing.T) {
	a, _ := newAdapter(t)
	_, err := a.Context(context.Background(), map[string]string{"foo": "bar"})
	if err == nil || !strings.Contains(err.Error(), "unknown context arg") {
		t.Fatalf("expected unknown-arg error, got %v", err)
	}
}

func TestContextPureReadDoesNotMutateRoot(t *testing.T) {
	// research/04 §12.1 invariant: cortex.Context is a pure read; no
	// journal entries, OverallRoot unchanged across the call. The
	// bridge must preserve that.
	a, c := newAdapter(t)
	writePref(t, c, "tone", "terse")

	before := rootHex(t, c)
	for i := 0; i < 3; i++ {
		if _, err := a.Context(context.Background(), map[string]string{"verb": "build"}); err != nil {
			t.Fatalf("Context: %v", err)
		}
	}
	after := rootHex(t, c)
	if before != after {
		t.Fatalf("Context altered OverallRoot: %s → %s", before, after)
	}
}

// ----- object parsing -----------------------------------------------------

func TestParseObjects(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
		ok   bool
	}{
		{"", nil, true},
		{"service:foo", map[string]string{"service": "foo"}, true},
		{"service:foo,agent:bar", map[string]string{"service": "foo", "agent": "bar"}, true},
		{"service:foo;agent:bar", map[string]string{"service": "foo", "agent": "bar"}, true},
		{"  service : foo , agent : bar  ", map[string]string{"service": "foo", "agent": "bar"}, true},
		{"bogus:foo", nil, false},                      // unknown kind
		{"service", nil, false},                        // missing ':'
		{"service:", nil, false},                       // empty ref
		{"service:foo;agent:bar,plan:baz", nil, false}, // mixed seps
	}
	for _, tc := range cases {
		got, err := parseObjects(tc.in)
		if tc.ok && err != nil {
			t.Fatalf("parseObjects(%q): unexpected error %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("parseObjects(%q): expected error, got %+v", tc.in, got)
		}
		if tc.ok && len(got) != len(tc.want) {
			t.Fatalf("parseObjects(%q): want %v got %v", tc.in, tc.want, got)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Fatalf("parseObjects(%q): key %q want %q got %q", tc.in, k, v, got[k])
			}
		}
	}
}

// ----- end-to-end through the MCL interpreter -----------------------------

// TestInterpreterResolvesViaBridge wires a real SKILL.mtx-style on-block
// resolve statement through the MCL interpreter into a live cortex via
// the bridge. Validates the contract: when the slot pre-fill contains
// a matrix:// URI as slot.target.prose, the interpreter resolves it via
// the bridge and the slot ends up SlotResolved.
//
// No LLM is required because the on-block we run has no prompt entry,
// only a resolve statement.
func TestInterpreterResolvesViaBridge(t *testing.T) {
	a, c := newAdapter(t)
	uri := writePref(t, c, "tone", "terse over verbose")

	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=modify
  resolve slot.target <- cortex.resolve(slot.target.prose)
end
§OUTPUTS
§FAILURE_MODES
`)

	file := mustParse(t, src)
	interp := interpreter.New(file, nil /*llm*/, a)
	result, err := interp.Run(context.Background(), &interpreter.RunInput{
		Verb:       "modify",
		Confidence: 1.0,
		SlotValues: map[string]string{
			"target": string(uri), // raw prose pre-filled with a real URI
		},
	})
	if err != nil {
		t.Fatalf("interp.Run: %v", err)
	}
	if !result.Executed {
		t.Fatalf("on-block did not execute (matched=%q)", result.MatchedCondition)
	}

	slot, ok := result.Slots["target"]
	if !ok {
		t.Fatalf("slot.target missing from result")
	}
	if slot.Status != interpreter.SlotResolved {
		t.Fatalf("expected SlotResolved, got status=%v value=%q", slot.Status, slot.Value)
	}
	if slot.Value != string(uri) {
		t.Fatalf("slot.target.value mismatch: got %q want %q", slot.Value, uri)
	}
}

// TestInterpreterFindByTypeViaBridge runs a build-verb on-block whose
// resolve calls cortex.find(type=Fact, limit=N) — i.e. the bridge's
// Find path is exercised through the real interpreter against a real
// cortex with no embedder and no LLM.
func TestInterpreterFindByTypeViaBridge(t *testing.T) {
	a, c := newAdapter(t)
	uri := writeFact(t, c, "andrew", "knows", "go")

	src := []byte(`§SKILL
id=test
§INPUTS
slot target: string
  required
§CORTEX
§TOOLS
none
§SUB_SKILLS
none
§PROCEDURE
on verb=build
  resolve slot.target <- cortex.find(type=Fact, limit=5)
end
§OUTPUTS
§FAILURE_MODES
`)

	file := mustParse(t, src)
	interp := interpreter.New(file, nil, a)
	result, err := interp.Run(context.Background(), &interpreter.RunInput{Verb: "build"})
	if err != nil {
		t.Fatalf("interp.Run: %v", err)
	}
	slot, ok := result.Slots["target"]
	if !ok {
		t.Fatalf("slot.target missing")
	}
	if slot.Status != interpreter.SlotResolved {
		t.Fatalf("expected SlotResolved, got %v", slot.Status)
	}
	if slot.Value != string(uri) {
		t.Fatalf("slot.target.value mismatch: got %q want %q", slot.Value, uri)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
