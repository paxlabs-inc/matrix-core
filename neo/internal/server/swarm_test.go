// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"matrix/neo/internal/tools"
)

func TestAggregateResults(t *testing.T) {
	out := aggregateResults([]subResult{
		{index: 1, name: "Go Analyst", persona: "senior Go reviewer", text: "Found a race in run().", status: subStatusResult},
		{index: 2, name: "Security", persona: "auditor", text: "Checked the callback path.", status: subStatusPartial, failure: "provider unavailable"},
		{index: 3, name: "Docs", status: subStatusEmpty},
		{index: 4, name: "Network", status: subStatusTimeout, failure: "worker deadline reached"},
	})
	start := strings.IndexByte(out, '{')
	end := strings.LastIndexByte(out, '}')
	if start < 0 || end < start {
		t.Fatalf("digest does not contain structured JSON: %s", out)
	}
	var payload struct {
		Reports []workerEvidenceReport `json:"reports"`
	}
	if err := json.Unmarshal([]byte(out[start:end+1]), &payload); err != nil {
		t.Fatalf("decode evidence reports: %v\n%s", err, out)
	}
	if len(payload.Reports) != 4 {
		t.Fatalf("report count = %d, want 4", len(payload.Reports))
	}
	for i, want := range []string{subStatusResult, subStatusPartial, subStatusEmpty, subStatusTimeout} {
		if payload.Reports[i].Status != want {
			t.Errorf("report %d status = %q, want %q", i, payload.Reports[i].Status, want)
		}
	}
	if payload.Reports[0].Evidence != "Found a race in run()." || !payload.Reports[0].Complete {
		t.Errorf("result report lost evidence/completion: %+v", payload.Reports[0])
	}
	if payload.Reports[1].Complete || payload.Reports[1].Failure == "" {
		t.Errorf("partial report is not honest: %+v", payload.Reports[1])
	}
}

func TestSubagentResultStatesAndBound(t *testing.T) {
	spec := tools.SubagentSpec{Name: "Researcher", Persona: "auditor"}
	cases := []struct {
		name      string
		text      string
		ok        bool
		err       error
		timedOut  bool
		cancelled bool
		want      string
	}{
		{name: "result", text: "evidence", ok: true, want: subStatusResult},
		{name: "empty", ok: true, want: subStatusEmpty},
		{name: "partial", text: "some evidence", err: errors.New("transport ended"), want: subStatusPartial},
		{name: "failed", err: errors.New("transport ended"), want: subStatusFailed},
		{name: "cancelled", err: context.Canceled, cancelled: true, want: subStatusCancelled},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := classifySubResult(1, spec, test.text, test.ok, test.err, test.timedOut, test.cancelled)
			if got.status != test.want {
				t.Fatalf("status = %q, want %q", got.status, test.want)
			}
		})
	}

	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	<-deadline.Done()
	timed := classifySubResult(1, spec, "", false, deadline.Err(), errors.Is(deadline.Err(), context.DeadlineExceeded), false)
	if timed.status != subStatusTimeout {
		t.Fatalf("real deadline status = %q, want timeout", timed.status)
	}

	long := strings.Repeat("e", subagentMaxResultRunes+500)
	bounded := classifySubResult(1, spec, long, true, nil, false, false)
	if !bounded.truncated || len([]rune(bounded.text)) != subagentMaxResultRunes {
		t.Fatalf("result bound not enforced: truncated=%v runes=%d", bounded.truncated, len([]rune(bounded.text)))
	}
}

// TestSwarmActiveGuard asserts the recursion backstop: a context already inside
// a swarm reports active, so a sub-agent that reaches runSwarm is refused.
func TestSwarmActiveGuard(t *testing.T) {
	if swarmActive(context.Background()) {
		t.Fatal("a plain context must not be marked swarm-active")
	}
	if !swarmActive(withSwarmActive(context.Background())) {
		t.Fatal("withSwarmActive must mark the context active")
	}
}
