// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortexclient

import (
	"errors"
	"strings"
	"testing"
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
	bundle.Sections[4].Items = []BundleItem{
		{Tier: 4, URI: "event://1", Content: append([]byte{byte(KindCheckpoint)}, userEnvelope...)},
		{Tier: 4, URI: "event://2", Content: []byte("NCEV raw envelope bytes")},
	}
	rendered := RenderBundle(bundle, nil)
	if strings.Contains(rendered, "NCEV") || strings.Contains(rendered, "semantic user content") {
		t.Fatalf("operational or malformed activation content leaked: %q", rendered)
	}
	if projected := ProjectBundle(bundle); len(projected) != 0 {
		t.Fatalf("unsafe UI projection = %#v", projected)
	}
}
