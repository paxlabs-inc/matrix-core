// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"encoding/json"
	"testing"
)

func TestAudioWireShape(t *testing.T) {
	t.Run("ordinary text remains a string", func(t *testing.T) {
		got, err := json.Marshal(toWireMessages([]Message{UserMessage("hello")}, false))
		if err != nil {
			t.Fatal(err)
		}
		want := `[{"role":"user","content":"hello"}]`
		if string(got) != want {
			t.Fatalf("wire = %s, want %s", got, want)
		}
	})

	t.Run("user audio emits exactly one input audio part", func(t *testing.T) {
		got := toWireMessages([]Message{UserAudioMessage("voice turn", "data:audio/wav;base64,AA==")}, false)
		parts, ok := got[0].Content.([]wireContentPart)
		if !ok {
			t.Fatalf("content type = %T, want []wireContentPart", got[0].Content)
		}
		if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "voice turn" {
			t.Fatalf("parts = %#v", parts)
		}
		if parts[1].Type != "input_audio" || parts[1].InputAudio == nil || parts[1].InputAudio.Data != "data:audio/wav;base64,AA==" {
			t.Fatalf("audio part = %#v", parts[1])
		}
	})

	t.Run("non user audio data never emits input audio", func(t *testing.T) {
		msg := AssistantMessage("answer")
		msg.AudioData = "data:audio/wav;base64,AA=="
		got := toWireMessages([]Message{msg}, false)
		if content, ok := got[0].Content.(string); !ok || content != "answer" {
			t.Fatalf("content = %#v, want string answer", got[0].Content)
		}
	})
}

func TestUsageParsesAudioTokens(t *testing.T) {
	var usage Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":17,"completion_tokens":3,"total_tokens":20,"prompt_tokens_details":{"audio_tokens":11}}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokensDetails.AudioTokens != 11 {
		t.Fatalf("audio tokens = %d, want 11", usage.PromptTokensDetails.AudioTokens)
	}
}
