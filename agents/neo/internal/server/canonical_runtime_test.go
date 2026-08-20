// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"centra/agents/neo/internal/agent"
	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/memory"
	"centra/agents/neo/internal/tools"
	"centra/packages/vault"
)

func openCanonicalTestRuntime(
	t *testing.T,
	cfg *config.Config,
	manager *tools.Manager,
	pager *memory.Pager,
	gatewayURL string,
) *agent.ResurrectionRuntime {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "canonical-server-test-token")
	cfg.GatewayURL = gatewayURL
	cfg.RuntimeProvider.GatewayURL = gatewayURL
	cfg.RuntimeProvider.MaxAttempts = 1
	if cfg.ActorDID == "" {
		cfg.ActorDID = "did:matrix:canonical-server-test"
	}
	vaultRoot := t.TempDir()
	session, err := vault.Boot(t.Context(), vault.Config{
		Required: true,
		DataDir:  vaultRoot,
		UserDID:  cfg.ActorDID,
		KEKHex:   hex.EncodeToString(bytes.Repeat([]byte{0x53}, 32)),
	})
	if err != nil {
		t.Fatalf("boot canonical runtime vault: %v", err)
	}
	cfg.Vault = session
	cfg.VaultUser = cfg.ActorDID
	runtime, err := agent.OpenResurrectionRuntime(
		t.Context(), *cfg, manager, pager,
		filepath.Join(t.TempDir(), "turnstate.db"),
	)
	if err != nil {
		t.Fatalf("open canonical runtime: %v", err)
	}
	return runtime
}
