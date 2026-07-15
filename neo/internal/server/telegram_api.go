// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const telegramAPIBase = "https://api.telegram.org"

type telegramAPI struct {
	client  *http.Client
	baseURL string
}

type telegramEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
}

type telegramFileAttachment struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type telegramMessage struct {
	MessageID int64                   `json:"message_id"`
	From      *telegramUser           `json:"from"`
	Chat      telegramChat            `json:"chat"`
	Text      string                  `json:"text"`
	Caption   string                  `json:"caption"`
	Photo     []telegramPhotoSize     `json:"photo"`
	Document  *telegramFileAttachment `json:"document"`
	Video     *telegramFileAttachment `json:"video"`
	Audio     *telegramFileAttachment `json:"audio"`
	Voice     *telegramFileAttachment `json:"voice"`
	Sticker   *telegramFileAttachment `json:"sticker"`
}

type telegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Message *telegramMessage `json:"message"`
	Data    string           `json:"data"`
}

type telegramUpdate struct {
	UpdateID      int64                  `json:"update_id"`
	Message       *telegramMessage       `json:"message"`
	CallbackQuery *telegramCallbackQuery `json:"callback_query"`
}

type telegramFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

type telegramInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type telegramInlineKeyboard struct {
	InlineKeyboard [][]telegramInlineButton `json:"inline_keyboard"`
}

func newTelegramAPI() *telegramAPI {
	return &telegramAPI{
		client:  &http.Client{Timeout: 55 * time.Second},
		baseURL: telegramAPIBase,
	}
}

func (a *telegramAPI) call(ctx context.Context, token, method string, body interface{}, out interface{}) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Telegram %s request: %w", method, err)
		}
	}
	endpoint := strings.TrimRight(a.baseURL, "/") + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build Telegram %s request: %s", method, sanitizeTelegramError(err, token))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram %s request failed: %s", method, sanitizeTelegramError(err, token))
	}
	defer resp.Body.Close()
	var envelope telegramEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Telegram %s returned an unreadable response", method)
	}
	if !envelope.OK {
		detail := strings.TrimSpace(envelope.Description)
		if detail == "" {
			detail = "request rejected"
		}
		detail = redactTelegramToken(detail, token)
		if envelope.ErrorCode != 0 {
			return fmt.Errorf("Telegram %s failed (%d): %s", method, envelope.ErrorCode, detail)
		}
		return fmt.Errorf("Telegram %s failed: %s", method, detail)
	}
	if out == nil || len(envelope.Result) == 0 || string(envelope.Result) == "true" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("Telegram %s returned invalid result data", method)
	}
	return nil
}

func (a *telegramAPI) getMe(ctx context.Context, token string) (telegramUser, error) {
	var user telegramUser
	err := a.call(ctx, token, "getMe", nil, &user)
	return user, err
}

func (a *telegramAPI) deleteWebhook(ctx context.Context, token string) error {
	return a.call(ctx, token, "deleteWebhook", map[string]interface{}{"drop_pending_updates": true}, nil)
}

func (a *telegramAPI) getUpdates(ctx context.Context, token string, offset int64) ([]telegramUpdate, error) {
	var updates []telegramUpdate
	err := a.call(ctx, token, "getUpdates", map[string]interface{}{
		"offset":          offset,
		"timeout":         40,
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

func (a *telegramAPI) sendMessage(ctx context.Context, token string, chatID int64, text string, keyboard *telegramInlineKeyboard) error {
	body := map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": false,
	}
	if keyboard != nil {
		body["reply_markup"] = keyboard
	}
	return a.call(ctx, token, "sendMessage", body, nil)
}

func (a *telegramAPI) sendChatAction(ctx context.Context, token string, chatID int64) error {
	return a.call(ctx, token, "sendChatAction", map[string]interface{}{"chat_id": chatID, "action": "typing"}, nil)
}

func (a *telegramAPI) sendPhoto(ctx context.Context, token string, chatID int64, name string, data []byte) error {
	return a.sendFile(ctx, token, "sendPhoto", "photo", chatID, name, data)
}

func (a *telegramAPI) sendVideo(ctx context.Context, token string, chatID int64, name string, data []byte) error {
	return a.sendFile(ctx, token, "sendVideo", "video", chatID, name, data)
}

func (a *telegramAPI) sendFile(ctx context.Context, token, method, field string, chatID int64, name string, data []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	part, err := writer.CreateFormFile(field, name)
	if err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("encode Telegram %s request: %w", method, err)
	}
	endpoint := strings.TrimRight(a.baseURL, "/") + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("build Telegram %s request: %s", method, sanitizeTelegramError(err, token))
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram %s request failed: %s", method, sanitizeTelegramError(err, token))
	}
	defer resp.Body.Close()
	var envelope telegramEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Telegram %s returned an unreadable response", method)
	}
	if envelope.OK {
		return nil
	}
	detail := strings.TrimSpace(envelope.Description)
	if detail == "" {
		detail = "request rejected"
	}
	detail = redactTelegramToken(detail, token)
	if envelope.ErrorCode != 0 {
		return fmt.Errorf("Telegram %s failed (%d): %s", method, envelope.ErrorCode, detail)
	}
	return fmt.Errorf("Telegram %s failed: %s", method, detail)
}

func (a *telegramAPI) answerCallback(ctx context.Context, token, callbackID, text string) error {
	body := map[string]interface{}{"callback_query_id": callbackID}
	if text != "" {
		body["text"] = text
	}
	return a.call(ctx, token, "answerCallbackQuery", body, nil)
}

func (a *telegramAPI) getFile(ctx context.Context, token, fileID string) (telegramFile, error) {
	var file telegramFile
	err := a.call(ctx, token, "getFile", map[string]interface{}{"file_id": fileID}, &file)
	return file, err
}

func (a *telegramAPI) downloadFile(ctx context.Context, token, path string) (io.ReadCloser, int64, error) {
	cleanPath := strings.TrimLeft(path, "/")
	endpoint := strings.TrimRight(a.baseURL, "/") + "/file/bot" + token + "/" + cleanPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build Telegram file request: %s", sanitizeTelegramError(err, token))
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Telegram file download failed: %s", sanitizeTelegramError(err, token))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("Telegram file download failed (HTTP %d)", resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

func sanitizeTelegramError(err error, token string) string {
	if err == nil {
		return "unknown error"
	}
	message := redactTelegramToken(err.Error(), token)
	if parsed, parseErr := url.Parse(message); parseErr == nil && parsed.Host != "" {
		return parsed.Host
	}
	return message
}

func redactTelegramToken(message, token string) string {
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "[redacted]")
}

func telegramChatID(value int64) string { return strconv.FormatInt(value, 10) }
