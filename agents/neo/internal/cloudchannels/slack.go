// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

const slackBodyLimit = 2 << 20

type SlackAuth struct {
	TeamID    string
	BotUserID string
	URL       string
}

type SlackAPI struct {
	Client  *http.Client
	BaseURL string
}

func NewSlackAPI() *SlackAPI {
	return &SlackAPI{Client: &http.Client{Timeout: 30 * time.Second}, BaseURL: "https://slack.com/api"}
}

func (a *SlackAPI) AuthTest(ctx context.Context, token string) (SlackAuth, error) {
	var response struct {
		OK     bool   `json:"ok"`
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		URL    string `json:"url"`
		Error  string `json:"error"`
	}
	if err := a.call(ctx, token, "auth.test", nil, &response); err != nil {
		return SlackAuth{}, err
	}
	if !response.OK || response.TeamID == "" || response.UserID == "" {
		return SlackAuth{}, fmt.Errorf("Slack rejected the bot identity: %s", response.Error)
	}
	return SlackAuth{TeamID: response.TeamID, BotUserID: response.UserID, URL: response.URL}, nil
}

func (a *SlackAPI) OpenSocket(ctx context.Context, appToken string) (string, error) {
	var response struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := a.call(ctx, appToken, "apps.connections.open", map[string]any{}, &response); err != nil {
		return "", err
	}
	endpoint, parseErr := url.Parse(response.URL)
	base, _ := url.Parse(a.BaseURL)
	testEndpoint := base != nil && base.Hostname() != "slack.com" && endpoint != nil && endpoint.Hostname() == base.Hostname() && endpoint.Scheme == "ws"
	if !response.OK || parseErr != nil || endpoint == nil || (endpoint.Scheme != "wss" && !testEndpoint) {
		return "", fmt.Errorf("Slack could not open Socket Mode: %s", response.Error)
	}
	return response.URL, nil
}

func (a *SlackAPI) PostMessage(ctx context.Context, token, channel, thread, text string, blocks any) (channelgateway.SendReceipt, error) {
	body := map[string]any{"channel": channel, "text": text}
	if thread != "" {
		body["thread_ts"] = thread
	}
	if blocks != nil {
		body["blocks"] = blocks
	}
	var response struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := a.call(ctx, token, "chat.postMessage", body, &response); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if !response.OK {
		permanent := response.Error == "channel_not_found" || response.Error == "not_in_channel" || response.Error == "invalid_auth"
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: response.Error, Message: "Slack delivery failed: " + response.Error, Permanent: permanent}
	}
	return channelgateway.SendReceipt{ExternalMessageID: response.TS, Code: "accepted"}, nil
}

func (a *SlackAPI) PostEphemeral(ctx context.Context, token, channel, user, thread, text string) (channelgateway.SendReceipt, error) {
	body := map[string]any{"channel": channel, "user": user, "text": text}
	if thread != "" {
		body["thread_ts"] = thread
	}
	var response struct {
		OK        bool   `json:"ok"`
		MessageTS string `json:"message_ts"`
		Error     string `json:"error"`
	}
	if err := a.call(ctx, token, "chat.postEphemeral", body, &response); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if !response.OK {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: response.Error, Message: "Slack progress delivery failed: " + response.Error}
	}
	return channelgateway.SendReceipt{ExternalMessageID: response.MessageTS, Code: "accepted"}, nil
}

