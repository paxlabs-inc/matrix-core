// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/channelgateway"
)

type DingTalkAPI struct {
	Client   *http.Client
	BaseURL  string
	mu       sync.Mutex
	token    string
	tokenFor string
	expires  time.Time
}

func NewDingTalkAPI() *DingTalkAPI {
	return &DingTalkAPI{Client: &http.Client{Timeout: 30 * time.Second}}
}

func (a *DingTalkAPI) base() string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://api.dingtalk.com"
}

func (a *DingTalkAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a *DingTalkAPI) AccessToken(ctx context.Context, config DingTalkConfig) (string, error) {
	key := config.ClientID
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"appKey": config.ClientID, "appSecret": config.ClientSecret})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/v1.0/oauth2/accessToken", bytes.NewReader(body))
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
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
		Code        string `json:"code"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || result.AccessToken == "" {
		return "", fmt.Errorf("DingTalk token exchange failed: %s %s", result.Code, result.Message)
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.AccessToken, key, time.Now().Add(time.Duration(result.ExpireIn)*time.Second)
	a.mu.Unlock()
	return result.AccessToken, nil
}

func (a *DingTalkAPI) PostText(ctx context.Context, config DingTalkConfig, conversationID, participantID string, group bool, text string) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	payload := map[string]any{
		"msgParam":  jsonString(map[string]string{"content": text}),
		"msgKey":    "sampleText",
		"robotCode": config.RobotCode,
	}
	endpoint := a.base() + "/v1.0/robot/oToMessages/batchSend"
	if group {
		endpoint = a.base() + "/v1.0/robot/groupMessages/send"
		payload["openConversationId"] = conversationID
	} else {
		payload["userIds"] = []string{participantID}
	}
	body, _ := json.Marshal(payload)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	request.Header.Set("x-acs-dingtalk-access-token", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "DingTalk rate limit", RetryAfter: time.Duration(retry) * time.Second}
	}
	var result struct {
		ProcessQueryKey string `json:"processQueryKey"`
		Code            string `json:"code"`
		Message         string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
	if response.StatusCode < 200 || response.StatusCode >= 300 || result.Code != "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: result.Code, Message: "DingTalk delivery failed: " + result.Message, Permanent: response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests}
	}
	return channelgateway.SendReceipt{ExternalMessageID: result.ProcessQueryKey, Code: "accepted"}, nil
}

func (a *DingTalkAPI) DownloadURL(ctx context.Context, config DingTalkConfig, downloadCode string) (string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"downloadCode": downloadCode, "robotCode": config.RobotCode})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/v1.0/robot/messageFiles/download", bytes.NewReader(body))
	request.Header.Set("x-acs-dingtalk-access-token", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		DownloadURL string `json:"downloadUrl"`
		Code        string `json:"code"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || result.DownloadURL == "" {
		return "", fmt.Errorf("DingTalk media lookup failed: %s %s", result.Code, result.Message)
	}
	return result.DownloadURL, nil
}

func (a *DingTalkAPI) Download(ctx context.Context, config DingTalkConfig, downloadCode string, maxBytes int64) ([]byte, string, error) {
	raw, err := a.DownloadURL(ctx, config, downloadCode)
	if err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, "", errors.New("DingTalk media URL is invalid")
	}
	trusted := strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".dingtalk.com") || strings.EqualFold(parsed.Hostname(), "dingtalk.com") || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".aliyuncs.com")
	if a.BaseURL != "" {
		if base, baseErr := url.Parse(a.BaseURL); baseErr == nil && strings.EqualFold(base.Host, parsed.Host) {
			trusted = true
		}
	}
	if !trusted {
		return nil, "", errors.New("DingTalk media URL host is not trusted")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	response, err := a.client().Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("DingTalk media download failed with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", errors.New("DingTalk media exceeds the configured size limit")
	}
	return data, response.Header.Get("Content-Type"), nil
}

type DingTalkMessage struct {
	ConversationID            string `json:"conversationId"`
	ChatbotCorpID             string `json:"chatbotCorpId"`
	ChatbotUserID             string `json:"chatbotUserId"`
	MsgID                     string `json:"msgId"`
	SenderStaffID             string `json:"senderStaffId"`
	SenderID                  string `json:"senderId"`
	CreateAt                  int64  `json:"createAt"`
	ConversationType          string `json:"conversationType"`
	IsInAtList                bool   `json:"isInAtList"`
	SessionWebhook            string `json:"sessionWebhook,omitempty"`
	SessionWebhookExpiredTime int64  `json:"sessionWebhookExpiredTime,omitempty"`
	MsgType                   string `json:"msgtype"`
	Text                      struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content,omitempty"`
}

func NormalizeDingTalkMessage(config DingTalkConfig, message DingTalkMessage) (channelgateway.Envelope, bool, error) {
	if message.MsgID == "" || message.ConversationID == "" || message.ChatbotCorpID == "" || message.ChatbotCorpID == message.SenderID {
		return channelgateway.Envelope{}, false, nil
	}
	participant := firstString(message.SenderStaffID, message.SenderID)
	if participant == "" {
		return channelgateway.Envelope{}, false, nil
	}
	group := message.ConversationType != "1"
	if group && config.GroupTrigger != "all" && !message.IsInAtList {
		return channelgateway.Envelope{}, false, nil
	}
	text := strings.TrimSpace(message.Text.Content)
	var media []channelgateway.Media
	if message.MsgType != "" && message.MsgType != "text" {
		var content map[string]any
		_ = json.Unmarshal(message.Content, &content)
		downloadCode, _ := content["downloadCode"].(string)
		if downloadCode != "" {
			kind := channelgateway.MediaFile
			switch message.MsgType {
			case "picture", "image", "richText":
				kind = channelgateway.MediaImage
			case "audio", "voice":
				kind = channelgateway.MediaAudio
			case "video":
				kind = channelgateway.MediaVideo
			}
			media = append(media, channelgateway.Media{Kind: kind, Ref: "dingtalk-resource:" + downloadCode})
		}
	}
	if text == "" && len(media) == 0 {
		return channelgateway.Envelope{}, false, nil
	}
	scope := channelgateway.ScopeDirect
	if group {
		scope = channelgateway.ScopeGroup
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(text, "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if message.CreateAt > 0 {
		occurred = time.UnixMilli(message.CreateAt).UTC()
	}
	metadata := map[string]string{"channel": message.ConversationID, "robot_code": config.RobotCode}
	return channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: kind,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelDingTalk, AccountID: message.ChatbotCorpID, ConversationID: message.ConversationID, ParticipantID: participant, Scope: scope},
		ExternalEventID: message.MsgID, ExternalMessageID: message.MsgID, IdempotencyKey: "dingtalk:" + message.MsgID,
		Text: text, Media: media, OccurredAt: occurred, Metadata: metadata,
	}, true, nil
}

func jsonString(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
