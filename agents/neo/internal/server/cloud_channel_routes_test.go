// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"centra/agents/neo/internal/channelgateway"
	"centra/agents/neo/internal/cloudchannels"
)

func newCloudRouteServer(t *testing.T) (*Server, *cloudChannelBridge) {
	t.Helper()
	user := "did:matrix:alice"
	session := mediaVaultSession(t, user)
	engine := newRunRecordEngine(t)
	engine.vault, engine.vaultUser = session, user
	gateway, err := channelgateway.Open(context.Background(), t.TempDir(), session, user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateway.Close() })
	engine.channelGateway = gateway
	engine.channelDispatch = channelgateway.NewDispatcher(gateway, channelgateway.RetryPolicy{})
	store, err := cloudchannels.Open(t.TempDir(), session, user)
	if err != nil {
		t.Fatal(err)
	}
	bridge := newCloudChannelBridge(engine, store, nil)
	engine.cloudChannels = bridge
	return &Server{engine: engine}, bridge
}

func TestSlackConfigurationAndSignedChallengeUseRealProtocol(t *testing.T) {
	server, bridge := newCloudRouteServer(t)
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth.test" || r.Header.Get("Authorization") != "Bearer xoxb-real-bot-token" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T1", "user_id": "U1"})
	}))
	defer protocol.Close()
	bridge.slackAPI.BaseURL, bridge.slackAPI.Client = protocol.URL, protocol.Client()

	configure := []byte(`{"mode":"events","bot_token":"xoxb-real-bot-token","signing_secret":"real-signing-secret-123","group_trigger":"mention_or_reply"}`)
	recorder := httptest.NewRecorder()
	server.handleSlack(recorder, httptest.NewRequest(http.MethodPut, "/integrations/slack", bytes.NewReader(configure)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "xoxb-real-bot-token") || !strings.Contains(recorder.Body.String(), `"account_id":"T1"`) {
		t.Fatalf("configure = %d %s", recorder.Code, recorder.Body.String())
	}

	body := []byte(`{"type":"url_verification","challenge":"challenge-value"}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("real-signing-secret-123"))
	_, _ = mac.Write(append([]byte("v0:"+timestamp+":"), body...))
	request := httptest.NewRequest(http.MethodPost, "/integrations/slack/events", bytes.NewReader(body))
	request.Header.Set("X-Slack-Request-Timestamp", timestamp)
	request.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	recorder = httptest.NewRecorder()
	server.handleSlackEvents(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "challenge-value") {
		t.Fatalf("challenge = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDiscordConfigurationAndSignedInteractionUseRealProtocol(t *testing.T) {
	server, bridge := newCloudRouteServer(t)
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" || r.Header.Get("Authorization") != "Bot discord-real-token" {
			t.Errorf("request = %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(cloudchannels.DiscordIdentity{ID: "D1", Username: "neo", Bot: true})
	}))
	defer protocol.Close()
	bridge.discordAPI.BaseURL, bridge.discordAPI.Client = protocol.URL, protocol.Client()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	configure := []byte(fmt.Sprintf(`{"bot_token":"discord-real-token","application_id":"A1","public_key":"%s","gateway":false}`, hex.EncodeToString(publicKey)))
	recorder := httptest.NewRecorder()
	server.handleDiscord(recorder, httptest.NewRequest(http.MethodPut, "/integrations/discord", bytes.NewReader(configure)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "discord-real-token") || !strings.Contains(recorder.Body.String(), `"bot_user_id":"D1"`) {
		t.Fatalf("configure = %d %s", recorder.Code, recorder.Body.String())
	}

	body := []byte(`{"id":"I1","application_id":"A1","type":1}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := ed25519.Sign(privateKey, append([]byte(timestamp), body...))
	request := httptest.NewRequest(http.MethodPost, "/integrations/discord/interactions", bytes.NewReader(body))
	request.Header.Set("X-Signature-Timestamp", timestamp)
	request.Header.Set("X-Signature-Ed25519", hex.EncodeToString(signature))
	recorder = httptest.NewRecorder()
	server.handleDiscordInteractions(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":1`) {
		t.Fatalf("interaction = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEnterpriseConfigurationsAndEncryptedCallbackVerification(t *testing.T) {
	server, bridge := newCloudRouteServer(t)
	larkProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "lark-token", "expire": 7200})
		case "/bot/v3/info/":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "bot": map[string]string{"open_id": "bot-open"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer larkProtocol.Close()
	bridge.larkAPI.BaseURL, bridge.larkAPI.Client = larkProtocol.URL, larkProtocol.Client()
	recorder := httptest.NewRecorder()
	server.handleLark(recorder, httptest.NewRequest(http.MethodPut, "/integrations/lark", strings.NewReader(`{"region":"lark","mode":"webhook","app_id":"app","app_secret":"lark-secret","verification_token":"verify-token"}`)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "lark-secret") || !strings.Contains(recorder.Body.String(), "bot-open") {
		t.Fatalf("Lark configure = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.handleLarkEvents(recorder, httptest.NewRequest(http.MethodPost, "/integrations/lark/events", strings.NewReader(`{"type":"url_verification","challenge":"challenge","token":"verify-token"}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "challenge") {
		t.Fatalf("Lark challenge = %d %s", recorder.Code, recorder.Body.String())
	}
	cardBody := `{"schema":"2.0","header":{"event_id":"card-event","app_id":"app","tenant_key":"tenant","token":"verify-token"},"event":{"operator":{"open_id":"user"},"action":{"value":{"action":"neo_gate_approve"}},"context":{"open_message_id":"message","open_chat_id":"chat"}}}`
	recorder = httptest.NewRecorder()
	server.handleLarkCards(recorder, httptest.NewRequest(http.MethodPost, "/integrations/lark/cards", strings.NewReader(cardBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "no longer active") {
		t.Fatalf("Lark card = %d %s", recorder.Code, recorder.Body.String())
	}

	dingProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "ding-token", "expireIn": 7200})
	}))
	defer dingProtocol.Close()
	bridge.dingAPI.BaseURL, bridge.dingAPI.Client = dingProtocol.URL, dingProtocol.Client()
	recorder = httptest.NewRecorder()
	server.handleDingTalk(recorder, httptest.NewRequest(http.MethodPut, "/integrations/dingtalk", strings.NewReader(`{"mode":"stream","client_id":"client","client_secret":"ding-secret","robot_code":"robot","enabled":false}`)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "ding-secret") {
		t.Fatalf("DingTalk configure = %d %s", recorder.Code, recorder.Body.String())
	}

	aesKey := strings.TrimSuffix(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), "=")
	botConfig := fmt.Sprintf(`{"mode":"callback","bot_id":"bot","token":"callback-token","encoding_aes_key":"%s"}`, aesKey)
	recorder = httptest.NewRecorder()
	server.handleWeComBot(recorder, httptest.NewRequest(http.MethodPut, "/integrations/wecom-bot", strings.NewReader(botConfig)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "callback-token") {
		t.Fatalf("WeCom Bot configure = %d %s", recorder.Code, recorder.Body.String())
	}
	botCrypto, _ := cloudchannels.NewWeComCrypto("callback-token", aesKey, "")
	encryptedEcho, _ := botCrypto.Encrypt([]byte("verified"))
	query := "?msg_signature=" + url.QueryEscape(botCrypto.Signature("1", "nonce", encryptedEcho)) + "&timestamp=1&nonce=nonce&echostr=" + url.QueryEscape(encryptedEcho)
	recorder = httptest.NewRecorder()
	server.handleWeComBotCallback(recorder, httptest.NewRequest(http.MethodGet, "/integrations/wecom-bot/callback"+query, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "verified" {
		t.Fatalf("WeCom Bot verify = %d %s", recorder.Code, recorder.Body.String())
	}

	wecomProtocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "wecom-token", "expires_in": 7200})
	}))
	defer wecomProtocol.Close()
	bridge.wecomAPI.BaseURL, bridge.wecomAPI.Client = wecomProtocol.URL, wecomProtocol.Client()
	appConfig := fmt.Sprintf(`{"corp_id":"corp","agent_id":9,"secret":"app-secret","token":"callback-token","encoding_aes_key":"%s"}`, aesKey)
	recorder = httptest.NewRecorder()
	server.handleWeComApp(recorder, httptest.NewRequest(http.MethodPut, "/integrations/wecom-app", strings.NewReader(appConfig)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "app-secret") {
		t.Fatalf("WeCom App configure = %d %s", recorder.Code, recorder.Body.String())
	}
	appCrypto, _ := cloudchannels.NewWeComCrypto("callback-token", aesKey, "corp")
	encryptedEcho, _ = appCrypto.Encrypt([]byte("verified-app"))
	query = "?msg_signature=" + url.QueryEscape(appCrypto.Signature("2", "nonce", encryptedEcho)) + "&timestamp=2&nonce=nonce&echostr=" + url.QueryEscape(encryptedEcho)
	recorder = httptest.NewRecorder()
	server.handleWeComAppCallback(recorder, httptest.NewRequest(http.MethodGet, "/integrations/wecom-app/callback"+query, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "verified-app" {
		t.Fatalf("WeCom App verify = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestQQAndWeChatFamilyConfigurationAndVerifiedCallbacks(t *testing.T) {
	server, bridge := newCloudRouteServer(t)
	protocol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "qq-token", "expires_in": 7200})
		case "/gateway":
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "ws://127.0.0.1:9/gateway"})
		case "/cgi-bin/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mp-token", "expires_in": 7200})
		case "/cgi-bin/gettoken":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "kf-token", "expires_in": 7200})
		case "/cgi-bin/kf/sync_msg":
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "next_cursor": "cursor-1", "has_more": 0, "msg_list": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer protocol.Close()
	bridge.qqAPI.APIBase, bridge.qqAPI.AuthBase, bridge.qqAPI.Client = protocol.URL, protocol.URL, protocol.Client()
	bridge.wechatMP.BaseURL, bridge.wechatMP.Client = protocol.URL, protocol.Client()
	bridge.wechatKF.BaseURL, bridge.wechatKF.Client = protocol.URL, protocol.Client()
	recorder := httptest.NewRecorder()
	server.handleQQ(recorder, httptest.NewRequest(http.MethodPut, "/integrations/qq", strings.NewReader(`{"app_id":"qq-app","client_secret":"qq-secret","enabled":false}`)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "qq-secret") {
		t.Fatalf("QQ configure=%d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.handleWeixin(recorder, httptest.NewRequest(http.MethodPut, "/integrations/weixin", strings.NewReader(`{"token":"weixin-token","bot_id":"weixin-bot","enabled":false}`)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "weixin-token") {
		t.Fatalf("Weixin configure=%d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.handleWeChatMP(recorder, httptest.NewRequest(http.MethodPut, "/integrations/wechat-official", strings.NewReader(`{"app_id":"mp-app","app_secret":"mp-secret","token":"mp-callback","enabled":true}`)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "mp-secret") {
		t.Fatalf("MP configure=%d %s", recorder.Code, recorder.Body.String())
	}
	timestamp, nonce := "100", "nonce"
	signature := cloudchannels.CallbackSignature("mp-callback", timestamp, nonce)
	recorder = httptest.NewRecorder()
	server.handleWeChatMPCallback(recorder, httptest.NewRequest(http.MethodGet, "/integrations/wechat-official/callback?signature="+signature+"&timestamp="+timestamp+"&nonce="+nonce+"&echostr=verified", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "verified" {
		t.Fatalf("MP verify=%d %s", recorder.Code, recorder.Body.String())
	}
	body := `<xml><ToUserName>gh_account</ToUserName><FromUserName>external</FromUserName><CreateTime>1700000000</CreateTime><MsgType>text</MsgType><Content>hello</Content><MsgId>mp-message</MsgId></xml>`
	recorder = httptest.NewRecorder()
	server.handleWeChatMPCallback(recorder, httptest.NewRequest(http.MethodPost, "/integrations/wechat-official/callback?signature="+signature+"&timestamp="+timestamp+"&nonce="+nonce, strings.NewReader(body)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "success" {
		t.Fatalf("MP callback=%d %s", recorder.Code, recorder.Body.String())
	}
	aesKey := strings.TrimSuffix(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "=")
	kfBody := fmt.Sprintf(`{"corp_id":"corp","secret":"kf-secret","token":"kf-callback","encoding_aes_key":"%s","enabled":true}`, aesKey)
	recorder = httptest.NewRecorder()
	server.handleWeChatKF(recorder, httptest.NewRequest(http.MethodPut, "/integrations/wechat-customer-service", strings.NewReader(kfBody)))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "kf-secret") {
		t.Fatalf("KF configure=%d %s", recorder.Code, recorder.Body.String())
	}
	crypt, _ := cloudchannels.NewWeComCrypto("kf-callback", aesKey, "corp")
	encryptedEcho, _ := crypt.Encrypt([]byte("verified-kf"))
	query := "?msg_signature=" + url.QueryEscape(crypt.Signature("101", "nonce", encryptedEcho)) + "&timestamp=101&nonce=nonce&echostr=" + url.QueryEscape(encryptedEcho)
	recorder = httptest.NewRecorder()
	server.handleWeChatKFCallback(recorder, httptest.NewRequest(http.MethodGet, "/integrations/wechat-customer-service/callback"+query, nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "verified-kf" {
		t.Fatalf("KF verify=%d %s", recorder.Code, recorder.Body.String())
	}
	inner := []byte(`<xml><ToUserName>corp</ToUserName><CreateTime>1700000000</CreateTime><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>event-token</Token><OpenKfId>open-kf</OpenKfId></xml>`)
	encrypted, _ := crypt.Encrypt(inner)
	wrapper := fmt.Sprintf(`<xml><Encrypt><![CDATA[%s]]></Encrypt></xml>`, encrypted)
	callbackQuery := "?msg_signature=" + url.QueryEscape(crypt.Signature("102", "nonce", encrypted)) + "&timestamp=102&nonce=nonce"
	recorder = httptest.NewRecorder()
	server.handleWeChatKFCallback(recorder, httptest.NewRequest(http.MethodPost, "/integrations/wechat-customer-service/callback"+callbackQuery, strings.NewReader(wrapper)))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "success" {
		t.Fatalf("KF callback=%d %s", recorder.Code, recorder.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && bridge.store.View().WeChatKF.Cursors["open-kf"] != "cursor-1" {
		time.Sleep(10 * time.Millisecond)
	}
	if bridge.store.View().WeChatKF.Cursors["open-kf"] != "cursor-1" {
		t.Fatal("KF cursor was not persisted after successful sync")
	}
}
