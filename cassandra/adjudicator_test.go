// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeDecoder returns a canned response and records what it was asked.
type fakeDecoder struct {
	resp       string
	err        error
	calls      int
	lastSystem string
	lastUser   string
}

func (f *fakeDecoder) Decode(_ context.Context, system, user string) (string, error) {
	f.calls++
	f.lastSystem = system
	f.lastUser = user
	return f.resp, f.err
}

func TestAdjudicate_PrimaryOnly(t *testing.T) {
	d := &fakeDecoder{resp: `{"grounded": true, "coverage": "full", "missing": []}`}
	a := &Adjudicator{Primary: d}
	v, err := a.Adjudicate(context.Background(), AuditInput{Request: "read the block height", Evidence: "TOOL chain_info\n  -> {\"blockNumber\": 42}"})
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if !v.CoverageComplete() {
		t.Fatalf("expected complete verdict, got %#v", v)
	}
	if d.calls != 1 {
		t.Fatalf("expected exactly 1 decode call, got %d", d.calls)
	}
	if !strings.Contains(d.lastUser, "read the block height") || !strings.Contains(d.lastUser, "chain_info") {
		t.Fatal("prompt should carry the request and evidence")
	}
}

func TestAdjudicate_NoEscalateOnReversibleLowCertainty(t *testing.T) {
	primary := &fakeDecoder{resp: `{"coverage": "full", "missing": [], "certainty": 0.1}`}
	escalate := &fakeDecoder{resp: `{"coverage": "partial", "missing": ["x"]}`}
	a := &Adjudicator{Primary: primary, Escalate: escalate}
	// HighStakes=false: even low certainty must NOT escalate.
	if _, err := a.Adjudicate(context.Background(), AuditInput{Request: "r", Evidence: "e"}); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if escalate.calls != 0 {
		t.Fatalf("escalate must not run on a reversible turn, calls=%d", escalate.calls)
	}
}

func TestAdjudicate_EscalateOnHighStakesLowCertainty(t *testing.T) {
	primary := &fakeDecoder{resp: `{"coverage": "full", "missing": [], "certainty": 0.1}`}
	escalate := &fakeDecoder{resp: `{"coverage": "partial", "missing": ["verify the deploy"], "certainty": 0.9}`}
	a := &Adjudicator{Primary: primary, Escalate: escalate}
	v, err := a.Adjudicate(context.Background(), AuditInput{Request: "deploy", Evidence: "e", HighStakes: true})
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if escalate.calls != 1 {
		t.Fatalf("expected escalation, calls=%d", escalate.calls)
	}
	if v.CoverageComplete() {
		t.Fatal("escalated verdict (partial) should win")
	}
}

func TestAdjudicate_NoEscalateWhenCertaintyHigh(t *testing.T) {
	primary := &fakeDecoder{resp: `{"coverage": "full", "missing": [], "certainty": 0.95}`}
	escalate := &fakeDecoder{resp: `{"coverage": "partial", "missing": ["x"]}`}
	a := &Adjudicator{Primary: primary, Escalate: escalate}
	if _, err := a.Adjudicate(context.Background(), AuditInput{Request: "r", Evidence: "e", HighStakes: true}); err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if escalate.calls != 0 {
		t.Fatalf("high certainty must not escalate, calls=%d", escalate.calls)
	}
}

func TestAdjudicate_EscalationFailureKeepsPrimary(t *testing.T) {
	primary := &fakeDecoder{resp: `{"coverage": "full", "missing": [], "certainty": 0.1}`}
	escalate := &fakeDecoder{err: errors.New("upstream 500")}
	a := &Adjudicator{Primary: primary, Escalate: escalate}
	v, err := a.Adjudicate(context.Background(), AuditInput{Request: "r", Evidence: "e", HighStakes: true})
	if err != nil {
		t.Fatalf("escalation failure must be non-fatal, got %v", err)
	}
	if !v.CoverageComplete() {
		t.Fatal("primary verdict should survive a failed escalation")
	}
}

func TestAdjudicate_DecodeErrorPropagates(t *testing.T) {
	a := &Adjudicator{Primary: &fakeDecoder{err: errors.New("boom")}}
	if _, err := a.Adjudicate(context.Background(), AuditInput{Request: "r", Evidence: "e"}); err == nil {
		t.Fatal("expected decode error to propagate so the caller can fail open")
	}
}

func TestAdjudicate_NilPrimary(t *testing.T) {
	a := &Adjudicator{}
	if _, err := a.Adjudicate(context.Background(), AuditInput{}); err == nil {
		t.Fatal("expected error when no primary decoder is set")
	}
}

func TestBuildAuditPrompt_IncludesPriorsHint(t *testing.T) {
	in := AuditInput{
		Request:  "do the thing",
		Evidence: "(no plan executed)",
		Priors:   ScanPriors(PriorInput{Evidence: "(no plan executed)"}),
	}
	p := BuildAuditPrompt(in)
	if !strings.Contains(p, "DETERMINISTIC PRE-PASS") {
		t.Fatalf("expected priors hint section in prompt:\n%s", p)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
