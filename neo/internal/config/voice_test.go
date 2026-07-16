// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVoiceConfigDefaultsAndPrecedence(t *testing.T) {
	c := Default()
	if c.VoiceEnabled || c.VoiceMode != "native" || c.VoiceASRDeadline != 30*time.Second || c.VoiceTTSVoice != "Mia" || c.VoiceIdleDisconnect != 120*time.Second {
		t.Fatalf("defaults = enabled=%v mode=%q deadline=%s", c.VoiceEnabled, c.VoiceMode, c.VoiceASRDeadline)
	}

	path := filepath.Join(t.TempDir(), "neo.kvx")
	if err := os.WriteFile(path, []byte("[voice]\nenabled = true\nmode = \"asr_first\"\nasr_deadline_seconds = 19\ntts_voice = \"Luna\"\ntts_style = \"Quiet and clear.\"\ntts_deadline_seconds = 23\nidle_disconnect_seconds = 41\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.VoiceEnabled || c.VoiceMode != "asr_first" || c.VoiceASRDeadline != 19*time.Second || c.VoiceTTSVoice != "Luna" || c.VoiceTTSDeadline != 23*time.Second || c.VoiceIdleDisconnect != 41*time.Second {
		t.Fatalf("kvx = enabled=%v mode=%q deadline=%s", c.VoiceEnabled, c.VoiceMode, c.VoiceASRDeadline)
	}

	t.Setenv("NEO_VOICE_ENABLED", "false")
	t.Setenv("NEO_VOICE_MODE", "native")
	t.Setenv("NEO_VOICE_ASR_DEADLINE_SECONDS", "7")
	t.Setenv("NEO_VOICE_TTS_VOICE", "Mia")
	t.Setenv("NEO_VOICE_TTS_STYLE", "Direct.")
	t.Setenv("NEO_VOICE_TTS_DEADLINE_SECONDS", "11")
	t.Setenv("VOICE_IDLE_DISCONNECT_S", "17")
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.VoiceEnabled || c.VoiceMode != "native" || c.VoiceASRDeadline != 7*time.Second || c.VoiceTTSStyle != "Direct." || c.VoiceTTSDeadline != 11*time.Second || c.VoiceIdleDisconnect != 17*time.Second {
		t.Fatalf("env = enabled=%v mode=%q deadline=%s", c.VoiceEnabled, c.VoiceMode, c.VoiceASRDeadline)
	}
}

func TestVoiceConfigInvalidModeFallsBack(t *testing.T) {
	c := Default()
	if got := voiceModeOr("invalid", c.VoiceMode); got != "native" {
		t.Fatalf("invalid mode = %q, want native", got)
	}
	if got := voiceModeOr("ASR_FIRST", c.VoiceMode); got != "asr_first" {
		t.Fatalf("normalized mode = %q, want asr_first", got)
	}
}
