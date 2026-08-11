// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"strings"
	"testing"

	"matrix/neo/internal/runtime/protocol"
)

func TestCanonicalWindowHasOneIdentityAndOneTranscriptProjection(t *testing.T) {
	const identity = "canonical structural identity marker"
	messages := requestMessages(identity, []protocol.Message{
		{Role: protocol.RoleUser, Content: "first exact turn"},
		{Role: protocol.RoleAssistant, Content: "second exact turn"},
		{Role: protocol.RoleUser, Content: "newest exact request"},
	}, "", "[memory]\nolder unrelated evidence")
	joined := ""
	for _, message := range messages {
		joined += "\n" + message.Content
	}
	for _, unique := range []string{identity, "first exact turn", "second exact turn", "newest exact request"} {
		if strings.Count(joined, unique) != 1 {
			t.Fatalf("%q appears %d times in composed window", unique, strings.Count(joined, unique))
		}
	}
	if messages[len(messages)-1].Role != protocol.RoleUser || messages[len(messages)-1].Content != "newest exact request" {
		t.Fatalf("newest genuine request is not authoritative: %#v", messages)
	}
}