func (a *SlackAPI) UploadFile(ctx context.Context, token, channel, thread, name string, data []byte, caption string) (channelgateway.SendReceipt, error) {
	if len(data) == 0 || len(data) > 20<<20 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "media_size", Message: "Slack media must be between 1 byte and 20 MB", Permanent: true}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "" {
		name = "neo-media"
	}
	var allocated struct {
		OK        bool   `json:"ok"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
		Error     string `json:"error"`
	}
	if err := a.call(ctx, token, "files.getUploadURLExternal", map[string]any{"filename": name, "length": len(data)}, &allocated); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	parsed, err := url.Parse(allocated.UploadURL)
	base, _ := url.Parse(a.BaseURL)
	trustedTestEndpoint := base != nil && base.Hostname() != "slack.com" && base.Hostname() != "www.slack.com" && parsed.Host == base.Host
	if err != nil || !allocated.OK || allocated.FileID == "" || (parsed.Scheme != "https" && !trustedTestEndpoint) || (!trustedTestEndpoint && parsed.Hostname() != "files.slack.com" && !strings.HasSuffix(parsed.Hostname(), ".slack.com")) {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "protocol", Message: "Slack returned an invalid external upload allocation"}
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("filename", name)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if _, err := part.Write(data); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if err := writer.Close(); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), &upload)
	if err != nil {
		return channelgateway.SendReceipt{}, err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := a.client().Do(request)
	if err != nil {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, slackBodyLimit))
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: "upload", Message: "Slack media upload returned " + response.Status}
	}
	complete := map[string]any{"files": []map[string]string{{"id": allocated.FileID, "title": name}}, "channel_id": channel, "initial_comment": caption}
	if thread != "" {
		complete["thread_ts"] = thread
	}
	var completed struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := a.call(ctx, token, "files.completeUploadExternal", complete, &completed); err != nil {
		return channelgateway.SendReceipt{}, err
	}
	if !completed.OK {
		return channelgateway.SendReceipt{}, &channelgateway.DeliveryError{Code: completed.Error, Message: "Slack media completion failed: " + completed.Error}
	}
	return channelgateway.SendReceipt{ExternalMessageID: allocated.FileID, Code: "accepted"}, nil
}

func (a *SlackAPI) Download(ctx context.Context, token, rawURL string, maximum int64) ([]byte, string, error) {
	if maximum <= 0 {
		return nil, "", errors.New("Slack media limit is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "files.slack.com" && !strings.HasSuffix(parsed.Hostname(), ".slack-files.com")) {
		return nil, "", errors.New("Slack media URL is outside Slack's file service")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.client().Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Slack media download returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maximum {
		return nil, "", fmt.Errorf("Slack media exceeds the %d-byte limit", maximum)
	}
	return data, response.Header.Get("Content-Type"), nil
}

func (a *SlackAPI) call(ctx context.Context, token, method string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/"+method, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client().Do(request)
	if err != nil {
		return &channelgateway.DeliveryError{Code: "transport", Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		return &channelgateway.DeliveryError{Code: "rate_limited", Message: "Slack rate limit", RetryAfter: time.Duration(retry) * time.Second}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &channelgateway.DeliveryError{Code: "http", Message: "Slack returned " + response.Status, Permanent: response.StatusCode >= 400 && response.StatusCode < 500}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, slackBodyLimit))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Slack response: %w", err)
	}
	return nil
}

func (a *SlackAPI) client() *http.Client {
	if a != nil && a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func VerifySlackRequest(header http.Header, body []byte, secret string, now time.Time) error {
	timestamp := strings.TrimSpace(header.Get("X-Slack-Request-Timestamp"))
	signature := strings.TrimSpace(header.Get("X-Slack-Signature"))
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || signature == "" {
		return errors.New("Slack signature headers are missing")
	}
	if delta := now.UTC().Sub(time.Unix(unix, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
		return errors.New("Slack request timestamp is outside the replay window")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, "v0:"+timestamp+":")
	_, _ = mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("Slack request signature is invalid")
	}
	return nil
}

type SlackEventPayload struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge,omitempty"`
	TeamID    string          `json:"team_id,omitempty"`
	EventID   string          `json:"event_id,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
}

type SlackMessageEvent struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype,omitempty"`
	BotID       string      `json:"bot_id,omitempty"`
	User        string      `json:"user,omitempty"`
	Text        string      `json:"text,omitempty"`
	Channel     string      `json:"channel"`
	ChannelType string      `json:"channel_type,omitempty"`
	TS          string      `json:"ts"`
	ThreadTS    string      `json:"thread_ts,omitempty"`
	Files       []SlackFile `json:"files,omitempty"`
}

