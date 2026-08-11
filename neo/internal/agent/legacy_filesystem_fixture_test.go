// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	executortool "matrix/executor/tool"
)

// fsToolManifest is a test-only compatibility fixture for legacy agent-loop
// properties that exercise generic MCP dispatch. It deliberately does not
// clone a production manifest: default.json and neo.json must never expose
// this local coding server. Durable Build tests cover the production path.
func fsToolManifest(t *testing.T, root string) string {
	t.Helper()
	command := "npx"
	args := []string{"-y", "@modelcontextprotocol/server-filesystem@2026.1.14", root}
	if installed := os.Getenv("MATRIX_FS_MCP_COMMAND"); installed != "" {
		command = installed
		args = []string{root}
	}
	names := []string{
		"read_file", "read_text_file", "read_media_file", "read_multiple_files",
		"write_file", "edit_file", "create_directory", "list_directory",
		"list_directory_with_sizes", "list_allowed_directories", "directory_tree",
		"move_file", "search_files", "get_file_info",
	}
	entries := make([]executortool.ToolEntry, 0, len(names))
	for _, name := range names {
		effect := executortool.SideEffectRead
		switch name {
		case "write_file", "edit_file", "create_directory", "move_file":
			effect = executortool.SideEffectWrite
		}
		entries = append(entries, executortool.ToolEntry{Name: name, SideEffectClass: effect})
	}
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/legacy-filesystem-test",
		Servers: []executortool.ServerEntry{{
			Alias: "fs", Transport: "stdio", Command: command,
			Args:          args,
			PackageDigest: "sha256:" + strings.Repeat("a", 64),
			Version:       "2026.1.14",
			Tools:         entries,
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy-filesystem-manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
