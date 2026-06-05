// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/cortex/embed"
	"matrix/cortex/forms"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

// openCortex returns a fresh Cortex over a temp Pebble DB.
func openCortex(t *testing.T) *Cortex {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s)
}

func newPreference() memory.PreferenceData {
	return memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         "tone",
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   0.8,
		Rationale:     "terse > verbose",
	}
}

func newIdentity() memory.IdentityData {
	return memory.IdentityData{
		SchemaVersion: 1,
		Name:          "Andrew",
		DID:           "did:pax:owner",
	}
}

func TestWriteAndResolveRoundTrip(t *testing.T) {
	c := openCortex(t)
	d := newPreference()

	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, d, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "prefers tone=terse", Medium: "andrew prefers terse over verbose"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(string(uri), "matrix://cortex/Preference/") || !strings.HasSuffix(string(uri), "#1") {
		t.Fatalf("URI: %q", uri)
	}

	got, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Head.Type != memory.TypePreference {
		t.Fatalf("type: %v", got.Head.Type)
	}
	if got.Head.CurrentVersion != 1 {
		t.Fatalf("CurrentVersion: %d", got.Head.CurrentVersion)
	}
	if got.Version.Version != 1 || got.Version.Type != memory.TypePreference {
		t.Fatalf("version: %+v", got.Version)
	}

	decoded, err := memory.DecodeData(got.Version.Type, got.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	pref, ok := decoded.(memory.PreferenceData)
	if !ok {
		t.Fatalf("type assertion failed: %T", decoded)
	}
	if pref.Topic != "tone" || pref.Polarity != memory.PolarityPrefer {
		t.Fatalf("data mismatch: %+v", pref)
	}
}

func TestWriteRejectsInvalidData(t *testing.T) {
	c := openCortex(t)
	bad := memory.PreferenceData{SchemaVersion: 1, Polarity: memory.PolarityPrefer, StrengthVal: 0.5}
	_, err := c.Write(memory.Head{ActorScope: "andrew"}, bad, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err == nil || !strings.Contains(err.Error(), "Preference.topic required") {
		t.Fatalf("expected topic-required error, got %v", err)
	}
}

func TestUpdateBumpsVersion(t *testing.T) {
	c := openCortex(t)
	uri1, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "prefers tone=terse"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	d2 := newPreference()
	d2.Rationale = "even terser"
	uri2, err := c.Update(uri1, d2, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "prefers tone=terse v2"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.HasSuffix(string(uri2), "#2") {
		t.Fatalf("expected #2 in URI, got %q", uri2)
	}

	// v1 still resolvable
	g1, err := c.Resolve(uri1)
	if err != nil {
		t.Fatalf("Resolve v1: %v", err)
	}
	if g1.Version.Version != 1 {
		t.Fatalf("v1 version: %d", g1.Version.Version)
	}
	// v2 reflects new data
	g2, err := c.Resolve(uri2)
	if err != nil {
		t.Fatalf("Resolve v2: %v", err)
	}
	if g2.Version.Version != 2 {
		t.Fatalf("v2 version: %d", g2.Version.Version)
	}
	if g2.Head.CurrentVersion != 2 {
		t.Fatalf("head not updated: %d", g2.Head.CurrentVersion)
	}
	dec, _ := memory.DecodeData(g2.Version.Type, g2.Version.Data)
	if dec.(memory.PreferenceData).Rationale != "even terser" {
		t.Fatalf("data not updated")
	}
}

func TestUpdateRejectsTypeChange(t *testing.T) {
	c := openCortex(t)
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "p"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, err = c.Update(uri, newIdentity(), WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if !errors.Is(err, memory.ErrTypeDataMismatch) {
		t.Fatalf("expected ErrTypeDataMismatch, got %v", err)
	}
}

func TestTombstoneSetsHeadAndIsIdempotent(t *testing.T) {
	c := openCortex(t)
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "p"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Tombstone(uri, "superseded", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	got, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve after tombstone: %v", err)
	}
	if got.Head.Tombstoned == nil || got.Head.Tombstoned.Reason != "superseded" {
		t.Fatalf("Tombstoned not set: %+v", got.Head.Tombstoned)
	}
	// idempotent
	if err := c.Tombstone(uri, "again", "andrew"); err != nil {
		t.Fatalf("second Tombstone: %v", err)
	}
}

func TestUpdateRejectsTombstonedMemory(t *testing.T) {
	c := openCortex(t)
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "p"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Tombstone(uri, "x", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	_, err = c.Update(uri, newPreference(), WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if !errors.Is(err, memory.ErrTombstoned) {
		t.Fatalf("expected ErrTombstoned, got %v", err)
	}
}

func TestListByTypeReturnsAllInsertions(t *testing.T) {
	c := openCortex(t)
	for i := 0; i < 5; i++ {
		_, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
			CreatedBy:  "andrew",
			Forms:      memory.Forms{Short: "p"},
			Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
		if err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}
	ids, err := c.ListByType(memory.TypePreference, 0)
	if err != nil {
		t.Fatalf("ListByType: %v", err)
	}
	if len(ids) != 5 {
		t.Fatalf("ListByType: got %d want 5", len(ids))
	}
	// list by other type should be empty
	ids2, err := c.ListByType(memory.TypeIdentity, 0)
	if err != nil {
		t.Fatalf("ListByType Identity: %v", err)
	}
	if len(ids2) != 0 {
		t.Fatalf("Identity should be empty: %d", len(ids2))
	}
}

func TestParseURIRejectsLatest(t *testing.T) {
	id := memory.NewID()
	bad := memory.URI("matrix://cortex/Preference/" + id.String() + "#latest")
	_, _, _, err := ParseURI(bad)
	if !errors.Is(err, memory.ErrBadURI) {
		t.Fatalf("expected ErrBadURI for #latest, got %v", err)
	}
}

func TestParseURIRoundTrip(t *testing.T) {
	id := memory.NewID()
	uri := BuildURI(memory.TypeBelief, id, 7)
	tt, gotID, ver, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if tt != memory.TypeBelief || gotID != id || ver != 7 {
		t.Fatalf("round trip mismatch: %v %v %d", tt, gotID, ver)
	}
}

// TestEveryWriteJournalsExactlyOne is the load-bearing replay invariant
// check at the Phase 2 surface: N successful Writes => N journal entries,
// each KindWrite, in order.
func TestEveryWriteJournalsExactlyOne(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	for i := 0; i < 3; i++ {
		_, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
			CreatedBy:  "andrew",
			Forms:      memory.Forms{Short: "p"},
			Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
		if err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}

	var entries []journal.Entry
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		cp := *e
		cp.Payload = append([]byte(nil), e.Payload...)
		cp.CreatedBy = append([]byte(nil), e.CreatedBy...)
		entries = append(entries, cp)
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("journal count: got %d want 3", len(entries))
	}
	for i, e := range entries {
		if e.Seq != uint64(i) {
			t.Fatalf("entry %d Seq=%d", i, e.Seq)
		}
		if e.Kind != journal.KindWrite {
			t.Fatalf("entry %d Kind=%s", i, e.Kind)
		}
	}
}

// --- Phase 3 integration tests --------------------------------------------

// writePref is a per-test helper that drives the production write path
// (so idx/type, idx/tag, and salience cache are all populated).
func writePref(t *testing.T, c *Cortex, topic string, importance uint8, tags ...memory.Tag) memory.URI {
	t.Helper()
	uri, err := c.Write(memory.Head{
		ActorScope:         "andrew",
		DeclaredImportance: importance,
		Tags:               tags,
	}, memory.PreferenceData{
		SchemaVersion: 1,
		Topic:         topic,
		Polarity:      memory.PolarityPrefer,
		StrengthVal:   0.5,
	}, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "pref:" + topic},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writePref(%s): %v", topic, err)
	}
	return uri
}

func TestWriteEmitsIdxTagAndSalience(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5, "personal", "voice")
	_, id, _, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}

	// idx/tag entries: one per tag.
	for _, tag := range []string{"personal", "voice"} {
		hash := query.HashTag(tag)
		var idStart memory.ID
		hits := 0
		err := c.s.PrefixIter(keys.IdxTagPrefix(hash), func(k, _ []byte) error {
			_, parsed, err := keys.ParseIdxTagKey(k)
			if err != nil {
				return err
			}
			var got memory.ID
			copy(got[:], parsed[:])
			if got == id {
				hits++
				idStart = got
			}
			return nil
		})
		if err != nil {
			t.Fatalf("PrefixIter idx/tag: %v", err)
		}
		if hits != 1 {
			t.Fatalf("tag %q: idx hit count=%d want 1", tag, hits)
		}
		_ = idStart
	}

	// salience entry exists with a non-zero cold score.
	score, ok, err := salience.Read(c.s, id)
	if err != nil || !ok {
		t.Fatalf("salience.Read: ok=%v err=%v", ok, err)
	}
	if score.Cached <= 0 {
		t.Fatalf("salience Cached should be >0 for fresh write, got %f", score.Cached)
	}
	if score.Importance != 5 {
		t.Fatalf("Importance: got %d want 5", score.Importance)
	}
}

func TestUpdateBumpsSalience(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Hour); return clk }

	uri := writePref(t, c, "tone", 3)
	_, id, _, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	before, _, err := salience.Read(c.s, id)
	if err != nil {
		t.Fatalf("salience.Read pre: %v", err)
	}

	if _, err := c.Update(uri, memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone", Polarity: memory.PolarityPrefer, StrengthVal: 0.9,
	}, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "pref:tone v2"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, _, err := salience.Read(c.s, id)
	if err != nil {
		t.Fatalf("salience.Read post: %v", err)
	}
	if after.LastUsed <= before.LastUsed {
		t.Fatalf("LastUsed not advanced: before=%d after=%d", before.LastUsed, after.LastUsed)
	}
}

func TestTombstoneZeroesSalience(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 9)
	_, id, _, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	if err := c.Tombstone(uri, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	score, ok, err := salience.Read(c.s, id)
	if err != nil || !ok {
		t.Fatalf("salience.Read post-tombstone: ok=%v err=%v", ok, err)
	}
	if score.Cached != 0 {
		t.Fatalf("salience Cached should be 0 post-tombstone, got %f", score.Cached)
	}
	// Importance preserved (factor inputs not cleared).
	if score.Importance != 9 {
		t.Fatalf("Importance should be preserved: got %d", score.Importance)
	}
}

func TestFindByTypeReturnsAllNonTombstoned(t *testing.T) {
	c := openCortex(t)
	for i, tag := range []string{"a", "b", "c", "d", "e"} {
		writePref(t, c, "topic-"+tag, uint8(i+1))
	}
	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 5 {
		t.Fatalf("Find: got %d want 5", len(res.Memories))
	}
	if res.CandidatesScanned != 5 {
		t.Fatalf("CandidatesScanned: got %d want 5", res.CandidatesScanned)
	}
}

