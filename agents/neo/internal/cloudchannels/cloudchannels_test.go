// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"centra/agents/neo/internal/channelgateway"
	"centra/packages/vault"

	"github.com/gorilla/websocket"
)

func cloudVault(t *testing.T, root string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{Required: true, DataDir: root, UserDID: "did:matrix:alice", KEKHex: kek})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestStoreSealsAllCloudChannelSecretsAndReloads(t *testing.T) {
	root, keyRoot := t.TempDir(), t.TempDir()
	store, err := Open(root, cloudVault(t, keyRoot), "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceSlack(SlackConfig{Enabled: true, Mode: "socket", BotToken: "xoxb-private-slack-token", AppToken: "xapp-private-app-token", TeamID: "T1", BotUserID: "U1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDiscord(DiscordConfig{Enabled: true, BotToken: "private-discord-token", ApplicationID: "A1", PublicKey: strings.Repeat("ab", 32), BotUserID: "D1", Gateway: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceLark(LarkConfig{Enabled: true, Region: "lark", Mode: "webhook", AppID: "L1", AppSecret: "private-lark-secret", VerificationToken: "private-lark-token", BotOpenID: "LB1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceDingTalk(DingTalkConfig{Enabled: true, Mode: "stream", ClientID: "DT1", ClientSecret: "private-ding-secret", RobotCode: "DR1"}); err != nil {
		t.Fatal(err)
	}
	aesKey := strings.Repeat("a", 43)
	if err := store.ReplaceWeComBot(WeComBotConfig{Enabled: true, Mode: "websocket", BotID: "WB1", Secret: "private-wecom-bot-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeComApp(WeComAppConfig{Enabled: true, CorpID: "WC1", AgentID: 9, Secret: "private-wecom-app-secret", Token: "private-wecom-token", EncodingAESKey: aesKey}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceQQ(QQConfig{Enabled: true, AppID: "QQ1", ClientSecret: "private-qq-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeixin(WeixinConfig{Enabled: true, Token: "private-weixin-token", BotID: "WX1", Contexts: map[string]WeixinContext{"user": {Token: "private-context-token", UpdatedAt: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeChatMP(WeChatMPConfig{Enabled: true, AppID: "MP1", AppSecret: "private-mp-secret", Token: "private-mp-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeChatKF(WeChatKFConfig{Enabled: true, CorpID: "KF1", Secret: "private-kf-secret", Token: "private-kf-token", EncodingAESKey: aesKey, Cursors: map[string]string{"open": "private-kf-cursor"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"xoxb-private-slack-token", "xapp-private-app-token", "private-discord-token", "private-lark-secret", "private-lark-token", "private-ding-secret", "private-wecom-bot-secret", "private-wecom-app-secret", "private-wecom-token", "private-qq-secret", "private-weixin-token", "private-context-token", "private-mp-secret", "private-mp-token", "private-kf-secret", "private-kf-token", "private-kf-cursor"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("sealed state exposed %q", secret)
		}
	}
	reopened, err := Open(root, cloudVault(t, keyRoot), "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.View()
	if state.Slack.TeamID != "T1" || state.Discord.BotUserID != "D1" || !state.Discord.Gateway || state.Lark.BotOpenID != "LB1" || state.DingTalk.RobotCode != "DR1" || state.WeComBot.BotID != "WB1" || state.WeComApp.AgentID != 9 || state.QQ.AppID != "QQ1" || state.Weixin.Contexts["user"].Token != "private-context-token" || state.WeChatMP.AppID != "MP1" || state.WeChatKF.Cursors["open"] != "private-kf-cursor" {
		t.Fatalf("reloaded state = %+v", state)
	}
}

func TestWeChatMapStateIsolationConcurrencyAndRollback(t *testing.T) {
	weixinContexts := map[string]WeixinContext{"user": {Token: "context-original", UpdatedAt: 1}}
	kfCursors := map[string]string{"open-kf": "cursor-original"}
	store, err := Open(t.TempDir(), cloudVault(t, t.TempDir()), "did:matrix:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeixin(WeixinConfig{Enabled: true, Token: "weixin-token", BotID: "bot", Contexts: weixinContexts}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWeChatKF(WeChatKFConfig{Enabled: true, CorpID: "corp", Secret: "kf-secret", Token: "callback-token", EncodingAESKey: strings.Repeat("a", 43), Cursors: kfCursors}); err != nil {
		t.Fatal(err)
	}

	weixinContexts["user"] = WeixinContext{Token: "caller-mutated"}
	kfCursors["open-kf"] = "caller-mutated"
	view := store.View()
	if view.Weixin.Contexts["user"].Token != "context-original" || view.WeChatKF.Cursors["open-kf"] != "cursor-original" {
		t.Fatalf("store retained caller-owned maps: %+v %+v", view.Weixin.Contexts, view.WeChatKF.Cursors)
	}
	view.Weixin.Contexts["user"] = WeixinContext{Token: "view-mutated"}
	view.WeChatKF.Cursors["open-kf"] = "view-mutated"
	unchanged := store.View()
	if unchanged.Weixin.Contexts["user"].Token != "context-original" || unchanged.WeChatKF.Cursors["open-kf"] != "cursor-original" {
		t.Fatalf("View exposed mutable store maps: %+v %+v", unchanged.Weixin.Contexts, unchanged.WeChatKF.Cursors)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < 200; index++ {
			view.Weixin.Contexts["user"] = WeixinContext{Token: fmt.Sprintf("detached-%d", index)}
			view.WeChatKF.Cursors["open-kf"] = fmt.Sprintf("detached-%d", index)
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		for index := 0; index < 20; index++ {
			if err := store.UpdateWeixinProgress(fmt.Sprintf("cursor-%d", index), "user", fmt.Sprintf("context-%d", index), int64(index)); err != nil {
				t.Errorf("update Weixin progress: %v", err)
				return
			}
			if err := store.UpdateWeChatKFCursor("open-kf", fmt.Sprintf("kf-cursor-%d", index)); err != nil {
				t.Errorf("update KF cursor: %v", err)
				return
			}
		}
	}()
	close(start)
	wait.Wait()

	previous := store.View()
	store.vault = nil
	if err := store.UpdateWeixinProgress("uncommitted-cursor", "user", "uncommitted-context", 999); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("Weixin persistence error = %v", err)
	}
	if err := store.UpdateWeChatKFCursor("open-kf", "uncommitted-kf-cursor"); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("KF persistence error = %v", err)
	}
	afterFailure := store.View()
	if afterFailure.Weixin.UpdatesCursor != previous.Weixin.UpdatesCursor || afterFailure.Weixin.Contexts["user"] != previous.Weixin.Contexts["user"] || afterFailure.WeChatKF.Cursors["open-kf"] != previous.WeChatKF.Cursors["open-kf"] {
		t.Fatalf("failed persistence mutated live state: before=%+v after=%+v", previous, afterFailure)
	}
}

func TestSlackSignatureNormalizationAndRealREST(t *testing.T) {
	secret := "a-real-signing-secret"
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"app_mention","user":"U2","text":"<@U1> inspect this","channel":"C1","channel_type":"channel","ts":"1710000000.123"}}`)
	timestamp := time.Now().UTC().Unix()
	// Sign the exact Slack base string rather than using a helper under test.
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(append([]byte("v0:"+strconvFormat(timestamp)+":"), body...))
	header := http.Header{}
	header.Set("X-Slack-Request-Timestamp", strconvFormat(timestamp))
	header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	if err := VerifySlackRequest(header, body, secret, time.Now().UTC()); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var payload SlackEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	envelope, event, ok, err := NormalizeSlackEvent(SlackConfig{TeamID: "T1", BotUserID: "U1", GroupTrigger: "mention_or_reply"}, payload)
	if err != nil || !ok || envelope.Text != "inspect this" || envelope.Address.Scope != channelgateway.ScopeGroup || event.Channel != "C1" {
		t.Fatalf("normalized = %+v event=%+v ok=%v err=%v", envelope, event, ok, err)
	}

	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth.test" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T1", "user_id": "U1"})
			return
		}
		if r.URL.Path != "/chat.postMessage" || r.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1710000001.456"})
	}))
	defer protocol.Close()
	api := &SlackAPI{Client: protocol.Client(), BaseURL: protocol.URL}
	if auth, err := api.AuthTest(context.Background(), "xoxb-test"); err != nil || auth.TeamID != "T1" {
		t.Fatalf("auth = %+v, %v", auth, err)
	}
	receipt, err := api.PostMessage(context.Background(), "xoxb-test", "C1", "1710000000.123", "answer", nil)
	if err != nil || receipt.ExternalMessageID != "1710000001.456" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
}

func TestDiscordSignatureNormalizationAndRealREST(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":1,"application_id":"A1"}`)
	timestamp := "1710000000"
	signature := ed25519.Sign(privateKey, append([]byte(timestamp), body...))
	header := http.Header{}
	header.Set("X-Signature-Timestamp", timestamp)
	header.Set("X-Signature-Ed25519", hex.EncodeToString(signature))
	if err := VerifyDiscordRequest(header, body, hex.EncodeToString(publicKey)); err != nil {
		t.Fatalf("signature: %v", err)
	}
	message := DiscordMessage{ID: "M1", ChannelID: "C1", GuildID: "G1", Content: "<@D1> inspect", Author: DiscordIdentity{ID: "U1"}, Mentions: []DiscordIdentity{{ID: "D1"}}}
	envelope, ok := NormalizeDiscordMessage(DiscordConfig{ApplicationID: "A1", BotUserID: "D1", GroupTrigger: "mention_or_reply"}, message)
	if !ok || envelope.Text != "inspect" || envelope.Address.Scope != channelgateway.ScopeGroup {
		t.Fatalf("normalized = %+v ok=%v", envelope, ok)
	}

	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/@me" {
			_ = json.NewEncoder(w).Encode(DiscordIdentity{ID: "D1", Username: "neo", Bot: true})
			return
		}
		if r.URL.Path != "/channels/C1/messages" || r.Header.Get("Authorization") != "Bot discord-token" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "M2"})
	}))
	defer protocol.Close()
	api := &DiscordAPI{Client: protocol.Client(), BaseURL: protocol.URL}
	if identity, err := api.Identity(context.Background(), "discord-token"); err != nil || identity.ID != "D1" {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	receipt, err := api.PostMessage(context.Background(), "discord-token", "C1", "answer", "M1", nil)
	if err != nil || receipt.ExternalMessageID != "M2" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
}

func TestNativeMediaUploadsRateLimitsAndBounds(t *testing.T) {
	var slackUpload, slackComplete bool
	slackProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": "http://" + r.Host + "/upload", "file_id": "F1"})
		case "/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("Slack upload form: %v", err)
			}
			slackUpload = true
			w.WriteHeader(http.StatusOK)
		case "/files.completeUploadExternal":
			slackComplete = true
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected Slack path %s", r.URL.Path)
		}
	}))
	defer slackProtocol.Close()
	slackAPI := &SlackAPI{Client: slackProtocol.Client(), BaseURL: slackProtocol.URL}
	receipt, err := slackAPI.UploadFile(context.Background(), "xoxb-token", "C1", "T1", "image.png", []byte("real-image-bytes"), "caption")
	if err != nil || receipt.ExternalMessageID != "F1" || !slackUpload || !slackComplete {
		t.Fatalf("Slack upload = %+v upload=%v complete=%v err=%v", receipt, slackUpload, slackComplete, err)
	}

	discordProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("Discord content type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("Discord upload form: %v", err)
		}
		if _, _, err := r.FormFile("files[0]"); err != nil {
			t.Errorf("Discord upload file: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "M-media"})
	}))
	defer discordProtocol.Close()
	discordAPI := &DiscordAPI{Client: discordProtocol.Client(), BaseURL: discordProtocol.URL}
	receipt, err = discordAPI.UploadFile(context.Background(), "token", "C1", "image.png", []byte("real-image-bytes"), "caption", "M1")
	if err != nil || receipt.ExternalMessageID != "M-media" {
		t.Fatalf("Discord upload = %+v err=%v", receipt, err)
	}

	rateProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "slack") {
			w.Header().Set("Retry-After", "2")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"retry_after":1.5}`))
	}))
	defer rateProtocol.Close()
	slackAPI.BaseURL = rateProtocol.URL + "/slack"
	_, err = slackAPI.PostMessage(context.Background(), "token", "C1", "", "text", nil)
	var deliveryErr *channelgateway.DeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.RetryAfter != 2*time.Second {
		t.Fatalf("Slack rate error = %#v (%v)", deliveryErr, err)
	}
	discordAPI.BaseURL = rateProtocol.URL
	_, err = discordAPI.PostMessage(context.Background(), "token", "C1", "text", "", nil)
	if !errors.As(err, &deliveryErr) || deliveryErr.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("Discord rate error = %#v (%v)", deliveryErr, err)
	}
	if _, err := slackAPI.UploadFile(context.Background(), "token", "C1", "", "too-large", make([]byte, (20<<20)+1), ""); err == nil {
		t.Fatal("Slack accepted media above its bound")
	}
	if _, err := discordAPI.UploadFile(context.Background(), "token", "C1", "empty", nil, "", ""); err == nil {
		t.Fatal("Discord accepted empty media")
	}
}

func TestSlackSocketAndDiscordGatewayUseRealWebSockets(t *testing.T) {
	upgrader := websocket.Upgrader{}
	slackHandled := make(chan SlackEventPayload, 1)
	slackAcked := make(chan string, 1)
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Slack upgrade: %v", err)
			return
		}
		defer connection.Close()
		_ = connection.WriteJSON(map[string]any{"envelope_id": "socket-1", "type": "events_api", "payload": map[string]any{"type": "event_callback", "team_id": "T1", "event_id": "E1"}})
		var ack map[string]string
		if err := connection.ReadJSON(&ack); err == nil {
			slackAcked <- ack["envelope_id"]
		}
	}))
	defer slackServer.Close()
	slackCtx, cancelSlack := context.WithCancel(context.Background())
	slack := NewSlackSocket(NewSlackAPI(), func() SlackConfig { return SlackConfig{} }, func(_ context.Context, payload SlackEventPayload) error {
		slackHandled <- payload
		cancelSlack()
		return nil
	})
	if err := slack.serve(slackCtx, "ws"+strings.TrimPrefix(slackServer.URL, "http")); err != nil && !errorsIsContext(err) {
		t.Fatalf("Slack socket: %v", err)
	}
	if (<-slackAcked) != "socket-1" || (<-slackHandled).EventID != "E1" {
		t.Fatal("Slack socket did not ack and dispatch the real frame")
	}

	discordHandled := make(chan DiscordMessage, 1)
	discordServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Discord upgrade: %v", err)
			return
		}
		defer connection.Close()
		_ = connection.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 60000}})
		var identify map[string]any
		if err := connection.ReadJSON(&identify); err != nil || identify["op"] != float64(2) {
			t.Errorf("identify = %#v err=%v", identify, err)
			return
		}
		_ = connection.WriteJSON(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]string{"session_id": "S1", "resume_gateway_url": "wss://gateway.discord.gg"}})
		_ = connection.WriteJSON(map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": DiscordMessage{ID: "M1", ChannelID: "C1", Content: "hello", Author: DiscordIdentity{ID: "U1"}}})
		<-r.Context().Done()
	}))
	defer discordServer.Close()
	discordCtx, cancelDiscord := context.WithCancel(context.Background())
	discord := NewDiscordGateway(NewDiscordAPI(), nil, func() DiscordConfig { return DiscordConfig{BotToken: "token"} }, func(_ context.Context, message DiscordMessage) error {
		discordHandled <- message
		cancelDiscord()
		return nil
	})
	if err := discord.serve(discordCtx, "ws"+strings.TrimPrefix(discordServer.URL, "http"), DiscordConfig{BotToken: "token"}); err != nil && !errorsIsContext(err) {
		t.Fatalf("Discord Gateway: %v", err)
	}
	if (<-discordHandled).ID != "M1" {
		t.Fatal("Discord Gateway did not dispatch MESSAGE_CREATE")
	}
}

func TestSlackSocketReconnectsAfterProtocolDisconnect(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var connections atomic.Int32
	var protocol *httptest.Server
	protocol = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws" + strings.TrimPrefix(protocol.URL, "http") + "/socket"})
		case "/socket":
			connection, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			attempt := connections.Add(1)
			_ = connection.WriteJSON(map[string]any{"envelope_id": fmt.Sprintf("socket-%d", attempt), "type": "events_api", "payload": map[string]any{"type": "event_callback", "team_id": "T1", "event_id": fmt.Sprintf("E%d", attempt)}})
			var ack map[string]string
			_ = connection.ReadJSON(&ack)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer protocol.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	api := &SlackAPI{Client: protocol.Client(), BaseURL: protocol.URL}
	socket := NewSlackSocket(api, func() SlackConfig {
		return SlackConfig{Enabled: true, Mode: "socket", BotToken: "xoxb-token", AppToken: "xapp-token"}
	}, func(_ context.Context, payload SlackEventPayload) error {
		if payload.EventID == "E2" {
			cancel()
		}
		return nil
	})
	socket.run(ctx)
	if connections.Load() < 2 {
		t.Fatalf("Socket Mode connections = %d, want reconnect", connections.Load())
	}
}

func TestDiscordGatewayReconnectsAfterProtocolDisconnect(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var connections atomic.Int32
	var protocol *httptest.Server
	protocol = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gateway/bot":
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(protocol.URL, "http") + "/gateway"})
		case "/gateway":
			connection, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			attempt := connections.Add(1)
			_ = connection.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 60000}})
			var identify map[string]any
			if err := connection.ReadJSON(&identify); err != nil {
				t.Errorf("identify: %v", err)
				return
			}
			if attempt == 1 {
				return
			}
			_ = connection.WriteJSON(map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": DiscordMessage{ID: "M2", ChannelID: "C1", Content: "hello", Author: DiscordIdentity{ID: "U1"}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer protocol.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	api := &DiscordAPI{Client: protocol.Client(), BaseURL: protocol.URL}
	config := DiscordConfig{Enabled: true, Gateway: true, BotToken: "token", ApplicationID: "A1", PublicKey: strings.Repeat("ab", 32)}
	gateway := NewDiscordGateway(api, nil, func() DiscordConfig { return config }, func(_ context.Context, message DiscordMessage) error {
		if message.ID == "M2" {
			cancel()
		}
		return nil
	})
	gateway.run(ctx)
	if connections.Load() < 2 {
		t.Fatalf("Discord Gateway connections = %d, want reconnect", connections.Load())
	}
}

func strconvFormat(value int64) string {
	return fmt.Sprintf("%d", value)
}

func errorsIsContext(err error) bool {
	return err == context.Canceled || strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "use of closed network connection")
}
