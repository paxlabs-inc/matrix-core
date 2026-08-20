// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"centra/core/cortex/keys"
	"centra/packages/vault"
)

func lexicalSnapshot(t *testing.T, c *Cortex) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := c.s.PrefixIter(keys.PrefixLexical, func(k, v []byte) error {
		out[string(k)] = append([]byte(nil), v...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLexicalIndexIncrementalQueryRebuildAndRootIdentity(t *testing.T) {
	c := openCortex(t)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, msg := range []Message{
		{ConversationID: "alpha", Role: RoleUser, Content: "quasar-needle allocator failure", TS: base.UnixNano()},
		{ConversationID: "alpha", Role: RoleAssistant, Content: "fixed the allocator by aligning the arena", TS: base.Add(time.Minute).UnixNano()},
		{ConversationID: "beta", Role: RoleUser, Content: "ordinary unrelated message", TS: base.Add(2 * time.Minute).UnixNano()},
	} {
		if _, err := c.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := c.QueryLexical("quasar needle", time.Time{}, time.Time{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ConversationID != "alpha" || hits[0].Seq != 0 {
		t.Fatalf("lexical hits = %+v", hits)
	}
	if filtered, _ := c.QueryLexical("quasar", base.Add(time.Hour), base.Add(2*time.Hour), 5); len(filtered) != 0 {
		t.Fatalf("time filter returned %+v", filtered)
	}

	rootWith, err := c.OverallRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, anchoredWith, _, err := c.snap.CurrentRoots()
	if err != nil {
		t.Fatal(err)
	}
	incremental := lexicalSnapshot(t, c)
	if err := c.s.ReplaceDerivedPrefix(keys.PrefixLexical, nil); err != nil {
		t.Fatal(err)
	}
	rootWithout, _ := c.OverallRoot()
	if rootWith != rootWithout {
		t.Fatalf("OverallRoot changed with index absent: %x != %x", rootWith, rootWithout)
	}
	_, anchoredWithout, _, err := c.snap.CurrentRoots()
	if err != nil {
		t.Fatal(err)
	}
	if len(anchoredWith) != len(anchoredWithout) {
		t.Fatalf("anchored root count changed with index absent: %d != %d", len(anchoredWith), len(anchoredWithout))
	}
	for namespace, want := range anchoredWith {
		if got := anchoredWithout[namespace]; got != want {
			t.Fatalf("anchored root %q changed with index absent: %x != %x", namespace, got, want)
		}
	}
	if err := c.RebuildLexicalIndex(); err != nil {
		t.Fatal(err)
	}
	rebuilt := lexicalSnapshot(t, c)
	if len(incremental) != len(rebuilt) {
		t.Fatalf("row count incremental=%d rebuilt=%d", len(incremental), len(rebuilt))
	}
	for key, want := range incremental {
		if !bytes.Equal(rebuilt[key], want) {
			t.Fatalf("rebuilt row differs at %q", key)
		}
	}
	rootRebuilt, _ := c.OverallRoot()
	if rootWith != rootRebuilt {
		t.Fatalf("OverallRoot changed after rebuild: %x != %x", rootWith, rootRebuilt)
	}
	if err := c.RebuildLexicalIndex(); err != nil {
		t.Fatal(err)
	}
	if again := lexicalSnapshot(t, c); len(again) != len(rebuilt) {
		t.Fatalf("second rebuild rows=%d want %d", len(again), len(rebuilt))
	}

	replayResult, err := c.Rebuild(RebuildOptions{Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	if replayResult.PreOverallRoot != replayResult.PostOverallRoot || replayResult.PostOverallRoot != rootWith {
		t.Fatalf("replay root drift: pre=%x post=%x want=%x", replayResult.PreOverallRoot, replayResult.PostOverallRoot, rootWith)
	}
	replayed := lexicalSnapshot(t, c)
	if len(replayed) != len(incremental) {
		t.Fatalf("replayed row count=%d want %d", len(replayed), len(incremental))
	}
	for key, want := range incremental {
		if !bytes.Equal(replayed[key], want) {
			t.Fatalf("replayed row differs at %q", key)
		}
	}
}

func TestQueryLexicalConversationDoesNotLeakMatchingForeignThread(t *testing.T) {
	c := openCortex(t)
	for _, message := range []Message{
		{ConversationID: "native-tools", Role: RoleUser, Content: "what happened to the native workspace tools"},
		{ConversationID: "machine-mail", Role: RoleUser, Content: "what happened with Machine Mail neo-o1@machinemail.org"},
	} {
		if _, err := c.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := c.QueryLexicalConversation(
		"what happened", "native-tools", time.Time{}, time.Time{}, 8,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ConversationID != "native-tools" {
		t.Fatalf("conversation-scoped lexical hits leaked: %+v", hits)
	}
	global, err := c.QueryLexical("what happened", time.Time{}, time.Time{}, 8)
	if err != nil || len(global) != 2 {
		t.Fatalf("explicit global lexical recall = %+v, err=%v", global, err)
	}
}

func TestLexicalIndexValuesVaultSealedAndCorruptionFailsOpen(t *testing.T) {
	c := openSealedCortex(t)
	if _, err := c.AppendMessage(Message{ConversationID: "sealed", Role: RoleUser, Content: "cipherword exact needle"}); err != nil {
		t.Fatal(err)
	}
	lo, hi := keys.PrefixRange(keys.PrefixLexical)
	it, err := c.s.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for it.First(); it.Valid(); it.Next() {
		value, verr := it.ValueAndErr()
		if verr != nil {
			t.Fatal(verr)
		}
		if !vault.IsVault(value) {
			t.Fatalf("lexical value at %q is plaintext", it.Key())
		}
		count++
	}
	it.Close()
	if count == 0 {
		t.Fatal("no lexical rows")
	}

	rows := lexicalSnapshot(t, c)
	for key := range rows {
		if bytes.HasPrefix([]byte(key), keys.PrefixLexicalDoc) {
			rows[key] = []byte("corrupt")
			break
		}
	}
	if err := c.s.ReplaceDerivedPrefix(keys.PrefixLexical, rows); err != nil {
		t.Fatal(err)
	}
	hits, err := c.QueryLexical("cipherword", time.Time{}, time.Time{}, 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("corrupt query hits=%+v err=%v", hits, err)
	}
}