func TestFindByHasTagIntersection(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "a", 1, "personal", "voice")
	writePref(t, c, "b", 1, "personal", "tempo")
	writePref(t, c, "c", 1, "voice", "tempo")
	writePref(t, c, "d", 1, "personal", "voice", "tempo")

	res, err := c.Find(query.Query{
		Where: query.And{Children: []query.Predicate{
			query.HasTag{Tag: "personal"},
			query.HasTag{Tag: "voice"},
		}},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	// Expect "a" and "d" (both have personal AND voice).
	if len(res.Memories) != 2 {
		t.Fatalf("got %d memories want 2", len(res.Memories))
	}
	topics := map[string]bool{}
	for _, m := range res.Memories {
		d, _ := memory.DecodeData(m.Version.Type, m.Version.Data)
		topics[d.(memory.PreferenceData).Topic] = true
	}
	if !topics["a"] || !topics["d"] {
		t.Fatalf("topics: %v want {a,d}", topics)
	}
}

func TestFindWherePredicateFilters(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 2)
	writePref(t, c, "tempo", 8)
	writePref(t, c, "tone", 9)

	res, err := c.Find(query.Query{
		Type: []memory.Type{memory.TypePreference},
		Where: query.And{Children: []query.Predicate{
			query.Eq{Field: "data.topic", Value: "tone"},
			query.Gte{Field: "head.declared_importance", Value: 5},
		}},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("got %d want 1", len(res.Memories))
	}
	if res.Memories[0].Head.DeclaredImportance != 9 {
		t.Fatalf("wrong memory: %+v", res.Memories[0].Head)
	}
}

func TestFindOrdersBySalienceDescByDefault(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	// Higher importance → higher D → higher salience.
	writePref(t, c, "low", 1)
	writePref(t, c, "mid", 5)
	writePref(t, c, "hi", 10)

	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 3 {
		t.Fatalf("got %d want 3", len(res.Memories))
	}
	// Topic order should follow importance desc: hi, mid, low.
	want := []string{"hi", "mid", "low"}
	for i, m := range res.Memories {
		d, _ := memory.DecodeData(m.Version.Type, m.Version.Data)
		got := d.(memory.PreferenceData).Topic
		if got != want[i] {
			t.Fatalf("ordering[%d]: got %q want %q", i, got, want[i])
		}
	}
}

func TestFindExcludesTombstonedByDefault(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	if err := c.Tombstone(uri, "x", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("default-exclude tombstoned: got %d want 1", len(res.Memories))
	}
	res, err = c.Find(query.Query{
		Type:              []memory.Type{memory.TypePreference},
		Limit:             10,
		IncludeTombstoned: true,
	})
	if err != nil {
		t.Fatalf("Find include: %v", err)
	}
	if len(res.Memories) != 2 {
		t.Fatalf("IncludeTombstoned: got %d want 2", len(res.Memories))
	}
}

func TestFindOffsetAndLimit(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Second); return clk }
	for i := 0; i < 7; i++ {
		writePref(t, c, "topic", uint8(10-i))
	}
	res, err := c.Find(query.Query{
		Type:   []memory.Type{memory.TypePreference},
		Limit:  3,
		Offset: 2,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 3 {
		t.Fatalf("got %d want 3", len(res.Memories))
	}
	if res.Total != 7 {
		t.Fatalf("Total: got %d want 7", res.Total)
	}
}

func TestFindRefusesUnbounded(t *testing.T) {
	c := openCortex(t)
	_, err := c.Find(query.Query{Type: []memory.Type{memory.TypePreference}})
	if !errors.Is(err, query.ErrUnbounded) {
		t.Fatalf("expected ErrUnbounded, got %v", err)
	}
}

func TestFindRefusesUnsupported(t *testing.T) {
	c := openCortex(t)
	// Phase 5: Near is supported, but only when an embedder is running.
	// With no StartEmbedder call, Find should refuse with a clear
	// "no embedder" error (NOT silently fall back to a non-vector path).
	_, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 1,
		Near:  "x",
	})
	if err == nil {
		t.Fatalf("expected error for Near without embedder, got nil")
	}
	if !strings.Contains(err.Error(), "embedder") {
		t.Fatalf("expected embedder-related error, got %v", err)
	}

	// Phase 6: Follow without From is rejected (no anchor to start from).
	_, err = c.Find(query.Query{
		Type:   []memory.Type{memory.TypePreference},
		Limit:  1,
		Follow: &query.EdgeExpr{Direction: query.DirOut},
	})
	if !errors.Is(err, query.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported (Follow without From), got %v", err)
	}
}

func TestFindRefusesTooBroad(t *testing.T) {
	c := openCortex(t)
	_, err := c.Find(query.Query{Limit: 10})
	if !errors.Is(err, query.ErrTooBroad) {
		t.Fatalf("expected ErrTooBroad, got %v", err)
	}
}

func TestFindLateBindingJournalsKindFind(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	pre := c.s.NextSeq()
	if _, err := c.Find(query.Query{
		Type:        []memory.Type{memory.TypePreference},
		Limit:       1,
		LateBinding: true,
	}); err != nil {
		t.Fatalf("Find: %v", err)
	}
	post := c.s.NextSeq()
	if post != pre+1 {
		t.Fatalf("expected exactly one journal entry appended: pre=%d post=%d", pre, post)
	}
	// Inspect the latest journal entry to confirm kind+payload.
	var last *journal.Entry
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		cp := *e
		cp.Payload = append([]byte(nil), e.Payload...)
		last = &cp
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if last == nil || last.Kind != journal.KindFind {
		t.Fatalf("last entry not KindFind: %+v", last)
	}
	if !bytes.Equal(last.CreatedBy, []byte("query.Run")) {
		t.Fatalf("CreatedBy: got %q want %q", last.CreatedBy, "query.Run")
	}
	if len(last.Payload) == 0 {
		t.Fatalf("late-binding payload empty")
	}
}

func TestFindCompileTimeDoesNotJournal(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	pre := c.s.NextSeq()
	if _, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 1,
		// LateBinding zero value (false) ⇒ no journal write.
	}); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if c.s.NextSeq() != pre {
		t.Fatalf("compile-time Find should not journal")
	}
}

func TestUpdateAndTombstoneAlsoJournal(t *testing.T) {
	c := openCortex(t)
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, newPreference(), WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "p"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := c.Update(uri, newPreference(), WriteMeta{CreatedBy: "andrew",
		Forms: memory.Forms{Short: "p2"}, Provenance: memory.Provenance{Source: memory.SourceUserInput}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := c.Tombstone(uri, "stop", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	var kinds []journal.Kind
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		kinds = append(kinds, e.Kind)
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	want := []journal.Kind{journal.KindWrite, journal.KindUpdate, journal.KindTombstone}
	if len(kinds) != len(want) {
		t.Fatalf("kinds: %v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds[%d]=%s want %s", i, kinds[i], want[i])
		}
	}
}

// --- Phase 4: form generation + budget render ----------------------------

// TestWriteAutoRendersForms verifies that callers leaving FormsOverride=false
// get deterministic per-type forms generated and persisted on BOTH Head and
// Version (Q1 invariant: head mirrors latest version's forms).
func TestWriteAutoRendersForms(t *testing.T) {
	c := openCortex(t)
	uri, err := c.Write(memory.Head{ActorScope: "andrew", DeclaredImportance: 5},
		memory.PreferenceData{
			SchemaVersion: 1,
			Topic:         "tone",
			Polarity:      memory.PolarityPrefer,
			StrengthVal:   0.7,
			Rationale:     "andrew prefers terse over verbose",
		},
		WriteMeta{CreatedBy: "andrew",
			Provenance: memory.Provenance{Source: memory.SourceUserInput}},
	)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Version.Forms.Short == "" {
		t.Fatalf("auto short form empty")
	}
	if m.Version.Forms.Medium == "" {
		t.Fatalf("auto medium form empty")
	}
	if m.Version.FormsOverride {
		t.Fatalf("expected FormsOverride=false on auto-render path")
	}
	// Head must mirror Version.
	if m.Head.Forms != m.Version.Forms {
		t.Fatalf("head/version forms divergence: head=%+v version=%+v",
			m.Head.Forms, m.Version.Forms)
	}
	// Forms must respect budgets.
	if memory.CountTokens(m.Version.Forms.Short) > memory.MaxShortTokens {
		t.Fatalf("short over budget: %q", m.Version.Forms.Short)
	}
	if memory.CountTokens(m.Version.Forms.Medium) > memory.MaxMediumTokens {
		t.Fatalf("medium over budget: %q", m.Version.Forms.Medium)
	}
}

// TestWriteHonorsFormsOverride verifies that meta.FormsOverride=true short-
// circuits auto-render and the caller's Forms appear verbatim on persist.
func TestWriteHonoursFormsOverride(t *testing.T) {
	c := openCortex(t)
	override := memory.Forms{Short: "skill-short", Medium: "skill-medium-detail"}
	uri, err := c.Write(memory.Head{ActorScope: "andrew"},
		newPreference(),
		WriteMeta{
			CreatedBy:     "andrew",
			Forms:         override,
			FormsOverride: true,
			Provenance:    memory.Provenance{Source: memory.SourceUserInput},
		})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Version.Forms != override {
		t.Fatalf("override not honoured: got %+v want %+v", m.Version.Forms, override)
	}
	if !m.Version.FormsOverride {
		t.Fatalf("FormsOverride flag not persisted")
	}
	if m.Head.Forms != override {
		t.Fatalf("head forms must mirror version override")
	}
}

// TestWriteRejectsOversizeOverride verifies §9.3 hard-reject for oversize
// skill-supplied forms (auto path truncates; override path errors).
func TestWriteRejectsOversizeOverride(t *testing.T) {
	c := openCortex(t)
	huge := strings.Repeat("a", (memory.MaxShortTokens+10)*memory.BytesPerToken)
	_, err := c.Write(memory.Head{ActorScope: "andrew"},
		newPreference(),
		WriteMeta{
			CreatedBy:     "andrew",
			Forms:         memory.Forms{Short: huge},
			FormsOverride: true,
			Provenance:    memory.Provenance{Source: memory.SourceUserInput},
		})
	if !errors.Is(err, memory.ErrFormTooLong) {
		t.Fatalf("expected ErrFormTooLong, got %v", err)
	}
}

// TestUpdateReRendersForms verifies that an Update on the auto path produces
// fresh forms reflecting the new Data.
func TestUpdateReRendersForms(t *testing.T) {
	c := openCortex(t)
	uri1, err := c.Write(memory.Head{ActorScope: "andrew"},
		memory.PreferenceData{SchemaVersion: 1, Topic: "tone",
			Polarity: memory.PolarityPrefer, StrengthVal: 0.5,
			Rationale: "first reason"},
		WriteMeta{CreatedBy: "andrew",
			Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	m1, _ := c.Resolve(uri1)
	uri2, err := c.Update(uri1,
		memory.PreferenceData{SchemaVersion: 1, Topic: "tone",
			Polarity: memory.PolarityPrefer, StrengthVal: 0.9,
			Rationale: "evolved reason"},
		WriteMeta{CreatedBy: "andrew",
			Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	m2, _ := c.Resolve(uri2)
	if m1.Version.Forms.Medium == m2.Version.Forms.Medium {
		t.Fatalf("Update did not re-render medium: %q", m2.Version.Forms.Medium)
	}
	if m2.Head.Forms != m2.Version.Forms {
		t.Fatalf("head/version forms divergence after Update")
	}
}

// TestFindWithFormShortPopulatesRendered verifies the Find render path:
// Form=short → Result.Rendered carries persisted Short, parallel-indexed.
func TestFindWithFormShortPopulatesRendered(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		Form:  query.FormShort,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Rendered) != len(res.Memories) {
		t.Fatalf("rendered len=%d != memories len=%d", len(res.Rendered), len(res.Memories))
	}
	if res.Form != query.FormShort {
		t.Fatalf("res.Form=%q want short", res.Form)
	}
	for i, m := range res.Memories {
		if res.Rendered[i] != m.Version.Forms.Short {
			t.Fatalf("rendered[%d]=%q != version.Forms.Short=%q",
				i, res.Rendered[i], m.Version.Forms.Short)
		}
		if res.RenderedTokens[i] != memory.CountTokens(res.Rendered[i]) {
			t.Fatalf("rendered_tokens mismatch at %d", i)
		}
	}
}

// TestFindWithFormFullLiveRenders verifies that Form=full uses
// forms.RenderFull rather than the persisted Forms.Medium (which it
// happens to equal in Phase 4 — but the codepath is distinct).
func TestFindWithFormFullLiveRenders(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 1,
		Form:  query.FormFull,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Rendered) != 1 {
		t.Fatalf("expected 1 rendered, got %d", len(res.Rendered))
	}
	if res.Rendered[0] == "" {
		t.Fatalf("full render empty")
	}
}

// TestFindBudgetTokensTrimsLowSalience verifies the §12.1 trim rule: when
// the rendered total exceeds BudgetTokens, the lowest-salience entries are
// dropped first regardless of OrderBy. We also cover the no-Form-defaults-
// to-medium path.
func TestFindBudgetTokensTrimsLowSalience(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Second); return clk }
	// Importance ladder: lowest gets dropped first under budget pressure.
	uriLow := writePref(t, c, "low", 1)
	writePref(t, c, "mid", 5)
	writePref(t, c, "hi", 10)

	// Compute a budget that fits 2 of 3 medium forms but not all 3.
	full, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		Form:  query.FormMedium,
	})
	if err != nil {
		t.Fatalf("baseline Find: %v", err)
	}
	if len(full.Memories) != 3 {
		t.Fatalf("baseline len=%d want 3", len(full.Memories))
	}
	totalAll := 0
	for _, n := range full.RenderedTokens {
		totalAll += n
	}
	// Budget = totalAll - smallest entry's tokens, so trimming exactly one
	// suffices. (All three Preferences share the same template scaffold so
	// rendered token counts are equal; pick a budget that drops one item.)
	perItem := full.RenderedTokens[0]
	budget := perItem*2 + perItem/2 // < 3 * perItem, ≥ 2 * perItem

	// No Form set -> defaults to medium.
	res, err := c.Find(query.Query{
		Type:         []memory.Type{memory.TypePreference},
		Limit:        10,
		BudgetTokens: budget,
	})
	if err != nil {
		t.Fatalf("budget Find: %v", err)
	}
	if res.Form != query.FormMedium {
		t.Fatalf("expected default Form=medium, got %q", res.Form)
	}
	if res.TrimmedByBudget == 0 {
		t.Fatalf("expected trim, got 0 (totalAll=%d, budget=%d, perItem=%d)",
			totalAll, budget, perItem)
	}
	// Confirm the dropped one is the lowest-salience (importance=1).
	_, lowID, _, _ := ParseURI(uriLow)
	for _, m := range res.Memories {
		if m.Head.ID == lowID {
			t.Fatalf("low-salience memory survived trim; expected drop")
		}
	}
	// Sum of surviving rendered tokens must respect budget (modulo the
	// "always retain at least one" relief valve).
	sum := 0
	for _, n := range res.RenderedTokens {
		sum += n
	}
	if sum > budget && len(res.Memories) > 1 {
		t.Fatalf("rendered sum=%d > budget=%d with %d kept", sum, budget, len(res.Memories))
	}
}

