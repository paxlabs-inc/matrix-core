// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/agents/neo/internal/config"
)

func TestPrepareVoiceInputFailOpenModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "turn.wav"), []byte("RIFF\x00\x00\x00\x00WAVE"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := "please respond\n\n[attached audio: /media/turn.wav]"

	disabled := &Engine{cfg: config.Default(), mediaDir: dir}
	got, audio, voice, err := disabled.prepareVoiceInput(context.Background(), message)
	if err != nil || got != message || audio != nil || voice {
		t.Fatalf("disabled = message=%q audio=%v voice=%v err=%v", got, audio, voice, err)
	}

	nativeCfg := config.Default()
	nativeCfg.VoiceEnabled = true
	native := &Engine{cfg: nativeCfg, mediaDir: dir}
	got, audio, voice, err = native.prepareVoiceInput(context.Background(), message)
	if err != nil || got != message || audio == nil || !voice {
		t.Fatalf("native = message=%q audio=%v voice=%v err=%v", got, audio, voice, err)
	}
	if audio.Ref != "/media/turn.wav" || !strings.HasPrefix(audio.DataURL, "data:audio/wav;base64,") {
		t.Fatalf("native audio = %#v", audio)
	}
	result := <-audio.Transcript
	if result.Err == nil || result.Text != "" {
		t.Fatalf("missing ASR must report an internal failure for durable fail-open, got %#v", result)
	}

	asrCfg := nativeCfg
	asrCfg.VoiceMode = "asr_first"
	asrFirst := &Engine{cfg: asrCfg, mediaDir: dir}
	got, audio, voice, err = asrFirst.prepareVoiceInput(context.Background(), message)
	if err != nil || audio != nil || !voice {
		t.Fatalf("asr_first = message=%q audio=%v voice=%v err=%v", got, audio, voice, err)
	}
	want := "[Voice message could not be transcribed]\n\n[attached audio: /media/turn.wav]"
	if got != want {
		t.Fatalf("asr_first fallback = %q, want %q", got, want)
	}
}

func TestPrepareVoiceInputRejectsUnsafeAudio(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.VoiceEnabled = true
	e := &Engine{cfg: cfg, mediaDir: dir}

	if err := os.WriteFile(filepath.Join(dir, "turn.txt"), []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, voice, err := e.prepareVoiceInput(context.Background(), "[attached audio: /media/turn.txt]")
	if err == nil || !voice || strings.Contains(strings.ToLower(err.Error()), "provider") {
		t.Fatalf("invalid audio error = %v voice=%v", err, voice)
	}

	tooLarge := make([]byte, maxVoiceEncodedBytes*3/4+1)
	if err := os.WriteFile(filepath.Join(dir, "huge.wav"), tooLarge, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, voice, err = e.prepareVoiceInput(context.Background(), "[attached audio: /media/huge.wav]")
	if err == nil || !voice || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized audio error = %v voice=%v", err, voice)
	}
}
