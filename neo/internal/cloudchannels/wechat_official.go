// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/channelgateway"
)

func CallbackSignature(token, timestamp, nonce string) string {
	parts := []string{strings.TrimSpace(token), timestamp, nonce}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}
func VerifyCallbackSignature(token, signature, timestamp, nonce string) error {
	if len(strings.TrimSpace(token)) < 8 || !constantStringEqual(CallbackSignature(token, timestamp, nonce), strings.ToLower(strings.TrimSpace(signature))) {
		return errors.New("WeChat callback signature is invalid")
	}
	return nil
}

type WeChatMPMessage struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        string   `xml:"MsgId"`
	MediaID      string   `xml:"MediaId"`
	Format       string   `xml:"Format"`
	Recognition  string   `xml:"Recognition"`
	Event        string   `xml:"Event"`
	EventKey     string   `xml:"EventKey"`
}

func NormalizeWeChatMPMessage(config WeChatMPConfig, message WeChatMPMessage) (channelgateway.Envelope, bool) {
	if message.FromUserName == "" || message.ToUserName == "" {
		return channelgateway.Envelope{}, false
	}
	id := message.MsgID
	if id == "" {
		id = strconv.FormatInt(message.CreateTime, 10) + ":" + message.FromUserName + ":" + message.MsgType + ":" + message.Event
	}
	text := strings.TrimSpace(message.Content)
	var media []channelgateway.Media
	switch message.MsgType {
	case "text":
	case "voice":
		if strings.TrimSpace(message.Recognition) != "" {
			text = strings.TrimSpace(message.Recognition)
		} else if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaAudio, Ref: "wechat-mp-media:" + message.MediaID, Name: message.MediaID + "." + message.Format})
		}
	case "image":
		if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaImage, Ref: "wechat-mp-media:" + message.MediaID, Name: message.MediaID + ".png"})
		}
	case "video", "shortvideo":
		if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaVideo, Ref: "wechat-mp-media:" + message.MediaID, Name: message.MediaID + ".mp4"})
		}
	case "event":
		if strings.EqualFold(message.Event, "CLICK") {
			text = strings.TrimSpace(message.EventKey)
		} else {
			return channelgateway.Envelope{}, false
		}
	default:
		return channelgateway.Envelope{}, false
	}
	if text == "" && len(media) == 0 {
		return channelgateway.Envelope{}, false
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(text, "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if message.CreateTime > 0 {
		occurred = time.Unix(message.CreateTime, 0).UTC()
	}
	return channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: kind, Address: channelgateway.Address{Channel: channelgateway.ChannelWeChatMP, AccountID: config.AppID, ConversationID: message.FromUserName, ParticipantID: message.FromUserName, Scope: channelgateway.ScopeDirect}, ExternalEventID: id, ExternalMessageID: id, IdempotencyKey: "wechat-mp:" + id, Text: text, Media: media, OccurredAt: occurred, Metadata: map[string]string{"channel": message.FromUserName, "reply_window_started_at": strconv.FormatInt(occurred.Unix(), 10)}}, true
}

type WeChatMPAPI struct {
	Client          *http.Client
	BaseURL         string
	mu              sync.Mutex
	token, tokenFor string
	expires         time.Time
}

