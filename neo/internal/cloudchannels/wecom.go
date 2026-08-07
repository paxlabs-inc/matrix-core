// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
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

type WeComCrypto struct {
	token     string
	key       []byte
	receiveID string
}

func NewWeComCrypto(token, encodingAESKey, receiveID string) (*WeComCrypto, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodingAESKey) + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("WeCom EncodingAESKey must contain 43 valid base64 characters")
	}
	if len(strings.TrimSpace(token)) < 8 {
		return nil, errors.New("WeCom callback token is invalid")
	}
	return &WeComCrypto{token: strings.TrimSpace(token), key: key, receiveID: strings.TrimSpace(receiveID)}, nil
}

func (c *WeComCrypto) Signature(timestamp, nonce, encrypted string) string {
	parts := []string{c.token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func (c *WeComCrypto) Verify(signature, timestamp, nonce, encrypted string) error {
	if c == nil || !constantStringEqual(c.Signature(timestamp, nonce, encrypted), strings.ToLower(strings.TrimSpace(signature))) {
		return errors.New("WeCom callback signature is invalid")
	}
	return nil
}

func (c *WeComCrypto) Decrypt(encrypted string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("WeCom callback crypto is unavailable")
	}
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("WeCom encrypted payload is invalid")
	}
	block, _ := aes.NewCipher(c.key)
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, c.key[:aes.BlockSize]).CryptBlocks(plain, data)
	plain, err = unpadPKCS7(plain, 32)
	if err != nil || len(plain) < 20 {
		return nil, errors.New("WeCom encrypted payload padding is invalid")
	}
	size := int(binary.BigEndian.Uint32(plain[16:20]))
	if size < 0 || 20+size > len(plain) {
		return nil, errors.New("WeCom encrypted payload length is invalid")
	}
	payload := append([]byte(nil), plain[20:20+size]...)
	receiveID := string(plain[20+size:])
	if receiveID != c.receiveID {
		return nil, errors.New("WeCom callback receive identity is invalid")
	}
	return payload, nil
}

func (c *WeComCrypto) Encrypt(payload []byte) (string, error) {
	if c == nil {
		return "", errors.New("WeCom callback crypto is unavailable")
	}
	prefix := make([]byte, 16)
	if _, err := rand.Read(prefix); err != nil {
		return "", err
	}
	plain := append(prefix, make([]byte, 4)...)
	binary.BigEndian.PutUint32(plain[16:20], uint32(len(payload)))
	plain = append(plain, payload...)
	plain = append(plain, []byte(c.receiveID)...)
	plain = padPKCS7(plain, 32)
	block, _ := aes.NewCipher(c.key)
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, c.key[:aes.BlockSize]).CryptBlocks(encrypted, plain)
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func padPKCS7(value []byte, blockSize int) []byte {
	padding := blockSize - len(value)%blockSize
	return append(value, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func unpadPKCS7(value []byte, blockSize int) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("empty padded value")
	}
	padding := int(value[len(value)-1])
	if padding < 1 || padding > blockSize || padding > len(value) {
		return nil, errors.New("invalid padding")
	}
	for _, item := range value[len(value)-padding:] {
		if int(item) != padding {
			return nil, errors.New("invalid padding")
		}
	}
	return value[:len(value)-padding], nil
}

func constantStringEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var result byte
	for index := range left {
		result |= left[index] ^ right[index]
	}
	return result == 0
}

type WeComEncryptedJSON struct {
	Encrypt string `json:"encrypt"`
}

type WeComEncryptedXML struct {
	XMLName   xml.Name `xml:"xml"`
	Encrypt   string   `xml:"Encrypt"`
	Signature string   `xml:"MsgSignature,omitempty"`
	Timestamp string   `xml:"TimeStamp,omitempty"`
	Nonce     string   `xml:"Nonce,omitempty"`
}

type WeComBotMessage struct {
	MsgID       string `json:"msgid"`
	MsgType     string `json:"msgtype"`
	CreateTime  int64  `json:"create_time"`
	ChatType    string `json:"chattype"`
	ChatID      string `json:"chatid"`
	AIBotID     string `json:"aibotid"`
	ResponseURL string `json:"response_url,omitempty"`
	From        struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Voice struct {
		Content string `json:"content"`
	} `json:"voice"`
	Image struct {
		URL    string `json:"url"`
		AESKey string `json:"aeskey"`
	} `json:"image"`
	File struct {
		URL    string `json:"url"`
		AESKey string `json:"aeskey"`
		Name   string `json:"filename"`
	} `json:"file"`
	Video struct {
		URL    string `json:"url"`
		AESKey string `json:"aeskey"`
	} `json:"video"`
}

