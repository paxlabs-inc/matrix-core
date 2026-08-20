// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"centra/agents/neo/internal/channelgateway"
)

const qqIntents = (1 << 25) | (1 << 30)

type QQAPI struct {
	Client   *http.Client
	APIBase  string
	AuthBase string
	mu       sync.Mutex
	token    string
	tokenFor string
	expires  time.Time
}

func NewQQAPI() *QQAPI { return &QQAPI{Client: &http.Client{Timeout: 30 * time.Second}} }

func (a *QQAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}
func (a *QQAPI) apiBase() string {
	if strings.TrimSpace(a.APIBase) != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return "https://api.sgroup.qq.com"
}
func (a *QQAPI) authBase() string {
	if strings.TrimSpace(a.AuthBase) != "" {
		return strings.TrimRight(a.AuthBase, "/")
	}
	return "https://bots.qq.com"
}

func (a *QQAPI) AccessToken(ctx context.Context, config QQConfig) (string, error) {
	key := config.AppID + "\x00" + config.ClientSecret
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"appId": config.AppID, "clientSecret": config.ClientSecret})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.authBase()+"/app/getAppAccessToken", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
		Code        any    `json:"code"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || result.AccessToken == "" {
		return "", fmt.Errorf("QQ token exchange failed with status %d: %s", resp.StatusCode, result.Message)
	}
	expires := 7200
	switch value := result.ExpiresIn.(type) {
	case float64:
		expires = int(value)
	case string:
		if parsed, err := strconv.Atoi(value); err == nil {
			expires = parsed
		}
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.AccessToken, key, time.Now().Add(time.Duration(expires)*time.Second)
	a.mu.Unlock()
	return result.AccessToken, nil
}

func (a *QQAPI) GatewayURL(ctx context.Context, config QQConfig) (string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.apiBase()+"/gateway", nil)
	req.Header.Set("Authorization", "QQBot "+token)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || result.URL == "" {
		return "", fmt.Errorf("QQ gateway discovery failed with status %d", resp.StatusCode)
	}
	return result.URL, nil
}

func (a *QQAPI) PostText(ctx context.Context, config QQConfig, address channelgateway.Address, eventType, replyTo, text string, sequence int) (channelgateway.SendReceipt, error) {
	endpoint, body, err := qqMessageRequest(a.apiBase(), address, eventType, replyTo, sequence)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "address", Message: err.Error(), Permanent: true}
	}
	body["content"], body["msg_type"] = text, 0
	return a.post(ctx, config, endpoint, body)
}

func (a *QQAPI) PostMedia(ctx context.Context, config QQConfig, address channelgateway.Address, eventType, replyTo, name string, data []byte, kind channelgateway.MediaKind, sequence int) (channelgateway.SendReceipt, error) {
	if eventType != "GROUP_AT_MESSAGE_CREATE" && eventType != "C2C_MESSAGE_CREATE" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media_scope", Message: "QQ binary media replies are supported only for group and C2C conversations", Permanent: true}
	}
	fileType := 4
	switch kind {
	case channelgateway.MediaImage:
		fileType = 1
	case channelgateway.MediaVideo:
		fileType = 2
	case channelgateway.MediaAudio:
		fileType = 3
	}
	target := address.ConversationID
	prefix := "/v2/users/"
	if eventType == "GROUP_AT_MESSAGE_CREATE" {
		prefix = "/v2/groups/"
	}
	upload := a.apiBase() + prefix + url.PathEscape(target) + "/files"
	receipt, payload, err := a.postJSON(ctx, config, upload, map[string]any{"file_type": fileType, "file_data": base64.StdEncoding.EncodeToString(data), "srv_send_msg": false})
	if err != nil {
		return receipt, err
	}
	fileInfo, _ := payload["file_info"].(string)
	if fileInfo == "" {
		return receipt, errors.New("QQ rich-media upload returned no file_info")
	}
	endpoint, body, err := qqMessageRequest(a.apiBase(), address, eventType, replyTo, sequence)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	body["msg_type"], body["media"] = 7, map[string]string{"file_info": fileInfo}
	_ = name
	return a.post(ctx, config, endpoint, body)
}

func qqMessageRequest(base string, address channelgateway.Address, eventType, replyTo string, sequence int) (string, map[string]any, error) {
	body := map[string]any{}
	if replyTo != "" {
		body["msg_id"] = replyTo
	}
	if sequence > 0 && (eventType == "GROUP_AT_MESSAGE_CREATE" || eventType == "C2C_MESSAGE_CREATE") {
		body["msg_seq"] = sequence
	}
	switch eventType {
	case "GROUP_AT_MESSAGE_CREATE":
		return base + "/v2/groups/" + url.PathEscape(address.ConversationID) + "/messages", body, nil
	case "C2C_MESSAGE_CREATE":
		return base + "/v2/users/" + url.PathEscape(address.ParticipantID) + "/messages", body, nil
	case "AT_MESSAGE_CREATE":
		return base + "/channels/" + url.PathEscape(address.ConversationID) + "/messages", body, nil
	case "DIRECT_MESSAGE_CREATE":
		return base + "/dms/" + url.PathEscape(address.ConversationID) + "/messages", body, nil
	default:
		if address.Scope == channelgateway.ScopeGroup {
			return base + "/v2/groups/" + url.PathEscape(address.ConversationID) + "/messages", body, nil
		}
		return base + "/v2/users/" + url.PathEscape(address.ParticipantID) + "/messages", body, nil
	}
}

func (a *QQAPI) post(ctx context.Context, config QQConfig, endpoint string, body map[string]any) (channelgateway.SendReceipt, error) {
	receipt, _, err := a.postJSON(ctx, config, endpoint, body)
	return receipt, err
}
func (a *QQAPI) postJSON(ctx context.Context, config QQConfig, endpoint string, body map[string]any) (channelgateway.SendReceipt, map[string]any, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, nil, err
	}
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return channelgateway.SendReceipt{}, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return channelgateway.SendReceipt{}, nil, err
	}
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			retry := time.Second
			if value := resp.Header.Get("Retry-After"); value != "" {
				if seconds, parseErr := strconv.Atoi(value); parseErr == nil && seconds > 0 {
					retry = time.Duration(seconds) * time.Second
				}
			}
			return channelgateway.SendReceipt{}, result, &channelgateway.DeliveryError{Code: "rate_limited", Message: "QQ rate limit", RetryAfter: retry}
		}
		return channelgateway.SendReceipt{}, result, &channelgateway.DeliveryError{Code: "qq_http_" + strconv.Itoa(resp.StatusCode), Message: strings.TrimSpace(string(data)), Permanent: resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429}
	}
	external, _ := result["id"].(string)
	if external == "" {
		external, _ = result["file_uuid"].(string)
	}
	return channelgateway.SendReceipt{ExternalMessageID: external, Code: "accepted"}, result, nil
}

type QQAttachment struct {
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
}
type QQMessage struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Timestamp   string `json:"timestamp"`
	ChannelID   string `json:"channel_id"`
	GuildID     string `json:"guild_id"`
	GroupOpenID string `json:"group_openid"`
	Author      struct {
		ID           string `json:"id"`
		MemberOpenID string `json:"member_openid"`
		UserOpenID   string `json:"user_openid"`
		Username     string `json:"username"`
	} `json:"author"`
	Attachments []QQAttachment `json:"attachments"`
}

func NormalizeQQMessage(config QQConfig, eventType string, message QQMessage) (channelgateway.Envelope, bool) {
	if message.ID == "" {
		return channelgateway.Envelope{}, false
	}
	participant := firstQQNonEmpty(message.Author.MemberOpenID, message.Author.UserOpenID, message.Author.ID)
	conversation, scope := "", channelgateway.ScopeDirect
	switch eventType {
	case "GROUP_AT_MESSAGE_CREATE":
		conversation, scope = message.GroupOpenID, channelgateway.ScopeGroup
	case "C2C_MESSAGE_CREATE":
		conversation = participant
	case "AT_MESSAGE_CREATE":
		conversation, scope = message.ChannelID, channelgateway.ScopeGroup
	case "DIRECT_MESSAGE_CREATE":
		conversation = message.GuildID
	default:
		return channelgateway.Envelope{}, false
	}
	if conversation == "" || participant == "" {
		return channelgateway.Envelope{}, false
	}
	media := make([]channelgateway.Media, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		ref := strings.TrimSpace(attachment.URL)
		if ref == "" {
			continue
		}
		if !strings.Contains(ref, "://") {
			ref = "https://" + ref
		}
		kind := channelgateway.MediaFile
		switch {
		case strings.HasPrefix(attachment.ContentType, "image/"):
			kind = channelgateway.MediaImage
		case strings.HasPrefix(attachment.ContentType, "audio/"):
			kind = channelgateway.MediaAudio
		case strings.HasPrefix(attachment.ContentType, "video/"):
			kind = channelgateway.MediaVideo
		}
		media = append(media, channelgateway.Media{Kind: kind, Ref: ref, Name: filepath.Base(attachment.Filename), MIMEType: attachment.ContentType, Size: attachment.Size})
	}
	text := strings.TrimSpace(message.Content)
	if text == "" && len(media) == 0 {
		return channelgateway.Envelope{}, false
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(text, "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, message.Timestamp); err == nil {
		occurred = parsed.UTC()
	}
	return channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: kind, Address: channelgateway.Address{Channel: channelgateway.ChannelQQ, AccountID: config.AppID, ConversationID: conversation, ParticipantID: participant, Scope: scope}, ExternalEventID: message.ID, ExternalMessageID: message.ID, IdempotencyKey: "qq:" + message.ID, Text: text, Media: media, OccurredAt: occurred, Metadata: map[string]string{"event_type": eventType, "channel": conversation, "reply_to": message.ID}}, true
}

func (a *QQAPI) Download(ctx context.Context, raw string, maxBytes int64, allowedTestHost string) ([]byte, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, "", errors.New("QQ media URL is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	trusted := host == "qq.com" || strings.HasSuffix(host, ".qq.com") || host == "qpic.cn" || strings.HasSuffix(host, ".qpic.cn") || (allowedTestHost != "" && parsed.Host == allowedTestHost)
	if !trusted {
		return nil, "", errors.New("QQ media URL host is not trusted")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("QQ media download failed with status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", errors.New("QQ media exceeds the configured size limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func firstQQNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
