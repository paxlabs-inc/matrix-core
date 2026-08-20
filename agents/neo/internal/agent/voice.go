// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"strings"
)

// AudioTranscript is the bounded ASR lane's terminal result.
type AudioTranscript struct {
	Text string
	Err  error
}

// AudioTurn carries one sealed media reference, its request-only data URL, and
// the concurrently running ASR result into the ordinary Chat loop.
type AudioTurn struct {
	Ref          string
	DataURL      string
	Transcript   <-chan AudioTranscript
	OnTranscript func(string)
}

func audioData(audio *AudioTurn) string {
	if audio == nil {
		return ""
	}
	return strings.TrimSpace(audio.DataURL)
}

func (a *Agent) finalizeAudioTurn(ctx context.Context, index int, audio *AudioTurn) string {
	if audio == nil {
		return ""
	}
	var transcript string
	if audio.Transcript != nil {
		select {
		case result := <-audio.Transcript:
			if result.Err == nil {
				transcript = strings.TrimSpace(result.Text)
			}
		case <-ctx.Done():
		}
	}
	durable := durableAudioText(transcript, audio.Ref)
	if index >= 0 && index < len(a.working) {
		a.working[index].Content = durable
	}
	a.cmRecordUser(durable)
	if audio.OnTranscript != nil {
		audio.OnTranscript(durable)
	}
	return durable
}

func durableAudioText(transcript, ref string) string {
	transcript = strings.TrimSpace(transcript)
	ref = strings.TrimSpace(ref)
	marker := "[Voice message could not be transcribed]"
	if transcript != "" {
		marker = transcript
	}
	if ref == "" {
		return marker
	}
	return marker + "\n\n[attached audio: " + ref + "]"
}
