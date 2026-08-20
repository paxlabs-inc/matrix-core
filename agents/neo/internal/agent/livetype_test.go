// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"testing"

	"centra/agents/neo/internal/llm"
)

// The resumable decoder remains for historical trace compatibility. New
// production coding does not expose a filesystem MCP surface; file and
// terminal evidence now comes from durable Build jobs.
func feedFragments(t *testing.T, raw string, n int) (map[string]string, []ToolEvent) {
	t.Helper()
	var events []ToolEvent
	lt := newLiveTyper(func(ev ToolEvent) { events = append(events, ev) })
	for i := 0; i < len(raw); i += n {
		end := i + n
		if end > len(raw) {
			end = len(raw)
		}
		lt.feed(&llm.ToolCallDelta{Index: 0, ID: "call_w", Name: "fs__write_file", Args: raw[i:end]})
	}
	got := map[string]string{}
	offset := 0
	for _, ev := range events {
		if ev.Phase != ToolStream {
			t.Fatalf("unexpected phase %q", ev.Phase)
		}
		if ev.StreamOffset != offset {
			t.Fatalf("offset gap: got %d, want %d", ev.StreamOffset, offset)
		}
		got[ev.StreamPath] += ev.StreamDelta
		offset += len(ev.StreamDelta)
	}
	return got, events
}

func TestLiveTyperDecodesHistoricalFragments(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tprintln(\"h\\i\")\t// tab + quote + backslash\n}\n∆ünïcode 🚀 done\n"
	for _, order := range []string{"path_first", "content_first"} {
		if order == "path_first" {
			payload, _ := json.Marshal(content)
			raw := `{"path":"src/app.go","content":` + string(payload) + `}`
			for n := 1; n <= 13; n += 3 {
				got, _ := feedFragments(t, raw, n)
				if got["src/app.go"] != content {
					t.Fatalf("%s frag=%d: decoded %q != content", order, n, got["src/app.go"])
				}
			}
			continue
		}
		raw, _ := json.Marshal(map[string]string{"content": content, "path": "src/app.go"})
		for n := 1; n <= 13; n += 3 {
			got, _ := feedFragments(t, string(raw), n)
			if got["src/app.go"] != content {
				t.Fatalf("%s frag=%d: decoded %q != content", order, n, got["src/app.go"])
			}
		}
	}
}

func TestLiveTyperIgnoresNonWriteTools(t *testing.T) {
	var events []ToolEvent
	lt := newLiveTyper(func(ev ToolEvent) { events = append(events, ev) })
	lt.feed(&llm.ToolCallDelta{Index: 0, Name: "fs__read_text_file", Args: `{"path":"a.txt","content":"x"}`})
	lt.feed(&llm.ToolCallDelta{Index: 1, Name: "exec__shell", Args: `{"command":"ls"}`})
	if len(events) != 0 {
		t.Fatalf("non-write tools must not stream, got %d events", len(events))
	}
}
