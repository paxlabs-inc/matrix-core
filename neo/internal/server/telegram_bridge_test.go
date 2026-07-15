// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

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
	"testing"

	"matrix/construct/backchannel"
	"matrix/construct/schema/primitives"
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
