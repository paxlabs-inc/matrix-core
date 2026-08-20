// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"centra/agents/neo/internal/agent"
)

const maxVoiceEncodedBytes = 10 << 20

var attachedAudioRE = regexp.MustCompile(`\[attached audio:\s*(/media/[^\]\s]+)(?:\s+\([^)]+\))?\]`)

func audioRefFromMessage(message string) string {
	match := attachedAudioRE.FindStringSubmatch(message)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func (e *Engine) prepareVoiceInput(ctx context.Context, message string) (string, *agent.AudioTurn, bool, error) {
	if !e.cfg.VoiceEnabled {
		return message, nil, false, nil
	}
	ref := audioRefFromMessage(message)
	if ref == "" {
		return message, nil, false, nil
	}
	dataURL, err := e.loadVoiceDataURL(ref)
	if err != nil {
		return "", nil, true, err
	}
	if e.cfg.VoiceMode == "asr_first" {
		transcript, _ := e.transcribeVoice(ctx, dataURL)
		return durableVoiceText(transcript, ref), nil, true, nil
	}
	result := make(chan agent.AudioTranscript, 1)
	go func() {
		transcript, err := e.transcribeVoice(context.Background(), dataURL)
		result <- agent.AudioTranscript{Text: transcript, Err: err}
	}()
	return message, &agent.AudioTurn{Ref: ref, DataURL: dataURL, Transcript: result}, true, nil
}

func durableVoiceText(transcript, ref string) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		transcript = "[Voice message could not be transcribed]"
	}
	return transcript + "\n\n[attached audio: " + ref + "]"
}

func (e *Engine) loadVoiceDataURL(ref string) (string, error) {
	if e.mediaDir == "" || !strings.HasPrefix(ref, "/media/") {
		return "", fmt.Errorf("voice audio storage is not configured")
	}
	name := safeMediaName(strings.TrimPrefix(ref, "/media/"))
	if name == "" {
		return "", fmt.Errorf("bad audio reference")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".wav" && ext != ".mp3" {
		return "", fmt.Errorf("voice input must be wav or mp3")
	}
	f, err := os.Open(filepath.Join(e.mediaDir, name))
	if err != nil {
		return "", fmt.Errorf("voice audio not found")
	}
	defer f.Close()
	var reader io.Reader = f
	if sealed, serr := sniffVaultFile(f); serr != nil {
		return "", fmt.Errorf("voice audio unreadable")
	} else if sealed {
		if e.vault == nil {
			return "", fmt.Errorf("voice audio requires the data vault")
		}
		uv := e.vault.UserVault()
		if uv == nil {
			return "", fmt.Errorf("voice audio requires the data vault")
		}
		sr, err := uv.StreamReader(f, e.mediaAD(name))
		if err != nil {
			return "", fmt.Errorf("voice audio unreadable")
		}
		reader = sr
	}
	maxRaw := maxVoiceEncodedBytes * 3 / 4
	data, err := io.ReadAll(io.LimitReader(reader, int64(maxRaw+1)))
	if err != nil {
		return "", fmt.Errorf("voice audio unreadable")
	}
	if len(data) > maxRaw {
		return "", fmt.Errorf("voice audio exceeds the 10 MB encoded-input limit")
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) > maxVoiceEncodedBytes {
		return "", fmt.Errorf("voice audio exceeds the 10 MB encoded-input limit")
	}
	mime := "audio/wav"
	if ext == ".mp3" {
		mime = "audio/mpeg"
	}
	return "data:" + mime + ";base64," + encoded, nil
}

func (e *Engine) transcribeVoice(parent context.Context, dataURL string) (string, error) {
	if e.voiceASRURL == "" || e.voiceASRKey == "" {
		return "", fmt.Errorf("voice ASR is not configured")
	}
	deadline := e.cfg.VoiceASRDeadline
	if deadline <= 0 {
		deadline = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model": "mimo-v2.5-asr",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]string{"data": dataURL},
			}},
		}},
		"asr_options": map[string]string{"language": "auto"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.voiceASRURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+e.voiceASRKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("voice ASR unavailable")
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("voice ASR returned no transcript")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
