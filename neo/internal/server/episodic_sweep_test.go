// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

func TestEpisodicSweepBusyDefersAndIdleRuns(t *testing.T) {
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "sweep-test"
	p, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e := &Engine{pager: p, runs: map[string]*run{"busy": {}}}
	if !e.MaybeHandleEpisodicSweep(episodicSweepMarker) {
		t.Fatal("marker not handled")
	}
	delete(e.runs, "busy")
	if !e.MaybeHandleEpisodicSweep(episodicSweepMarker) {
		t.Fatal("idle marker not handled")
	}
	if e.MaybeHandleEpisodicSweep("normal message") {
		t.Fatal("normal message intercepted")
	}
	result, err := p.EpisodicBackfill(1)
	if err != nil || !result.Complete {
		t.Fatalf("idle sweep did not persist completion: %+v err=%v", result, err)
	}
}
