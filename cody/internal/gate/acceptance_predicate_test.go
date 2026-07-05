// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package gate

import (
	"context"
	"testing"

	"matrix/cassandra"
)

// acceptanceCase is one shared verdict shape exercised across the Neo, Cody,
// and Cassandra acceptance tests (the identical table lives in each module's
// test) so the three consumers are proven to decide a given Verdict the same
// way — the anti-drift proof for req 6.3 / X1.
type acceptanceCase struct {
	name    string
	verdict string // the raw adjudicator JSON
	sound   bool   // the canonical cassandra.Sound() expectation
}

func acceptanceTable() []acceptanceCase {
	return []acceptanceCase{
		{"grounded_full", `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.9}`, true},
		{"ungrounded_full", `{"grounded": false, "coverage": "full", "missing": [], "unverified_claims": [], "certainty": 0.8}`, false},
		{"grounded_but_unverified", `{"grounded": true, "coverage": "full", "missing": [], "unverified_claims": ["the deploy succeeded"], "certainty": 0.8}`, false},
		{"grounded_partial", `{"grounded": true, "coverage": "partial", "missing": ["criterion 2 unexercised"], "certainty": 0.7}`, false},
		{"full_with_missing", `{"grounded": true, "coverage": "full", "missing": ["the refund was never sent"], "certainty": 0.7}`, false},
	}
}

// TestGateAcceptsIffSound proves Cody's gate.Adjudicate decides acceptance
// through the shared cassandra.Sound() predicate: the REAL gate (real
// cassandra.Adjudicator + real llm client over an SSE endpoint) accepts a
// turn-in exactly when Sound() is true for the verdict it judged — no
// re-implemented inline rule (req 6.1, 6.2).
func TestGateAcceptsIffSound(t *testing.T) {
	for _, tc := range acceptanceTable() {
		t.Run(tc.name, func(t *testing.T) {
			adj := scriptedAdjudicator(t, tc.verdict)

			// The canonical predicate over the very verdict the gate will judge
			// (the scripted adjudicator is deterministic across calls).
			v, err := adj.Adjudicate(context.Background(), cassandra.AuditInput{
				Request:  BuildRequest(sheetFor(t)),
				Evidence: BuildEvidence("", doneReport(), "[GREEN exit 0] true\n"),
			})
			if err != nil {
				t.Fatalf("adjudicate errored: %v", err)
			}
			if got := v.Sound(); got != tc.sound {
				t.Fatalf("Sound()=%v, table expects %v for %s", got, tc.sound, tc.name)
			}

			accepted := Adjudicate(context.Background(), adj, "", sheetFor(t), doneReport(), "[GREEN exit 0] true\n") == ""
			if accepted != v.Sound() {
				t.Fatalf("gate accepted=%v but Sound()=%v — the gate re-implements acceptance instead of using Sound()", accepted, v.Sound())
			}
		})
	}
}