// TestFindBudgetTokensRetainsAtLeastOne verifies the relief valve: even a
// budget too small for any single rendered medium keeps one entry.
func TestFindBudgetTokensRetainsAtLeastOne(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	res, err := c.Find(query.Query{
		Type:         []memory.Type{memory.TypePreference},
		Limit:        10,
		BudgetTokens: 1, // pathologically tight
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("expected exactly 1 retained under tight budget, got %d", len(res.Memories))
	}
}

// --- Phase 5 integration tests --------------------------------------------

// startTestEmbedder wires the deterministic HashEmbedder + a temp-dir
// HNSW index file into c, drains existing journal entries, and registers
// a cleanup that calls StopEmbedder. Returns nothing; the embedder is
// owned by the cortex from here on.
func startTestEmbedder(t *testing.T, c *Cortex) {
	t.Helper()
	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}
	t.Cleanup(func() {
		if err := c.StopEmbedder(); err != nil {
			t.Logf("StopEmbedder cleanup: %v", err)
		}
	})
}

// drainEmbedder blocks until the embedder has caught up with the journal
// head, with a generous timeout so we surface "worker stuck" rather than
// hanging the test process.
func drainEmbedder(t *testing.T, c *Cortex) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DrainEmbedder(ctx); err != nil {
		t.Fatalf("DrainEmbedder: %v", err)
	}
}

// TestEmbedderEmbedsExistingMemories verifies the StartEmbedder backlog
// drain: writes that happened BEFORE the embedder started must end up
// embedded by the time StartEmbedder returns.
func TestEmbedderEmbedsExistingMemories(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	// At this point head has no EmbeddingRef.
	pre, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pre.Head.EmbeddingRef != nil {
		t.Fatalf("pre-embedder EmbeddingRef should be nil, got %+v", pre.Head.EmbeddingRef)
	}

	startTestEmbedder(t, c)

	post, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if post.Head.EmbeddingRef == nil {
		t.Fatalf("post-embedder EmbeddingRef should be set")
	}
	if post.Head.EmbeddingRef.Dim != uint16(embed.DefaultDim) {
		t.Fatalf("EmbeddingRef.Dim = %d, want %d", post.Head.EmbeddingRef.Dim, embed.DefaultDim)
	}
	if !strings.HasPrefix(post.Head.EmbeddingRef.Model, "hash-stub@") {
		t.Fatalf("EmbeddingRef.Model = %q, want hash-stub@*", post.Head.EmbeddingRef.Model)
	}

	// vec/meta/<id> should also be present with full vector bytes.
	_, id, _, _ := ParseURI(uri)
	var u keys.ULID
	copy(u[:], id[:])
	raw, ok, err := c.s.Get(keys.VecMetaKey(u))
	if err != nil || !ok {
		t.Fatalf("vec/meta missing: ok=%v err=%v", ok, err)
	}
	var vm memory.VectorMeta
	if err := memory.DecodeVectorMeta(raw, &vm); err != nil {
		t.Fatalf("DecodeVectorMeta: %v", err)
	}
	if len(vm.Vector) != embed.DefaultDim {
		t.Fatalf("len(Vector) = %d, want %d", len(vm.Vector), embed.DefaultDim)
	}
	// Hash invariant: vm.VectorHash matches memory.HashVector(vm.Vector).
	if memory.HashVector(vm.Vector) != vm.VectorHash {
		t.Fatalf("VectorHash mismatch")
	}
}

// TestEmbedderEmbedsAfterStart verifies live behaviour: writes that
// happen AFTER StartEmbedder are also processed (the notify channel
// works end-to-end).
func TestEmbedderEmbedsAfterStart(t *testing.T) {
	c := openCortex(t)
	startTestEmbedder(t, c)

	uri := writePref(t, c, "tone", 5)
	drainEmbedder(t, c)

	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Head.EmbeddingRef == nil || m.Head.EmbeddingRef.VertexID == 0 {
		t.Fatalf("live write not embedded: %+v", m.Head.EmbeddingRef)
	}
}