func NewWeChatMPAPI() *WeChatMPAPI {
	return &WeChatMPAPI{Client: &http.Client{Timeout: 30 * time.Second}}
}
func (a *WeChatMPAPI) base() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://api.weixin.qq.com"
}
func (a *WeChatMPAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}
func (a *WeChatMPAPI) AccessToken(ctx context.Context, config WeChatMPConfig) (string, error) {
	key := config.AppID + "\x00" + config.AppSecret
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	endpoint := a.base() + "/cgi-bin/token?grant_type=client_credential&appid=" + url.QueryEscape(config.AppID) + "&secret=" + url.QueryEscape(config.AppSecret)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return "", redactProtocolError(err, config.AppSecret)
	}
	defer resp.Body.Close()
	var result struct {
		Token        string `json:"access_token"`
		Expires      int    `json:"expires_in"`
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("WeChat Official Account token exchange failed: %d %s", result.ErrorCode, result.ErrorMessage)
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.Token, key, time.Now().Add(time.Duration(result.Expires)*time.Second)
	a.mu.Unlock()
	return result.Token, nil
}
func (a *WeChatMPAPI) PostText(ctx context.Context, config WeChatMPConfig, to, text string, windowStarted int64) (channelgateway.SendReceipt, error) {
	if windowStarted <= 0 || time.Since(time.Unix(windowStarted, 0)) > 48*time.Hour {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "reply_window", Message: "WeChat Official Account customer-service replies require user activity within 48 hours", Permanent: true}
	}
	return a.send(ctx, config, map[string]any{"touser": to, "msgtype": "text", "text": map[string]string{"content": text}})
}
func (a *WeChatMPAPI) PostMedia(ctx context.Context, config WeChatMPConfig, to, name string, data []byte, kind channelgateway.MediaKind, windowStarted int64) (channelgateway.SendReceipt, error) {
	if windowStarted <= 0 || time.Since(time.Unix(windowStarted, 0)) > 48*time.Hour {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "reply_window", Message: "WeChat Official Account media replies require user activity within 48 hours", Permanent: true}
	}
	if kind == channelgateway.MediaFile {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media_type", Message: "WeChat Official Account customer-service messages do not support generic file attachments", Permanent: true}
	}
	mediaType := "file"
	field := "file"
	switch kind {
	case channelgateway.MediaImage:
		mediaType, field = "image", "image"
	case channelgateway.MediaAudio:
		mediaType, field = "voice", "voice"
	case channelgateway.MediaVideo:
		mediaType, field = "video", "video"
	}
	mediaID, err := a.upload(ctx, config, mediaType, name, data)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	return a.send(ctx, config, map[string]any{"touser": to, "msgtype": field, field: map[string]string{"media_id": mediaID}})
}
func (a *WeChatMPAPI) send(ctx context.Context, config WeChatMPConfig, payload map[string]any) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/message/custom/send?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return channelgateway.SendReceipt{}, redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	var result struct {
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
		MessageID    int64  `json:"msgid"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if result.ErrorCode != 0 {
		if result.ErrorCode == 45009 {
			return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "WeChat Official Account rate limit", RetryAfter: time.Minute}
		}
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "wechat_mp_" + strconv.Itoa(result.ErrorCode), Message: result.ErrorMessage, Permanent: result.ErrorCode == 45015 || result.ErrorCode == 40003}
	}
	return channelgateway.SendReceipt{ExternalMessageID: strconv.FormatInt(result.MessageID, 10), Code: "accepted"}, nil
}
func (a *WeChatMPAPI) upload(ctx context.Context, config WeChatMPConfig, mediaType, name string, data []byte) (string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", filepath.Base(name))
	if err != nil {
		return "", err
	}
	if _, err = part.Write(data); err != nil {
		return "", err
	}
	_ = writer.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/media/upload?access_token="+url.QueryEscape(token)+"&type="+url.QueryEscape(mediaType), &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := a.client().Do(req)
	if err != nil {
		return "", redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	var result struct {
		MediaID      string `json:"media_id"`
		ErrorCode    int    `json:"errcode"`
		ErrorMessage string `json:"errmsg"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.MediaID == "" {
		return "", fmt.Errorf("WeChat Official Account media upload failed: %d %s", result.ErrorCode, result.ErrorMessage)
	}
	return result.MediaID, nil
}
func (a *WeChatMPAPI) DownloadMedia(ctx context.Context, config WeChatMPConfig, mediaID string, maxBytes int64) ([]byte, string, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return nil, "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+"/cgi-bin/media/get?access_token="+url.QueryEscape(token)+"&media_id="+url.QueryEscape(mediaID), nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, "", redactProtocolError(err, token)
	}
	defer resp.Body.Close()
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, "", fmt.Errorf("WeChat Official Account media download failed: %s", strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxBytes {
		return nil, "", errors.New("WeChat Official Account media exceeds the configured size limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}
