// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionCurrentIntentFlagWiring(t *testing.T) {
	defaults := Default()
	if defaults.SessionCurrentIntent || defaults.SessionExactProjection || defaults.InteractionPosture {
		t.Fatal("hello-world session flags must default off")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "neo.kvx")
	if err := os.WriteFile(path, []byte("[session]\ncurrent_intent = true\nexact_projection = true\ninteraction_posture = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromKVX, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fromKVX.SessionCurrentIntent || !fromKVX.SessionExactProjection || !fromKVX.InteractionPosture {
		t.Fatal("[session] hello-world flags did not load")
	}

	t.Setenv("NEO_SESSION_CURRENT_INTENT", "true")
	t.Setenv("NEO_SESSION_EXACT_PROJECTION", "true")
	t.Setenv("NEO_INTERACTION_POSTURE", "true")
	fromEnv, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !fromEnv.SessionCurrentIntent || !fromEnv.SessionExactProjection || !fromEnv.InteractionPosture {
		t.Fatal("hello-world session env flags did not load")
	}
}
