// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveExaSearchAndContents(t *testing.T) {
	key := os.Getenv(APIKeyEnv)
	if key == "" {
		t.Skipf("live probe skipped: %s is not set", APIKeyEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := NewClient(ClientConfig{APIKey: key})
	search, err := client.Search(ctx, SearchRequest{Query: "latest Apple investor relations earnings release", Type: "fast", NumResults: 2})
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	if search.RequestID == "" || len(search.Results) == 0 || len(search.Results[0].Highlights) == 0 {
		t.Fatalf("live search shape: %#v", search)
	}
	contents, err := client.Contents(ctx, ContentsRequest{URLs: []string{search.Results[0].URL}, Highlights: true})
	if err != nil {
		t.Fatalf("live contents: %v", err)
	}
	if contents.RequestID == "" || len(contents.Statuses) != 1 || contents.Statuses[0].Status != "success" {
		t.Fatalf("live contents shape: %#v", contents)
	}
}
