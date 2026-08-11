// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/o1"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/tools"
)

func TestSemanticCheckpointCompactionAtMillionAndThirtyTwoK(t *testing.T) {
	for _, window := range []int{1000000, 32000} {
		t.Run(fmt.Sprintf("window_%d", window), func(t *testing.T) {
			ctx := context.Background()
			journal := openRealAgentJournal(t)
			var (
				mu     sync.Mutex
				bodies [][]byte
			)
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, append([]byte(nil), raw...))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"acknowledged"},"finish_reason":"stop"}]}`+"\n")
				fmt.Fprint(w, "data: [DONE]\n")
			}))
			t.Cleanup(model.Close)

			cfg := config.Default()
			cfg.ContextWindowTokens = window
			cfg.SessionCurrentIntent = true
			cfg.SessionExactProjection = true
			cfg.CassandraEnabled = false
			cfg.EpistemicPremises = false
			cfg.EpistemicPredictions = false
			a := New(Options{Config: cfg, Main: windowLawClient(t, model.URL), Tools: &tools.Manager{}, ConvID: fmt.Sprintf("conv-compact-%d", window), Journal: journal})
			statements := map[int]string{
				0:  "Decision: use PostgreSQL for the session cache.",
				5:  "Correction: use SQLite instead, not PostgreSQL.",
				10: "Open question: who owns the migration review?",
				11: "Alice is my migration reviewer and works with Bob.",
				12: "I prefer deterministic migrations and never destructive resets.",
				13: "The direct rewrite approach failed and did not work.",
				14: "The old deployment task is completed and superseded.",
			}
			filler := strings.Repeat(" topic-change-context", 260)
			for turn := 0; turn < 30; turn++ {
				statement := fmt.Sprintf("Topic %d moved to a different everyday subject.", turn)
				if selected := statements[turn]; selected != "" {
					statement = selected
				}
				if err := a.Chat(ctx, statement+filler); err != nil {
					t.Fatalf("turn %d: %v", turn, err)
				}
			}
			call, err := journal.Append(ctx, sessionjournal.Event{
				ConversationID: a.cmConvID(), Kind: sessionjournal.KindToolCall,
				DisplayContent: "inspect migration", ToolCall: &sessionjournal.ToolCall{CallID: "checkpoint-call", Name: "inspect_migration"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(ctx, sessionjournal.Event{
				ConversationID: a.cmConvID(), Kind: sessionjournal.KindToolResult,
				DisplayContent: "migration checksum sha256:abc123 is valid",
				ToolResult:     &sessionjournal.ToolResult{CallID: call.ToolCall.CallID, Name: call.ToolCall.Name, Result: []byte("migration checksum sha256:abc123 is valid")},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(ctx, sessionjournal.Event{
				ConversationID: a.cmConvID(), Kind: sessionjournal.KindArtifact,
				DisplayContent: "migration plan", Artifact: &sessionjournal.Artifact{ArtifactID: "artifact-plan", Kind: "file", Path: "/workspace/migration.plan", Digest: "sha256:def456"},
			}); err != nil {
				t.Fatal(err)
			}

			a.compactExactWorking()
			checkpoint := a.SemanticCheckpoint()
			rendered := checkpoint.Render()
			for _, required := range []string{
				"use PostgreSQL", "Correction: use SQLite instead", "who owns the migration review?",
				"Alice is my migration reviewer", "deterministic migrations", "direct rewrite approach failed",
				"migration checksum sha256:abc123", "/workspace/migration.plan", "completed and superseded",
			} {
				if !strings.Contains(rendered, required) {
					t.Fatalf("checkpoint missing %q:\n%s", required, rendered)
				}
			}
			if estimateMessagesTokens(a.working) > a.protectedTailBudget() {
				t.Fatalf("protected tail exceeds budget: tokens=%d budget=%d", estimateMessagesTokens(a.working), a.protectedTailBudget())
			}
			if err := o1.ValidateConversationTruth(a.working); err != nil {
				t.Fatalf("protected tail is not protocol-shaped: %v", err)
			}
			if err := a.Chat(ctx, "Current request: tell me a one-line database joke."); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			lastBody := append([]byte(nil), bodies[len(bodies)-1]...)
			mu.Unlock()
			messages := decodeWireMessages(t, lastBody)
			sidecar := messages[len(messages)-1].Content
			if !strings.Contains(sidecar, "latest real user message always wins") || !strings.Contains(sidecar, "Correction: use SQLite instead") || a.TurnObjective() != "Current request: tell me a one-line database joke." {
				t.Fatalf("checkpoint overrode current intent:\n%s", sidecar)
			}
			events, err := journal.Read(ctx, a.cmConvID(), 1, 0)
			if err != nil || len(events) < 30 {
				t.Fatalf("journal was compacted with projection: events=%d err=%v", len(events), err)
			}
		})
	}
}
