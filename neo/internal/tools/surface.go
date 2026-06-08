// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import "strings"

// Surface is where an action sits relative to the wall into MCL.
//
// Per the frozen spec's execution surface, reversible actions stay "Natural"
// (Neo performs them directly, fully permissive) while actions that move or
// commit the user's on-chain funds — or need a wallet signature — "Escalate"
// across the wall into the MCL pipeline (which owns the approval gate). Neo
// holds no signing key, so escalate-class tools are never advertised as
// directly-callable functions; they are reachable only through core_execute.
type Surface int

const (
	Natural Surface = iota
	Escalate
)

func (s Surface) String() string {
	if s == Escalate {
		return "escalate"
	}
	return "natural"
}

// DefaultEscalatePatterns are case-insensitive substrings of a tool's name
// that mark it as crossing the wall (moving/committing funds or signing).
// Heuristic + tunable (Options.EscalatePatterns); on-chain READS and
// dry-runs (compile/test/simulate/lookup/list) deliberately do NOT match.
var DefaultEscalatePatterns = []string{
	"send", "transfer", "swap", "approve", "deploy", "settle",
	"fund", "mint", "withdraw", "stake", "invoke", "bridge",
}

// Classifier decides Natural vs Escalate for a tool.
type Classifier struct {
	patterns []string
}

// NewClassifier builds a classifier; empty patterns uses the defaults.
func NewClassifier(patterns []string) *Classifier {
	if len(patterns) == 0 {
		patterns = DefaultEscalatePatterns
	}
	lp := make([]string, len(patterns))
	for i, p := range patterns {
		lp[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return &Classifier{patterns: lp}
}

// Classify returns the surface for a tool given its server-local name and
// declared side-effect class.
func (c *Classifier) Classify(toolName, sideEffect string) Surface {
	n := strings.ToLower(toolName)
	for _, p := range c.patterns {
		if p != "" && strings.Contains(n, p) {
			return Escalate
		}
	}
	return Natural
}
