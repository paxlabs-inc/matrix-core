// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
	"matrix/vault"
)

func TestVoiceBridgeSubprocessAgainstRealNeoServer(t *testing.T) {
	t.Setenv(envMediaSeal, "true")
	python := filepath.Clean("../../../tools/gideon/voice/.venv/bin/python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("LiveKit Python environment is not installed")
	}
	var (
		mu       sync.Mutex
		modelReq []byte
		ttsReqs  [][]byte
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		modelReq = append([]byte(nil), raw...)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"The first spoken sentence. The second spoken sentence."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer model.Close()
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		ttsReqs = append(ttsReqs, append([]byte(nil), raw...))
		mu.Unlock()
		pcm := base64.StdEncoding.EncodeToString([]byte{1, 0, 2, 0, 3, 0, 4, 0})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":%q}}}]}\n", pcm)
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer tts.Close()
	client, err := llm.New(mcllm.Config{Model: "accounts/fireworks/models/gpt-oss-120b", Provider: mcllm.ProviderFireworks, ProviderSet: true, GatewayURL: model.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.SuperviseTasks = false
	cfg.VoiceEnabled = true
	cfg.ActorDID = "did:matrix:voice-e2e"
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "voice-bridge-actor"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	manager := &tools.Manager{}
	runtime := openCanonicalTestRuntime(t, &cfg, manager, pager, model.URL)
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Tools:           manager,
		Pager:           pager,
		Runtime:         runtime,
		ConversationDir: t.TempDir(),
		TaskDir:         t.TempDir(),
		TraceDir:        t.TempDir(),
		MediaDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
		Vault:           mediaVaultSession(t, cfg.ActorDID),
	})
	defer e.Close()
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()

	wavBytes := realSilentWAV()
	wav := filepath.Join(t.TempDir(), "turn.wav")
	if err := os.WriteFile(wav, wavBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "../../../tools/voice/probe.py", "--url", httpServer.URL, "--conversation", "voice-bridge", "--wav", wav, "--synthesize")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MIMO_API_KEY=proof-key", "MATRIX_MIMO_BASE="+tts.URL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("voice probe: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "The first spoken sentence.") || !strings.Contains(stdout.String(), "The second spoken sentence.") {
		t.Fatalf("sentence-flushed output = %q", stdout.String())
	}

	mu.Lock()
	wire := append([]byte(nil), modelReq...)
	mu.Unlock()
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(wire, &request); err != nil {
		t.Fatal(err)
	}
	foundAudio := false
	for _, message := range request.Messages {
		if strings.Contains(string(message.Content), `"type":"input_audio"`) {
			foundAudio = true
		}
	}
	if !foundAudio {
		t.Fatalf("real Neo model request has no input_audio part: %s", wire)
	}
	mu.Lock()
	spoken := append([][]byte(nil), ttsReqs...)
	mu.Unlock()
	if len(spoken) != 2 {
		t.Fatalf("sentence-flushed TTS requests = %d, want 2", len(spoken))
	}
	for i, raw := range spoken {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Audio struct {
				Format string `json:"format"`
				Voice  string `json:"voice"`
			} `json:"audio"`
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatalf("TTS request %d: %v", i, err)
		}
		if request.Model != "mimo-v2.5-tts" || request.Audio.Format != "pcm16" || request.Audio.Voice != "Mia" || !request.Stream || len(request.Messages) != 2 {
			t.Fatalf("TTS request %d shape = %#v", i, request)
		}
	}
	record := e.conv.Get("voice-bridge")
	if record == nil || len(record.Turns) < 2 || !strings.Contains(record.Turns[0].Text, "/media/") || record.Turns[1].Text != "The first spoken sentence. The second spoken sentence." {
		t.Fatalf("durable conversation missing voice media turn: %#v", record)
	}
	ref := audioRefFromMessage(record.Turns[0].Text)
	raw, err := os.ReadFile(filepath.Join(e.mediaDir, strings.TrimPrefix(ref, "/media/")))
	if err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(raw) || bytes.Contains(raw, wavBytes) {
		t.Fatal("voice upload was not sealed at rest")
	}
}

