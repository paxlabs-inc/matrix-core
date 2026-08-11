// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"matrix/neo/internal/channelgateway"
)

func TestQQProtocolNormalizationRESTAndGatewayResume(t *testing.T) {
	var serverURL string
	var connections atomic.Int32
	resumed := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			writeTestJSON(w, map[string]any{"access_token": "qq-token", "expires_in": "7200"})
		case "/gateway":
			if r.Header.Get("Authorization") != "QQBot qq-token" {
				t.Errorf("gateway authorization=%q", r.Header.Get("Authorization"))
			}
			writeTestJSON(w, map[string]string{"url": "ws" + strings.TrimPrefix(serverURL, "http") + "/ws"})
		case "/v2/groups/group/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["content"] != "reply" || int(body["msg_seq"].(float64)) != 7 {
				t.Errorf("message body=%v", body)
			}
			writeTestJSON(w, map[string]string{"id": "sent"})
		case "/v2/groups/rate/messages":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			writeTestJSON(w, map[string]string{"message": "slow down"})
		case "/v2/groups/group/files":
			writeTestJSON(w, map[string]string{"file_info": "file-info", "file_uuid": "file"})
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			current := connections.Add(1)
			_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 1000}})
			var auth map[string]any
			if conn.ReadJSON(&auth) != nil {
				return
			}
			if current == 1 {
				if int(auth["op"].(float64)) != 2 {
					t.Errorf("first auth=%v", auth)
				}
				_ = conn.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{"session_id": "session", "resume_gateway_url": "ws" + strings.TrimPrefix(serverURL, "http") + "/ws", "user": map[string]string{"id": "bot"}}})
				_ = conn.Close()
				return
			}
			if int(auth["op"].(float64)) == 6 {
				resumed <- struct{}{}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	api := NewQQAPI()
	api.Client = server.Client()
	api.APIBase, api.AuthBase = server.URL, server.URL
	config := QQConfig{Enabled: true, AppID: "app", ClientSecret: "secret"}
	receipt, err := api.PostText(context.Background(), config, channelgateway.Address{ConversationID: "group", ParticipantID: "user", Scope: channelgateway.ScopeGroup}, "GROUP_AT_MESSAGE_CREATE", "original", "reply", 7)
	if err != nil || receipt.ExternalMessageID != "sent" {
		t.Fatalf("send=%+v %v", receipt, err)
	}
	_, err = api.PostText(context.Background(), config, channelgateway.Address{ConversationID: "rate", ParticipantID: "user", Scope: channelgateway.ScopeGroup}, "GROUP_AT_MESSAGE_CREATE", "original", "reply", 8)
	var deliveryError *channelgateway.DeliveryError
	if !errors.As(err, &deliveryError) || deliveryError.Code != "rate_limited" || deliveryError.RetryAfter != 3*time.Second || deliveryError.Permanent {
		t.Fatalf("QQ rate limit = %#v (%v)", deliveryError, err)
	}
	message := QQMessage{ID: "message", Content: "hello", GroupOpenID: "group"}
	message.Author.MemberOpenID = "user"
	envelope, ok := NormalizeQQMessage(config, "GROUP_AT_MESSAGE_CREATE", message)
	if !ok || envelope.Address.Channel != channelgateway.ChannelQQ || envelope.Address.Scope != channelgateway.ScopeGroup {
		t.Fatalf("normalize=%+v %v", envelope, ok)
	}
	store, err := Open(t.TempDir(), cloudVault(t, t.TempDir()), "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ReplaceQQ(config); err != nil {
		t.Fatal(err)
	}
	gateway := NewQQGateway(api, store, func() QQConfig { return store.View().QQ }, func(context.Context, string, QQMessage) error { return nil })
	gateway.Start(context.Background())
	defer gateway.Stop()
	select {
	case <-resumed:
	case <-time.After(6 * time.Second):
		t.Fatal("QQ Gateway did not resume after disconnect")
	}
	state := store.View().QQ
	if state.SessionID != "session" || state.Sequence != 1 {
		t.Fatalf("session=%+v", state)
	}
}

type protocolRoundTripper func(*http.Request) (*http.Response, error)

