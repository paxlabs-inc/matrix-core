// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"matrix/router/internal/proxy"
)

func TestTokenRoundTripAndCrossUserDenial(t *testing.T) {
	var startBody map[string]string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if proxy.Subject(r.Context()) != "user-a" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/voice/session/start" {
			if err := json.NewDecoder(r.Body).Decode(&startBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.URL.Path != "/conversations/conv-owned" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conversation_id":"conv-owned"}`))
	})
	now := time.Unix(1_800_000_000, 0).UTC()
	h := &Handler{Proxy: backend, ServerURL: "wss://voice.example", APIKey: "api-key", Secret: "secret", TTL: 5 * time.Minute, Now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodGet, "/voice/token?conversation_id=conv-owned&voice=Chloe&style=Calm+and+concise.", nil)
	req = req.WithContext(proxy.WithSubject(context.Background(), "user-a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ServerURL string `json:"server_url"`
		Token     string `json:"token"`
		Room      string `json:"room"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	claims, ok := VerifyForTest(response.Token, "secret")
	if !ok || response.ServerURL != "wss://voice.example" || response.Room != "voice:conv-owned" {
		t.Fatalf("response=%#v claims=%#v ok=%v", response, claims, ok)
	}
	if startBody["voice"] != "Chloe" || startBody["style"] != "Calm and concise." {
		t.Fatalf("start body = %#v", startBody)
	}
	video := claims["video"].(map[string]any)
	if claims["iss"] != "api-key" || claims["sub"] != "user-a" || video["room"] != "voice:conv-owned" || video["roomJoin"] != true {
		t.Fatalf("claims=%#v", claims)
	}

	req = httptest.NewRequest(http.MethodGet, "/voice/token?conversation_id=conv-owned", nil)
	req = req.WithContext(proxy.WithSubject(context.Background(), "user-b"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user status = %d, want 404", rec.Code)
	}
}

func TestTokenRequiresConfiguredSecretAndSafeConversationID(t *testing.T) {
	h := &Handler{Proxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })}
	for _, id := range []string{"../other", "voice:other", ""} {
		req := httptest.NewRequest(http.MethodGet, "/voice/token?conversation_id="+id, nil)
		req = req.WithContext(proxy.WithSubject(context.Background(), "user"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id %q status = %d, want 404", id, rec.Code)
		}
	}
}
