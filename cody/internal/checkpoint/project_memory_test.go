// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package checkpoint

import (
	"strings"
	"testing"

	cortex "matrix/cortex"
	"matrix/cortex/store"
)

func TestProjectMemoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := openCortex(t, dir)
	pm := NewProjectMemory(c, "airbnb-clone")

	if recs, err := pm.Recent(10); err != nil || len(recs) != 0 {
		t.Fatalf("Recent() on empty project = %v, %v", recs, err)
	}
	if err := pm.Record("sdr", "React Router framework mode on Node/TS"); err != nil {
		t.Fatal(err)
	}
	if err := pm.Record("task", "t1: created the listing grid"); err != nil {
		t.Fatal(err)
	}
	if err := pm.Record("dlr", "Editorial style, Fraunces/Inter, terracotta accent"); err != nil {
		t.Fatal(err)
	}
	// Unrelated session traffic on the same conversation must not corrupt reads.
	if _, err := c.AppendMessage(cortex.Message{ConversationID: "cody-project-airbnb-clone", Role: cortex.RoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := pm.Record("", "no kind"); err == nil {
		t.Fatal("Record accepted an empty kind")
	}

	recs, err := pm.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Kind != "sdr" || recs[1].Kind != "task" || recs[2].Kind != "dlr" {
		t.Fatalf("Recent() = %+v", recs)
	}

	// Recall cap keeps the newest records.
	if capped, err := pm.Recent(2); err != nil || len(capped) != 2 || capped[0].Kind != "task" {
		t.Fatalf("Recent(2) = %+v, %v", capped, err)
	}

	// Rendering: decisions (SDR/DLR) lead, deliveries follow.
	out := RenderProjectMemory(recs)
	sdrIdx := strings.Index(out, "Stack decision: React Router")
	dlrIdx := strings.Index(out, "Design language: Editorial style")
	taskIdx := strings.Index(out, "Delivered: t1: created the listing grid")
	if sdrIdx < 0 || dlrIdx < 0 || taskIdx < 0 {
		t.Fatalf("render missing records:\n%s", out)
	}
	if !(sdrIdx < taskIdx && dlrIdx < taskIdx) {
		t.Fatalf("decisions must render before deliveries:\n%s", out)
	}
	if RenderProjectMemory(nil) != "" {
		t.Fatal("empty records must render empty")
	}

	// Projects are isolated: another project sees nothing.
	other := NewProjectMemory(c, "other-project")
	if recs, err := other.Recent(10); err != nil || len(recs) != 0 {
		t.Fatalf("cross-project leak: %+v, %v", recs, err)
	}
}

func TestProjectMemorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	s1, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	pm1 := NewProjectMemory(cortex.New(s1), "demo")
	if err := pm1.Record("sdr", "Go service"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir, "cody", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	pm2 := NewProjectMemory(cortex.New(s2), "demo")
	recs, err := pm2.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Summary != "Go service" {
		t.Fatalf("project memory lost across restart: %+v", recs)
	}
}