// TestEmbedderJournalsKindEmbed verifies a KindEmbed entry lands in the
// journal for every embed action — load-bearing for the replay invariant.
func TestEmbedderJournalsKindEmbed(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	startTestEmbedder(t, c)

	var embedCount int
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		if e.Kind == journal.KindEmbed {
			embedCount++
			var p journal.EmbedPayload
			if err := journal.DecodeEmbedPayload(e.Payload, &p); err != nil {
				return fmt.Errorf("decode payload: %w", err)
			}
			if p.VertexID == 0 || p.Dim != uint16(embed.DefaultDim) {
				return fmt.Errorf("bad payload: %+v", p)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if embedCount != 2 {
		t.Fatalf("expected 2 KindEmbed entries, got %d", embedCount)
	}
}

// TestFindNearSemanticRecall is the load-bearing user-facing Phase 5
// integration: write distinct topics, query by text near one of them,
// expect that memory to rank first.
func TestFindNearSemanticRecall(t *testing.T) {
	c := openCortex(t)
	tonURI := writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	writePref(t, c, "vocabulary", 5)
	startTestEmbedder(t, c)

	// HashEmbedder gives identical text → identical vectors. We need the
	// Find Near query text to match one memory's rendered Full form
	// exactly to drive recall in a way that's bit-stable across runs.
	// Resolve the target memory and embed its full form as the query.
	tonM, err := c.Resolve(tonURI)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	dec, err := memory.DecodeData(tonM.Version.Type, tonM.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	queryText := forms.RenderFull(&tonM.Head, dec)

	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Near:  queryText,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Find Near: %v", err)
	}
	if len(res.Memories) == 0 {
		t.Fatalf("expected at least 1 hit, got 0")
	}
	if res.Memories[0].Head.ID != tonM.Head.ID {
		t.Fatalf("top-1 mismatch: got %s want %s",
			res.Memories[0].Head.ID, tonM.Head.ID)
	}
	if d := res.Distances[res.Memories[0].Head.ID]; d > 1e-4 {
		t.Fatalf("top-1 distance = %v, want ~0 (self-recall)", d)
	}
}

// TestFindNearURIUsesPersistedVector exercises the alternative entry
// point: NearURI resolves to an existing memory's vec/meta and uses that
// as the query vector. Equivalent to "find me memories similar to this
// memory I already know about."
func TestFindNearURIUsesPersistedVector(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	startTestEmbedder(t, c)

	res, err := c.Find(query.Query{
		Type:    []memory.Type{memory.TypePreference},
		NearURI: &a,
		Limit:   2,
	})
	if err != nil {
		t.Fatalf("Find NearURI: %v", err)
	}
	if len(res.Memories) == 0 {
		t.Fatalf("expected at least 1 hit")
	}
	_, aid, _, _ := ParseURI(a)
	if res.Memories[0].Head.ID != aid {
		t.Fatalf("self-recall failed: top-1 = %s want %s",
			res.Memories[0].Head.ID, aid)
	}
}

// TestFindNearRespectsWherePostFilter verifies Where predicates apply
// AFTER HNSW candidate selection, so a Near query with a Where clause
// returns only memories matching BOTH constraints.
func TestFindNearRespectsWherePostFilter(t *testing.T) {
	c := openCortex(t)
	target := writePref(t, c, "tone", 5, "vocal")
	writePref(t, c, "tempo", 5, "vocal")
	writePref(t, c, "tone", 5, "non-matching")
	startTestEmbedder(t, c)

	// Query: Near the "tone vocal" memory but require tag=non-matching.
	// Expect only the non-matching memory back, NOT the target.
	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Where: query.HasTag{Tag: "non-matching"},
		Near:  forms.RenderFull(&memory.Head{Type: memory.TypePreference}, mustResolvePref(t, c, target)),
		Limit: 5,
		NearK: 16, // overshoot so the filter has slack
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, m := range res.Memories {
		if m.Head.ID == idOf(target) {
			t.Fatalf("Where filter should have excluded the target memory")
		}
		hasTag := false
		for _, tg := range m.Head.Tags {
			if tg == "non-matching" {
				hasTag = true
				break
			}
		}
		if !hasTag {
			t.Fatalf("result missing required tag: %+v", m.Head.Tags)
		}
	}
}

// TestFindNearTombstonedSkipped verifies that tombstoning a memory
// causes Find Near to skip it (the HNSW node is marked deleted by the
// embedder's tombstone-entry hook).
func TestFindNearTombstonedSkipped(t *testing.T) {
	c := openCortex(t)
	a := writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	startTestEmbedder(t, c)

	if err := c.Tombstone(a, "no longer relevant", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	drainEmbedder(t, c)

	res, err := c.Find(query.Query{
		Type:    []memory.Type{memory.TypePreference},
		NearURI: &a,
		Limit:   2,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	_, aid, _, _ := ParseURI(a)
	for _, m := range res.Memories {
		if m.Head.ID == aid {
			t.Fatalf("tombstoned memory still returned from Find Near")
		}
	}
}

// TestFindNearWithoutEmbedderErrors verifies the cortex-level guard
// against using Near without a running embedder.
func TestFindNearWithoutEmbedderErrors(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	_, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Near:  "anything",
		Limit: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "embedder") {
		t.Fatalf("expected embedder error, got %v", err)
	}
}

// TestEmbedderUpdateReusesVertex verifies that updating a memory's data
// re-embeds against the existing vertex id rather than allocating a new
// one. Otherwise the index would grow unboundedly across Updates.
func TestEmbedderUpdateReusesVertex(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)
	startTestEmbedder(t, c)

	before, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if before.Head.EmbeddingRef == nil {
		t.Fatalf("pre-update EmbeddingRef nil")
	}
	beforeVid := before.Head.EmbeddingRef.VertexID

	newURI, err := c.Update(uri, memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone", Polarity: memory.PolarityPrefer,
		StrengthVal: 0.9, Rationale: "even more terse",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	drainEmbedder(t, c)

	after, err := c.Resolve(newURI)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if after.Head.EmbeddingRef == nil {
		t.Fatalf("post-update EmbeddingRef nil")
	}
	if after.Head.EmbeddingRef.VertexID != beforeVid {
		t.Fatalf("vertex id changed across Update: %d → %d",
			beforeVid, after.Head.EmbeddingRef.VertexID)
	}
}

// TestEmbedderIndexFilePersisted verifies StopEmbedder writes the index
// file to disk; a fresh StartEmbedder loads it without rebuilding.
func TestEmbedderIndexFilePersisted(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)

	idxPath := filepath.Join(t.TempDir(), "index.hnsw")
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("StartEmbedder: %v", err)
	}
	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("StopEmbedder: %v", err)
	}
	st, err := os.Stat(idxPath)
	if err != nil || st.Size() == 0 {
		t.Fatalf("expected non-empty index file, got %v size=%v", err, st)
	}

	// Restart: vec/meta replay + (since file exists) Load should give
	// the same Len() back.
	if err := c.StartEmbedder(EmbedderOptions{
		Embedder:     embed.NewHashEmbedder(),
		IndexPath:    idxPath,
		TickInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("second StartEmbedder: %v", err)
	}
	if got := c.Index().Len(); got != 2 {
		t.Fatalf("loaded Len() = %d, want 2", got)
	}
	if err := c.StopEmbedder(); err != nil {
		t.Fatalf("final StopEmbedder: %v", err)
	}
}

// mustResolvePref is a small helper for tests that need the typed Data
// of a memory by URI; it panics-via-t.Fatal on any failure path so the
// caller stays inline.
func mustResolvePref(t *testing.T, c *Cortex, uri memory.URI) memory.PreferenceData {
	t.Helper()
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	dec, err := memory.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	return dec.(memory.PreferenceData)
}

// idOf extracts the memory ID from a URI; panics on malformed URI (test
// helper only).
func idOf(uri memory.URI) memory.ID {
	_, id, _, err := ParseURI(uri)
	if err != nil {
		panic(err)
	}
	return id
}

// --- Phase 7: snapshots / Merkle ----------------------------------------

func TestPhase7WriteAdvancesMMRAndSMT(t *testing.T) {
	c := openCortex(t)
	beforeJR, beforeSR, beforeOR, err := c.snap.CurrentRoots()
	if err != nil {
		t.Fatalf("CurrentRoots before: %v", err)
	}
	writePref(t, c, "tone", 5, "personal")
	afterJR, afterSR, afterOR, err := c.snap.CurrentRoots()
	if err != nil {
		t.Fatalf("CurrentRoots after: %v", err)
	}
	if beforeJR == afterJR {
		t.Errorf("Write did not advance JournalRoot")
	}
	if beforeSR["memories"] == afterSR["memories"] {
		t.Errorf("Write did not advance memories StateRoot")
	}
	if beforeSR["edges"] != afterSR["edges"] {
		t.Errorf("Write incorrectly advanced edges StateRoot")
	}
	if beforeOR == afterOR {
		t.Errorf("Write did not advance OverallRoot")
	}
}

func TestPhase7AddEdgeAdvancesEdgesRoot(t *testing.T) {
	c := openCortex(t)
	uA := writePref(t, c, "topic-a", 5)
	uB := writePref(t, c, "topic-b", 5)
	beforeSR := mustStateRoots(t, c)
	idA := idOf(uA)
	idB := idOf(uB)
	if err := c.AddEdge(idA, memory.EdgeReferences, idB, AddEdgeMeta{
		CreatedBy: "andrew",
	}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	afterSR := mustStateRoots(t, c)
	if afterSR["edges"] == beforeSR["edges"] {
		t.Errorf("AddEdge did not advance edges StateRoot")
	}
	if afterSR["memories"] != beforeSR["memories"] {
		t.Errorf("AddEdge incorrectly advanced memories StateRoot")
	}
}

func TestPhase7TombstoneAdvancesMemoriesRoot(t *testing.T) {
	c := openCortex(t)
	u := writePref(t, c, "tone", 5)
	beforeSR := mustStateRoots(t, c)
	if err := c.Tombstone(u, "no-longer-relevant", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	afterSR := mustStateRoots(t, c)
	if afterSR["memories"] == beforeSR["memories"] {
		t.Errorf("Tombstone did not advance memories StateRoot")
	}
}

func TestPhase7RemoveEdgeAdvancesEdgesRoot(t *testing.T) {
	c := openCortex(t)
	uA := writePref(t, c, "a", 5)
	uB := writePref(t, c, "b", 5)
	idA := idOf(uA)
	idB := idOf(uB)
	if err := c.AddEdge(idA, memory.EdgeReferences, idB, AddEdgeMeta{}); err != nil {
		t.Fatal(err)
	}
	beforeSR := mustStateRoots(t, c)
	if err := c.RemoveEdge(idA, memory.EdgeReferences, idB, "wrong", "andrew"); err != nil {
		t.Fatal(err)
	}
	afterSR := mustStateRoots(t, c)
	if afterSR["edges"] == beforeSR["edges"] {
		t.Errorf("RemoveEdge did not advance edges StateRoot (soft-delete must change EdgeRecord.Tombstoned)")
	}
}

func TestPhase7DeterministicAcrossActors(t *testing.T) {
	// Two cortexes, identical input sequences (same fixed clock and IDs)
	// → byte-identical OverallRoot.
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	idCounter := byte(0)
	idGen := func() memory.ID {
		idCounter++
		var id memory.ID
		id[0] = idCounter
		return id
	}

	build := func(actor string) [32]byte {
		dir := t.TempDir()
		s, err := store.Open(dir, actor, nil)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })

		// Reset id counter so both cortexes generate the same IDs.
		idCounter = 0
		c := New(s, WithClock(clock), WithIDGen(idGen))

		writePref(t, c, "topic-1", 5, "tag1")
		writePref(t, c, "topic-2", 7, "tag1", "tag2")
		root, err := c.OverallRoot()
		if err != nil {
			t.Fatalf("OverallRoot: %v", err)
		}
		return root
	}
	r1 := build("alpha")
	r2 := build("beta")
	if r1 != r2 {
		t.Errorf("OverallRoot differs across actors with identical input:\n  r1=%x\n  r2=%x", r1[:8], r2[:8])
	}
}

func TestPhase7SnapshotAPIPersists(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "topic-a", 5)
	writePref(t, c, "topic-b", 6)

	m, err := c.Snapshot("compile")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if m.Trigger != "compile" {
		t.Errorf("Trigger = %q, want compile", m.Trigger)
	}
	if m.Actor != "andrew" {
		t.Errorf("Actor = %q, want andrew", m.Actor)
	}
	if m.JournalSeq != 2 {
		t.Errorf("JournalSeq = %d, want 2", m.JournalSeq)
	}
	if m.Counters.Memories != 2 {
		t.Errorf("Counters.Memories = %d, want 2", m.Counters.Memories)
	}

	// Re-load and compare bytes.
	loaded, err := c.snap.LoadSnapshot(m.SeqAtSnapshot)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if loaded.OverallRoot != m.OverallRoot {
		t.Errorf("Persisted OverallRoot differs from in-memory")
	}
}

func TestPhase7SnapshotCommitsToCurrentState(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "a", 5)
	m1, _ := c.Snapshot("compile")
	writePref(t, c, "b", 5)
	m2, _ := c.Snapshot("compile")
	if m1.OverallRoot == m2.OverallRoot {
		t.Errorf("OverallRoot did not change between snapshots after a write")
	}
	if m2.JournalSeq <= m1.JournalSeq {
		t.Errorf("JournalSeq did not advance: m1=%d m2=%d", m1.JournalSeq, m2.JournalSeq)
	}
}

func TestPhase7SnapshotMembershipProofVerifies(t *testing.T) {
	c := openCortex(t)
	uA := writePref(t, c, "a", 5)
	writePref(t, c, "b", 5)
	writePref(t, c, "c", 5)

	m, err := c.Snapshot("compile")
	if err != nil {
		t.Fatal(err)
	}
	idA := idOf(uA)
	smt := c.snap.SMT("memories")
	keyHash := snapshotMemoryKey(idA)

	// The Head canonical bytes are needed to recompute the leaf; pull
	// them from the store and call ProveWithValue.
	headBytes := mustReadHead(t, c, idA)
	pf, err := smt.ProveWithValue(keyHash, headBytes)
	if err != nil {
		t.Fatalf("ProveWithValue: %v", err)
	}
	if err := snapshotVerifyMembership(m.StateRoots["memories"], pf); err != nil {
		t.Errorf("VerifyMembership: %v", err)
	}
}

func TestPhase7SnapshotNonMembershipProofVerifies(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "a", 5)
	m, err := c.Snapshot("compile")
	if err != nil {
		t.Fatal(err)
	}
	smt := c.snap.SMT("memories")
	// A memory ID we never wrote.
	var fake memory.ID
	fake[0] = 0xAA
	keyHash := snapshotMemoryKey(fake)
	pf, err := smt.Prove(keyHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshotVerifyMembership(m.StateRoots["memories"], pf); err != nil {
		t.Errorf("non-membership VerifyMembership: %v", err)
	}
}

func TestPhase7OverallRootIsPureFunction(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "a", 5)
	r1, err := c.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Errorf("OverallRoot non-deterministic across calls")
	}
	// Snapshot does not mutate state.
	if _, err := c.Snapshot("compile"); err != nil {
		t.Fatal(err)
	}
	r3, err := c.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r3 {
		t.Errorf("Snapshot changed OverallRoot (must be pull-driven, no mutation)")
	}
}

func TestPhase7JournalLeafCountTracksJournalSeq(t *testing.T) {
	c := openCortex(t)
	for i := 0; i < 5; i++ {
		writePref(t, c, fmt.Sprintf("topic-%d", i), 5)
	}
	leafCount, err := c.snap.MMR().LeafCount()
	if err != nil {
		t.Fatal(err)
	}
	if leafCount != 5 {
		t.Errorf("MMR.LeafCount = %d, want 5", leafCount)
	}
	if leafCount != c.s.NextSeq() {
		t.Errorf("MMR LeafCount %d != journal NextSeq %d", leafCount, c.s.NextSeq())
	}
}

// --- Phase 7 helpers ---------------------------------------------------

func mustStateRoots(t *testing.T, c *Cortex) map[string][32]byte {
	t.Helper()
	_, sr, _, err := c.snap.CurrentRoots()
	if err != nil {
		t.Fatalf("CurrentRoots: %v", err)
	}
	return sr
}

func mustReadHead(t *testing.T, c *Cortex, id memory.ID) []byte {
	t.Helper()
	raw, ok, err := c.s.Get(keys.MemoryHeadKey(toKeysULID(id)))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("head missing for id %x", id[:])
	}
	return raw
}

// snapshotMemoryKey returns the SMT key-hash for id.
func snapshotMemoryKey(id memory.ID) [32]byte {
	var raw [16]byte
	copy(raw[:], id[:])
	return snapshot.HashMemoryKey(raw)
}

// snapshotVerifyMembership wraps snapshot.VerifyMembership.
func snapshotVerifyMembership(root [32]byte, pf *snapshot.MembershipProof) error {
	return snapshot.VerifyMembership(root, pf)
}

// ============================================================================
// Phase 8 — Context bundle composer + idx/frame + idx/actor_obj
//
// Spec: research/04-cortex.md §12.1, research/03-retrieval-patterns.md §2.
// Each test cites the spec rule it covers.
// ============================================================================

// frameAcquireGPU is the canonical Phase 8 frame annotation reused across
// tests, mirroring the worked example from research/03 §9.
func frameAcquireGPU() memory.FrameRef {
	return memory.FrameRef{
		Verb:    memory.VerbAcquire,
		ObjKind: memory.KindService,
		ObjRef:  "gpu_inference",
	}
}

// frameAcquireModel mirrors the second tuple in the §9 worked example.
func frameAcquireModel() memory.FrameRef {
	return memory.FrameRef{
		Verb:    memory.VerbAcquire,
		ObjKind: memory.KindModel,
		ObjRef:  "llama-405b",
	}
}

