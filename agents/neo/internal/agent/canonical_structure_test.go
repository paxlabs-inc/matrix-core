// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionRuntimeAliasesHaveOneConstructionAndOneChatPath(t *testing.T) {
	servePath := filepath.Join("..", "..", "cmd", "neo", "serve.go")
	serve, err := os.ReadFile(servePath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(serve)
	if strings.Count(source, "agent.OpenResurrectionRuntime(") != 1 ||
		!strings.Contains(source, `case "", "legacy":`) ||
		!strings.Contains(source, `case "resurrection":`) ||
		strings.Count(source, "runtime: canonical") != 1 {
		t.Fatalf("daemon runtime routing is not single-path")
	}
	agentSource, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(agentSource)
	if !strings.Contains(text, `a.runtimeMode = "canonical"`) ||
		!strings.Contains(text, "return a.chatResurrection(ctx, userInput)") {
		t.Fatal("agent public Chat is not routed to the canonical path")
	}
}
