// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/channelgateway"
)

type LarkAPI struct {
	Client   *http.Client
	BaseURL  string
	mu       sync.Mutex
	token    string
	tokenFor string
	expires  time.Time
}

func NewLarkAPI() *LarkAPI { return &LarkAPI{Client: &http.Client{Timeout: 30 * time.Second}} }

func LarkBaseURL(region string) string {
	if strings.EqualFold(region, "feishu") {
		return "https://open.feishu.cn/open-apis"
	}
	return "https://open.larksuite.com/open-apis"
}

func (a *LarkAPI) base(region string) string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return LarkBaseURL(region)
}

func (a *LarkAPI) AccessToken(ctx context.Context, config LarkConfig) (string, error) {
	key := config.Region + "\x00" + config.AppID
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"app_id": config.AppID, "app_secret": config.AppSecret})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base(config.Region)+"/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || result.Code != 0 || result.Token == "" {
		return "", fmt.Errorf("Lark token exchange failed: %s", result.Msg)
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.Token, key, time.Now().Add(time.Duration(result.Expire)*time.Second)
	a.mu.Unlock()
	return result.Token, nil
}

func (a *LarkAPI) BotOpenID(ctx context.Context, config LarkConfig) (string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base(config.Region)+"/bot/v3/info/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Code != 0 || result.Bot.OpenID == "" {
		return "", fmt.Errorf("Lark bot identity failed: %s", result.Msg)
	}
	return result.Bot.OpenID, nil
}

func (a *LarkAPI) PostText(ctx context.Context, config LarkConfig, receiveID, receiveType, text string) (channelgateway.SendReceipt, error) {
	return a.postMessage(ctx, config, receiveID, receiveType, "text", jsonString(map[string]string{"text": text}))
}

func (a *LarkAPI) PostCard(ctx context.Context, config LarkConfig, receiveID, receiveType, card string) (channelgateway.SendReceipt, error) {
	if !json.Valid([]byte(card)) {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "card", Message: "Lark card JSON is invalid", Permanent: true}
	}
	return a.postMessage(ctx, config, receiveID, receiveType, "interactive", card)
}

func (a *LarkAPI) PostMedia(ctx context.Context, config LarkConfig, receiveID, receiveType, name string, data []byte, kind channelgateway.MediaKind) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	msgType, field, endpoint := "file", "file_key", a.base(config.Region)+"/im/v1/files"
	if kind == channelgateway.MediaImage {
		msgType, field, endpoint = "image", "image_key", a.base(config.Region)+"/im/v1/images"
		_ = writer.WriteField("image_type", "message")
	} else {
		_ = writer.WriteField("file_type", larkFileType(name, kind))
		_ = writer.WriteField("file_name", filepath.Base(name))
	}
	part, err := writer.CreateFormFile("file", filepath.Base(name))
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if _, err := part.Write(data); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	_ = writer.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	defer response.Body.Close()
	var result struct {
		Code int               `json:"code"`
		Msg  string            `json:"msg"`
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	key := result.Data[field]
	if response.StatusCode != http.StatusOK || result.Code != 0 || key == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: fmt.Sprint(result.Code), Message: "Lark media upload failed: " + result.Msg, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	return a.postMessage(ctx, config, receiveID, receiveType, msgType, jsonString(map[string]string{field: key}))
}

func (a *LarkAPI) postMessage(ctx context.Context, config LarkConfig, receiveID, receiveType, msgType, content string) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	body, _ := json.Marshal(map[string]string{"receive_id": receiveID, "msg_type": msgType, "content": content})
	endpoint := a.base(config.Region) + "/im/v1/messages?receive_id_type=" + url.QueryEscape(receiveType)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "Lark rate limit", RetryAfter: time.Duration(retry) * time.Second}
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || result.Code != 0 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: fmt.Sprint(result.Code), Message: "Lark delivery failed: " + result.Msg, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	return channelgateway.SendReceipt{ExternalMessageID: result.Data.MessageID, Code: "accepted"}, nil
}

func (a *LarkAPI) DownloadResource(ctx context.Context, config LarkConfig, messageID, resourceKey, resourceType string) (string, []byte, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", nil, err
	}
	endpoint := a.base(config.Region) + "/im/v1/messages/" + url.PathEscape(messageID) + "/resources/" + url.PathEscape(resourceKey) + "?type=" + url.QueryEscape(resourceType)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.client().Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("Lark resource download failed with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (25<<20)+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > 25<<20 {
		return "", nil, errors.New("Lark resource exceeds 25 MiB")
	}
	name := resourceKey
	if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = filepath.Base(params["filename"])
	}
	return name, data, nil
}

func larkFileType(name string, kind channelgateway.MediaKind) string {
	if kind == channelgateway.MediaAudio {
		return "opus"
	}
	if kind == channelgateway.MediaVideo {
		return "mp4"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf":
		return "pdf"
	case ".doc", ".docx":
		return "doc"
	case ".xls", ".xlsx":
		return "xls"
	case ".ppt", ".pptx":
		return "ppt"
	default:
		return "stream"
	}
}

func (a *LarkAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

type LarkEventPayload struct {
	Schema    string `json:"schema,omitempty"`
	Type      string `json:"type,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`
	Encrypt   string `json:"encrypt,omitempty"`
	Header    struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		CreateTime string `json:"create_time"`
		Token      string `json:"token"`
		AppID      string `json:"app_id"`
		TenantKey  string `json:"tenant_key"`
	} `json:"header"`
	Event LarkMessageEvent `json:"event"`
}

type LarkMessageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		RootID      string `json:"root_id"`
		ParentID    string `json:"parent_id"`
		CreateTime  string `json:"create_time"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			ID struct {
				OpenID string `json:"open_id"`
			} `json:"id"`
			Key string `json:"key"`
		} `json:"mentions"`
	} `json:"message"`
}

func DecodeLarkPayload(body []byte, encryptKey string) (LarkEventPayload, error) {
	var payload LarkEventPayload
	plain, err := decodeLarkBody(body, encryptKey)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func decodeLarkBody(body []byte, encryptKey string) ([]byte, error) {
	var wrapper struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Encrypt == "" {
		return body, nil
	}
	if encryptKey == "" {
		return nil, errors.New("Lark encrypted callback requires encrypt_key")
	}
	key := sha256.Sum256([]byte(encryptKey))
	encrypted, err := base64.StdEncoding.DecodeString(wrapper.Encrypt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil || len(encrypted)%aes.BlockSize != 0 {
		return nil, errors.New("Lark encrypted callback is invalid")
	}
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, encrypted)
	if len(plain) == 0 {
		return nil, errors.New("Lark encrypted callback is empty")
	}
	pad := int(plain[len(plain)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("Lark encrypted callback padding is invalid")
	}
	plain = plain[:len(plain)-pad]
	return plain, nil
}

type LarkCardActionPayload struct {
	Schema string `json:"schema"`
	Header struct {
		EventID    string `json:"event_id"`
		AppID      string `json:"app_id"`
		TenantKey  string `json:"tenant_key"`
		Token      string `json:"token"`
		CreateTime string `json:"create_time"`
	} `json:"header"`
	Event struct {
		Operator struct {
			OpenID    string  `json:"open_id"`
			TenantKey *string `json:"tenant_key,omitempty"`
		} `json:"operator"`
		Action struct {
			Value map[string]any `json:"value"`
		} `json:"action"`
		Context struct {
			OpenMessageID string `json:"open_message_id"`
			OpenChatID    string `json:"open_chat_id"`
		} `json:"context"`
	} `json:"event"`
}

func DecodeLarkCardAction(body []byte, encryptKey string) (LarkCardActionPayload, error) {
	var payload LarkCardActionPayload
	plain, err := decodeLarkBody(body, encryptKey)
	if err != nil {
		return payload, err
	}
	err = json.Unmarshal(plain, &payload)
	return payload, err
}

func NormalizeLarkMessage(config LarkConfig, payload LarkEventPayload) (channelgateway.Envelope, bool, error) {
	if payload.Header.EventType != "im.message.receive_v1" || payload.Header.AppID != config.AppID || payload.Header.Token != config.VerificationToken {
		return channelgateway.Envelope{}, false, nil
	}
	msg := payload.Event.Message
	sender := payload.Event.Sender.SenderID.OpenID
	if msg.MessageID == "" || msg.ChatID == "" || sender == "" || payload.Event.Sender.SenderType != "user" {
		return channelgateway.Envelope{}, false, nil
	}
	group := msg.ChatType == "group"
	if !group && msg.ChatType != "p2p" {
		return channelgateway.Envelope{}, false, nil
	}
	if group && config.GroupTrigger != "all" {
		mentioned := false
		for _, m := range msg.Mentions {
			if m.ID.OpenID == config.BotOpenID {
				mentioned = true
				break
			}
		}
		if !mentioned {
			return channelgateway.Envelope{}, false, nil
		}
	}
	text := ""
	var media []channelgateway.Media
	var content map[string]any
	if err := json.Unmarshal([]byte(msg.Content), &content); err != nil {
		return channelgateway.Envelope{}, false, err
	}
	switch msg.MessageType {
	case "text":
		text, _ = content["text"].(string)
		for _, m := range msg.Mentions {
			text = strings.ReplaceAll(text, m.Key, "")
		}
	case "image":
		if key, _ := content["image_key"].(string); key != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaImage, Ref: "lark-resource:" + msg.MessageID + ":" + key})
		}
	case "file", "audio", "media":
		key, _ := content["file_key"].(string)
		if key != "" {
			kind := channelgateway.MediaFile
			if msg.MessageType == "audio" {
				kind = channelgateway.MediaAudio
			} else if msg.MessageType == "media" {
				kind = channelgateway.MediaVideo
			}
			media = append(media, channelgateway.Media{Kind: kind, Ref: "lark-resource:" + msg.MessageID + ":" + key})
		}
	default:
		return channelgateway.Envelope{}, false, nil
	}
	scope := channelgateway.ScopeDirect
	if group {
		scope = channelgateway.ScopeGroup
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(strings.TrimSpace(text), "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if millis, err := strconv.ParseInt(msg.CreateTime, 10, 64); err == nil {
		occurred = time.UnixMilli(millis).UTC()
	}
	envelope := channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: kind, Address: channelgateway.Address{Channel: channelgateway.ChannelLark, AccountID: payload.Header.TenantKey, ConversationID: msg.ChatID, ParticipantID: sender, Scope: scope}, ExternalEventID: payload.Header.EventID, ExternalMessageID: msg.MessageID, IdempotencyKey: "lark:" + payload.Header.EventID, Text: strings.TrimSpace(text), Media: media, OccurredAt: occurred, Metadata: map[string]string{"receive_id": msg.ChatID, "receive_id_type": "chat_id"}}
	return envelope, true, nil
}
