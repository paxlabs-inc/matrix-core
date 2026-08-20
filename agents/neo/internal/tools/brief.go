// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package tools

import (
	"fmt"
	"strings"

	"centra/agents/neo/internal/llm"
)

// briefToolBaseNames are the read-only information tools a morning-brief run may
// use (ORACLE req 15.1): recent-news search, general web search, and read-only
// fetch. A tool's advertised function name is "<alias>__<name>", so membership
// is tested on the trailing base name.
var briefToolBaseNames = []string{"web_news", "web_search", "fetch"}

// IsBriefTool reports whether an advertised function name is one of the brief's
// positive-allowlist read-only information tools (matched on the base name after
// the "<alias>__" prefix), OR the synthetic memory_recall tool. It is the
// predicate the brief surface is built from and asserted against.
func IsBriefTool(fn string) bool {
	if fn == MemoryRecallTool {
		return true
	}
	base := fn
	if i := strings.LastIndex(fn, "__"); i >= 0 {
		base = fn[i+2:]
	}
	for _, b := range briefToolBaseNames {
		if base == b {
			return true
		}
	}
	return false
}

// BriefSchemas is the tool surface advertised to a morning-brief run — a POSITIVE
// allowlist tighter than the Automatrix restricted surface (req 15.1): ONLY the
// read-only information tools (web_news / web_search / fetch) plus memory_recall
// when wired. Financial, signing, filesystem, deployment, and
// arbitrary-execution tools (including core_execute, spawn_subagents,
// construct_render, write_skill, preview, todo) are structurally ABSENT — they
// are simply never added to the set, regardless of model intent. Deterministic
// order.
func (m *Manager) BriefSchemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(briefToolBaseNames)+1)
	for _, fn := range m.order {
		if !IsBriefTool(fn) {
			continue
		}
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	if m.recall != nil {
		out = append(out, memoryRecallSchema())
	}
	return out
}

// AssertBriefSurface returns an error naming the first tool in a schema set that
// is NOT an allowed brief tool (read-only info tool or memory_recall). The brief
// structural guarantee (req 15.1/18.1) depends on this returning nil for the
// advertised surface a brief agent hands the model. read_overflow is exempt: it
// is a synthetic read-the-rest-of-a-truncated-result tool, not an information or
// value-moving tool.
func AssertBriefSurface(schemas []llm.Tool) error {
	for _, s := range schemas {
		name := s.Function.Name
		// read_overflow is the synthetic read-the-rest-of-a-truncated-result
		// tool the agent always advertises; it is not an information or
		// value-moving tool, so it is exempt from the positive allowlist.
		if name == "read_overflow" {
			continue
		}
		if !IsBriefTool(name) {
			return fmt.Errorf("brief surface must not advertise non-information tool %q", name)
		}
	}
	return nil
}