type SlackFile struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	MIMEType   string `json:"mimetype,omitempty"`
	Size       int64  `json:"size,omitempty"`
	PrivateURL string `json:"url_private_download,omitempty"`
}

type SlackActionPayload struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	Team      struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

func NormalizeSlackEvent(config SlackConfig, payload SlackEventPayload) (channelgateway.Envelope, SlackMessageEvent, bool, error) {
	if payload.Type != "event_callback" || payload.EventID == "" || payload.TeamID != config.TeamID {
		return channelgateway.Envelope{}, SlackMessageEvent{}, false, nil
	}
	var event SlackMessageEvent
	if err := json.Unmarshal(payload.Event, &event); err != nil {
		return channelgateway.Envelope{}, SlackMessageEvent{}, false, err
	}
	if event.Type != "message" && event.Type != "app_mention" {
		return channelgateway.Envelope{}, event, false, nil
	}
	if event.BotID != "" || event.User == config.BotUserID || event.Subtype != "" || event.Channel == "" || event.TS == "" {
		return channelgateway.Envelope{}, event, false, nil
	}
	group := event.ChannelType != "im"
	if group && !slackTriggered(config, event) {
		return channelgateway.Envelope{}, event, false, nil
	}
	text := stripSlackMention(event.Text, config.BotUserID)
	kind := channelgateway.KindMessage
	if strings.EqualFold(strings.TrimSpace(text), "/cancel") || strings.EqualFold(strings.TrimSpace(text), "/stop") {
		kind = channelgateway.KindInterrupt
	}
	scope := channelgateway.ScopeDirect
	if group {
		scope = channelgateway.ScopeGroup
	}
	conversation := event.Channel
	if group && event.ThreadTS != "" {
		conversation += ":" + event.ThreadTS
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: kind,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelSlack, AccountID: config.TeamID, ConversationID: conversation, ParticipantID: event.User, Scope: scope},
		ExternalEventID: payload.EventID, ExternalMessageID: event.TS, IdempotencyKey: "slack:" + payload.EventID,
		Text: text, OccurredAt: slackTime(event.TS), Metadata: map[string]string{"channel": event.Channel, "thread_ts": firstNonEmpty(event.ThreadTS, event.TS)},
	}
	for _, file := range event.Files {
		if file.PrivateURL == "" {
			continue
		}
		envelope.Media = append(envelope.Media, channelgateway.Media{Kind: slackMediaKind(file.MIMEType), Ref: file.PrivateURL, Name: file.Name, MIMEType: file.MIMEType, Size: file.Size})
	}
	if envelope.Kind == channelgateway.KindMessage && strings.TrimSpace(envelope.Text) == "" && len(envelope.Media) == 0 {
		return channelgateway.Envelope{}, event, false, nil
	}
	return envelope, event, true, nil
}

func slackTriggered(config SlackConfig, event SlackMessageEvent) bool {
	if config.GroupTrigger == "all" || event.Type == "app_mention" {
		return true
	}
	mentioned := config.BotUserID != "" && strings.Contains(event.Text, "<@"+config.BotUserID+">")
	if mentioned {
		return true
	}
	return config.GroupTrigger == "mention_or_reply" && event.ThreadTS != ""
}

func stripSlackMention(text, botUserID string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "<@"+botUserID+">", ""))
}

func slackTime(ts string) time.Time {
	seconds, _ := strconv.ParseFloat(ts, 64)
	if seconds <= 0 {
		return time.Now().UTC()
	}
	nanos := int64(seconds * float64(time.Second))
	return time.Unix(0, nanos).UTC()
}

func slackMediaKind(mimeType string) channelgateway.MediaKind {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
