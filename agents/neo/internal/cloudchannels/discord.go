// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"centra/agents/neo/internal/channelgateway"
)

const discordBodyLimit = 2 << 20

type DiscordAPI struct {
	Client  *http.Client
	BaseURL string
}

type DiscordIdentity struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

func NewDiscordAPI() *DiscordAPI {
	return &DiscordAPI{Client: &http.Client{Timeout: 30 * time.Second}, BaseURL: "https://discord.com/api/v10"}
}

func (a *DiscordAPI) Identity(ctx context.Context, token string) (DiscordIdentity, error) {
	var identity DiscordIdentity
	if err := a.call(ctx, token, http.MethodGet, "/users/@me", nil, &identity); err != nil {
		return DiscordIdentity{}, err
	}
	if identity.ID == "" || !identity.Bot {
		return DiscordIdentity{}, errors.New("Discord token does not identify a bot")
	}
	return identity, nil
}

func (a *DiscordAPI) GatewayURL(ctx context.Context, token string) (string, error) {
	var response struct {
		URL string `json:"url"`
	}
	if err := a.call(ctx, token, http.MethodGet, "/gateway/bot", nil, &response); err != nil {
		return "", err
	}
	return normalizeDiscordGatewayURL(response.URL, a.BaseURL)
}

func normalizeDiscordGatewayURL(rawURL, apiBase string) (string, error) {
	parsed, err := url.Parse(rawURL)
	base, _ := url.Parse(apiBase)
	testEndpoint := base != nil && base.Hostname() != "discord.com" && parsed != nil && parsed.Hostname() == base.Hostname() && parsed.Scheme == "ws"
	if err != nil || parsed == nil || (parsed.Scheme != "wss" && !testEndpoint) || parsed.Host == "" {
		return "", errors.New("Discord returned an invalid Gateway URL")
	}
	query := parsed.Query()
	query.Set("v", "10")
	query.Set("encoding", "json")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (a *DiscordAPI) PostMessage(ctx context.Context, token, channel, text, replyTo string, components any) (channelgateway.SendReceipt, error) {
	body := map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
	if replyTo != "" {
		body["message_reference"] = map[string]string{"message_id": replyTo}
	}
	if components != nil {
		body["components"] = components
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := a.call(ctx, token, http.MethodPost, "/channels/"+url.PathEscape(channel)+"/messages", body, &response); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if response.ID == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "protocol", Message: "Discord response omitted the message identity"}
	}
	return channelgateway.SendReceipt{ExternalMessageID: response.ID, Code: "accepted"}, nil
}

func (a *DiscordAPI) TriggerTyping(ctx context.Context, token, channel string) (channelgateway.SendReceipt, error) {
	if err := a.call(ctx, token, http.MethodPost, "/channels/"+url.PathEscape(channel)+"/typing", nil, nil); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	return channelgateway.SendReceipt{Code: "accepted"}, nil
}

func (a *DiscordAPI) UploadFile(ctx context.Context, token, channel, name string, data []byte, caption, replyTo string) (channelgateway.SendReceipt, error) {
	if len(data) == 0 || len(data) > 25<<20 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media_size", Message: "Discord media must be between 1 byte and 25 MB", Permanent: true}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "" {
		name = "neo-media"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	payload := map[string]any{"content": caption, "allowed_mentions": map[string]any{"parse": []string{}}}
	if replyTo != "" {
		payload["message_reference"] = map[string]string{"message_id": replyTo}
	}
	encoded, _ := json.Marshal(payload)
	if err := writer.WriteField("payload_json", string(encoded)); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	part, err := writer.CreateFormFile("files[0]", name)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if _, err := part.Write(data); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if err := writer.Close(); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/channels/"+url.PathEscape(channel)+"/messages", &body)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	request.Header.Set("Authorization", "Bot "+token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, discordBodyLimit+1))
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		var rate struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(responseData, &rate)
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "rate_limited", Message: "Discord rate limit", RetryAfter: time.Duration(rate.RetryAfter * float64(time.Second))}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "http", Message: "Discord media delivery returned " + response.Status, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(responseData, &result); err != nil || result.ID == "" {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "protocol", Message: "Discord media response omitted the message identity"}
	}
	return channelgateway.SendReceipt{ExternalMessageID: result.ID, Code: "accepted"}, nil
}

