// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"matrix/neo/internal/channelgateway"
)

const (
	weixinDefaultBase = "https://ilinkai.weixin.qq.com"
	weixinDefaultCDN  = "https://novac2c.cdn.weixin.qq.com/c2c"
	weixinVersion     = "2.0.0"
)

type WeixinAPI struct {
	Client     *http.Client
	BaseURL    string
	CDNBaseURL string
}

func NewWeixinAPI() *WeixinAPI { return &WeixinAPI{Client: &http.Client{Timeout: 45 * time.Second}} }
func (a *WeixinAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}
func (a *WeixinAPI) base(config WeixinConfig) string {
	if strings.TrimSpace(a.BaseURL) != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	if config.BaseURL != "" {
		return strings.TrimRight(config.BaseURL, "/")
	}
	return weixinDefaultBase
}
func (a *WeixinAPI) cdn(config WeixinConfig) string {
	if strings.TrimSpace(a.CDNBaseURL) != "" {
		return strings.TrimRight(a.CDNBaseURL, "/")
	}
	if config.CDNBaseURL != "" {
		return strings.TrimRight(config.CDNBaseURL, "/")
	}
	return weixinDefaultCDN
}

func weixinHeaders(token string) http.Header {
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	uin := base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binaryUint32(random)), 10)))
	header := http.Header{"Content-Type": []string{"application/json"}, "AuthorizationType": []string{"ilink_bot_token"}, "X-WECHAT-UIN": []string{uin}, "iLink-App-Id": []string{"bot"}, "iLink-App-ClientVersion": []string{"131072"}}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	return header
}
func binaryUint32(value []byte) uint32 {
	if len(value) < 4 {
		return 0
	}
	return uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
}

func (a *WeixinAPI) post(ctx context.Context, config WeixinConfig, endpoint string, payload map[string]any, timeout time.Duration) (map[string]any, error) {
	payload["base_info"] = map[string]string{"channel_version": weixinVersion}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base(config)+"/"+strings.TrimLeft(endpoint, "/"), bytes.NewReader(body))
	req.Header = weixinHeaders(config.Token)
	client := *a.client()
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return nil, errors.New("Weixin returned invalid JSON")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Weixin API failed with status %d", resp.StatusCode)
	}
	return result, nil
}

type WeixinMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`
	EncryptType       int    `json:"encrypt_type"`
}
type WeixinItem struct {
	Type     int `json:"type"`
	TextItem struct {
		Text string `json:"text"`
	} `json:"text_item"`
	ImageItem struct {
		Media WeixinMedia `json:"media"`
	} `json:"image_item"`
	VoiceItem struct {
		Media WeixinMedia `json:"media"`
		Text  string      `json:"text"`
	} `json:"voice_item"`
	FileItem struct {
		Media    WeixinMedia `json:"media"`
		FileName string      `json:"file_name"`
		Len      string      `json:"len"`
	} `json:"file_item"`
	VideoItem struct {
		Media WeixinMedia `json:"media"`
	} `json:"video_item"`
}
type WeixinMessage struct {
	MessageID    string       `json:"message_id"`
	Sequence     any          `json:"seq"`
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ContextToken string       `json:"context_token"`
	CreateTimeMS int64        `json:"create_time_ms"`
	ItemList     []WeixinItem `json:"item_list"`
}
type WeixinUpdates struct {
	Ret                  int             `json:"ret"`
	ErrorCode            int             `json:"errcode"`
	ErrorMessage         string          `json:"errmsg"`
	Cursor               string          `json:"get_updates_buf"`
	LongPollingTimeoutMS int             `json:"longpolling_timeout_ms"`
	Messages             []WeixinMessage `json:"msgs"`
}

func (a *WeixinAPI) GetUpdates(ctx context.Context, config WeixinConfig) (WeixinUpdates, error) {
	result, err := a.post(ctx, config, "ilink/bot/getupdates", map[string]any{"get_updates_buf": config.UpdatesCursor}, 45*time.Second)
	if err != nil {
		return WeixinUpdates{}, err
	}
	encoded, _ := json.Marshal(result)
	var updates WeixinUpdates
	if json.Unmarshal(encoded, &updates) != nil {
		return updates, errors.New("Weixin updates response is invalid")
	}
	if updates.Ret != 0 || updates.ErrorCode != 0 {
		return updates, fmt.Errorf("Weixin getupdates failed: ret=%d errcode=%d %s", updates.Ret, updates.ErrorCode, updates.ErrorMessage)
	}
	return updates, nil
}

func NormalizeWeixinMessage(config WeixinConfig, message WeixinMessage) (channelgateway.Envelope, bool) {
	if message.MessageType != 1 || message.FromUserID == "" {
		return channelgateway.Envelope{}, false
	}
	id := strings.TrimSpace(message.MessageID)
	if id == "" {
		id = fmt.Sprint(message.Sequence)
	}
	if id == "" {
		return channelgateway.Envelope{}, false
	}
	var texts []string
	var media []channelgateway.Media
	appendMedia := func(kind channelgateway.MediaKind, value WeixinMedia, name string) {
		if value.EncryptQueryParam != "" && value.AESKey != "" {
			media = append(media, channelgateway.Media{Kind: kind, Ref: "weixin-media:" + base64.RawURLEncoding.EncodeToString([]byte(value.EncryptQueryParam)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(value.AESKey)), Name: filepath.Base(name)})
		}
	}
	for _, item := range message.ItemList {
		switch item.Type {
		case 1:
			if text := strings.TrimSpace(item.TextItem.Text); text != "" {
				texts = append(texts, text)
			}
		case 2:
			appendMedia(channelgateway.MediaImage, item.ImageItem.Media, "image.png")
		case 3:
			if text := strings.TrimSpace(item.VoiceItem.Text); text != "" {
				texts = append(texts, text)
			} else {
				appendMedia(channelgateway.MediaAudio, item.VoiceItem.Media, "voice")
			}
		case 4:
			appendMedia(channelgateway.MediaFile, item.FileItem.Media, item.FileItem.FileName)
		case 5:
			appendMedia(channelgateway.MediaVideo, item.VideoItem.Media, "video.mp4")
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(media) == 0 {
		return channelgateway.Envelope{}, false
	}
	kind := channelgateway.KindMessage
	if strings.EqualFold(strings.TrimSpace(text), "/stop") {
		kind = channelgateway.KindInterrupt
	}
	occurred := time.Now().UTC()
	if message.CreateTimeMS > 0 {
		occurred = time.UnixMilli(message.CreateTimeMS).UTC()
	}
	return channelgateway.Envelope{Direction: channelgateway.Inbound, Kind: kind, Address: channelgateway.Address{Channel: channelgateway.ChannelWeixin, AccountID: config.BotID, ConversationID: message.FromUserID, ParticipantID: message.FromUserID, Scope: channelgateway.ScopeDirect}, ExternalEventID: id, ExternalMessageID: id, IdempotencyKey: "weixin:" + id, Text: text, Media: media, OccurredAt: occurred, Metadata: map[string]string{"context_token": message.ContextToken, "channel": message.FromUserID}}, true
}

func (a *WeixinAPI) SendText(ctx context.Context, config WeixinConfig, to, contextToken, text string) (channelgateway.SendReceipt, error) {
	return a.sendItems(ctx, config, to, contextToken, []map[string]any{{"type": 1, "text_item": map[string]string{"text": text}}})
}

func (a *WeixinAPI) SendTyping(ctx context.Context, config WeixinConfig, to, contextToken string, status int) (channelgateway.SendReceipt, error) {
	if contextToken == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "context_token", Message: "Weixin requires a current context_token from the recipient", Permanent: true}
	}
	if status != 1 && status != 2 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "typing_status", Message: "Weixin typing status must be 1 or 2", Permanent: true}
	}
	configuration, err := a.post(ctx, config, "ilink/bot/getconfig", map[string]any{"ilink_user_id": to, "context_token": contextToken}, 10*time.Second)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if deliveryError := weixinResultError(configuration, "getconfig"); deliveryError != nil {
		return channelgateway.SendReceipt{}, deliveryError
	}
	ticket, _ := configuration["typing_ticket"].(string)
	if strings.TrimSpace(ticket) == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "typing_ticket", Message: "Weixin getconfig returned no typing ticket"}
	}
	result, err := a.post(ctx, config, "ilink/bot/sendtyping", map[string]any{"ilink_user_id": to, "typing_ticket": ticket, "status": status}, 10*time.Second)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if deliveryError := weixinResultError(result, "sendtyping"); deliveryError != nil {
		return channelgateway.SendReceipt{}, deliveryError
	}
	return channelgateway.SendReceipt{Code: "accepted"}, nil
}

func weixinResultError(result map[string]any, operation string) error {
	ret := intFromAny(result["ret"])
	code := intFromAny(result["errcode"])
	if ret == 0 && code == 0 {
		return nil
	}
	value := firstNonZero(code, ret)
	return &channelgateway.DeliveryError{Code: "weixin_" + strconv.Itoa(value), Message: "Weixin " + operation + " failed: " + fmt.Sprint(result["errmsg"]), Permanent: value == -14}
}

func (a *WeixinAPI) sendItems(ctx context.Context, config WeixinConfig, to, contextToken string, items []map[string]any) (channelgateway.SendReceipt, error) {
	if contextToken == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "context_token", Message: "Weixin requires a current context_token from the recipient", Permanent: true}
	}
	clientID := make([]byte, 8)
	_, _ = rand.Read(clientID)
	result, err := a.post(ctx, config, "ilink/bot/sendmessage", map[string]any{"msg": map[string]any{"from_user_id": "", "to_user_id": to, "client_id": hex.EncodeToString(clientID), "message_type": 2, "message_state": 2, "item_list": items, "context_token": contextToken}}, 20*time.Second)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if deliveryError := weixinResultError(result, "sendmessage"); deliveryError != nil {
		return channelgateway.SendReceipt{}, deliveryError
	}
	return channelgateway.SendReceipt{ExternalMessageID: hex.EncodeToString(clientID), Code: "accepted"}, nil
}

func (a *WeixinAPI) SendMedia(ctx context.Context, config WeixinConfig, to, contextToken, name string, data []byte, kind channelgateway.MediaKind) (channelgateway.SendReceipt, error) {
	mediaType, itemType := 3, 4
	if kind == channelgateway.MediaImage {
		mediaType, itemType = 1, 2
	} else if kind == channelgateway.MediaVideo {
		mediaType, itemType = 2, 5
	}
	aesKey := make([]byte, 16)
	_, _ = rand.Read(aesKey)
	encrypted := aesECBEncrypt(data, aesKey)
	fileKeyBytes := make([]byte, 16)
	_, _ = rand.Read(fileKeyBytes)
	fileKey := hex.EncodeToString(fileKeyBytes)
	sum := md5.Sum(data)
	result, err := a.post(ctx, config, "ilink/bot/getuploadurl", map[string]any{"filekey": fileKey, "media_type": mediaType, "to_user_id": to, "rawsize": len(data), "rawfilemd5": hex.EncodeToString(sum[:]), "filesize": len(encrypted), "aeskey": hex.EncodeToString(aesKey), "no_need_thumb": true}, 20*time.Second)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	uploadURL, _ := result["upload_full_url"].(string)
	if uploadURL == "" {
		if parameter, _ := result["upload_param"].(string); parameter != "" {
			uploadURL = a.cdn(config) + "/upload?encrypted_query_param=" + url.QueryEscape(parameter) + "&filekey=" + url.QueryEscape(fileKey)
		}
	}
	if !a.trustedWeixinURL(uploadURL, config) {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "upload_url", Message: "Weixin returned an untrusted media upload URL", Permanent: true}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(encrypted))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := a.client().Do(req)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return channelgateway.SendReceipt{}, fmt.Errorf("Weixin CDN upload failed with status %d", resp.StatusCode)
	}
	downloadParam := resp.Header.Get("X-Encrypted-Param")
	if downloadParam == "" {
		return channelgateway.SendReceipt{}, errors.New("Weixin CDN upload returned no encrypted parameter")
	}
	encodedKey := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(aesKey)))
	media := map[string]any{"encrypt_query_param": downloadParam, "aes_key": encodedKey, "encrypt_type": 1}
	item := map[string]any{"type": itemType}
	switch itemType {
	case 2:
		item["image_item"] = map[string]any{"media": media, "mid_size": len(encrypted)}
	case 4:
		item["file_item"] = map[string]any{"media": media, "file_name": filepath.Base(name), "len": strconv.Itoa(len(data))}
	case 5:
		item["video_item"] = map[string]any{"media": media, "video_size": len(encrypted)}
	}
	return a.sendItems(ctx, config, to, contextToken, []map[string]any{item})
}

func (a *WeixinAPI) DownloadMedia(ctx context.Context, config WeixinConfig, ref string, maxBytes int64) ([]byte, error) {
	parts := strings.SplitN(strings.TrimPrefix(ref, "weixin-media:"), ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("Weixin media reference is invalid")
	}
	queryBytes, e1 := base64.RawURLEncoding.DecodeString(parts[0])
	keyValue, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	if e1 != nil || e2 != nil {
		return nil, errors.New("Weixin media reference encoding is invalid")
	}
	endpoint := a.cdn(config) + "/download?encrypted_query_param=" + url.QueryEscape(string(queryBytes))
	if !a.trustedWeixinURL(endpoint, config) {
		return nil, errors.New("Weixin media download URL is untrusted")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	encrypted, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+aes.BlockSize+1))
	if err != nil {
		return nil, err
	}
	key, err := decodeWeixinAESKey(string(keyValue))
	if err != nil {
		return nil, err
	}
	plain, err := aesECBDecrypt(encrypted, key)
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > maxBytes {
		return nil, errors.New("Weixin media exceeds the configured size limit")
	}
	return plain, nil
}

func (a *WeixinAPI) trustedWeixinURL(raw string, config WeixinConfig) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "weixin.qq.com" || strings.HasSuffix(host, ".weixin.qq.com") {
		return true
	}
	for _, base := range []string{config.BaseURL, config.CDNBaseURL, a.BaseURL, a.CDNBaseURL} {
		if base != "" {
			candidate, _ := url.Parse(base)
			if candidate != nil && candidate.Host == parsed.Host {
				return true
			}
		}
	}
	return false
}
func aesECBEncrypt(data, key []byte) []byte {
	block, _ := aes.NewCipher(key)
	plain := padPKCS7(data, aes.BlockSize)
	out := make([]byte, len(plain))
	for offset := 0; offset < len(plain); offset += aes.BlockSize {
		block.Encrypt(out[offset:offset+aes.BlockSize], plain[offset:offset+aes.BlockSize])
	}
	return out
}
func aesECBDecrypt(data, key []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("Weixin encrypted media is invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	for offset := 0; offset < len(data); offset += aes.BlockSize {
		block.Decrypt(out[offset:offset+aes.BlockSize], data[offset:offset+aes.BlockSize])
	}
	return unpadPKCS7(out, aes.BlockSize)
}
func decodeWeixinAESKey(value string) ([]byte, error) {
	if key, err := hex.DecodeString(value); err == nil && len(key) == 16 {
		return key, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("Weixin media AES key is invalid")
	}
	if len(decoded) == 16 {
		return decoded, nil
	}
	if len(decoded) == 32 {
		key, err := hex.DecodeString(string(decoded))
		if err == nil && len(key) == 16 {
			return key, nil
		}
	}
	return nil, errors.New("Weixin media AES key length is invalid")
}
func intFromAny(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}
func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

type WeixinQRCode struct {
	QRCode    string `json:"qrcode"`
	QRCodeURL string `json:"qrcode_img_content"`
}

func (a *WeixinAPI) FetchQRCode(ctx context.Context, baseURL string) (WeixinQRCode, error) {
	if baseURL != "" && !validWeixinEndpoint(baseURL) {
		return WeixinQRCode{}, errors.New("Weixin QR base URL is not trusted")
	}
	base := strings.TrimRight(baseURL, "/")
	if strings.TrimSpace(a.BaseURL) != "" {
		base = strings.TrimRight(a.BaseURL, "/")
	}
	if base == "" {
		base = weixinDefaultBase
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ilink/bot/get_bot_qrcode?bot_type=3", nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return WeixinQRCode{}, err
	}
	defer resp.Body.Close()
	var result WeixinQRCode
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if err == nil && result.QRCode == "" {
		err = errors.New("Weixin QR response is incomplete")
	}
	return result, err
}

type WeixinQRStatus struct {
	Status   string `json:"status"`
	BotToken string `json:"bot_token,omitempty"`
	BotID    string `json:"ilink_bot_id,omitempty"`
	BaseURL  string `json:"baseurl,omitempty"`
	UserID   string `json:"ilink_user_id,omitempty"`
}

func (a *WeixinAPI) PollQRCode(ctx context.Context, baseURL, qrcode string) (WeixinQRStatus, error) {
	if baseURL != "" && !validWeixinEndpoint(baseURL) {
		return WeixinQRStatus{}, errors.New("Weixin QR base URL is not trusted")
	}
	base := strings.TrimRight(baseURL, "/")
	if strings.TrimSpace(a.BaseURL) != "" {
		base = strings.TrimRight(a.BaseURL, "/")
	}
	if base == "" {
		base = weixinDefaultBase
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(qrcode), nil)
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", "131072")
	resp, err := a.client().Do(req)
	if err != nil {
		return WeixinQRStatus{}, err
	}
	defer resp.Body.Close()
	var result WeixinQRStatus
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	return result, err
}
