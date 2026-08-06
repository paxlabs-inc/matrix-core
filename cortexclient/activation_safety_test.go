// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortexclient

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestActivationRejectsMismatchedAndOperationalEnvelopes(t *testing.T) {
	conversation := ConversationBytes("activation-safety")
	userEnvelope := wrapEnvelope(
		UserMsgEvent(conversation, "semantic user content"),
		[16]byte{}, 1, 0, 1,
	)
	if got := eventText(KindToolResult, userEnvelope); got != "" {
		t.Fatalf("mismatched union payload decoded as %q", got)
	}
	if _, err := DecodeEvent(KindToolResult, userEnvelope); !errors.Is(err, ErrProtocol) {
		t.Fatalf("mismatched payload error = %v, want protocol violation", err)
	}

	bundle := &Bundle{}
	bundle.Sections[4].Tier = 4
	excluded := []EventKind{
		KindReasoning, KindProviderFrame, KindToolCall, KindToolResult,
		KindEffect, KindApproval, KindOutcome, KindCheckpoint, KindSupervisor,
		KindRecovery, KindIntentSet, KindLoopOpened, KindLoopClosed,
		KindConsolidation, KindEmbedding, KindRetract, KindAttestation,
	}
	for _, kind := range excluded {
		bundle.Sections[4].Items = append(bundle.Sections[4].Items, BundleItem{
			Tier: 4, URI: "operational://excluded",
			Content: append([]byte{byte(kind)}, userEnvelope...),
		})
	}
	bundle.Sections[4].Items = append(bundle.Sections[4].Items,
		BundleItem{Tier: 4, URI: "raw://ncev", Content: []byte("NCEV raw envelope bytes")},
		BundleItem{Tier: 4, URI: "memory-state://checkpoint", Content: append([]byte{byte(KindCheckpoint)}, userEnvelope...)},
	)
	rendered := RenderBundle(bundle, nil)
	if strings.Contains(rendered, "NCEV") || strings.Contains(rendered, "semantic user content") {
		t.Fatalf("operational or malformed activation content leaked: %q", rendered)
	}
	if projected := ProjectBundle(bundle); len(projected) != 0 {
		t.Fatalf("unsafe UI projection = %#v", projected)
	}
}

func TestRecallLanesRequireProvenanceAndApplySameTurnExclusion(t *testing.T) {
	conversation := ConversationBytes("recall-lanes")
	envelope := wrapEnvelope(UserMsgEvent(conversation, "remember cobalt lighthouse"), [16]byte{}, 1, 0, 1)
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	bundle := &Bundle{}
	bundle.Sections[6].Tier = 6
	bundle.Sections[6].Items = []BundleItem{
		{Tier: 6, URI: "recall://conv-old/41?at_ns=" + strconv.FormatInt(now.UnixNano(), 10) + "&source=1&score=900&vector_rank=1&why=vector", Provenance: []uint64{41}, Content: append([]byte{byte(KindUserMsg)}, envelope...)},
		{Tier: 6, URI: "recall://conv-missing/42", Content: append([]byte{byte(KindUserMsg)}, envelope...)},
	}
	projected := ProjectBundle(bundle)
	if len(projected) != 1 {
		t.Fatalf("projected recalls = %#v, want one fully provenanced item", projected)
	}
	item := projected[0]
	if item.ConversationID != "conv-old" || item.Date != "2026-08-05" ||
		item.SourceType == "" || item.Confidence <= 0 || item.RelevanceScore <= 0 ||
		item.SelectionReason != "vector" || item.SourceIdentity == "" ||
		item.EpistemicStatus != "observed" || len(item.Provenance) != 1 {
		t.Fatalf("incomplete explicit recall provenance: %#v", item)
	}
	excluded := ProjectBundleExcluding(bundle, ProjectionExclusions{
		ContentHashes: map[[32]byte]struct{}{sha256.Sum256([]byte(item.Text)): {}},
	})
	if len(excluded) != 0 {
		t.Fatalf("same-turn content re-entered recall: %#v", excluded)
	}
}
