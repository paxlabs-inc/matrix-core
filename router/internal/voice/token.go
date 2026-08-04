// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package voice

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"matrix/router/internal/proxy"
)

type Handler struct {
	Proxy     http.Handler
	ServerURL string
	APIKey    string
	Secret    string
	TTL       time.Duration
	Now       func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := strings.TrimSpace(proxy.Subject(r.Context()))
	conversationID := strings.TrimSpace(r.URL.Query().Get("conversation_id"))
	voice := strings.TrimSpace(r.URL.Query().Get("voice"))
	style := strings.TrimSpace(r.URL.Query().Get("style"))
	if userID == "" || !validConversationID(conversationID) || !validVoiceSettings(voice, style) {
		http.Error(w, "voice session is unavailable", http.StatusNotFound)
		return
	}
	if h.Proxy == nil || h.ServerURL == "" || h.APIKey == "" || h.Secret == "" {
		http.Error(w, "voice is not configured", http.StatusServiceUnavailable)
		return
	}
	probe := httptest.NewRequest(http.MethodGet, "/conversations/"+url.PathEscape(conversationID), http.NoBody).WithContext(r.Context())
	probe.Header = r.Header.Clone()
	recorder := httptest.NewRecorder()
	h.Proxy.ServeHTTP(recorder, probe)
	if recorder.Code != http.StatusOK {
		http.Error(w, "voice session is unavailable", http.StatusNotFound)
		return
	}
	startBody, _ := json.Marshal(map[string]string{"conversation_id": conversationID, "voice": voice, "style": style})
	start := httptest.NewRequest(http.MethodPost, "/voice/session/start", bytes.NewReader(startBody)).WithContext(r.Context())
	start.Header = r.Header.Clone()
	start.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	h.Proxy.ServeHTTP(recorder, start)
	if recorder.Code < 200 || recorder.Code >= 300 {
		http.Error(w, "voice session is unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	room := "voice:" + conversationID
	token, err := sign(h.APIKey, h.Secret, userID, room, now, ttl)
	if err != nil {
		http.Error(w, "voice token could not be created", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"server_url": h.ServerURL,
		"token":      token,
		"room":       room,
		"expires_at": now.Add(ttl).Format(time.RFC3339),
	})
}

func validVoiceSettings(voice, style string) bool {
	if len(style) > 500 {
		return false
	}
	switch voice {
	case "", "mimo_default", "冰糖", "茉莉", "苏打", "白桦", "Mia", "Chloe", "Milo", "Dean":
		return true
	default:
		return false
	}
}

func validConversationID(value string) bool {
	if len(value) < 2 || len(value) > 160 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func sign(apiKey, secret, identity, room string, now time.Time, ttl time.Duration) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"iss": apiKey,
		"sub": identity,
		"nbf": now.Unix() - 5,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		"video": map[string]any{
			"roomJoin":             true,
			"room":                 room,
			"canPublish":           true,
			"canSubscribe":         true,
			"canPublishData":       true,
			"canUpdateOwnMetadata": true,
		},
	})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyForTest(token, secret string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var claims map[string]any
	return claims, dec.Decode(&claims) == nil
}
