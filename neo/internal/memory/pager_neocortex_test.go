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
		{name: "missing token", socket: "/tmp/cortexd.sock"},
		{name: "short token", socket: "/tmp/cortexd.sock", token: "00"},
		{name: "non-hex token", socket: "/tmp/cortexd.sock", token: strings.Repeat("zz", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := dialNeocortex(config.Config{
				MemorySubstrate: config.SubstrateNeocortex,
				CortexdSocket:   test.socket,
				CortexdToken:    test.token,
			})
			if client != nil {
				_ = client.Close()
				t.Fatal("dialNeocortex returned a client for invalid credentials")
			}
			if err == nil || !strings.Contains(err.Error(), "requires NEO_CORTEXD_SOCKET and a 64-hex NEO_CORTEXD_TOKEN") {
				t.Fatalf("dialNeocortex error = %v", err)
			}
		})
	}
}