func (a *DiscordAPI) Download(ctx context.Context, rawURL string, maximum int64) ([]byte, string, error) {
	if maximum <= 0 {
		return nil, "", errors.New("Discord media limit is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "cdn.discordapp.com" && parsed.Hostname() != "media.discordapp.net") {
		return nil, "", errors.New("Discord attachment URL is outside Discord's media service")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	response, err := a.client().Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Discord media download returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maximum {
		return nil, "", fmt.Errorf("Discord media exceeds the %d-byte limit", maximum)
	}
	return data, response.Header.Get("Content-Type"), nil
}

func (a *DiscordAPI) call(ctx context.Context, token, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bot "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client().Do(request)
	if err != nil {
		return &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, discordBodyLimit+1))
	if err != nil {
		return err
	}
	if len(data) > discordBodyLimit {
		return &channelgateway.DeliveryError{Code: "protocol", Message: "Discord response exceeds the bounded limit"}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		var rate struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(data, &rate)
		return &channelgateway.DeliveryError{Code: "rate_limited", Message: "Discord rate limit", RetryAfter: time.Duration(rate.RetryAfter * float64(time.Second))}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &channelgateway.DeliveryError{Code: "http", Message: "Discord returned " + response.Status, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	if output != nil && len(data) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode Discord response: %w", err)
		}
	}
	return nil
}

func (a *DiscordAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func VerifyDiscordRequest(header http.Header, body []byte, publicKeyHex string) error {
	timestamp := strings.TrimSpace(header.Get("X-Signature-Timestamp"))
	signatureHex := strings.TrimSpace(header.Get("X-Signature-Ed25519"))
	publicKey, err := hex.DecodeString(strings.TrimSpace(publicKeyHex))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("Discord public key is invalid")
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil || len(signature) != ed25519.SignatureSize || timestamp == "" {
		return errors.New("Discord signature headers are invalid")
	}
	message := append(append(make([]byte, 0, len(timestamp)+len(body)), timestamp...), body...)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return errors.New("Discord request signature is invalid")
	}
	return nil
}

type DiscordGatewayPayload struct {
	Op       int             `json:"op"`
	Data     json.RawMessage `json:"d"`
	Sequence *int64          `json:"s,omitempty"`
	Event    string          `json:"t,omitempty"`
}

type DiscordMessage struct {
	ID          string              `json:"id"`
	ChannelID   string              `json:"channel_id"`
	GuildID     string              `json:"guild_id,omitempty"`
	Content     string              `json:"content,omitempty"`
	Author      DiscordIdentity     `json:"author"`
	Mentions    []DiscordIdentity   `json:"mentions,omitempty"`
	Reference   *DiscordReference   `json:"referenced_message,omitempty"`
	Attachments []DiscordAttachment `json:"attachments,omitempty"`
}

type DiscordReference struct {
	ID     string          `json:"id"`
	Author DiscordIdentity `json:"author"`
}

type DiscordAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	URL         string `json:"url"`
}

func NormalizeDiscordMessage(config DiscordConfig, message DiscordMessage) (channelgateway.Envelope, bool) {
	if message.ID == "" || message.ChannelID == "" || message.Author.ID == "" || message.Author.Bot || message.Author.ID == config.BotUserID {
		return channelgateway.Envelope{}, false
	}
	group := message.GuildID != ""
	if group && !discordTriggered(config, message) {
		return channelgateway.Envelope{}, false
	}
	text := stripDiscordMention(message.Content, config.BotUserID)
	kind := channelgateway.KindMessage
	if strings.EqualFold(strings.TrimSpace(text), "/cancel") || strings.EqualFold(strings.TrimSpace(text), "/stop") {
		kind = channelgateway.KindInterrupt
	}
	scope := channelgateway.ScopeDirect
	if group {
		scope = channelgateway.ScopeGroup
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: kind,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelDiscord, AccountID: config.ApplicationID, ConversationID: message.ChannelID, ParticipantID: message.Author.ID, Scope: scope},
		ExternalEventID: message.ID, ExternalMessageID: message.ID, IdempotencyKey: "discord:" + message.ID,
		Text: text, OccurredAt: time.Now().UTC(), Metadata: map[string]string{"channel": message.ChannelID, "reply_to": message.ID},
	}
	if message.Reference != nil {
		envelope.Quote = &channelgateway.Quote{ExternalMessageID: message.Reference.ID}
	}
	for _, attachment := range message.Attachments {
		if attachment.URL == "" {
			continue
		}
		envelope.Media = append(envelope.Media, channelgateway.Media{Kind: discordMediaKind(attachment.ContentType), Ref: attachment.URL, Name: attachment.Filename, MIMEType: attachment.ContentType, Size: attachment.Size})
	}
	if envelope.Kind == channelgateway.KindMessage && strings.TrimSpace(envelope.Text) == "" && len(envelope.Media) == 0 {
		return channelgateway.Envelope{}, false
	}
	return envelope, true
}

func discordTriggered(config DiscordConfig, message DiscordMessage) bool {
	if config.GroupTrigger == "all" {
		return true
	}
	for _, mention := range message.Mentions {
		if mention.ID == config.BotUserID {
			return true
		}
	}
	return config.GroupTrigger == "mention_or_reply" && message.Reference != nil && message.Reference.Author.ID == config.BotUserID
}

func stripDiscordMention(text, botUserID string) string {
	text = strings.ReplaceAll(text, "<@"+botUserID+">", "")
	text = strings.ReplaceAll(text, "<@!"+botUserID+">", "")
	return strings.TrimSpace(text)
}

func discordMediaKind(mimeType string) channelgateway.MediaKind {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return channelgateway.MediaImage
	case strings.HasPrefix(mimeType, "audio/"):
		return channelgateway.MediaAudio
	case strings.HasPrefix(mimeType, "video/"):
		return channelgateway.MediaVideo
	default:
		return channelgateway.MediaFile
	}
}

func DiscordInteractionResponse(kind int, content string) map[string]any {
	return map[string]any{"type": kind, "data": map[string]any{"content": content, "flags": 64}}
}

func parseSnowflake(value string) (uint64, error) {
	return strconv.ParseUint(value, 10, 64)
}
