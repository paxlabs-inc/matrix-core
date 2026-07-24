// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// a11y.go — the deterministic middle rung of the driving ladder (DOJO req 3.3):
// an AT-SPI accessibility-tree dump. XFCE/GTK apps register on the a11y bus, so
// their interactive elements (buttons, menu items, fields, tabs, checkboxes)
// come back with roles, labels, and SCREEN-PIXEL coordinates with no vision
// model involved. desktop_look (omni) is the last rung, reached only when the
// a11y tree is empty (canvas/Electron UIs).

// A11yNode is one accessible element with its center in absolute screen pixels.
type A11yNode struct {
	App   string `json:"app"`
	Label string `json:"label"`
	Role  string `json:"role"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// A11yTree is the parsed dump.
type A11yTree struct {
	Apps  []string   `json:"apps"`
	Nodes []A11yNode `json:"nodes"`
}

// a11yDumpPython is the AT-SPI walker. It runs inside the appliance as the
// desktop user (WrapDesktopExec supplies DISPLAY + the session bus), walks each
// registered application to a bounded depth, and prints one JSON object of the
// on-screen interactive elements with their center in screen pixels. Every
// AT-SPI call is defensive: a flaky node degrades to skipped, never a crash.
const a11yDumpPython = `
import json, sys, warnings
warnings.filterwarnings("ignore")
try:
    import gi
    gi.require_version('Atspi', '2.0')
    from gi.repository import Atspi
except Exception as e:
    print(json.dumps({"error": "atspi unavailable: %s" % e})); sys.exit(0)

INTERACTIVE = {
    "push button","toggle button","check box","radio button","menu item",
    "check menu item","radio menu item","menu","text","entry","password text",
    "link","page tab","list item","combo box","spin button","slider","icon",
    "table cell","tree item",
}
MAX_DEPTH = 12
MAX_NODES = 400
nodes = []
apps = []

def onscreen(a):
    try:
        st = a.get_state_set()
        return st.contains(Atspi.StateType.SHOWING) and st.contains(Atspi.StateType.VISIBLE)
    except Exception:
        return True

def component(a):
    for attr in ("get_component_iface", "get_component"):
        fn = getattr(a, attr, None)
        if fn is None:
            continue
        try:
            c = fn()
            if c is not None:
                return c
        except Exception:
            continue
    return None

def extents(a):
    try:
        comp = component(a)
        if comp is None:
            return None
        e = comp.get_extents(Atspi.CoordType.SCREEN)
        if e.width <= 0 or e.height <= 0:
            return None
        return (e.x, e.y, e.width, e.height)
    except Exception:
        return None

def walk(a, appname, depth):
    if depth > MAX_DEPTH or len(nodes) >= MAX_NODES:
        return
    try:
        role = a.get_role_name()
    except Exception:
        role = ""
    try:
        name = (a.get_name() or "").strip()
    except Exception:
        name = ""
    if role in INTERACTIVE and onscreen(a):
        ext = extents(a)
        if ext is not None:
            x, y, w, h = ext
            nodes.append({"app": appname, "label": name, "role": role,
                          "x": x + w // 2, "y": y + h // 2, "w": w, "h": h})
    try:
        n = a.get_child_count()
    except Exception:
        n = 0
    for i in range(min(n, 64)):
        try:
            child = a.get_child_at_index(i)
        except Exception:
            child = None
        if child is not None:
            walk(child, appname, depth + 1)

try:
    desktop = Atspi.get_desktop(0)
    for i in range(desktop.get_child_count()):
        try:
            app = desktop.get_child_at_index(i)
            appname = app.get_name() or ""
        except Exception:
            continue
        apps.append(appname)
        walk(app, appname, 0)
except Exception as e:
    print(json.dumps({"error": "walk failed: %s" % e})); sys.exit(0)

print(json.dumps({"apps": apps, "nodes": nodes}))
`

// a11yPayloadCommand returns the shell that decodes and runs the AT-SPI dumper.
// The script is base64-carried so its Python survives nested shell quoting.
func a11yPayloadCommand() string {
	b64 := base64.StdEncoding.EncodeToString([]byte(a11yDumpPython))
	return fmt.Sprintf("printf %%s %s | base64 -d | python3", b64)
}

// ParseA11yTree decodes the dumper's JSON output, failing closed on an error
// object or unparsable output so a broken a11y bus is a typed error the planner
// reads, never a silently empty tree.
func ParseA11yTree(raw string) (A11yTree, error) {
	body := stripFence(raw)
	if body == "" {
		return A11yTree{}, fmt.Errorf("a11y dumper produced no output")
	}
	var probe struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &probe) == nil && probe.Error != "" {
		return A11yTree{}, fmt.Errorf("a11y: %s", probe.Error)
	}
	var tree A11yTree
	if err := json.Unmarshal([]byte(body), &tree); err != nil {
		return A11yTree{}, fmt.Errorf("a11y output is not the expected JSON: %w", err)
	}
	return tree, nil
}

// FormatA11yTree renders the tree as the compact text the desktop_a11y tool
// returns to the model. Nodes are ordered top-to-bottom, left-to-right.
func FormatA11yTree(tree A11yTree) string {
	var b strings.Builder
	if len(tree.Nodes) == 0 {
		b.WriteString("No interactive elements are exposed on the accessibility tree for this screen. ")
		b.WriteString("Fall back to desktop_look (vision grounding) for this view.")
		return b.String()
	}
	fmt.Fprintf(&b, "Accessibility tree — %d interactive elements across %d applications ", len(tree.Nodes), len(tree.Apps))
	b.WriteString("(label [role] in app at center x,y screen pixels):\n")
	for _, n := range tree.Nodes {
		label := n.Label
		if label == "" {
			label = "(unlabeled)"
		}
		fmt.Fprintf(&b, "- %s [%s] in %s at %d,%d\n", label, n.Role, n.App, n.X, n.Y)
	}
	b.WriteString("Coordinates are absolute screen pixels — pass them straight to click_mouse / move_mouse.")
	return b.String()
}
