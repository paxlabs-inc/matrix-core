// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVoiceSessionDurableLifecycle(t *testing.T) {
	e := NewEngine(EngineOptions{ConversationDir: t.TempDir(), TraceDir: t.TempDir(), BackendURL: "http://127.0.0.1:1"})
	t.Cleanup(e.Close)
	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	e.conv.AppendUser("voice_conv", "voice_run", "start voice")
	e.registerRun(&run{id: "voice_run", convID: "voice_conv"})
	e.broker.ensure("voice_run")

	e.publishVoiceSession("voice_conv", "start", "connected")
	e.publishVoiceSession("voice_conv", "state", "speaking")
	e.publishVoiceSession("voice_conv", "stop", "idle")
	e.trace.Flush()

	req := httptest.NewRequest(http.MethodGet, "/conversations/voice_conv/trace", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	for _, typ := range []string{"voice.session.start", "voice.session.state", "voice.session.stop"} {
		if !strings.Contains(rec.Body.String(), typ) {
			t.Fatalf("trace missing %s: %s", typ, rec.Body.String())
		}
	}
}