func NormalizeWeComBotMessage(config WeComBotConfig, message WeComBotMessage) (channelgateway.Envelope, bool, error) {
	if message.MsgID == "" || message.From.UserID == "" || (message.AIBotID != "" && message.AIBotID != config.BotID) {
		return channelgateway.Envelope{}, false, nil
	}
	group := message.ChatType == "group"
	conversation := message.From.UserID
	if group {
		conversation = message.ChatID
	}
	if conversation == "" {
		return channelgateway.Envelope{}, false, nil
	}
	text := strings.TrimSpace(message.Text.Content)
	if message.MsgType == "voice" {
		text = strings.TrimSpace(message.Voice.Content)
	}
	var media []channelgateway.Media
	appendMedia := func(kind channelgateway.MediaKind, ref, name string) {
		if ref != "" {
			media = append(media, channelgateway.Media{Kind: kind, Ref: ref, Name: filepath.Base(name)})
		}
	}
	switch message.MsgType {
	case "image":
		appendMedia(channelgateway.MediaImage, "wecom-media:"+message.Image.AESKey+":"+message.Image.URL, "image")
	case "file":
		appendMedia(channelgateway.MediaFile, "wecom-media:"+message.File.AESKey+":"+message.File.URL, message.File.Name)
	case "video":
		appendMedia(channelgateway.MediaVideo, "wecom-media:"+message.Video.AESKey+":"+message.Video.URL, "video.mp4")
	case "text", "voice":
	default:
		return channelgateway.Envelope{}, false, nil
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
	if message.CreateTime > 0 {
		occurred = time.Unix(message.CreateTime, 0).UTC()
	}
	metadata := map[string]string{"channel": conversation}
	if safeResponseURL(message.ResponseURL) {
		metadata["response_url"] = message.ResponseURL
	}
	return channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: kind,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelWeComBot, AccountID: config.BotID, ConversationID: conversation, ParticipantID: message.From.UserID, Scope: scope},
		ExternalEventID: message.MsgID, ExternalMessageID: message.MsgID, IdempotencyKey: "wecom-bot:" + message.MsgID,
		Text: text, Media: media, OccurredAt: occurred, Metadata: metadata,
	}, true, nil
}

func safeResponseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && (strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".weixin.qq.com") || strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".wecom.cn"))
}

func DownloadWeComBotMedia(ctx context.Context, client *http.Client, ref, defaultAESKey string, maxBytes int64, allowedTestHost string) ([]byte, string, error) {
	value := strings.TrimPrefix(ref, "wecom-media:")
	aesKey, raw, ok := strings.Cut(value, ":")
	if !ok || raw == "" {
		return nil, "", errors.New("WeCom Bot media reference is invalid")
	}
	if aesKey == "" {
		aesKey = defaultAESKey
	}
	key, err := base64.StdEncoding.DecodeString(aesKey + strings.Repeat("=", (4-len(aesKey)%4)%4))
	if err != nil || len(key) != 32 {
		return nil, "", errors.New("WeCom Bot media key is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, "", errors.New("WeCom Bot media URL is invalid")
	}
	host := strings.ToLower(parsed.Hostname())
	trusted := host == "weixin.qq.com" || strings.HasSuffix(host, ".weixin.qq.com") || host == "wecom.cn" || strings.HasSuffix(host, ".wecom.cn") || (allowedTestHost != "" && strings.EqualFold(parsed.Host, allowedTestHost))
	if !trusted {
		return nil, "", errors.New("WeCom Bot media URL host is not trusted")
	}
	if client == nil {
		client = http.DefaultClient
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("WeCom Bot media download failed with status %d", response.StatusCode)
	}
	encrypted, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+33))
	if err != nil {
		return nil, "", err
	}
	if len(encrypted) == 0 || len(encrypted)%aes.BlockSize != 0 {
		return nil, "", errors.New("WeCom Bot encrypted media is invalid")
	}
	block, _ := aes.NewCipher(key)
	plain := make([]byte, len(encrypted))
	cipher.NewCBCDecrypter(block, key[:aes.BlockSize]).CryptBlocks(plain, encrypted)
	plain, err = unpadPKCS7(plain, 32)
	if err != nil {
		return nil, "", errors.New("WeCom Bot media padding is invalid")
	}
	if int64(len(plain)) > maxBytes {
		return nil, "", errors.New("WeCom Bot media exceeds the configured size limit")
	}
	return plain, response.Header.Get("Content-Type"), nil
}

type WeComAppMessage struct {
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
}

func NormalizeWeComAppMessage(config WeComAppConfig, message WeComAppMessage) (channelgateway.Envelope, bool) {
	if message.FromUserName == "" || message.ToUserName != config.CorpID || message.MsgID == "" {
		return channelgateway.Envelope{}, false
	}
	text := strings.TrimSpace(message.Content)
	var media []channelgateway.Media
	switch message.MsgType {
	case "text":
	case "voice":
		if strings.TrimSpace(message.Recognition) != "" {
			text = strings.TrimSpace(message.Recognition)
		} else if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaAudio, Ref: "wecom-app-media:" + message.MediaID, Name: message.MediaID + "." + message.Format})
		}
	case "image":
		if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaImage, Ref: "wecom-app-media:" + message.MediaID, Name: message.MediaID + ".png"})
		}
	case "video", "shortvideo":
		if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaVideo, Ref: "wecom-app-media:" + message.MediaID, Name: message.MediaID + ".mp4"})
		}
	case "file":
		if message.MediaID != "" {
			media = append(media, channelgateway.Media{Kind: channelgateway.MediaFile, Ref: "wecom-app-media:" + message.MediaID, Name: message.MediaID})
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
	return channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: kind,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelWeComApp, AccountID: config.CorpID, ConversationID: message.FromUserName, ParticipantID: message.FromUserName, Scope: channelgateway.ScopeDirect},
		ExternalEventID: message.MsgID, ExternalMessageID: message.MsgID, IdempotencyKey: "wecom-app:" + message.MsgID,
		Text: text, Media: media, OccurredAt: occurred, Metadata: map[string]string{"channel": message.FromUserName},
	}, true
}

