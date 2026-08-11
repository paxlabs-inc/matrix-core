// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"matrix/neo/internal/channelgateway"
)

func TestLarkProtocolEncryptionNormalizationAndRealAPI(t *testing.T) {
	var delivered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/v3/tenant_access_token/internal":
			writeTestJSON(w, map[string]any{"code": 0, "tenant_access_token": "tenant-token", "expire": 7200})
		case "/bot/v3/info/":
			if r.Header.Get("Authorization") != "Bearer tenant-token" {
				t.Errorf("missing Lark token")
			}
			writeTestJSON(w, map[string]any{"code": 0, "bot": map[string]string{"open_id": "bot-open"}})
		case "/im/v1/messages":
			delivered.Add(1)
			writeTestJSON(w, map[string]any{"code": 0, "data": map[string]string{"message_id": "sent-1"}})
		case "/im/v1/messages/message-1/resources/image-1":
			w.Header().Set("Content-Disposition", `attachment; filename="source.png"`)
			_, _ = w.Write([]byte("real-image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := LarkConfig{Enabled: true, Region: "lark", Mode: "webhook", AppID: "app-1", AppSecret: "secret-1", VerificationToken: "verify-token", EncryptKey: "encrypt-key", BotOpenID: "bot-open", GroupTrigger: "mention"}
	api := NewLarkAPI()
	api.BaseURL = server.URL
	if botID, err := api.BotOpenID(context.Background(), config); err != nil || botID != "bot-open" {
		t.Fatalf("bot identity: %q %v", botID, err)
	}
	if _, err := api.PostText(context.Background(), config, "chat-1", "chat_id", "hello"); err != nil {
		t.Fatalf("post text: %v", err)
	}
	name, data, err := api.DownloadResource(context.Background(), config, "message-1", "image-1", "image")
	if err != nil || name != "source.png" || string(data) != "real-image" {
		t.Fatalf("resource: %q %q %v", name, data, err)
	}
	if delivered.Load() != 1 {
		t.Fatalf("deliveries=%d", delivered.Load())
	}

	payload := testLarkPayload(config, "event-1")
	plain, _ := json.Marshal(payload)
	encrypted := encryptLarkTestPayload(t, config.EncryptKey, plain)
	wrapper, _ := json.Marshal(map[string]string{"encrypt": encrypted})
	decoded, err := DecodeLarkPayload(wrapper, config.EncryptKey)
	if err != nil {
		t.Fatal(err)
	}
	envelope, ok, err := NormalizeLarkMessage(config, decoded)
	if err != nil || !ok {
		t.Fatalf("normalize: ok=%v err=%v", ok, err)
	}
	if envelope.Address.Channel != channelgateway.ChannelLark || envelope.Text != "hello" || envelope.IdempotencyKey != "lark:event-1" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
}

func TestDingTalkProtocolNormalizationAndRealAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			writeTestJSON(w, map[string]any{"accessToken": "ding-token", "expireIn": 7200})
		case "/v1.0/robot/groupMessages/send":
			if r.Header.Get("x-acs-dingtalk-access-token") != "ding-token" {
				t.Errorf("missing DingTalk token")
			}
			writeTestJSON(w, map[string]any{})
		case "/v1.0/robot/oToMessages/batchSend":
			writeTestJSON(w, map[string]string{"processQueryKey": "query-1"})
		case "/v1.0/robot/messageFiles/download":
			writeTestJSON(w, map[string]string{"downloadUrl": "https://download.dingtalk.com/file"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	config := DingTalkConfig{Enabled: true, Mode: "stream", ClientID: "client", ClientSecret: "secret", RobotCode: "robot", GroupTrigger: "mention"}
	api := NewDingTalkAPI()
	api.BaseURL = server.URL
	if _, err := api.PostText(context.Background(), config, "conversation", "user", true, "hello"); err != nil {
		t.Fatal(err)
	}
	if receipt, err := api.PostText(context.Background(), config, "conversation", "user", false, "hello"); err != nil || receipt.ExternalMessageID != "query-1" {
		t.Fatalf("direct: %+v %v", receipt, err)
	}
	if raw, err := api.DownloadURL(context.Background(), config, "download-code"); err != nil || raw != "https://download.dingtalk.com/file" {
		t.Fatalf("download: %q %v", raw, err)
	}
	var message DingTalkMessage
	_ = json.Unmarshal([]byte(`{"conversationId":"conversation","chatbotCorpId":"corp","chatbotUserId":"bot","msgId":"message","senderStaffId":"user","senderId":"sender","createAt":1700000000000,"conversationType":"2","isInAtList":true,"msgtype":"text","text":{"content":"hello"}}`), &message)
	envelope, ok, err := NormalizeDingTalkMessage(config, message)
	if err != nil || !ok || envelope.Address.Scope != channelgateway.ScopeGroup || envelope.Text != "hello" {
		t.Fatalf("normalize: %+v %v %v", envelope, ok, err)
	}
}

func TestWeComCryptoNormalizationAndRealAppAPI(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	key = strings.TrimSuffix(key, "=")
	crypt, err := NewWeComCrypto("callback-token", key, "corp-1")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`<xml><ToUserName>corp-1</ToUserName><FromUserName>user-1</FromUserName><CreateTime>1700000000</CreateTime><MsgType>text</MsgType><Content>hello</Content><MsgId>msg-1</MsgId></xml>`)
	encrypted, err := crypt.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	timestamp, nonce := "1700000000", "nonce"
	signature := crypt.Signature(timestamp, nonce, encrypted)
	if err := crypt.Verify(signature, timestamp, nonce, encrypted); err != nil {
		t.Fatal(err)
	}
	decrypted, err := crypt.Decrypt(encrypted)
	if err != nil || !bytes.Equal(decrypted, plain) {
		t.Fatalf("decrypt: %q %v", decrypted, err)
	}
	var appMessage WeComAppMessage
	if err := xml.Unmarshal(decrypted, &appMessage); err != nil {
		t.Fatal(err)
	}
	appConfig := WeComAppConfig{Enabled: true, CorpID: "corp-1", AgentID: 9, Secret: "secret", Token: "callback-token", EncodingAESKey: key}
	envelope, ok := NormalizeWeComAppMessage(appConfig, appMessage)
	if !ok || envelope.Text != "hello" {
		t.Fatalf("app normalize: %+v %v", envelope, ok)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			writeTestJSON(w, map[string]any{"errcode": 0, "access_token": "wecom-token", "expires_in": 7200})
		case "/cgi-bin/message/send":
			writeTestJSON(w, map[string]any{"errcode": 0, "msgid": "sent-1"})
		case "/cgi-bin/media/upload":
			writeTestJSON(w, map[string]any{"errcode": 0, "media_id": "media-1"})
		case "/cgi-bin/media/get":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("image-data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := NewWeComAppAPI()
	api.BaseURL = server.URL
	if receipt, err := api.PostText(context.Background(), appConfig, "user-1", "reply"); err != nil || receipt.ExternalMessageID != "sent-1" {
		t.Fatalf("send: %+v %v", receipt, err)
	}
	if _, err := api.PostMedia(context.Background(), appConfig, "user-1", "image.png", []byte("image-data"), channelgateway.MediaImage); err != nil {
		t.Fatal(err)
	}
	if data, err := api.DownloadMedia(context.Background(), appConfig, "media-1"); err != nil || string(data) != "image-data" {
		t.Fatalf("media: %q %v", data, err)
	}

	botConfig := WeComBotConfig{Enabled: true, Mode: "callback", BotID: "bot-1", Token: "callback-token", EncodingAESKey: key, GroupTrigger: "mention"}
	var botMessage WeComBotMessage
	_ = json.Unmarshal([]byte(`{"msgid":"bot-msg","msgtype":"text","create_time":1700000000,"chattype":"single","aibotid":"bot-1","from":{"userid":"user-1"},"text":{"content":"hello"}}`), &botMessage)
	botEnvelope, botOK, botErr := NormalizeWeComBotMessage(botConfig, botMessage)
	if botErr != nil || !botOK || botEnvelope.Address.Channel != channelgateway.ChannelWeComBot {
		t.Fatalf("bot normalize: %+v %v %v", botEnvelope, botOK, botErr)
	}
}

func TestWeComBotEncryptedMediaDownloadUsesTrustedBoundedTransport(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	plain := []byte("real encrypted media")
	padded := padPKCS7(append([]byte(nil), plain...), 32)
	block, _ := aes.NewCipher(key)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(encrypted, padded)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(encrypted)
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	keyText := strings.TrimSuffix(base64.StdEncoding.EncodeToString(key), "=")
	data, contentType, err := DownloadWeComBotMedia(context.Background(), server.Client(), "wecom-media:"+keyText+":"+server.URL+"/media", "", 1024, parsed.Host)
	if err != nil || !bytes.Equal(data, plain) || contentType != "application/octet-stream" {
		t.Fatalf("download: %q %q %v", data, contentType, err)
	}
}

func TestLarkAndWeComSocketsReconnectAfterProtocolDisconnect(t *testing.T) {
	var larkConnections atomic.Int32
	var larkHandled atomic.Int32
	upgrader := websocket.Upgrader{}
	var larkServer *httptest.Server
	larkServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == larkws.GenEndpointUri {
			writeTestJSON(w, larkws.EndpointResp{Code: 0, Data: &larkws.Endpoint{Url: "ws" + strings.TrimPrefix(larkServer.URL, "http") + "/socket?service_id=1"}})
			return
		}
		if r.URL.Path != "/socket" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		larkConnections.Add(1)
		payload, _ := json.Marshal(testLarkPayload(LarkConfig{AppID: "app", VerificationToken: "verify-token", BotOpenID: "bot"}, "socket-event"))
		headers := larkws.Headers{}
		headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
		headers.Add(larkws.HeaderMessageID, "socket-event")
		headers.Add(larkws.HeaderSum, "1")
		headers.Add(larkws.HeaderSeq, "0")
		frame := larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: headers, Payload: payload}
		encoded, _ := frame.Marshal()
		_ = conn.WriteMessage(websocket.BinaryMessage, encoded)
		_, _, _ = conn.ReadMessage()
	}))
	defer larkServer.Close()
	larkConfig := LarkConfig{Enabled: true, Region: "lark", Mode: "websocket", AppID: "app", AppSecret: "secret", VerificationToken: "verify-token", BotOpenID: "bot", GroupTrigger: "all"}
	larkSocket := NewLarkSocket(func() LarkConfig { return larkConfig }, func(context.Context, LarkEventPayload) error { larkHandled.Add(1); return nil })
	larkSocket.Domain = larkServer.URL
	larkSocket.Start(context.Background())
	defer larkSocket.Stop()
	waitForTest(t, 5*time.Second, func() bool { return larkConnections.Load() >= 2 && larkHandled.Load() >= 2 })

	var wecomConnections atomic.Int32
	var wecomHandled atomic.Int32
	wecomServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		wecomConnections.Add(1)
		var subscribe map[string]any
		if conn.ReadJSON(&subscribe) != nil {
			return
		}
		zero := 0
		_ = conn.WriteJSON(map[string]any{"errcode": zero, "errmsg": "ok", "headers": map[string]string{"req_id": "subscribe"}})
		body := map[string]any{"msgid": "message", "msgtype": "text", "create_time": time.Now().Unix(), "chattype": "single", "aibotid": "bot", "from": map[string]string{"userid": "user"}, "text": map[string]string{"content": "hello"}}
		_ = conn.WriteJSON(map[string]any{"cmd": "aibot_msg_callback", "headers": map[string]string{"req_id": "request"}, "body": body})
		time.Sleep(50 * time.Millisecond)
	}))
	defer wecomServer.Close()
	wecomConfig := WeComBotConfig{Enabled: true, Mode: "websocket", BotID: "bot", Secret: "secret", GroupTrigger: "all"}
	wecomSocket := NewWeComBotSocket(func() WeComBotConfig { return wecomConfig }, func(context.Context, WeComBotMessage) error { wecomHandled.Add(1); return nil })
	wecomSocket.URL = "ws" + strings.TrimPrefix(wecomServer.URL, "http")
	wecomSocket.Start(context.Background())
	defer wecomSocket.Stop()
	waitForTest(t, 5*time.Second, func() bool { return wecomConnections.Load() >= 2 && wecomHandled.Load() >= 2 })
}

