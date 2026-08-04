// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package export

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/cortexclient"
	"matrix/vault"
)

func cortexdBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("NEOCORTEX_CORTEXD"); path != "" {
		return path
	}
	for _, candidate := range []string{
		"../../neocortex/build/debug/cortexd",
		"../../neocortex/build/repro-a/cortexd",
	} {
		absolute, err := filepath.Abs(candidate)
		if err == nil {
			if _, statErr := os.Stat(absolute); statErr == nil {
				return absolute
			}
		}
	}
	t.Fatalf("real cortexd binary not found; build neocortex or set NEOCORTEX_CORTEXD")
	return ""
}

func startCortexd(t *testing.T) (string, *cortexclient.Client, *cortexclient.Client) {
	t.Helper()
	directory := t.TempDir()
	socket := filepath.Join(directory, "cortexd.sock")
	configPath := filepath.Join(directory, "cortexd.conf")
	config := "socket=" + socket + "\n" +
		"data=" + filepath.Join(directory, "data") + "\n" +
		"user=0102030405060708090a0b0c0d0e0f10\n" +
		"kek=1111111111111111111111111111111111111111111111111111111111111111\n" +
		"signing_seed=2222222222222222222222222222222222222222222222222222222222222222\n" +
		"admin_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"actor=11:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write cortexd config: %v", err)
	}
	supervisor := &cortexclient.Supervisor{
		Binary:       cortexdBinary(t),
		ConfigPath:   configPath,
		RestartDelay: 100 * time.Millisecond,
	}
	if err := supervisor.Start(); err != nil {
		t.Fatalf("start cortexd: %v", err)
	}
	t.Cleanup(supervisor.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil &&
			info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cortexd socket never appeared at %s", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var actorToken, adminToken [32]byte
	for index := range actorToken {
		actorToken[index] = 0xbb
		adminToken[index] = 0xaa
	}
	actorID := [16]byte{0x71, 15: 0x71 ^ 0xa5}
	client, err := cortexclient.Dial(cortexclient.Config{
		SocketPath:      socket,
		CapabilityToken: actorToken,
		ConnectionID:    actorID,
	})
	if err != nil {
		t.Fatalf("dial cortexd: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	adminID := [16]byte{0x72, 15: 0x72 ^ 0xa5}
	admin, err := cortexclient.Dial(cortexclient.Config{
		SocketPath:      socket,
		CapabilityToken: adminToken,
		ConnectionID:    adminID,
	})
	if err != nil {
		t.Fatalf("dial cortexd admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	return socket, client, admin
}

func memoryID(t *testing.T, c *cortex.Cortex, uri memory.URI) memory.ID {
	t.Helper()
	resolved, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("resolve %s: %v", uri, err)
	}
	return resolved.Head.ID
}

func TestExportRoundTripThroughRealCortexd(t *testing.T) {
	sourceDir := t.TempDir()
	session, err := vault.Boot(t.Context(), vault.Config{
		Required: true, DataDir: sourceDir,
		UserDID: "did:matrix:export-test",
		KEKHex:  hex.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)),
	})
	if err != nil {
		t.Fatalf("boot vault: %v", err)
	}
	const actor = "export-actor"
	source, err := store.Open(sourceDir, actor, nil)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	source.SetVault(session, "did:matrix:export-test")
	c := cortex.New(source)

	appendMessage := func(m cortex.Message) {
		t.Helper()
		if _, err := c.AppendMessage(m); err != nil {
			t.Fatalf("append message %+v: %v", m.Role, err)
		}
	}
	appendMessage(cortex.Message{
		ConversationID: "conv-alpha", Role: cortex.RoleUser,
		Content: "what is the status of moltbook.com",
	})
	appendMessage(cortex.Message{
		ConversationID: "conv-alpha", Role: cortex.RoleToolCall,
		ToolName: "web_fetch",
		ToolArgs: []byte(`{"url":"https://moltbook.com"}`),
		Content:  "checking the domain",
	})
	appendMessage(cortex.Message{
		ConversationID: "conv-alpha", Role: cortex.RoleToolResult,
		ToolName: "web_fetch", Content: "moltbook lives",
	})
	appendMessage(cortex.Message{
		ConversationID: "conv-alpha", Role: cortex.RoleAssistant,
		Content: "moltbook.com is alive and serving traffic",
	})
	appendMessage(cortex.Message{
		ConversationID: "conv-alpha", Role: cortex.RoleSystem,
		Content: "system framing that has no evidence kind",
	})
	spilled := strings.Repeat("z", 9000) + " SPILLMARK"
	appendMessage(cortex.Message{
		ConversationID: "conv-beta", Role: cortex.RoleToolCall,
		ToolName: "exec_shell", ToolArgs: []byte(`{"command":"ls"}`),
	})
	appendMessage(cortex.Message{
		ConversationID: "conv-beta", Role: cortex.RoleToolResult,
		ToolName: "exec_shell", Content: spilled,
	})

	writeFact := func(statement string) memory.URI {
		t.Helper()
		uri, err := c.Write(memory.Head{ActorScope: actor},
			memory.FactData{
				SchemaVersion: 1, Statement: statement,
				Subject: "matrix://export-test", Predicate: "status",
			},
			cortex.WriteMeta{
				CreatedBy: "export-test",
				Forms: memory.Forms{
					Short:  statement,
					Medium: statement,
				},
				Provenance: memory.Provenance{Source: memory.SourceUserInput},
			})
		if err != nil {
			t.Fatalf("write fact %q: %v", statement, err)
		}
		return uri
	}
	superseded := writeFact("the paxeer chain is halted at height 7118247")
	superseding := writeFact("the paxeer chain recovered and is producing blocks")
	conflicting := writeFact("moltbook.com is alive and serving traffic today")
	doomed := writeFact("temporary scratch fact scheduled for retraction")

	supersededID := memoryID(t, c, superseded)
	supersedingID := memoryID(t, c, superseding)
	conflictingID := memoryID(t, c, conflicting)
	if err := c.AddEdge(supersedingID, memory.EdgeSupersedes, supersededID,
		cortex.AddEdgeMeta{CreatedBy: "export-test"}); err != nil {
		t.Fatalf("add supersedes edge: %v", err)
	}
	if err := c.AddEdge(conflictingID, memory.EdgeContradicts, supersedingID,
		cortex.AddEdgeMeta{CreatedBy: "export-test"}); err != nil {
		t.Fatalf("add contradicts edge: %v", err)
	}
	if err := c.Tombstone(doomed, "superseded by run", "export-test"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if _, err := c.RecordToolEvent(cortex.ToolEvent{
		CallID: "call-tev-1", ToolName: "web_fetch",
		Arguments:      json.RawMessage(`{"url":"https://moltbook.com"}`),
		Result:         json.RawMessage(`{"status":200}`),
		Expect:         "fetch the homepage",
		MatchVerdict:   "matched",
		SubgoalID:      "root",
		IdempotencyKey: "idem-tev-1",
		CreatedBy:      "export-test",
	}); err != nil {
		t.Fatalf("record tool event: %v", err)
	}

	// The probe suite is what the OLD substrate demonstrably holds: the
	// transcript reads back and every live fact resolves.
	transcript, err := c.Transcript("conv-alpha", 0, 16)
	if err != nil || len(transcript) != 5 {
		t.Fatalf("old transcript len=%d err=%v", len(transcript), err)
	}
	for _, uri := range []memory.URI{superseded, superseding, conflicting} {
		if _, err := c.Resolve(uri); err != nil {
			t.Fatalf("old substrate lost %s: %v", uri, err)
		}
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}

	_, client, admin := startCortexd(t)
	report, err := Run(t.Context(), Options{
		Root: sourceDir, Actor: actor,
		Vault: session, VaultUser: "did:matrix:export-test",
		Client: client,
	})
	if err != nil {
		t.Fatalf("export run: %v\nreport: %+v", err, report)
	}
	if err := report.Complete(); err != nil {
		t.Fatalf("fidelity gate: %v", err)
	}

	if report.Sessions.Source != 7 || report.Sessions.Transformed != 6 ||
		report.Sessions.Skipped["system role has no evidence kind"] != 1 {
		t.Fatalf("sessions lane: %+v", report.Sessions)
	}
	if report.SessionBlobs.Source != 1 || report.SessionBlobs.Transformed != 1 {
		t.Fatalf("session blobs lane: %+v", report.SessionBlobs)
	}
	if report.Memories.Source != 8 || report.Memories.Transformed != 8 {
		t.Fatalf("memories lane: %+v", report.Memories)
	}
	if report.ToolEvents.Source != 1 || report.ToolEvents.Imported != 1 {
		t.Fatalf("tool events lane: %+v", report.ToolEvents)
	}
	if report.Edges.Source != 2 || report.Edges.Transformed != 2 {
		t.Fatalf("edges lane: %+v", report.Edges)
	}
	if report.Journal.Source == 0 || report.Journal.SkippedTotal() !=
		report.Journal.Source {
		t.Fatalf("journal lane: %+v", report.Journal)
	}
	if report.EventsAppended == 0 || report.LastLsn != report.EventsAppended ||
		report.LeafCount != report.EventsAppended {
		t.Fatalf("append accounting: %+v", report)
	}

	// MEMORY VERIFIED on the new root: the engine's verification status
	// must agree byte-for-byte with the exporter's final acknowledged root.
	status, err := admin.AdminVerifyStatus(t.Context(), 11)
	if err != nil {
		t.Fatalf("verify status: %v", err)
	}
	var zero [32]byte
	if status.Root == zero || status.Root != report.Root ||
		status.LeafCount != report.LeafCount {
		t.Fatalf("verification root mismatch: engine=%+v exporter root=%x leaf=%d",
			status, report.Root, report.LeafCount)
	}

	// Post-import recall probes: everything the old substrate surfaced must
	// surface from cortexd.
	seam, err := cortexclient.NewLoopSeam(client, cortexclient.SeamConfig{
		ConversationID: "conv-alpha",
		BudgetTokens:   200_000,
	})
	if err != nil {
		t.Fatalf("new loop seam: %v", err)
	}
	rendered, err := seam.Activate(t.Context(), cortexclient.ActivationQuery{
		Query: "moltbook.com paxeer chain status",
	})
	if err != nil {
		t.Fatalf("activate on imported substrate: %v", err)
	}
	for _, probe := range []string{
		"what is the status of moltbook.com",
		"moltbook lives",
		"moltbook.com is alive and serving traffic",
		"the paxeer chain recovered and is producing blocks",
	} {
		if !strings.Contains(rendered, probe) {
			t.Fatalf("imported recall missing %q:\n%s", probe, rendered)
		}
	}

	records, truncated, err := client.Transcript(t.Context(),
		cortexclient.ConversationBytes("conv-alpha"), 0, 64)
	if err != nil || truncated {
		t.Fatalf("imported transcript truncated=%v err=%v", truncated, err)
	}
	kinds := make([]cortexclient.EventKind, 0, len(records))
	for _, record := range records {
		kinds = append(kinds, record.Kind)
	}
	expectedKinds := []cortexclient.EventKind{
		cortexclient.KindUserMsg, cortexclient.KindToolCall,
		cortexclient.KindToolResult, cortexclient.KindDeliveredMsg,
	}
	if len(kinds) != len(expectedKinds) {
		t.Fatalf("imported transcript kinds: %v", kinds)
	}
	for index, kind := range expectedKinds {
		if kinds[index] != kind {
			t.Fatalf("imported transcript kinds: %v", kinds)
		}
	}

	spillRecords, _, err := client.Transcript(t.Context(),
		cortexclient.ConversationBytes("conv-beta"), 0, 64)
	if err != nil || len(spillRecords) != 2 {
		t.Fatalf("spill transcript len=%d err=%v", len(spillRecords), err)
	}
	if !bytes.Contains(spillRecords[1].Payload, []byte("SPILLMARK")) {
		t.Fatalf("spilled tool result was not resolved into the import")
	}
}