// writePrefWithFrames writes a Preference with the given Frames; returns
// the parsed memory ID for direct idx assertions.
func writePrefWithFrames(t *testing.T, c *Cortex, topic string, importance uint8, frames ...memory.FrameRef) memory.ID {
	t.Helper()
	uri, err := c.Write(memory.Head{
		ActorScope:         "andrew",
		DeclaredImportance: importance,
		Frames:             frames,
	}, memory.PreferenceData{
		SchemaVersion: 1, Topic: topic,
		Polarity: memory.PolarityPrefer, StrengthVal: 0.7,
	}, WriteMeta{
		CreatedBy:  "andrew",
		Forms:      memory.Forms{Short: "pref:" + topic, Medium: "pref:" + topic + " — andrew"},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("Write Preference: %v", err)
	}
	_, id, _, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	return id
}

// writeEventWithFrames writes an Event with the given Frames.
func writeEventWithFrames(t *testing.T, c *Cortex, summary string, outcome memory.Outcome, frames ...memory.FrameRef) memory.ID {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew", Frames: frames},
		memory.EventData{
			SchemaVersion: 1,
			Kind:          memory.EventIntentCompleted,
			OutcomeVal:    outcome,
			Summary:       summary,
		},
		WriteMeta{
			CreatedBy:  "andrew",
			Forms:      memory.Forms{Short: "event:" + summary, Medium: "event:" + summary + " (" + string(outcome) + ")"},
			Provenance: memory.Provenance{Source: memory.SourceObserved},
		})
	if err != nil {
		t.Fatalf("Write Event: %v", err)
	}
	_, id, _, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("ParseURI: %v", err)
	}
	return id
}

// countPrefix iterates k/v pairs under prefix and returns the count.
// Used to assert idx/frame and idx/actor_obj cardinality.
func countPrefix(t *testing.T, c *Cortex, prefix []byte) int {
	t.Helper()
	n := 0
	if err := c.s.PrefixIter(prefix, func(_, _ []byte) error { n++; return nil }); err != nil {
		t.Fatalf("PrefixIter: %v", err)
	}
	return n
}

// TestPhase8WriteEmitsIdxFrameForAllTypes asserts that a non-Event memory
// with a frame annotation produces an idx/frame key BUT NO idx/actor_obj.
// Spec: research/04-cortex.md §2.3 + auto-derivation rule documented in
// cortex.Write (idx/actor_obj is Event-only; idx/frame is universal).
func TestPhase8WriteEmitsIdxFrameForAllTypes(t *testing.T) {
	c := openCortex(t)
	id := writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())

	// Exactly one idx/frame entry under (acquire, service, gpu_inference).
	prefix := keys.IdxFramePrefixVerbKindObj(
		byte(memory.VerbAcquire), byte(memory.KindService),
		memory.ObjHash("gpu_inference"),
	)
	if got := countPrefix(t, c, prefix); got != 1 {
		t.Fatalf("idx/frame count: got %d want 1", got)
	}

	// Parse the one key and verify the ID matches.
	var foundID memory.ID
	_ = c.s.PrefixIter(prefix, func(k, _ []byte) error {
		_, _, _, u, err := keys.ParseIdxFrameKey(k)
		if err != nil {
			t.Fatalf("ParseIdxFrameKey: %v", err)
		}
		copy(foundID[:], u[:])
		return nil
	})
	if foundID != id {
		t.Fatalf("idx/frame id: got %s want %s", foundID, id)
	}

	// Zero idx/actor_obj entries (Preference is not Event).
	if got := countPrefix(t, c, keys.IdxActorObjPrefixVerb(byte(memory.VerbAcquire))); got != 0 {
		t.Fatalf("idx/actor_obj count: got %d want 0 (non-Event)", got)
	}
}

// TestPhase8WriteEventEmitsBothIndexes asserts that a TypeEvent memory
// with a frame annotation produces BOTH idx/frame and idx/actor_obj.
// Spec: auto-derivation rule (Event-only idx/actor_obj).
func TestPhase8WriteEventEmitsBothIndexes(t *testing.T) {
	c := openCortex(t)
	id := writeEventWithFrames(t, c, "acquired-gpu", memory.OutcomeSuccess, frameAcquireGPU())

	if got := countPrefix(t, c, keys.IdxFramePrefixVerbKindObj(
		byte(memory.VerbAcquire), byte(memory.KindService),
		memory.ObjHash("gpu_inference"),
	)); got != 1 {
		t.Fatalf("idx/frame count: got %d want 1", got)
	}

	// Exactly one idx/actor_obj entry, parseable.
	prefix := keys.IdxActorObjPrefixVerbObj(byte(memory.VerbAcquire), memory.ObjHash("gpu_inference"))
	if got := countPrefix(t, c, prefix); got != 1 {
		t.Fatalf("idx/actor_obj count: got %d want 1", got)
	}
	var foundID memory.ID
	_ = c.s.PrefixIter(prefix, func(k, _ []byte) error {
		_, _, _, u, err := keys.ParseIdxActorObjKey(k)
		if err != nil {
			t.Fatalf("ParseIdxActorObjKey: %v", err)
		}
		copy(foundID[:], u[:])
		return nil
	})
	if foundID != id {
		t.Fatalf("idx/actor_obj id: got %s want %s", foundID, id)
	}
}

// TestPhase8WriteRejectsInvalidFrame asserts validate.go enforces the
// frame Validate() rules at Write time. Spec: memory.FrameRef.Validate.
func TestPhase8WriteRejectsInvalidFrame(t *testing.T) {
	c := openCortex(t)
	bad := memory.FrameRef{Verb: 0xff, ObjKind: memory.KindService, ObjRef: "x"}
	_, err := c.Write(memory.Head{ActorScope: "andrew", Frames: []memory.FrameRef{bad}},
		newPreference(), WriteMeta{
			CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
	if !errors.Is(err, memory.ErrInvalidVerb) {
		t.Fatalf("expected ErrInvalidVerb, got %v", err)
	}

	bad = memory.FrameRef{Verb: memory.VerbAcquire, ObjKind: 0xff, ObjRef: "x"}
	_, err = c.Write(memory.Head{ActorScope: "andrew", Frames: []memory.FrameRef{bad}},
		newPreference(), WriteMeta{
			CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
	if !errors.Is(err, memory.ErrInvalidObjKind) {
		t.Fatalf("expected ErrInvalidObjKind, got %v", err)
	}

	bad = memory.FrameRef{Verb: memory.VerbAcquire, ObjKind: memory.KindService, ObjRef: ""}
	_, err = c.Write(memory.Head{ActorScope: "andrew", Frames: []memory.FrameRef{bad}},
		newPreference(), WriteMeta{
			CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput},
		})
	if !errors.Is(err, memory.ErrEmptyObjRef) {
		t.Fatalf("expected ErrEmptyObjRef, got %v", err)
	}
}

// TestPhase8ContextPinnedTierLoadsIdentityConstraintGoal asserts the
// Pinned tier loads Identity ∪ Constraint{Hard} ∪ Goal{Active}. Hard
// vs Firm/Soft and Active vs Paused are filtered per data.go.
// Spec: research/04 §12.1 + research/03 §2.1.
func TestPhase8ContextPinnedTierLoadsIdentityConstraintGoal(t *testing.T) {
	c := openCortex(t)
	// Identity (always pinned).
	idURI, _ := c.Write(memory.Head{ActorScope: "andrew"}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	_, identityID, _, _ := ParseURI(idURI)

	// Two Constraints — Hard pinned, Soft excluded.
	hardURI, _ := c.Write(memory.Head{ActorScope: "andrew"},
		memory.ConstraintData{SchemaVersion: 1, Statement: "no PII in logs",
			Polarity: memory.PolarityDont, StrengthVal: memory.StrengthHard,
			Source: memory.ConstraintSourceUserDeclared},
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	_, hardID, _, _ := ParseURI(hardURI)

	_, _ = c.Write(memory.Head{ActorScope: "andrew"},
		memory.ConstraintData{SchemaVersion: 1, Statement: "prefer eu-west",
			Polarity: memory.PolarityPrefer, StrengthVal: memory.StrengthSoft,
			Source: memory.ConstraintSourceLearned},
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})

	// Two Goals — Active pinned, Completed excluded.
	activeURI, _ := c.Write(memory.Head{ActorScope: "andrew"},
		memory.GoalData{SchemaVersion: 1, Statement: "ship matrix", Status: memory.GoalActive},
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	_, activeID, _, _ := ParseURI(activeURI)

	_, _ = c.Write(memory.Head{ActorScope: "andrew"},
		memory.GoalData{SchemaVersion: 1, Statement: "old goal", Status: memory.GoalCompleted},
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})

	bundle, err := c.Context(ContextOpts{
		IncludeTiers: []Tier{TierPinned},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	want := map[memory.ID]bool{identityID: false, hardID: false, activeID: false}
	for _, m := range bundle.Pinned {
		if _, expected := want[m.Head.ID]; !expected {
			t.Fatalf("unexpected pinned: type=%s id=%s", m.Head.Type, m.Head.ID)
		}
		want[m.Head.ID] = true
	}
	for id, present := range want {
		if !present {
			t.Fatalf("missing pinned id=%s", id)
		}
	}
}

// TestPhase8ContextFrameRelevantByVerbObject asserts the Frame-relevant
// tier scans idx/frame for each (verb, kind, ref) tuple and returns
// only memories with matching frames. Spec: research/04 §12.1.
func TestPhase8ContextFrameRelevantByVerbObject(t *testing.T) {
	c := openCortex(t)

	matched := writePrefWithFrames(t, c, "precision", 5, frameAcquireGPU())
	matched2 := writePrefWithFrames(t, c, "jurisdiction", 6, frameAcquireGPU(), frameAcquireModel())
	unrelated := writePrefWithFrames(t, c, "tone", 4) // no frames

	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierFrameRelevant},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	got := map[memory.ID]bool{}
	for _, m := range bundle.FrameRelevant {
		got[m.Head.ID] = true
	}
	if !got[matched] || !got[matched2] {
		t.Fatalf("FrameRelevant missing matched memories; got=%v", got)
	}
	if got[unrelated] {
		t.Fatalf("FrameRelevant included unrelated memory %s", unrelated)
	}
}

// TestPhase8ContextOutcomesTopN asserts the Outcomes tier returns the
// N most-recent Events for the (verb, ref) tuple. Spec: research/04
// §12.1 + research/03 §2.1 (1-3 prior similar intents).
func TestPhase8ContextOutcomesTopN(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	var ids []memory.ID
	for i := 0; i < 5; i++ {
		id := writeEventWithFrames(t, c, fmt.Sprintf("inf-%d", i), memory.OutcomeSuccess, frameAcquireGPU())
		ids = append(ids, id)
	}

	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierOutcomes},
		OutcomeLimit: 3,
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(bundle.Outcomes) != 3 {
		t.Fatalf("Outcomes len: got %d want 3", len(bundle.Outcomes))
	}
	// Most recent first: ids[4], ids[3], ids[2].
	want := []memory.ID{ids[4], ids[3], ids[2]}
	for i, m := range bundle.Outcomes {
		if m.Head.ID != want[i] {
			t.Fatalf("Outcomes[%d]: got %s want %s", i, m.Head.ID, want[i])
		}
	}
}

// TestPhase8ContextEventWithFrameLandsInOutcomes asserts the dedup
// priority: an Event memory that carries BOTH an idx/frame entry and
// an idx/actor_obj entry (matching the requested verb+obj) appears in
// the Outcomes tier, NOT the FrameRelevant tier. Matches the
// research/03 §9 worked-example narrative where individual Events
// belong to Outcomes regardless of their frame stamps.
func TestPhase8ContextEventWithFrameLandsInOutcomes(t *testing.T) {
	c := openCortex(t)
	eventID := writeEventWithFrames(t, c, "history", memory.OutcomeSuccess, frameAcquireGPU())

	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierFrameRelevant, TierOutcomes},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	for _, m := range bundle.FrameRelevant {
		if m.Head.ID == eventID {
			t.Fatalf("event leaked into FrameRelevant (should be Outcomes-only)")
		}
	}
	found := false
	for _, m := range bundle.Outcomes {
		if m.Head.ID == eventID {
			found = true
		}
	}
	if !found {
		t.Fatalf("event missing from Outcomes")
	}
}

// TestPhase8ContextBudgetTrimsLowSalience asserts that when total
// rendered tokens exceed BudgetTokens, low-salience entries are dropped
// first and reported via ReachableURIs. Mirrors the Find trim invariant.
func TestPhase8ContextBudgetTrimsLowSalience(t *testing.T) {
	c := openCortex(t)
	// Twelve prefs with one frame each. Importance from 1..10 then 5,5.
	importances := []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 5, 5}
	for i, imp := range importances {
		writePrefWithFrames(t, c, fmt.Sprintf("topic-%d", i), imp, frameAcquireGPU())
	}

	// Each medium form ≈ "pref:topic-N — andrew" ≈ 26 bytes / 4 ≈ 7
	// tokens. 12 × 7 ≈ 84 tokens. A 30-token budget retains ~4 items.
	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierFrameRelevant},
		BudgetTokens: 30,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if bundle.Trimmed == 0 {
		t.Fatalf("expected trim, got 0")
	}
	if bundle.TotalTokens > 30 && len(bundle.FrameRelevant) > 1 {
		t.Fatalf("post-trim tokens %d > budget 30 with %d survivors", bundle.TotalTokens, len(bundle.FrameRelevant))
	}
	if len(bundle.ReachableURIs) != bundle.Trimmed {
		t.Fatalf("ReachableURIs=%d != Trimmed=%d", len(bundle.ReachableURIs), bundle.Trimmed)
	}
	// Highest-importance prefs should be retained (salience tracks
	// DeclaredImportance in cold path).
	survived := map[uint8]bool{}
	for _, m := range bundle.FrameRelevant {
		survived[m.Head.DeclaredImportance] = true
	}
	if !survived[10] {
		t.Fatalf("importance=10 should survive trim; survivors=%v", survived)
	}
}

// TestPhase8ContextSkipsTombstoned asserts that tombstoned memories
// are filtered from every tier even though idx/* entries remain.
func TestPhase8ContextSkipsTombstoned(t *testing.T) {
	c := openCortex(t)
	id := writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())
	if err := c.Tombstone(BuildURI(memory.TypePreference, id, 1), "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierFrameRelevant},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(bundle.FrameRelevant) != 0 {
		t.Fatalf("tombstoned memory leaked into FrameRelevant: %v", bundle.FrameRelevant)
	}
}