type WeComAppAPI struct {
	Client   *http.Client
	BaseURL  string
	mu       sync.Mutex
	token    string
	tokenFor string
	expires  time.Time
}

func NewWeComAppAPI() *WeComAppAPI {
	return &WeComAppAPI{Client: &http.Client{Timeout: 30 * time.Second}}
}

func (a *WeComAppAPI) base() string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return "https://qyapi.weixin.qq.com"
}

func (a *WeComAppAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func (a *WeComAppAPI) AccessToken(ctx context.Context, config WeComAppConfig) (string, error) {
	key := config.CorpID + "\x00" + strconv.FormatInt(config.AgentID, 10)
	a.mu.Lock()
	if a.token != "" && a.tokenFor == key && time.Now().Before(a.expires.Add(-time.Minute)) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	endpoint := a.base() + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(config.CorpID) + "&corpsecret=" + url.QueryEscape(config.Secret)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	response, err := a.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK || result.ErrCode != 0 || result.Token == "" {
		return "", fmt.Errorf("WeCom token exchange failed: %d %s", result.ErrCode, result.ErrMsg)
	}
	a.mu.Lock()
	a.token, a.tokenFor, a.expires = result.Token, key, time.Now().Add(time.Duration(result.Expires)*time.Second)
	a.mu.Unlock()
	return result.Token, nil
}

func (a *WeComAppAPI) PostText(ctx context.Context, config WeComAppConfig, userID, text string) (channelgateway.SendReceipt, error) {
	return a.postMessage(ctx, config, map[string]any{"touser": userID, "msgtype": "text", "agentid": config.AgentID, "text": map[string]string{"content": text}})
}

func (a *WeComAppAPI) PostMedia(ctx context.Context, config WeComAppConfig, userID, name string, data []byte, kind channelgateway.MediaKind) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	mediaType := "file"
	switch kind {
	case channelgateway.MediaImage:
		mediaType = "image"
	case channelgateway.MediaAudio:
		mediaType = "voice"
	case channelgateway.MediaVideo:
		mediaType = "video"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", filepath.Base(name))
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if _, err := part.Write(data); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	_ = writer.Close()
	endpoint := a.base() + "/cgi-bin/media/upload?access_token=" + url.QueryEscape(token) + "&type=" + url.QueryEscape(mediaType)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	defer response.Body.Close()
	var uploaded struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MediaID string `json:"media_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&uploaded); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if response.StatusCode != http.StatusOK || uploaded.ErrCode != 0 || uploaded.MediaID == "" {
		return channelgateway.SendReceipt{}, fmt.Errorf("WeCom media upload failed: %d %s", uploaded.ErrCode, uploaded.ErrMsg)
	}
	return a.postMessage(ctx, config, map[string]any{"touser": userID, "msgtype": mediaType, "agentid": config.AgentID, mediaType: map[string]string{"media_id": uploaded.MediaID}})
}

func (a *WeComAppAPI) DownloadMedia(ctx context.Context, config WeComAppConfig, mediaID string) ([]byte, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return nil, err
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+"/cgi-bin/media/get?access_token="+url.QueryEscape(token)+"&media_id="+url.QueryEscape(mediaID), nil)
	response, err := a.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WeCom media download failed with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (25<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 25<<20 {
		return nil, errors.New("WeCom media exceeds 25 MiB")
	}
	if strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		return nil, errors.New("WeCom media download returned an API error")
	}
	return data, nil
}

func (a *WeComAppAPI) postMessage(ctx context.Context, config WeComAppConfig, payload map[string]any) (channelgateway.SendReceipt, error) {
	token, err := a.AccessToken(ctx, config)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	body, _ := json.Marshal(payload)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/cgi-bin/message/send?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MsgID   string `json:"msgid"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if response.StatusCode == http.StatusTooManyRequests || result.ErrCode == 45009 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "WeCom rate limit", RetryAfter: time.Second}
	}
	if response.StatusCode != http.StatusOK || result.ErrCode != 0 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: strconv.Itoa(result.ErrCode), Message: "WeCom delivery failed: " + result.ErrMsg, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	return channelgateway.SendReceipt{ExternalMessageID: result.MsgID, Code: "accepted"}, nil
}

func PostWeComResponse(ctx context.Context, client *http.Client, responseURL, text string) (channelgateway.SendReceipt, error) {
	if !safeResponseURL(responseURL) {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "response_url", Message: "WeCom response URL is invalid", Permanent: true}
	}
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": text}})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: strconv.Itoa(response.StatusCode), Message: "WeCom response delivery failed", Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	return channelgateway.SendReceipt{Code: "accepted"}, nil
}
