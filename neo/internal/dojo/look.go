// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// look.go — the pure grounding logic for desktop_look (DOJO wave 2, req 3.2),
// frozen to the wave-1 probe verdict: a SINGLE-PASS stateless MiMo v2.5 omni
// call whose coordinates are NORMALIZED 0–1000 on BOTH axes (Qwen2.5-VL
// convention), rescaled to bytebotd pixels by x/1000*width, y/1000*height. The
// probe measured 0.3–3.7 px center error down to 10×10 targets, so no
// zoomed-crop refine rung is built. Everything here is pure and fixture-tested;
// the live omni call and screenshot capture live at the server seam.

// CoordSpace is omni's normalized coordinate range on each axis (0..CoordSpace).
const CoordSpace = 1000

// GroundingSystemPrompt fixes omni's job: it is a stateless GUI grounder, not a
// planner. It returns strict JSON in the normalized 0–1000 convention the
// wave-1 probe established, and nothing else.
const GroundingSystemPrompt = "You are a precise GUI visual grounding function. You are shown one screenshot of a Linux XFCE desktop. Identify the interactive on-screen elements (buttons, menu items, text fields, links, icons, checkboxes, tabs). For EACH, give a short human label, its role, and its CENTER point as integer coordinates normalized to 0–1000 on both axes where (0,0) is the top-left of the image and (1000,1000) is the bottom-right. If the user names a specific target, also return the single best click point for it. Respond with ONLY a JSON object, no prose and no markdown fences, of the exact shape: {\"elements\":[{\"label\":\"...\",\"role\":\"...\",\"x\":<0-1000>,\"y\":<0-1000>}],\"click\":{\"label\":\"...\",\"x\":<0-1000>,\"y\":<0-1000>}}. Omit \"click\" if no specific target was named. Coordinates MUST be integers in 0–1000."

// GroundingInstruction builds the per-call user instruction. A blank target
// asks for a full element inventory; a named target asks additionally for the
// single best click point.
func GroundingInstruction(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return "List the interactive elements on this screen with their normalized center coordinates."
	}
	return fmt.Sprintf("Find this target and return its click point plus the interactive elements on screen: %q", target)
}

// rawElement is one omni-reported element in the 0–1000 normalized space.
type rawElement struct {
	Label string  `json:"label"`
	Role  string  `json:"role"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type rawGrounding struct {
	Elements []rawElement `json:"elements"`
	Click    *rawElement  `json:"click"`
}

// GroundedElement is one element with coordinates rescaled to bytebotd pixels.
type GroundedElement struct {
	Label string `json:"label"`
	Role  string `json:"role,omitempty"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// Grounding is the rescaled result of one desktop_look.
type Grounding struct {
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Elements []GroundedElement `json:"elements"`
	Click    *GroundedElement  `json:"click,omitempty"`
}

// rescale maps a normalized 0–CoordSpace coordinate onto a pixel dimension,
// rounding to the nearest pixel and clamping to [0, dim-1]. This is THE
// deterministic denormalization the whole lane depends on.
func rescale(v float64, dim int) int {
	if dim <= 0 {
		return 0
	}
	p := int(math.Round(v / float64(CoordSpace) * float64(dim)))
	if p < 0 {
		p = 0
	}
	if p > dim-1 {
		p = dim - 1
	}
	return p
}

// ParseGrounding decodes an omni grounding response (tolerant of a stray
// markdown fence) and rescales every coordinate from the 0–1000 convention to
// pixels for a width×height screenshot. It fails closed on unparsable output so
// a bad grounding is a typed error the planner reads, never a silent wrong
// click.
func ParseGrounding(raw string, width, height int) (Grounding, error) {
	body := stripFence(raw)
	if body == "" {
		return Grounding{}, fmt.Errorf("empty grounding response")
	}
	var rg rawGrounding
	if err := json.Unmarshal([]byte(body), &rg); err != nil {
		return Grounding{}, fmt.Errorf("grounding is not the expected JSON: %w", err)
	}
	g := Grounding{Width: width, Height: height}
	for _, e := range rg.Elements {
		label := strings.TrimSpace(e.Label)
		if label == "" {
			continue
		}
		g.Elements = append(g.Elements, GroundedElement{
			Label: label,
			Role:  strings.TrimSpace(e.Role),
			X:     rescale(e.X, width),
			Y:     rescale(e.Y, height),
		})
	}
	if rg.Click != nil {
		g.Click = &GroundedElement{
			Label: strings.TrimSpace(rg.Click.Label),
			Role:  strings.TrimSpace(rg.Click.Role),
			X:     rescale(rg.Click.X, width),
			Y:     rescale(rg.Click.Y, height),
		}
	}
	if len(g.Elements) == 0 && g.Click == nil {
		return g, fmt.Errorf("grounding returned no elements")
	}
	return g, nil
}

// FormatGrounding renders the grounding as the compact text a stateless omni
// call is allowed to contribute to the window (planner_integrity: omni output
// enters only as a tool result). The click point, when present, leads.
func FormatGrounding(g Grounding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Desktop view %dx%d px.\n", g.Width, g.Height)
	if g.Click != nil {
		fmt.Fprintf(&b, "Best click for the target %q: (%d, %d).\n", g.Click.Label, g.Click.X, g.Click.Y)
	}
	els := append([]GroundedElement(nil), g.Elements...)
	sort.SliceStable(els, func(i, j int) bool {
		if els[i].Y != els[j].Y {
			return els[i].Y < els[j].Y
		}
		return els[i].X < els[j].X
	})
	if len(els) > 0 {
		b.WriteString("Interactive elements (label [role] at x,y pixels):\n")
		for _, e := range els {
			if e.Role != "" {
				fmt.Fprintf(&b, "- %s [%s] at %d,%d\n", e.Label, e.Role, e.X, e.Y)
			} else {
				fmt.Fprintf(&b, "- %s at %d,%d\n", e.Label, e.X, e.Y)
			}
		}
	}
	b.WriteString("Coordinates are absolute screen pixels — pass them straight to click_mouse / move_mouse.")
	return b.String()
}

// stripFence removes a ```json … ``` fence and surrounding prose so a model
// that ignored the "no fence" instruction still parses. It extracts the first
// balanced {…} object if the response wraps JSON in text.
func stripFence(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

// PNGDimensions reads the width and height from a PNG's IHDR chunk without
// decoding pixels, so desktop_look knows the screenshot size to rescale into.
func PNGDimensions(data []byte) (int, int, error) {
	// 8-byte signature + 4-byte length + "IHDR" + width(4) + height(4).
	const sigLen = 8
	if len(data) < sigLen+8+8 {
		return 0, 0, fmt.Errorf("not a PNG (too short)")
	}
	if string(data[12:16]) != "IHDR" {
		return 0, 0, fmt.Errorf("not a PNG (no IHDR)")
	}
	w := int(binary.BigEndian.Uint32(data[16:20]))
	h := int(binary.BigEndian.Uint32(data[20:24]))
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid PNG dimensions %dx%d", w, h)
	}
	return w, h, nil
}