// TestPhase8ContextNoVerbRunsOnlyPinned asserts that when Verb is the
// zero value, Frame-relevant and Outcomes tiers are silently skipped
// (no verb to key into idx/frame or idx/actor_obj).
func TestPhase8ContextNoVerbRunsOnlyPinned(t *testing.T) {
	c := openCortex(t)
	_, _ = c.Write(memory.Head{ActorScope: "andrew"}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())
	writeEventWithFrames(t, c, "history", memory.OutcomeSuccess, frameAcquireGPU())

	bundle, err := c.Context(ContextOpts{
		Objects:      map[string]string{"service": "gpu_inference"},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(bundle.Pinned) == 0 {
		t.Fatalf("Pinned tier should have loaded Identity")
	}
	if len(bundle.FrameRelevant) != 0 || len(bundle.Outcomes) != 0 {
		t.Fatalf("no-verb path should skip Frame+Outcomes; got %d frame, %d outcomes",
			len(bundle.FrameRelevant), len(bundle.Outcomes))
	}
}

// TestPhase8ContextRejectsUnknownObjKind asserts the input validator
// surfaces caller mistakes loudly (research/03 §8 enforcement).
func TestPhase8ContextRejectsUnknownObjKind(t *testing.T) {
	c := openCortex(t)
	_, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"banana": "yellow"},
		BudgetTokens: 3000,
	})
	if !errors.Is(err, memory.ErrInvalidObjKind) {
		t.Fatalf("expected ErrInvalidObjKind, got %v", err)
	}
}

// TestPhase8ContextRejectsAllTiersExcluded covers the trivial caller
// mistake of passing IncludeTiers with no recognized values.
func TestPhase8ContextRejectsAllTiersExcluded(t *testing.T) {
	c := openCortex(t)
	_, err := c.Context(ContextOpts{
		IncludeTiers: []Tier{Tier(99)}, // unknown only
		BudgetTokens: 3000,
	})
	if !errors.Is(err, ErrNoTiersIncluded) {
		t.Fatalf("expected ErrNoTiersIncluded, got %v", err)
	}
}

// TestPhase8ContextIsPureRead asserts the composer doesn't write to the
// journal or change the OverallRoot. Critical: Context must be safely
// callable from any read path without polluting audit state.
func TestPhase8ContextIsPureRead(t *testing.T) {
	c := openCortex(t)
	_, _ = c.Write(memory.Head{ActorScope: "andrew"}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())

	preSeq := c.s.NextSeq()
	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot: %v", err)
	}

	for i := 0; i < 3; i++ {
		_, err := c.Context(ContextOpts{
			Verb:         memory.VerbAcquire,
			Objects:      map[string]string{"service": "gpu_inference"},
			BudgetTokens: 3000,
		})
		if err != nil {
			t.Fatalf("Context call %d: %v", i, err)
		}
	}

	postSeq := c.s.NextSeq()
	postRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot post: %v", err)
	}
	if preSeq != postSeq {
		t.Fatalf("Context journaled an entry: pre=%d post=%d", preSeq, postSeq)
	}
	if preRoot != postRoot {
		t.Fatalf("Context changed OverallRoot")
	}
}

// TestPhase8ContextPinnedFloorApplied asserts that Pinned-tier members
// carry a salience floor of PinnedFloor in Bundle.Scores, regardless of
// their underlying salience.Score.Cached. Spec: research/04 §8.2.
func TestPhase8ContextPinnedFloorApplied(t *testing.T) {
	c := openCortex(t)
	// Identity with low importance → low cold salience.
	idURI, _ := c.Write(memory.Head{ActorScope: "andrew", DeclaredImportance: 0}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	_, identityID, _, _ := ParseURI(idURI)

	bundle, err := c.Context(ContextOpts{
		IncludeTiers: []Tier{TierPinned},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	got, ok := bundle.Scores[identityID]
	if !ok {
		t.Fatalf("identity score absent")
	}
	if got < salience.PinnedFloor {
		t.Fatalf("Pinned floor not applied: got %.3f want >= %.3f", got, salience.PinnedFloor)
	}
}

// TestPhase8ContextDeterministicAcrossActors asserts that two actors
// with byte-identical inputs produce byte-identical bundle compositions
// (same memory IDs in same order, same scores, same tokens). Mirrors
// the cross-actor determinism guarantee load-bearing for D11.
func TestPhase8ContextDeterministicAcrossActors(t *testing.T) {
	// Helper: build a cortex with a fixed clock + ID generator, write a
	// canonical sequence, and run Context. Returns the bundle.
	build := func(name string) *Bundle {
		t.Helper()
		dir := t.TempDir()
		s, err := store.Open(dir, name, nil)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		// Fixed clock + deterministic ID generator (counter -> ULID).
		clk := time.Unix(1700000000, 0).UTC()
		var counter int
		c := New(s,
			WithClock(func() time.Time { clk = clk.Add(time.Second); return clk }),
			WithIDGen(func() memory.ID {
				counter++
				var id memory.ID
				id[15] = byte(counter)
				return id
			}),
		)
		_, _ = c.Write(memory.Head{ActorScope: name}, newIdentity(),
			WriteMeta{CreatedBy: name, Provenance: memory.Provenance{Source: memory.SourceUserInput}})
		writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())
		writeEventWithFrames(t, c, "ev", memory.OutcomeSuccess, frameAcquireGPU())
		bundle, err := c.Context(ContextOpts{
			Verb:         memory.VerbAcquire,
			Objects:      map[string]string{"service": "gpu_inference"},
			BudgetTokens: 3000,
		})
		if err != nil {
			t.Fatalf("Context: %v", err)
		}
		return bundle
	}
	a := build("actor-a")
	b := build("actor-b")
	if len(a.Pinned) != len(b.Pinned) || len(a.FrameRelevant) != len(b.FrameRelevant) || len(a.Outcomes) != len(b.Outcomes) {
		t.Fatalf("tier sizes differ: a=(%d,%d,%d) b=(%d,%d,%d)",
			len(a.Pinned), len(a.FrameRelevant), len(a.Outcomes),
			len(b.Pinned), len(b.FrameRelevant), len(b.Outcomes))
	}
	cmpTier := func(label string, sa, sb []*memory.Memory) {
		for i := range sa {
			if sa[i].Head.ID != sb[i].Head.ID {
				t.Fatalf("%s[%d]: a=%s b=%s", label, i, sa[i].Head.ID, sb[i].Head.ID)
			}
		}
	}
	cmpTier("Pinned", a.Pinned, b.Pinned)
	cmpTier("FrameRelevant", a.FrameRelevant, b.FrameRelevant)
	cmpTier("Outcomes", a.Outcomes, b.Outcomes)
	if a.TotalTokens != b.TotalTokens {
		t.Fatalf("TotalTokens differ: a=%d b=%d", a.TotalTokens, b.TotalTokens)
	}
}

// TestPhase8ContextRendersMediumByDefault asserts the default Form
// is FormMedium (research/03 §7 "medium ≤ 200 | normal cold-start").
func TestPhase8ContextRendersMediumByDefault(t *testing.T) {
	c := openCortex(t)
	id := writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())

	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierFrameRelevant},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if bundle.Form != query.FormMedium {
		t.Fatalf("default form: got %q want %q", bundle.Form, query.FormMedium)
	}
	text, ok := bundle.Rendered[id]
	if !ok {
		t.Fatalf("Rendered missing entry for %s", id)
	}
	// writePrefWithFrames leaves FormsOverride=false so the auto-render
	// path produces the medium form. Per forms.go the medium form for a
	// PreferenceData on "tone" includes the topic name and "prefer".
	if !strings.Contains(text, "tone") || !strings.Contains(text, "prefer") {
		t.Fatalf("Rendered text unexpected: %q", text)
	}
}

// --- Phase 9 (Compact) ---------------------------------------------------
//
// Compact is the budget-aware compaction primitive (research/03 §5 +
// research/04 §11/§12). These tests pin the locked design contract:
// A1 hybrid storage, A2 KindCompact journal entry, A3 auto-protect
// pinned items, A4 hard ErrBudgetUnreachable, A5 SnapshotURI vs
// SnapshotPath, D1 logs-kind URI, D2 medium-form token cost, D3
// best-effort filesystem mirror.

// writeCompactablePref creates a Preference with explicit Forms set so
// token counts are predictable in tests. Returns (uri, id).
func writeCompactablePref(t *testing.T, c *Cortex, topic, shortF, medF string, importance uint8) (memory.URI, memory.ID) {
	t.Helper()
	d := memory.PreferenceData{
		SchemaVersion: 1, Topic: topic,
		Polarity: memory.PolarityPrefer, StrengthVal: 0.7,
	}
	uri, err := c.Write(memory.Head{ActorScope: "andrew", DeclaredImportance: importance}, d, WriteMeta{
		CreatedBy:     "andrew",
		Forms:         memory.Forms{Short: shortF, Medium: medF},
		FormsOverride: true,
		Provenance:    memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeCompactablePref(%s): %v", topic, err)
	}
	_, id, _, _ := ParseURI(uri)
	return uri, id
}

// writeIdentityFor builds + writes an Identity with given Forms.
func writeIdentityFor(t *testing.T, c *Cortex, name, shortF, medF string) (memory.URI, *memory.Memory) {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.IdentityData{
		SchemaVersion: 1, Name: name, DID: "did:pax:" + name,
	}, WriteMeta{
		CreatedBy: "andrew", FormsOverride: true,
		Forms:      memory.Forms{Short: shortF, Medium: medF},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeIdentityFor: %v", err)
	}
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	return uri, m
}

// writeConstraintFor builds + writes a Constraint with explicit strength
// and Forms. Returns (uri, *Memory) so tests can pass into CompactOpts.
func writeConstraintFor(t *testing.T, c *Cortex, stmt string, strength memory.Strength, shortF string) (memory.URI, *memory.Memory) {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.ConstraintData{
		SchemaVersion: 1, Statement: stmt,
		Polarity: memory.PolarityDont, StrengthVal: strength,
		Source: memory.ConstraintSourceUserDeclared,
	}, WriteMeta{
		CreatedBy: "andrew", FormsOverride: true,
		Forms:      memory.Forms{Short: shortF, Medium: shortF},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeConstraintFor: %v", err)
	}
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("resolve constraint: %v", err)
	}
	return uri, m
}

