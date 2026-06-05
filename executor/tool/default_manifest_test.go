// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"path/filepath"
	"testing"
)

// TestDefaultAgentManifest sanity-checks /root/matrix/agents/default.json
// loads cleanly through the validator. Path-relative so the test moves
// with the repo.
func TestDefaultAgentManifest(t *testing.T) {
	path, err := filepath.Abs("../../agents/default.json")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	m, err := LoadAgentManifest(path)
	if err != nil {
		t.Fatalf("LoadAgentManifest: %v", err)
	}
	wantAliases := map[string]bool{"fs": false, "fetch": false, "git": false}
	for _, s := range m.Servers {
		if _, ok := wantAliases[s.Alias]; ok {
			wantAliases[s.Alias] = true
		}
	}
	for alias, found := range wantAliases {
		if !found {
			t.Fatalf("default manifest missing server alias %q", alias)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
