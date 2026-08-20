// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sessionjournal

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPlaintextDevelopmentSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "journal.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append(ctx, Event{
		ConversationID: "plain-conversation", Kind: KindUserMessage,
		DisplayContent: "plain development event", APIContent: []byte(`{"role":"user","content":"plain development event"}`),
		Message: &Message{Role: RoleUser},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read(ctx, "plain-conversation", 1, 0)
	if err != nil || len(events) != 1 || events[0].DisplayContent != "plain development event" {
		t.Fatalf("plaintext round trip = %+v, %v", events, err)
	}
}
