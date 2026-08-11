// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/construct/backchannel"
	"matrix/construct/schema/primitives"
	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/telegramsettings"
)

func TestTelegramAskSignDenialIsTypedDecision(t *testing.T) {
	ask := &primitives.Ask{AskKind: primitives.AskSign}
	for _, response := range []func() (*primitives.AskResponse, error){
		func() (*primitives.AskResponse, error) { return telegramAskTextResponse(ask, "deny", "") },
		func() (*primitives.AskResponse, error) {
			result, _, err := telegramAskCallbackResponse(ask, 1)
			return result, err
		},
	} {
		got, err := response()
		if err != nil {
			t.Fatal(err)
		}
		if got.Confirmed == nil || *got.Confirmed || got.Signature != "" {
			t.Fatalf("denial encoded as %+v", got)
		}
		if err := backchannel.ValidateResponse(ask, got); err != nil {
			t.Fatalf("denial rejected by backchannel: %v", err)
		}
	}
}

func TestTelegramStaleGenerationCannotAdvanceOffset(t *testing.T) {
	store := telegramsettings.Open("")
	if err := store.Replace(telegramsettings.State{
		Token:        "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		BotID:        1,
		BotUsername:  "matrix_test_bot",
		LastUpdateID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	bridge := newTelegramBridge(nil, store)
	bridge.generation = 2
	current, err := bridge.setLastUpdateID(1, 100)
	if err != nil || current {
		t.Fatalf("stale write current=%v err=%v", current, err)
	}
	if got := store.View().LastUpdateID; got != 7 {
		t.Fatalf("stale worker advanced offset to %d", got)
	}
	current, err = bridge.setLastUpdateID(2, 8)
	if err != nil || !current || store.View().LastUpdateID != 8 {
		t.Fatalf("current write current=%v offset=%d err=%v", current, store.View().LastUpdateID, err)
	}
}

func TestTelegramStatusRedactsToken(t *testing.T) {
	token := "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	store := telegramsettings.Open("")
	if err := store.Replace(telegramsettings.State{Token: token, BotID: 1, BotUsername: "matrix_test_bot"}); err != nil {
		t.Fatal(err)
	}
	bridge := newTelegramBridge(nil, store)
	bridge.lastError = "request failed at https://api.telegram.org/bot" + token + "/getUpdates"
	status := bridge.Status()
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("status exposed token: %s", encoded)
	}
}

func TestTelegramAPIResponseRedactsToken(t *testing.T) {
	token := "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(telegramEnvelope{OK: false, ErrorCode: 401, Description: "rejected " + token})
	}))
	defer server.Close()
	api := newTelegramAPI()
	api.baseURL = server.URL
	api.client = server.Client()
	err := api.call(context.Background(), token, "getMe", nil, nil)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("unredacted API error: %v", err)
	}
}

func TestTelegramSendPhotoMultipart(t *testing.T) {
	token := "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+token+"/sendPhoto" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		if got := r.FormValue("chat_id"); got != "42" {
			t.Errorf("chat_id = %q", got)
		}
		file, header, err := r.FormFile("photo")
		if err != nil {
			t.Errorf("photo: %v", err)
		} else {
			defer file.Close()
			data, readErr := io.ReadAll(file)
			if readErr != nil || header.Filename != "result.png" || string(data) != "png-data" {
				t.Errorf("file=%q data=%q err=%v", header.Filename, data, readErr)
			}
		}
		_ = json.NewEncoder(w).Encode(telegramEnvelope{OK: true, Result: json.RawMessage("true")})
	}))
	defer server.Close()
	api := newTelegramAPI()
	api.baseURL = server.URL
	api.client = server.Client()
	if err := api.sendPhoto(context.Background(), token, 42, "result.png", []byte("png-data")); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramReadRunMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "result.png"), []byte("png-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	bridge := newTelegramBridge(&Engine{mediaDir: dir}, telegramsettings.Open(""))
	name, data, err := bridge.readRunMedia("/media/result.png")
	if err != nil || name != "result.png" || string(data) != "png-data" {
		t.Fatalf("name=%q data=%q err=%v", name, data, err)
	}
	if _, _, err := bridge.readRunMedia("/media/../secret"); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestTelegramGatewayDrainsDurableDeliveryAfterRestart(t *testing.T) {
	var calls atomic.Int32
	token := "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+token+"/sendMessage" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(telegramEnvelope{OK: false, ErrorCode: 503, Description: "temporary outage"})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode message: %v", err)
		}
		if body["chat_id"] != float64(42) || body["text"] != "durable Telegram answer" {
			t.Errorf("message = %#v", body)
		}
		_ = json.NewEncoder(w).Encode(telegramEnvelope{OK: true, Result: json.RawMessage(`{"message_id":99}`)})
	}))
	defer protocol.Close()

	ctx := context.Background()
	root := t.TempDir()
	gateway, err := channelgateway.Open(ctx, root, nil, "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{channelGateway: gateway, channelDispatch: channelgateway.NewDispatcher(gateway, channelgateway.RetryPolicy{
		BaseBackoff: time.Millisecond, MaximumBackoff: time.Millisecond, MaximumAttempts: 3,
	})}
	settings := telegramsettings.Open("")
	if err := settings.Replace(telegramsettings.State{
		Token: token, BotID: 7, BotUsername: "matrix_test_bot", ChatID: 42,
		TelegramUserID: 8, ConversationID: "tg-7-42",
	}); err != nil {
		t.Fatal(err)
	}
	bridge := newTelegramBridge(engine, settings)
	bridge.api.baseURL = protocol.URL
	bridge.api.client = protocol.Client()
	bridge.generation = 1
	if err := bridge.sendTextKey(ctx, token, 42, "durable Telegram answer", nil, "telegram:run-1:4:text"); err != nil {
		t.Fatalf("initial queued send: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("initial calls = %d", calls.Load())
	}
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	reopened, err := channelgateway.Open(ctx, root, nil, "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	engine.channelGateway = reopened
	engine.channelDispatch = channelgateway.NewDispatcher(reopened, channelgateway.RetryPolicy{
		BaseBackoff: time.Millisecond, MaximumBackoff: time.Millisecond, MaximumAttempts: 3,
	})
	bridge.drainOutbound(ctx, 1, token)
	if calls.Load() != 2 {
		t.Fatalf("calls after restart = %d, want 2", calls.Load())
	}
	due, err := reopened.Due(ctx, channelgateway.ChannelTelegram, "7", 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("remaining due = %+v, err %v", due, err)
	}
}
