// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"fmt"

	"matrix/neo/internal/llm"
)

// AutomatrixSchemas is the tool surface advertised to an autonomous Automatrix
// run. It IS the sub-agent restricted surface (the existing RestrictTools
// mechanism, SubagentSchemas): every Natural tool, but NOT the synthetic
// core_execute money/chain delegate, memory_recall, or spawn_subagents.
//
// This is the load-bearing structural non-financial guarantee (req.3.2): an
// Automatrix run physically cannot reach the signing path because the money
// tool is simply absent from the advertised schema set — regardless of model
// intent or a mis-classified opportunity. Layers 1–2 (classification at
// capture, eligible-only selection) decide WHAT runs; this layer decides what
// the run CAN do, and money is not in the set.
func (m *Manager) AutomatrixSchemas() []llm.Tool {
	return m.SubagentSchemas()
}

// ValueTransferPatterns are case-insensitive substrings of a tool's name that
// mark it as moving/committing funds or signing. The interactive Neo surface
// no longer escalates these (MCL is folded into Neo; every tool is directly
// callable there), but AUTONOMOUS surfaces still structurally exclude them —
// this list exists solely for that guard.
var ValueTransferPatterns = []string{
	"send", "transfer", "swap", "approve", "deploy", "settle",
	"fund", "mint", "withdraw", "stake", "invoke", "bridge",
}

// IsValueTransferTool reports whether an advertised function name is a
// value-moving / signing tool that must never appear on an autonomous surface:
// the synthetic core_execute money/chain delegate, or any name matching
// ValueTransferPatterns (send/transfer/swap/…). It is the predicate the
// Automatrix guard asserts the advertised surface against.
func IsValueTransferTool(name string) bool {
	if name == CoreExecuteTool {
		return true
	}
	return NewClassifier(ValueTransferPatterns).Classify(name, "") == Escalate
}

// AssertNoValueTransferTools returns an error naming the first value-moving /
// signing tool found in an advertised schema set, or nil if the set is clean.
// The Automatrix structural guarantee depends on this returning nil for the
// AutomatrixSchemas surface (and for the full set an Automatrix agent advertises
// to the model).
func AssertNoValueTransferTools(schemas []llm.Tool) error {
	for _, s := range schemas {
		if IsValueTransferTool(s.Function.Name) {
			return fmt.Errorf("autonomous surface must not advertise value-moving/signing tool %q", s.Function.Name)
		}
	}
	return nil
}
