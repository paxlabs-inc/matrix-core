// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// TestRescaleDeterminism pins the frozen wave-1 denormalization: normalized
// 0–1000 → pixels by v/1000*dim, rounded, clamped to [0,dim-1]. These are the
// exact numbers the whole desktop lane depends on being reproducible.
func TestRescaleDeterminism(t *testing.T) {
	cases := []struct {
		v    float64
		dim  int
		want int
	}{
		{0, 1280, 0},
		{1000, 1280, 1279},   // clamp to dim-1
		{500, 1280, 640},     // center
		{500, 960, 480},      // center, other axis
		{1, 1280, 1},         // round(1.28)=1
		{999, 960, 959},      // round(959.04)=959
		{7, 1000, 7},         // identity when dim == CoordSpace
		{-5, 1280, 0},        // clamp negative
		{123.4, 1280, 158},   // round(157.95)
		{876.6, 960, 842},    // round(841.5)=842
	}
	for _, c := range cases {
		if got := rescale(c.v, c.dim); got != c.want {
			t.Errorf("rescale(%v,%d) = %d, want %d", c.v, c.dim, got, c.want)
		}
	}
}

// TestParseGrounding drives the real omni-shaped JSON through parse + rescale on
// a recorded fixture — the deterministic contract of desktop_look independent of
// any live model call.
func TestParseGrounding(t *testing.T) {
	raw := `{"elements":[
		{"label":"Firefox","role":"push button","x":10,"y":990},
		{"label":"Search box","role":"text","x":500,"y":40}
	],"click":{"label":"Search box","x":500,"y":40}}`
	g, err := ParseGrounding(raw, 1280, 960)
	if err != nil {
		t.Fatalf("ParseGrounding: %v", err)
	}
	if g.Width != 1280 || g.Height != 960 {
		t.Fatalf("dims = %dx%d", g.Width, g.Height)
	}
	if len(g.Elements) != 2 {
		t.Fatalf("elements = %d, want 2", len(g.Elements))
	}
	// Firefox at normalized (10,990) -> pixels (13, 950).
	if g.Elements[0].X != 13 || g.Elements[0].Y != 950 {
		t.Errorf("firefox at %d,%d want 13,950", g.Elements[0].X, g.Elements[0].Y)
	}
	if g.Click == nil || g.Click.X != 640 || g.Click.Y != 38 {
		t.Errorf("click = %+v want 640,38", g.Click)
	}
	out := FormatGrounding(g)
	if !strings.Contains(out, "640, 38") || !strings.Contains(out, "Firefox") {
		t.Errorf("FormatGrounding missing content:\n%s", out)
	}
}

// TestParseGroundingFencedAndFailClosed proves the parser recovers JSON from a
// stray markdown fence and fails closed on garbage (a bad grounding is a typed
// error, never a silent wrong click).
func TestParseGroundingFencedAndFailClosed(t *testing.T) {
	fenced := "Here you go:\n```json\n{\"elements\":[{\"label\":\"OK\",\"role\":\"push button\",\"x\":250,\"y\":500}]}\n```"
	g, err := ParseGrounding(fenced, 1000, 1000)
	if err != nil {
		t.Fatalf("fenced parse: %v", err)
	}
	if len(g.Elements) != 1 || g.Elements[0].X != 250 || g.Elements[0].Y != 500 {
		t.Fatalf("fenced element wrong: %+v", g.Elements)
	}
	if _, err := ParseGrounding("not json at all", 100, 100); err == nil {
		t.Error("expected error on non-JSON grounding")
	}
	if _, err := ParseGrounding(`{"elements":[]}`, 100, 100); err == nil {
		t.Error("expected error on empty grounding")
	}
}

// TestPNGDimensions verifies the IHDR reader against a real encoded PNG.
func TestPNGDimensions(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 960))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	w, h, err := PNGDimensions(buf.Bytes())
	if err != nil {
		t.Fatalf("PNGDimensions: %v", err)
	}
	if w != 1280 || h != 960 {
		t.Errorf("dims = %dx%d want 1280x960", w, h)
	}
	if _, _, err := PNGDimensions([]byte("notpng")); err == nil {
		t.Error("expected error on non-PNG")
	}
}

// TestParseA11yTree proves the a11y parser decodes a real dumper payload and
// fails closed on an error object.
func TestParseA11yTree(t *testing.T) {
	raw := `{"apps":["xfce4-panel","Thunar"],"nodes":[
		{"app":"xfce4-panel","label":"Applications","role":"push button","x":40,"y":945,"w":80,"h":24},
		{"app":"Thunar","label":"","role":"push button","x":200,"y":100,"w":20,"h":20}
	]}`
	tree, err := ParseA11yTree(raw)
	if err != nil {
		t.Fatalf("ParseA11yTree: %v", err)
	}
	if len(tree.Nodes) != 2 || len(tree.Apps) != 2 {
		t.Fatalf("tree = %d nodes / %d apps", len(tree.Nodes), len(tree.Apps))
	}
	if tree.Nodes[0].X != 40 || tree.Nodes[0].Y != 945 {
		t.Errorf("node0 at %d,%d", tree.Nodes[0].X, tree.Nodes[0].Y)
	}
	out := FormatA11yTree(tree)
	if !strings.Contains(out, "Applications") || !strings.Contains(out, "(unlabeled)") {
		t.Errorf("FormatA11yTree missing content:\n%s", out)
	}
	if _, err := ParseA11yTree(`{"error":"atspi unavailable: no bus"}`); err == nil {
		t.Error("expected error passthrough from dumper error object")
	}
	empty := FormatA11yTree(A11yTree{})
	if !strings.Contains(empty, "desktop_look") {
		t.Errorf("empty tree should point at desktop_look fallback:\n%s", empty)
	}
}

// TestA11yPayloadCommand confirms the dumper is carried as decodable base64 so
// its Python survives nested shell quoting.
func TestA11yPayloadCommand(t *testing.T) {
	cmd := a11yPayloadCommand()
	if !strings.Contains(cmd, "base64 -d | python3") {
		t.Fatalf("payload command shape wrong: %s", cmd)
	}
}

// TestWrapDesktopExec proves the desktop-exec wrapper targets the appliance
// container as the desktop user with DISPLAY + session-bus discovery, and that
// the inner command survives single-quoting.
func TestWrapDesktopExec(t *testing.T) {
	got := WrapDesktopExec("dojo", "echo hi")
	for _, want := range []string{"docker exec -u user dojo bash -lc", "DISPLAY=:0", "DBUS_SESSION_BUS_ADDRESS", "xfce4-session", "echo hi"} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper missing %q:\n%s", want, got)
		}
	}
}