func TestVoiceWorkerCrashMidTurnLeavesTextTurnRunning(t *testing.T) {
	python := filepath.Clean("../../../tools/gideon/voice/.venv/bin/python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("LiveKit Python environment is not installed")
	}
	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var requestOnce sync.Once
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requestOnce.Do(func() { close(requestSeen) })
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"The text turn survived the voice worker crash."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer model.Close()
	client, err := llm.New(mcllm.Config{Model: "accounts/fireworks/models/gpt-oss-120b", Provider: mcllm.ProviderFireworks, ProviderSet: true, GatewayURL: model.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.SuperviseTasks = false
	cfg.VoiceEnabled = true
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "voice-crash-actor"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	manager := &tools.Manager{}
	runtime := openCanonicalTestRuntime(t, &cfg, manager, pager, model.URL)
	e := NewEngine(EngineOptions{
		Config:             cfg,
		Main:               client,
		Tools:              manager,
		Pager:              pager,
		Runtime:            runtime,
		ConversationDir:    t.TempDir(),
		TaskDir:            t.TempDir(),
		TraceDir:           t.TempDir(),
		MediaDir:           t.TempDir(),
		VoiceControllerURL: "http://127.0.0.1:1",
		BackendURL:         "http://127.0.0.1:1",
	})
	defer e.Close()
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	wav := filepath.Join(t.TempDir(), "turn.wav")
	if err := os.WriteFile(wav, realSilentWAV(), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "../../../tools/voice/probe.py", "--url", httpServer.URL, "--conversation", "voice-crash", "--wav", wav)
	cmd.Dir = "."
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestSeen:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("voice worker did not dispatch the turn")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	close(release)
	waitVoiceTextTurn(t, e, "voice-crash", "The text turn survived the voice worker crash.")

	deadTTS := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadTTSURL := deadTTS.URL
	deadTTS.Close()
	cmd = exec.Command(python, "../../../tools/voice/probe.py", "--url", httpServer.URL, "--conversation", "voice-dead-tts", "--wav", wav, "--synthesize")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MIMO_API_KEY=proof-key", "MATRIX_MIMO_BASE="+deadTTSURL, "NEO_VOICE_TTS_DEADLINE_SECONDS=0.2")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		t.Fatal("dead TTS unexpectedly synthesized audio")
	}
	if !strings.Contains(stdout.String(), "The text turn survived the voice worker crash.") {
		t.Fatalf("dead TTS hid the streamed text: %q", stdout.String())
	}
	waitVoiceTextTurn(t, e, "voice-dead-tts", "The text turn survived the voice worker crash.")

	startBody := strings.NewReader(`{"conversation_id":"voice-dead-livekit"}`)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/voice/session/start", startBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead voice controller status = %d, want 503", rec.Code)
	}
	chatBody := strings.NewReader(`{"conversation_id":"voice-dead-livekit","message":"typed chat remains available"}`)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/chat", chatBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("typed chat after dead voice transport = %d: %s", rec.Code, rec.Body.String())
	}
	waitVoiceTextTurn(t, e, "voice-dead-livekit", "The text turn survived the voice worker crash.")
}

func TestVoiceBargeInIsSingleFlightAndCancelsPriorRun(t *testing.T) {
	python := filepath.Clean("../../../tools/gideon/voice/.venv/bin/python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("LiveKit Python environment is not installed")
	}
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"The replacement voice turn completed."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer model.Close()
	client, err := llm.New(mcllm.Config{Model: "accounts/fireworks/models/gpt-oss-120b", Provider: mcllm.ProviderFireworks, ProviderSet: true, GatewayURL: model.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.SuperviseTasks = false
	cfg.VoiceEnabled = true
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "voice-barge-actor"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	manager := &tools.Manager{}
	runtime := openCanonicalTestRuntime(t, &cfg, manager, pager, model.URL)
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Tools:           manager,
		Pager:           pager,
		Runtime:         runtime,
		ConversationDir: t.TempDir(),
		TaskDir:         t.TempDir(),
		TraceDir:        t.TempDir(),
		MediaDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	defer e.Close()
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	wav := filepath.Join(t.TempDir(), "turn.wav")
	if err := os.WriteFile(wav, realSilentWAV(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "../../../tools/voice/probe.py", "--url", httpServer.URL, "--conversation", "voice-barge", "--wav", wav, "--interrupt-wav", wav, "--interrupt-after", "0.5")
	cmd.Dir = "."
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("barge-in probe: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls=%d, want interrupted run plus replacement", calls.Load())
	}
	if !strings.Contains(stdout.String(), "stopping the previous reply") || !strings.Contains(stdout.String(), "The replacement voice turn completed.") {
		t.Fatalf("barge-in output = %q", stdout.String())
	}
	waitVoiceTextTurn(t, e, "voice-barge", "The replacement voice turn completed.")
	record := e.conv.Get("voice-barge")
	firstIntent := ""
	for _, turn := range record.Turns {
		if turn.Role == "user" {
			firstIntent = turn.IntentID
			break
		}
	}
	if firstIntent == "" || !hasTerminal(collectUntilClosed(t, e, firstIntent, time.Second), "interrupted") {
		t.Fatalf("prior voice run was not durably interrupted: %#v", record)
	}
}

func waitVoiceTextTurn(t *testing.T, e *Engine, conversationID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		record := e.conv.Get(conversationID)
		if record != nil && len(record.Turns) >= 2 && e.activeRunForConv(conversationID) == "" {
			last := record.Turns[len(record.Turns)-1]
			if last.Role != "assistant" || last.Text != want {
				t.Fatalf("last turn = %#v", last)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("text turn did not complete for %s: %#v", conversationID, e.conv.Get(conversationID))
}

func realSilentWAV() []byte {
	const samples = 2400
	data := make([]byte, samples*2)
	buf := bytes.NewBuffer(make([]byte, 0, 44+len(data)))
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(buf, binary.LittleEndian, uint32(24000))
	_ = binary.Write(buf, binary.LittleEndian, uint32(48000))
	_ = binary.Write(buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}