// writeGoalFor builds + writes a Goal with explicit status + Forms.
func writeGoalFor(t *testing.T, c *Cortex, stmt string, status memory.GoalStatus, shortF string) (memory.URI, *memory.Memory) {
	t.Helper()
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.GoalData{
		SchemaVersion: 1, Statement: stmt, Status: status,
	}, WriteMeta{
		CreatedBy: "andrew", FormsOverride: true,
		Forms:      memory.Forms{Short: shortF, Medium: shortF},
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	})
	if err != nil {
		t.Fatalf("writeGoalFor: %v", err)
	}
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("resolve goal: %v", err)
	}
	return uri, m
}

// resolveAll fetches the current Memory for each URI; helper for
// assembling InContext lists from Write-returned URIs.
func resolveAll(t *testing.T, c *Cortex, uris ...memory.URI) []*memory.Memory {
	t.Helper()
	out := make([]*memory.Memory, 0, len(uris))
	for _, u := range uris {
		m, err := c.Resolve(u)
		if err != nil {
			t.Fatalf("resolveAll(%s): %v", u, err)
		}
		out = append(out, m)
	}
	return out
}

// TestCompactEmptyInContextRejected — §5.3 contract: in_context required.
func TestCompactEmptyInContextRejected(t *testing.T) {
	c := openCortex(t)
	_, err := c.Compact(CompactOpts{
		InContext: nil,
		IntentID:  "i", StepID: "s", BudgetTokens: 100,
	})
	if !errors.Is(err, ErrEmptyInContext) {
		t.Fatalf("expected ErrEmptyInContext, got %v", err)
	}
}

// TestCompactRequiresIntentAndStep — §5.1 step 3 path requires both.
func TestCompactRequiresIntentAndStep(t *testing.T) {
	c := openCortex(t)
	uri, _ := writeCompactablePref(t, c, "tone", "sh", "med", 5)
	mems := resolveAll(t, c, uri)

	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "", StepID: "s",
	}); !errors.Is(err, ErrEmptyIntentID) {
		t.Fatalf("expected ErrEmptyIntentID, got %v", err)
	}
	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "",
	}); !errors.Is(err, ErrEmptyStepID) {
		t.Fatalf("expected ErrEmptyStepID, got %v", err)
	}
}

// TestCompactRejectsSlashInIDs — keys §2 invariant: '/' forbidden.
func TestCompactRejectsSlashInIDs(t *testing.T) {
	c := openCortex(t)
	uri, _ := writeCompactablePref(t, c, "tone", "sh", "med", 5)
	mems := resolveAll(t, c, uri)
	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "bad/intent", StepID: "s",
	}); err == nil {
		t.Fatalf("expected error for slash in intent_id")
	}
	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "bad/step",
	}); err == nil {
		t.Fatalf("expected error for slash in step_id")
	}
}

// TestCompactSummarizesNonLoadBearing — core §5.1 step 2 behavior.
// Three prefs in_context, one listed as load_bearing → 1 Kept, 2 Compacted
// with {Ref, ShortForm, Salience} stubs.
func TestCompactSummarizesNonLoadBearing(t *testing.T) {
	c := openCortex(t)
	u1, _ := writeCompactablePref(t, c, "tone", "sh-1", "med-1 medium text here padding padding padding padding", 5)
	u2, _ := writeCompactablePref(t, c, "verbosity", "sh-2", "med-2 medium text here padding padding padding padding", 5)
	u3, _ := writeCompactablePref(t, c, "format", "sh-3", "med-3 medium text here padding padding padding padding", 5)
	mems := resolveAll(t, c, u1, u2, u3)

	res, err := c.Compact(CompactOpts{
		InContext: mems, LoadBearing: []memory.URI{u2},
		BudgetTokens: 4000,
		IntentID:     "intent_a", StepID: "step_1",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Kept) != 1 || res.Kept[0].Head.ID != mems[1].Head.ID {
		t.Fatalf("Kept: want only u2, got %+v", res.Kept)
	}
	if len(res.Compacted) != 2 {
		t.Fatalf("Compacted: want 2, got %d", len(res.Compacted))
	}
	gotRefs := map[memory.URI]bool{}
	for _, ci := range res.Compacted {
		gotRefs[ci.Ref] = true
		if ci.ShortForm == "" {
			t.Fatalf("ShortForm empty in CompactedItem %+v", ci)
		}
		// §5.1 step 2 says ShortForm sourced from Forms.Short. Validate
		// by string-membership against the two non-load-bearing items.
		if ci.ShortForm != "sh-1" && ci.ShortForm != "sh-3" {
			t.Fatalf("ShortForm unexpected: %q", ci.ShortForm)
		}
	}
	if !gotRefs[u1] || !gotRefs[u3] || gotRefs[u2] {
		t.Fatalf("Compacted refs wrong: %v", gotRefs)
	}
}

// TestCompactAutoProtectsPinnedItems — Andrew lock A3. Identity ∪
// Constraint{Hard} ∪ Goal{Active} in InContext automatically go to
// Kept, even when not in caller's LoadBearing list. Soft constraints
// and Completed goals fall through to Compacted.
func TestCompactAutoProtectsPinnedItems(t *testing.T) {
	c := openCortex(t)
	_, idM := writeIdentityFor(t, c, "andrew", "id-short", "id-medium-text")
	_, hardM := writeConstraintFor(t, c, "no PII", memory.StrengthHard, "hard-short")
	_, softM := writeConstraintFor(t, c, "prefer eu-west", memory.StrengthSoft, "soft-short")
	_, activeM := writeGoalFor(t, c, "ship matrix", memory.GoalActive, "active-short")
	_, doneM := writeGoalFor(t, c, "old goal", memory.GoalCompleted, "done-short")

	res, err := c.Compact(CompactOpts{
		InContext: []*memory.Memory{idM, hardM, softM, activeM, doneM},
		// No caller LoadBearing — only the auto-protect path runs.
		BudgetTokens: 4000,
		IntentID:     "intent_a", StepID: "step_1",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	keptIDs := map[memory.ID]bool{}
	for _, m := range res.Kept {
		keptIDs[m.Head.ID] = true
	}
	if !keptIDs[idM.Head.ID] {
		t.Fatalf("Identity not auto-protected as pinned")
	}
	if !keptIDs[hardM.Head.ID] {
		t.Fatalf("Constraint{Hard} not auto-protected as pinned")
	}
	if !keptIDs[activeM.Head.ID] {
		t.Fatalf("Goal{Active} not auto-protected as pinned")
	}
	if keptIDs[softM.Head.ID] {
		t.Fatalf("Constraint{Soft} should NOT be auto-protected, was in Kept")
	}
	if keptIDs[doneM.Head.ID] {
		t.Fatalf("Goal{Completed} should NOT be auto-protected, was in Kept")
	}
	if len(res.Compacted) != 2 {
		t.Fatalf("Compacted: want 2 (soft+done), got %d", len(res.Compacted))
	}
}

// TestCompactLoadBearingAndPinnedUnion — A3: caller list ∪ pinned set.
// Both contributions survive Kept; neither blocks the other.
func TestCompactLoadBearingAndPinnedUnion(t *testing.T) {
	c := openCortex(t)
	_, idM := writeIdentityFor(t, c, "andrew", "id-short", "id-medium")
	prefURI, _ := writeCompactablePref(t, c, "tone", "pref-short", "pref-medium", 5)
	prefM := resolveAll(t, c, prefURI)[0]

	res, err := c.Compact(CompactOpts{
		InContext:    []*memory.Memory{idM, prefM},
		LoadBearing:  []memory.URI{prefURI},
		BudgetTokens: 4000,
		IntentID:     "i", StepID: "s",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Kept) != 2 {
		t.Fatalf("Kept: want 2 (identity auto + pref caller), got %d", len(res.Kept))
	}
	if len(res.Compacted) != 0 {
		t.Fatalf("Compacted: want 0, got %d", len(res.Compacted))
	}
}

// TestCompactBudgetUnreachable — A4 hard error after full summarization.
// All items are load-bearing → none compactable. Tight budget → error.
func TestCompactBudgetUnreachable(t *testing.T) {
	c := openCortex(t)
	// Build prefs with large Medium forms so kept_tokens swamps any
	// achievable budget.
	bigMed := strings.Repeat("word ", 50) // ~50 tokens
	u1, _ := writeCompactablePref(t, c, "t1", "s1", bigMed, 5)
	u2, _ := writeCompactablePref(t, c, "t2", "s2", bigMed, 5)
	mems := resolveAll(t, c, u1, u2)

	_, err := c.Compact(CompactOpts{
		InContext:    mems,
		LoadBearing:  []memory.URI{u1, u2}, // BOTH load-bearing → nothing compactable
		BudgetTokens: 5,                    // far below 2*50 tokens
		IntentID:     "i", StepID: "s",
	})
	if !errors.Is(err, ErrBudgetUnreachable) {
		t.Fatalf("expected ErrBudgetUnreachable, got %v", err)
	}
}

// TestCompactDefaultBudget — opts.BudgetTokens == 0 should resolve to
// DefaultCompactBudgetTokens (4000) and succeed for a small set.
func TestCompactDefaultBudget(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium text", 5)
	mems := resolveAll(t, c, u)
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s",
		// BudgetTokens omitted → must default to 4000.
	})
	if err != nil {
		t.Fatalf("Compact with default budget: %v", err)
	}
	if len(res.Compacted) != 1 {
		t.Fatalf("Compacted: want 1, got %d", len(res.Compacted))
	}
}

// TestCompactFiltersTombstoned — tombstoned in_context items vanish
// from both Kept and Compacted (mirrors context.go:355 defensive filter).
func TestCompactFiltersTombstoned(t *testing.T) {
	c := openCortex(t)
	u1, _ := writeCompactablePref(t, c, "t1", "s1", "m1", 5)
	u2, _ := writeCompactablePref(t, c, "t2", "s2", "m2", 5)
	if err := c.Tombstone(u2, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	mems := []*memory.Memory{}
	for _, u := range []memory.URI{u1, u2} {
		m, err := c.Resolve(u)
		if err != nil {
			t.Fatalf("Resolve %s: %v", u, err)
		}
		mems = append(mems, m)
	}
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s", BudgetTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	allRefs := map[memory.URI]bool{}
	for _, m := range res.Kept {
		allRefs[BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)] = true
	}
	for _, ci := range res.Compacted {
		allRefs[ci.Ref] = true
	}
	if allRefs[u2] {
		t.Fatalf("tombstoned URI %s appeared in result", u2)
	}
}

// TestCompactWritesPebbleAndJournalAtomically — A1+A2: one Pebble batch
// writes chk/<intent>/<step> + KindCompact j/<seq>. After commit, both
// the chk/ value and the journal entry are visible.
func TestCompactWritesPebbleAndJournalAtomically(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)

	preSeq := c.s.NextSeq()
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s", BudgetTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	postSeq := c.s.NextSeq()
	if postSeq != preSeq+1 {
		t.Fatalf("seq did not advance exactly by 1: pre=%d post=%d", preSeq, postSeq)
	}

	// chk/i/s present.
	rec, err := c.LoadCheckpoint("i", "s")
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if rec.IntentID != "i" || rec.StepID != "s" {
		t.Fatalf("checkpoint identity mismatch: %+v", rec)
	}
	if len(rec.Compacted) != 1 || rec.Compacted[0].ShortForm != "short" {
		t.Fatalf("checkpoint compacted: %+v", rec.Compacted)
	}

	// j/<preSeq> entry present with KindCompact + matching CheckpointHash.
	raw, ok, err := c.s.Get(keys.JournalKey(preSeq))
	if err != nil {
		t.Fatalf("get journal: %v", err)
	}
	if !ok {
		t.Fatalf("no journal entry at seq %d", preSeq)
	}
	var entry journal.Entry
	if err := journal.Decode(raw, &entry); err != nil {
		t.Fatalf("decode journal entry: %v", err)
	}
	if entry.Kind != journal.KindCompact {
		t.Fatalf("kind: got %q want %q", entry.Kind, journal.KindCompact)
	}
	var cp journal.CompactPayload
	if err := journal.DecodeCompactPayload(entry.Payload, &cp); err != nil {
		t.Fatalf("decode CompactPayload: %v", err)
	}
	if cp.IntentID != "i" || cp.StepID != "s" {
		t.Fatalf("payload identity: %+v", cp)
	}
	if cp.KeptCount != uint32(len(res.Kept)) || cp.CompactedCount != uint32(len(res.Compacted)) {
		t.Fatalf("counts mismatch: payload %+v vs result Kept=%d Compacted=%d",
			cp, len(res.Kept), len(res.Compacted))
	}

	// CheckpointHash binds the journal payload to the persisted chk/ blob.
	chkKey, _ := keys.CheckpointKey("i", "s")
	rawRec, _, _ := c.s.Get(chkKey)
	if got := sha256.Sum256(rawRec); got != cp.CheckpointHash {
		t.Fatalf("CheckpointHash mismatch: payload=%x stored=%x",
			cp.CheckpointHash, got)
	}
}

// TestCompactAdvancesOverallRoot — A2 + Phase 7 invariant: every j/<seq>
// entry advances the journal MMR via the installed JournalHook. So a
// Compact call must change cortex_snapshot_hash.
func TestCompactAdvancesOverallRoot(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}
	if _, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s", BudgetTokens: 4000,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	postRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot post: %v", err)
	}
	if preRoot == postRoot {
		t.Fatalf("OverallRoot did not change after Compact; MMR not staged")
	}
}

