// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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

// DefaultEscalatePatterns is empty: with MCL folded into Neo, the interactive
// agent carries the full tool surface directly — every manifest tool is
// Natural and directly callable; nothing is walled behind core_execute.
// Spend policy is enforced network-side on the embedded wallet
// (PaxeerSpendPolicy / PAXEER_MAX_SPEND_WEI), not by withholding tools.
// The autonomous (Automatrix/sub-agent) surface keeps its own explicit
// money-tool guard — see ValueTransferPatterns in automatrix.go.
var DefaultEscalatePatterns []string

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