func (f protocolRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestWeChatTransportErrorsExcludeSecretsFromReturnedAndPersistedState(t *testing.T) {
	ctx := context.Background()
	mpSecret, mpToken := "mp-application-secret", "mp-access-token"
	kfSecret, kfToken := "kf-application-secret", "kf-access-token"

	credentialErrorClient := func() *http.Client {
		return &http.Client{Transport: protocolRoundTripper(func(request *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("transport rejected %s", request.URL.String())
		})}
	}
	mpCredentialAPI := NewWeChatMPAPI()
	mpCredentialAPI.Client, mpCredentialAPI.BaseURL = credentialErrorClient(), "https://mp.invalid"
	_, mpCredentialError := mpCredentialAPI.AccessToken(ctx, WeChatMPConfig{AppID: "mp-app", AppSecret: mpSecret})
	kfCredentialAPI := NewWeChatKFAPI()
	kfCredentialAPI.Client, kfCredentialAPI.BaseURL = credentialErrorClient(), "https://kf.invalid"
	_, kfCredentialError := kfCredentialAPI.AccessToken(ctx, WeChatKFConfig{CorpID: "kf-corp", Secret: kfSecret})
	for name, err := range map[string]error{"mp credential": mpCredentialError, "kf credential": kfCredentialError} {
		if err == nil || strings.Contains(err.Error(), mpSecret) || strings.Contains(err.Error(), kfSecret) {
			t.Fatalf("%s error exposed a credential: %v", name, err)
		}
	}

	protocolClient := func(tokenPath, token string) *http.Client {
		return &http.Client{Transport: protocolRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == tokenPath {
				body := `{"access_token":"` + token + `","expires_in":7200}`
				if tokenPath == "/cgi-bin/gettoken" {
					body = `{"errcode":0,"access_token":"` + token + `","expires_in":7200}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			}
			return nil, fmt.Errorf("transport rejected %s", request.URL.String())
		})}
	}
	now := time.Now().Unix()
	mpAPI := NewWeChatMPAPI()
	mpAPI.Client, mpAPI.BaseURL = protocolClient("/cgi-bin/token", mpToken), "https://mp.invalid"
	_, mpDeliveryError := mpAPI.PostText(ctx, WeChatMPConfig{AppID: "mp-app", AppSecret: mpSecret}, "user", "answer", now)
	kfAPI := NewWeChatKFAPI()
	kfAPI.Client, kfAPI.BaseURL = protocolClient("/cgi-bin/gettoken", kfToken), "https://kf.invalid"
	_, kfDeliveryError := kfAPI.PostText(ctx, WeChatKFConfig{CorpID: "kf-corp", Secret: kfSecret}, "user", "open-kf", "answer", now)
	for name, err := range map[string]error{"mp delivery": mpDeliveryError, "kf delivery": kfDeliveryError} {
		if err == nil || strings.Contains(err.Error(), mpToken) || strings.Contains(err.Error(), kfToken) {
			t.Fatalf("%s error exposed an access token: %v", name, err)
		}
	}

	root := t.TempDir()
	gateway, err := channelgateway.Open(ctx, root, cloudVault(t, t.TempDir()), "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	for index, item := range []struct {
		channel channelgateway.Channel
		account string
		err     error
	}{{channelgateway.ChannelWeChatMP, "mp-app", mpDeliveryError}, {channelgateway.ChannelWeChatKF, "kf-corp", kfDeliveryError}} {
		envelope := channelgateway.Envelope{Direction: channelgateway.Outbound, Kind: channelgateway.KindMessage, Address: channelgateway.Address{Channel: item.channel, AccountID: item.account, ConversationID: "conversation", Scope: channelgateway.ScopeDirect}, IdempotencyKey: fmt.Sprintf("redaction-%d", index), Text: "answer", SideEffectClass: "network"}
		delivery, _, err := gateway.QueueOutbound(ctx, envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := gateway.MarkFailed(ctx, delivery.ID, item.err.Error(), "transport"); err != nil {
			t.Fatal(err)
		}
		persisted, err := gateway.Delivery(ctx, delivery.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{mpSecret, mpToken, kfSecret, kfToken} {
			if strings.Contains(persisted.LastError, secret) {
				t.Fatalf("persisted %s delivery error exposed %q", item.channel, secret)
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{mpSecret, mpToken, kfSecret, kfToken} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("gateway file %s exposed %q", entry.Name(), secret)
			}
		}
	}
}

func TestWeixinLongPollContextMediaAndRealCDN(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("real inbound image")
	encrypted := aesECBEncrypt(plain, key)
	var uploaded atomic.Bool
	var sent atomic.Bool
	var typing atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			writeTestJSON(w, map[string]any{"ret": 0, "get_updates_buf": "cursor-2", "msgs": []any{}})
		case "/ilink/bot/getuploadurl":
			writeTestJSON(w, map[string]string{"upload_full_url": "https://" + r.Host + "/upload"})
		case "/upload":
			uploaded.Store(true)
			w.Header().Set("X-Encrypted-Param", "download-param")
			w.WriteHeader(http.StatusOK)
		case "/download":
			_, _ = w.Write(encrypted)
		case "/ilink/bot/sendmessage":
			sent.Store(true)
			writeTestJSON(w, map[string]any{"ret": 0})
		case "/ilink/bot/getconfig":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["ilink_user_id"] != "user" || payload["context_token"] != "context" {
				t.Errorf("getconfig payload=%v", payload)
			}
			writeTestJSON(w, map[string]any{"ret": 0, "typing_ticket": "typing-ticket"})
		case "/ilink/bot/sendtyping":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["ilink_user_id"] != "user" || payload["typing_ticket"] != "typing-ticket" || int(payload["status"].(float64)) != 1 {
				t.Errorf("sendtyping payload=%v", payload)
			}
			typing.Store(true)
			writeTestJSON(w, map[string]any{"ret": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := NewWeixinAPI()
	api.Client, api.BaseURL, api.CDNBaseURL = server.Client(), server.URL, server.URL
	config := WeixinConfig{Enabled: true, Token: "token", BotID: "bot"}
	updates, err := api.GetUpdates(context.Background(), config)
	if err != nil || updates.Cursor != "cursor-2" {
		t.Fatalf("updates=%+v %v", updates, err)
	}
	ref := "weixin-media:" + base64.RawURLEncoding.EncodeToString([]byte("query")) + ":" + base64.RawURLEncoding.EncodeToString([]byte(hex.EncodeToString(key)))
	data, err := api.DownloadMedia(context.Background(), config, ref, 1024)
	if err != nil || string(data) != string(plain) {
		t.Fatalf("download=%q %v", data, err)
	}
	if _, err = api.SendMedia(context.Background(), config, "user", "context", "file.txt", []byte("outbound"), channelgateway.MediaFile); err != nil {
		t.Fatal(err)
	}
	if _, err = api.SendTyping(context.Background(), config, "user", "context", 1); err != nil {
		t.Fatal(err)
	}
	if !uploaded.Load() || !sent.Load() || !typing.Load() {
		t.Fatalf("uploaded=%v sent=%v typing=%v", uploaded.Load(), sent.Load(), typing.Load())
	}
	message := WeixinMessage{MessageID: "m1", FromUserID: "user", MessageType: 1, ContextToken: "context", ItemList: []WeixinItem{{Type: 1}}}
	message.ItemList[0].TextItem.Text = "hello"
	envelope, ok := NormalizeWeixinMessage(config, message)
	if !ok || envelope.Metadata["context_token"] != "context" || envelope.Text != "hello" {
		t.Fatalf("normalize=%+v %v", envelope, ok)
	}
}

func TestWeChatOfficialAndCustomerServiceRealAPIsAndWindows(t *testing.T) {
	var mpSent, kfSent, kfSynced atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/token":
			writeTestJSON(w, map[string]any{"access_token": "mp-token", "expires_in": 7200})
		case "/cgi-bin/message/custom/send":
			mpSent.Store(true)
			writeTestJSON(w, map[string]any{"errcode": 0, "msgid": 11})
		case "/cgi-bin/gettoken":
			writeTestJSON(w, map[string]any{"errcode": 0, "access_token": "kf-token", "expires_in": 7200})
		case "/cgi-bin/kf/sync_msg":
			kfSynced.Store(true)
			writeTestJSON(w, map[string]any{"errcode": 0, "next_cursor": "next", "has_more": 0, "msg_list": []any{}})
		case "/cgi-bin/kf/send_msg":
			kfSent.Store(true)
			writeTestJSON(w, map[string]any{"errcode": 0, "msgid": "kf-sent"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	mp := NewWeChatMPAPI()
	mp.BaseURL, mp.Client = server.URL, server.Client()
	mpConfig := WeChatMPConfig{AppID: "app", AppSecret: "secret", Token: "callback-token"}
	now := time.Now().Unix()
	if _, err := mp.PostText(context.Background(), mpConfig, "user", "answer", now); err != nil {
		t.Fatal(err)
	}
	if !mpSent.Load() {
		t.Fatal("Official Account message was not sent")
	}
	if _, err := mp.PostText(context.Background(), mpConfig, "user", "late", time.Now().Add(-49*time.Hour).Unix()); err == nil {
		t.Fatal("expired Official Account reply window was accepted")
	}
	mpMessage := WeChatMPMessage{ToUserName: "gh_account", FromUserName: "user", CreateTime: now, MsgType: "text", Content: "hello", MsgID: "mp-message"}
	mpEnvelope, ok := NormalizeWeChatMPMessage(mpConfig, mpMessage)
	if !ok || mpEnvelope.Address.AccountID != "app" {
		t.Fatalf("mp normalize=%+v %v", mpEnvelope, ok)
	}
	if VerifyCallbackSignature("callback-token", CallbackSignature("callback-token", "1", "nonce"), "1", "nonce") != nil {
		t.Fatal("plaintext callback signature failed")
	}
	kf := NewWeChatKFAPI()
	kf.BaseURL, kf.Client = server.URL, server.Client()
	kfConfig := WeChatKFConfig{CorpID: "corp", Secret: "secret", Token: "callback-token", EncodingAESKey: strings.Repeat("a", 43)}
	page, err := kf.Sync(context.Background(), kfConfig, "event-token", "open-kf", "")
	if err != nil || page.NextCursor != "next" || !kfSynced.Load() {
		t.Fatalf("sync=%+v %v", page, err)
	}
	if _, err = kf.PostText(context.Background(), kfConfig, "external", "open-kf", "answer", now); err != nil {
		t.Fatal(err)
	}
	if !kfSent.Load() {
		t.Fatal("Customer Service message was not sent")
	}
	kfMessage := WeChatKFMessage{MessageID: "kf-message", OpenKFID: "open-kf", ExternalUserID: "external", SendTime: now, MessageType: "text"}
	kfMessage.Text.Content = "hello"
	kfEnvelope, ok := NormalizeWeChatKFMessage(kfConfig, kfMessage)
	if !ok || kfEnvelope.Metadata["open_kfid"] != "open-kf" {
		t.Fatalf("kf normalize=%+v %v", kfEnvelope, ok)
	}
}
