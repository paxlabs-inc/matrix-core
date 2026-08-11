// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tool

import (
	"path/filepath"
	"testing"
)

func TestProductionManifestsExcludeLocalCodingMCP(t *testing.T) {
	for _, name := range []string{"default.json", "neo.json"} {
		path, err := filepath.Abs(filepath.Join("../../agents", name))
		if err != nil {
			t.Fatalf("abs %s: %v", name, err)
		}
		m, err := LoadAgentManifest(path)
		if err != nil {
			t.Fatalf("LoadAgentManifest %s: %v", name, err)
		}
		aliases := make(map[string]bool, len(m.Servers))
		for _, server := range m.Servers {
			aliases[server.Alias] = true
		}
		for _, forbidden := range []string{"fs", "git", "exec"} {
			if aliases[forbidden] {
				t.Fatalf("%s exposes forbidden local coding MCP alias %q", name, forbidden)
			}
		}
		if !aliases["fetch"] {
			t.Fatalf("%s lost the remote fetch integration adapter", name)
		}
	}
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
