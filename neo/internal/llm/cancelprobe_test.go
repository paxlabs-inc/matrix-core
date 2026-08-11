// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
)

// A cancelled Chat must tear down its HTTP request: the server-side handler
// sees the disconnect and the connection is released. If this fails, every
// user stop leaks a live model call until the provider gives up on it.
func TestChatCancelReleasesConnection(t *testing.T) {
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http only watches for a client disconnect
		// (and cancels r.Context()) once the request body has been consumed —
		// exactly what a real provider does before generating.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(handlerDone)
	}))
	defer srv.CloseClientConnections()
	defer srv.Close()

	c, err := New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, cerr := c.Chat(ctx, ChatRequest{Messages: []Message{UserMessage("hi")}})
	if cerr == nil {
		t.Fatalf("Chat must fail on a cancelled context")
	}

	select {
	case <-handlerDone:
		// The request was torn down server-side — no leaked model call.
	case <-time.After(5 * time.Second):
		t.Fatalf("cancelled Chat leaked its HTTP request: server handler never saw the disconnect")
	}
}
