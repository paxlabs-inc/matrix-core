// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

type audioWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func TestAudioTurnWindowAndDurableTranscript(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"heard"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "voice-window-actor"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	a := New(Options{Config: cfg, Main: windowLawClient(t, srv.URL), Tools: &tools.Manager{}, Pager: pager, ConvID: "voice-window"})

	transcript := make(chan AudioTranscript, 1)
	transcript <- AudioTranscript{Text: "the cobalt lighthouse is ready"}
	const dataURL = "data:audio/wav;base64,UklGRgAAAAA="
	if err := a.ChatAudio(context.Background(), "[attached audio: /media/voice.wav]", &AudioTurn{Ref: "/media/voice.wav", DataURL: dataURL, Transcript: transcript}); err != nil {
		t.Fatal(err)
	}

	var wire struct {
		Messages []audioWireMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) < 3 || wire.Messages[0].Role != "system" || wire.Messages[len(wire.Messages)-1].Role != "system" {
		t.Fatalf("window roles do not preserve [system, transcript, context-sidecar]: %#v", wire.Messages)
	}
	var parts []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		InputAudio *struct {
			Data string `json:"data"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(wire.Messages[1].Content, &parts); err != nil {
		t.Fatalf("audio user content: %v", err)
	}
	audioParts := 0
	for _, part := range parts {
		if part.Type == "input_audio" {
			audioParts++
			if part.InputAudio == nil || part.InputAudio.Data != dataURL {
				t.Fatalf("input audio = %#v", part.InputAudio)
			}
		}
	}
	if audioParts != 1 {
		t.Fatalf("input_audio parts = %d, want 1", audioParts)
	}

	msgs, err := pager.Transcript("voice-window", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	durable := ""
	for _, msg := range msgs {
		durable += msg.Content + "\n"
	}
	if !strings.Contains(durable, "the cobalt lighthouse is ready") || !strings.Contains(durable, "/media/voice.wav") {
		t.Fatalf("durable transcript missing spoken words or media ref: %q", durable)
	}
	if strings.Contains(durable, "UklGRg") || strings.Contains(durable, "data:audio") {
		t.Fatalf("raw audio leaked into durable transcript: %q", durable)
	}

	hits := pager.EpisodicRetrieve(context.Background(), "cobalt lighthouse", memory.EpisodicTimeWindow{}, memory.EpisodicBudget{Hits: 4, Deadline: time.Second}, nil)
	found := false
	for _, hit := range hits {
		if strings.Contains(hit.Text, "cobalt lighthouse") {
			found = true
		}
	}
	if !found {
		t.Fatalf("spoken words were not retrievable from the lexical/episodic lane: %#v", hits)
	}
}

func TestTextTurnWireByteIdenticalWithVoiceEnabled(t *testing.T) {
	run := func(enabled bool) []byte {
		t.Helper()
		var (
			mu   sync.Mutex
			body []byte
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			body = raw
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}))
		defer srv.Close()
		cfg := config.Default()
		cfg.CassandraEnabled = false
		cfg.VoiceEnabled = enabled
		cfg.DataRoot = t.TempDir()
		cfg.NeocortexActor = "voice-byte-actor"
		pager, err := memory.Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer pager.Close()
		a := New(Options{Config: cfg, Main: windowLawClient(t, srv.URL), Tools: &tools.Manager{}, Pager: pager, ConvID: "voice-byte"})
		if err := a.Chat(context.Background(), "plain typed turn"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), body...)
	}

	off, on := run(false), run(true)
	if string(off) != string(on) {
		t.Fatalf("text-only request changed when voice was enabled\noff: %s\non:  %s", off, on)
	}
}