func TestWeComBotSocketUploadsNativeMedia(t *testing.T) {
	upgrader := websocket.Upgrader{}
	sent := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var subscribe map[string]any
		if conn.ReadJSON(&subscribe) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"errcode": 0, "headers": map[string]string{"req_id": "subscribe"}})
		for {
			var frame map[string]any
			if conn.ReadJSON(&frame) != nil {
				return
			}
			cmd, _ := frame["cmd"].(string)
			headers, _ := frame["headers"].(map[string]any)
			reqID, _ := headers["req_id"].(string)
			switch cmd {
			case "aibot_upload_media_init":
				_ = conn.WriteJSON(map[string]any{"errcode": 0, "headers": map[string]string{"req_id": reqID}, "body": map[string]string{"upload_id": "upload"}})
			case "aibot_upload_media_chunk":
				_ = conn.WriteJSON(map[string]any{"errcode": 0, "headers": map[string]string{"req_id": reqID}, "body": map[string]any{}})
			case "aibot_upload_media_finish":
				_ = conn.WriteJSON(map[string]any{"errcode": 0, "headers": map[string]string{"req_id": reqID}, "body": map[string]string{"media_id": "media"}})
			case "aibot_send_msg":
				sent <- frame
				return
			}
		}
	}))
	defer server.Close()
	config := WeComBotConfig{Enabled: true, Mode: "websocket", BotID: "bot", Secret: "secret", GroupTrigger: "all"}
	socket := NewWeComBotSocket(func() WeComBotConfig { return config }, nil)
	socket.URL = "ws" + strings.TrimPrefix(server.URL, "http")
	socket.Start(context.Background())
	defer socket.Stop()
	waitForTest(t, 3*time.Second, func() bool { return socket.Status().Connected })
	receipt, err := socket.SendMedia(context.Background(), "chat", true, "image.png", bytes.Repeat([]byte{1}, 600000), channelgateway.MediaImage)
	if err != nil || receipt.ExternalMessageID == "" {
		t.Fatalf("send media: %+v %v", receipt, err)
	}
	select {
	case frame := <-sent:
		body, _ := frame["body"].(map[string]any)
		if body["msgtype"] != "image" {
			t.Fatalf("message body=%v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("media send was not observed")
	}
}

func TestDingTalkStreamUsesRealWebSocketAndReconnects(t *testing.T) {
	var server *httptest.Server
	var connections atomic.Int32
	var handled atomic.Int32
	upgrader := websocket.Upgrader{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/gateway/connections/open" {
			writeTestJSON(w, map[string]string{"endpoint": "ws" + strings.TrimPrefix(server.URL, "http") + "/stream", "ticket": "ticket"})
			return
		}
		if r.URL.Path != "/stream" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		current := connections.Add(1)
		message := map[string]any{"conversationId": "conversation", "chatbotCorpId": "corp", "chatbotUserId": "bot", "msgId": "message-" + strconv.Itoa(int(current)), "senderStaffId": "user", "senderId": "sender", "createAt": time.Now().UnixMilli(), "conversationType": "1", "isInAtList": true, "msgtype": "text", "text": map[string]string{"content": "hello"}}
		encoded, _ := json.Marshal(message)
		frame := dingTalkFrame{SpecVersion: "1.0", Type: "CALLBACK", Time: time.Now().UnixMilli(), Headers: map[string]string{"topic": dingTalkBotTopic, "messageId": "frame", "contentType": "application/json"}, Data: string(encoded)}
		frameData, _ := json.Marshal(frame)
		_ = conn.WriteMessage(websocket.TextMessage, frameData)
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	config := DingTalkConfig{Enabled: true, Mode: "stream", ClientID: "client", ClientSecret: "secret", RobotCode: "robot", GroupTrigger: "all"}
	stream := NewDingTalkStream(func() DingTalkConfig { return config }, func(context.Context, DingTalkMessage) error { handled.Add(1); return nil })
	stream.OpenAPIHost = server.URL
	stream.Start(context.Background())
	defer stream.Stop()
	waitForTest(t, 8*time.Second, func() bool { return connections.Load() >= 2 && handled.Load() >= 2 })
}

func testLarkPayload(config LarkConfig, eventID string) LarkEventPayload {
	var payload LarkEventPayload
	payload.Schema = "2.0"
	payload.Header.EventID = eventID
	payload.Header.EventType = "im.message.receive_v1"
	payload.Header.AppID = config.AppID
	payload.Header.TenantKey = "tenant"
	payload.Header.Token = config.VerificationToken
	payload.Header.CreateTime = "1700000000000"
	payload.Event.Sender.SenderID.OpenID = "user"
	payload.Event.Sender.SenderType = "user"
	payload.Event.Message.MessageID = "message-1"
	payload.Event.Message.ChatID = "chat-1"
	payload.Event.Message.ChatType = "p2p"
	payload.Event.Message.MessageType = "text"
	payload.Event.Message.Content = `{"text":"hello"}`
	payload.Event.Message.CreateTime = "1700000000000"
	return payload
}
func encryptLarkTestPayload(t *testing.T, keyText string, plain []byte) string {
	t.Helper()
	key := sha256.Sum256([]byte(keyText))
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, _ := aes.NewCipher(key[:])
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out)
}
func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func waitForTest(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