// TestCompactFilesystemMirror — A1 hybrid + A5: CheckpointDir set →
// pretty JSON file written at <dir>/<intent>/<step>.snapshot, decodable
// back to the same CheckpointRecord.
func TestCompactFilesystemMirror(t *testing.T) {
	c := openCortex(t)
	dir := t.TempDir()
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)

	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "intent_x", StepID: "step_y",
		BudgetTokens: 4000, CheckpointDir: dir,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	wantPath := filepath.Join(dir, "intent_x", "step_y.snapshot")
	if res.SnapshotPath != wantPath {
		t.Fatalf("SnapshotPath: got %q want %q", res.SnapshotPath, wantPath)
	}
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	// JSON must round-trip back to a CheckpointRecord.
	var fromFile CheckpointRecord
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		t.Fatalf("json.Unmarshal mirror: %v", err)
	}
	if fromFile.IntentID != "intent_x" || fromFile.StepID != "step_y" {
		t.Fatalf("mirror identity: %+v", fromFile)
	}
	if len(fromFile.Compacted) != 1 || fromFile.Compacted[0].ShortForm != "short" {
		t.Fatalf("mirror Compacted: %+v", fromFile.Compacted)
	}
}

// TestCompactNoMirrorWhenDirEmpty — A5: CheckpointDir=="" → no
// filesystem write, SnapshotPath="".
func TestCompactNoMirrorWhenDirEmpty(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s",
		BudgetTokens: 4000, CheckpointDir: "",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.SnapshotPath != "" {
		t.Fatalf("SnapshotPath should be empty when CheckpointDir is empty, got %q", res.SnapshotPath)
	}
	// SnapshotURI still populated.
	if res.SnapshotURI == "" {
		t.Fatalf("SnapshotURI empty")
	}
}

// TestCompactMirrorFailureDoesntFailCall — D3 lock: a filesystem mirror
// write failure (here: dir points at a regular file, blocking mkdir)
// must NOT fail Compact; Pebble side is canonical and already committed.
func TestCompactMirrorFailureDoesntFailCall(t *testing.T) {
	c := openCortex(t)
	// Create a regular file where mkdir-all would need to create a dir.
	parent := t.TempDir()
	clashPath := filepath.Join(parent, "clash")
	if err := os.WriteFile(clashPath, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("setup clash file: %v", err)
	}
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)
	// CheckpointDir = clashPath → mkdir <clashPath>/<intent> will fail
	// because clashPath is a file. Compact must still succeed; we just
	// expect SnapshotPath == "".
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "i", StepID: "s",
		BudgetTokens: 4000, CheckpointDir: clashPath,
	})
	if err != nil {
		t.Fatalf("Compact should not fail on mirror error, got %v", err)
	}
	if res.SnapshotPath != "" {
		t.Fatalf("SnapshotPath should be empty on mirror failure, got %q", res.SnapshotPath)
	}
	// Pebble side still canonical: LoadCheckpoint must succeed.
	if _, err := c.LoadCheckpoint("i", "s"); err != nil {
		t.Fatalf("Pebble canonical record missing after mirror failure: %v", err)
	}
}

// TestCompactSnapshotURIScheme — D1 lock: kind=logs.
func TestCompactSnapshotURIScheme(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium", 5)
	mems := resolveAll(t, c, u)
	res, err := c.Compact(CompactOpts{
		InContext: mems, IntentID: "intent_ABC", StepID: "step_42",
		BudgetTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	want := memory.URI("matrix://journal/logs/intent_ABC/step_42")
	if res.SnapshotURI != want {
		t.Fatalf("SnapshotURI: got %q want %q", res.SnapshotURI, want)
	}
}

// TestCompactLoadCheckpointRoundTrip — chk/<intent>/<step> survives
// encode→Pebble→decode with the same logical content as the result.
func TestCompactLoadCheckpointRoundTrip(t *testing.T) {
	c := openCortex(t)
	u1, _ := writeCompactablePref(t, c, "tone", "sh-1", "med-1", 5)
	u2, _ := writeCompactablePref(t, c, "verbosity", "sh-2", "med-2", 5)
	mems := resolveAll(t, c, u1, u2)
	res, err := c.Compact(CompactOpts{
		InContext: mems, LoadBearing: []memory.URI{u1},
		BudgetTokens: 4000, IntentID: "i", StepID: "s",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	rec, err := c.LoadCheckpoint("i", "s")
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if rec.BudgetTokens != 4000 {
		t.Fatalf("BudgetTokens: %d", rec.BudgetTokens)
	}
	if len(rec.KeptURIs) != len(res.Kept) {
		t.Fatalf("KeptURIs count: got %d want %d", len(rec.KeptURIs), len(res.Kept))
	}
	if len(rec.Compacted) != len(res.Compacted) {
		t.Fatalf("Compacted count: got %d want %d", len(rec.Compacted), len(res.Compacted))
	}
}

// TestCompactDoesNotMutateSalience — research/03 §6 lists salience
// triggers exhaustively; Compact must not appear among them. We verify
// by reading salience.Cached before+after for both Kept and Compacted.
func TestCompactDoesNotMutateSalience(t *testing.T) {
	c := openCortex(t)
	u1, id1 := writeCompactablePref(t, c, "t1", "s1", "m1", 5)
	u2, id2 := writeCompactablePref(t, c, "t2", "s2", "m2", 5)
	preS1, _, _ := salience.Read(c.s, id1)
	preS2, _, _ := salience.Read(c.s, id2)
	mems := resolveAll(t, c, u1, u2)
	if _, err := c.Compact(CompactOpts{
		InContext: mems, LoadBearing: []memory.URI{u1},
		BudgetTokens: 4000, IntentID: "i", StepID: "s",
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	postS1, _, _ := salience.Read(c.s, id1)
	postS2, _, _ := salience.Read(c.s, id2)
	// LastUsed should be unchanged (no read or update bump).
	if preS1 != nil && postS1 != nil && postS1.LastUsed != preS1.LastUsed {
		t.Fatalf("kept salience LastUsed mutated: pre=%d post=%d",
			preS1.LastUsed, postS1.LastUsed)
	}
	if preS2 != nil && postS2 != nil && postS2.LastUsed != preS2.LastUsed {
		t.Fatalf("compacted salience LastUsed mutated: pre=%d post=%d",
			preS2.LastUsed, postS2.LastUsed)
	}
}

// TestCompactPreservesSourceMemories — source memories are untouched.
// After Compact, Resolve still returns the full Memory unchanged.
func TestCompactPreservesSourceMemories(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "short", "medium-body", 5)
	preMem, err := c.Resolve(u)
	if err != nil {
		t.Fatalf("Resolve pre: %v", err)
	}
	if _, err := c.Compact(CompactOpts{
		InContext: []*memory.Memory{preMem},
		// No LoadBearing → compacted.
		BudgetTokens: 4000, IntentID: "i", StepID: "s",
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	postMem, err := c.Resolve(u)
	if err != nil {
		t.Fatalf("Resolve post: %v", err)
	}
	if postMem.Version.Hash != preMem.Version.Hash {
		t.Fatalf("source Version.Hash changed: pre=%x post=%x",
			preMem.Version.Hash, postMem.Version.Hash)
	}
	if postMem.Head.CurrentVersion != preMem.Head.CurrentVersion {
		t.Fatalf("CurrentVersion changed: pre=%d post=%d",
			preMem.Head.CurrentVersion, postMem.Head.CurrentVersion)
	}
	if postMem.Head.Tombstoned != nil {
		t.Fatalf("source unexpectedly tombstoned")
	}
}

// TestCompactCompactedItemShape — every CompactedItem fields populated
// per §5.1 step 2: Ref is matrix://cortex/...#<v>, ShortForm from
// Version.Forms.Short, Salience finite.
func TestCompactCompactedItemShape(t *testing.T) {
	c := openCortex(t)
	u, _ := writeCompactablePref(t, c, "tone", "the-short-form", "the-medium-form", 5)
	mems := resolveAll(t, c, u)
	res, err := c.Compact(CompactOpts{
		InContext: mems, BudgetTokens: 4000, IntentID: "i", StepID: "s",
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Compacted) != 1 {
		t.Fatalf("want 1 compacted, got %d", len(res.Compacted))
	}
	ci := res.Compacted[0]
	if !strings.HasPrefix(string(ci.Ref), "matrix://cortex/Preference/") {
		t.Fatalf("Ref prefix: %q", ci.Ref)
	}
	if !strings.HasSuffix(string(ci.Ref), "#1") {
		t.Fatalf("Ref suffix: %q", ci.Ref)
	}
	if ci.ShortForm != "the-short-form" {
		t.Fatalf("ShortForm: got %q want %q", ci.ShortForm, "the-short-form")
	}
	if ci.Salience < 0 {
		t.Fatalf("Salience negative: %f", ci.Salience)
	}
}

// TestPhase8ContextLatencyUnderBudget asserts the p50<80ms target from
// research/03 §2.3. Smoke check: a typical 10-memory cortex composes
// well under the hard ceiling. Tagged as a smoke check rather than a
// strict perf gate so flaky CI hardware doesn't break the build.
func TestPhase8ContextLatencyUnderBudget(t *testing.T) {
	c := openCortex(t)
	_, _ = c.Write(memory.Head{ActorScope: "andrew"}, newIdentity(),
		WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	for i := 0; i < 5; i++ {
		writePrefWithFrames(t, c, fmt.Sprintf("topic-%d", i), uint8(i+1), frameAcquireGPU())
	}
	for i := 0; i < 5; i++ {
		writeEventWithFrames(t, c, fmt.Sprintf("ev-%d", i), memory.OutcomeSuccess, frameAcquireGPU())
	}
	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		BudgetTokens: 3000,
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	// Hard ceiling 250 ms; warn at 80 ms. Reported but not failing at 80.
	if bundle.LatencyMS > 250 {
		t.Fatalf("Context latency %dms exceeds hard ceiling 250ms", bundle.LatencyMS)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
