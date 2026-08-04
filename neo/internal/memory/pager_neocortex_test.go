// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"strings"
	"testing"

	"matrix/neo/internal/config"
)

func TestDialNeocortexRejectsMissingOrMalformedCredentials(t *testing.T) {
	validToken := strings.Repeat("01", 32)
	tests := []struct {
		name   string
		socket string
		token  string
	}{
		{name: "missing socket", token: validToken},
		{name: "missing token", socket: "/tmp/neocortex.sock"},
		{name: "short token", socket: "/tmp/neocortex.sock", token: "00"},
		{name: "non-hex token", socket: "/tmp/neocortex.sock", token: strings.Repeat("zz", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pager, err := Open(config.Config{
				NeocortexSocket: test.socket,
				NeocortexToken:  test.token,
			})
			if pager != nil {
				_ = pager.Close()
				t.Fatal("Open returned a pager for invalid credentials")
			}
			if err == nil || !strings.Contains(err.Error(), "NEO_NEOCORTEX_") {
				t.Fatalf("Open error = %v", err)
			}
		})
	}
}
