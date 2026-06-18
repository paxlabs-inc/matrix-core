// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package projection

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
)

// Event is a normalized agent-world-state event the passive projector maps
// onto Construct surfaces. It mirrors the daemon sseEvent{Type,Phase,Seq,Fields}
// shape (executor/cmd/mcl-execute/daemon_sse.go) WITHOUT importing the executor,
// so the projector stays a pure, module-decoupled function.
type Event struct {
	Type   string
	Phase  string
	Seq    uint64
	Fields map[string]interface{}
}

// str reads a string field, tolerating an absent map or non-string value.
func (e Event) str(k string) string {
	if e.Fields == nil {
		return ""
	}
	s, _ := e.Fields[k].(string)
	return s
}

// boolField reads a bool field, defaulting to false.
func (e Event) boolField(k string) bool {
	if e.Fields == nil {
		return false
	}
	b, _ := e.Fields[k].(bool)
	return b
}

// Caps keep a projected surface bounded regardless of how large the source
// payload is (a defensive floor; the upstream preview is already capped).
const (
	maxEntityFields = 24
	maxListRecords  = 50
	maxFieldValue   = 500
)

// ProjectEvent maps one normalized pipeline event onto zero or more Construct
// surfaces (the PASSIVE tier — deterministic, no model). Pure + deterministic:
// the same event in yields the same surfaces out (golden-testable), so a replay
// of the stored surface events reproduces them identically. An unrecognised
// event yields nil — the projector stays silent rather than emitting noise.
//
// The set of handled types is deliberately tight: the produced step text and
// the tool result are the two high-value "dark-matter" wins documented in the
// frozen spec ([coverage].tool_result) and the post-launch bug report (the
// tool-only-plan opacity). Richer typing of open-ended blobs is the agent's job
// via the ACTIVE tier (RenderTools), not a heuristic guess here.
func ProjectEvent(ev Event) []*schema.Surface {
	switch ev.Type {
	case "step.text":
		return projectStepText(ev)
	case "plan.tool.result":
		return projectToolResult(ev)
	}
	return nil
}

// projectStepText surfaces the executor's produced text as a Narration. It is
// tagged thinking (working text in the run's workspace), distinct from the
// final chat answer the Liaison composes — so the two never duplicate.
func projectStepText(ev Event) []*schema.Surface {
	text := strings.TrimSpace(ev.str("text"))
	if text == "" {
		return nil
	}
	id := "step:" + firstNonEmpty(ev.str("node_id"), "text")
	s := schema.NewNarration(id, &primitives.Narration{
		Text: text,
		Role: primitives.NarrationThinking,
	}).WithSeq(ev.Seq)
	return []*schema.Surface{s}
}

// projectToolResult is the tap-out fix: it gives the raw tool return a TYPED
// shape instead of a 200-char dump. A JSON object becomes an Entity of fields,
// a JSON array a Structure(list), a bare numeric scalar a Metric; an error or
// anything else falls back to an Entity carrying the result text.
func projectToolResult(ev Event) []*schema.Surface {
	node := ev.str("node_id")
	toolRef := ev.str("tool")
	preview := strings.TrimSpace(ev.str("result_preview"))
	isErr := ev.boolField("is_error")

	id := "tool:" + firstNonEmpty(node, toolRef, "result")
	label := humanizeTool(toolRef)
	ident := firstNonEmpty(node, toolRef, id)

	var s *schema.Surface
	switch {
	case isErr:
		s = schema.NewEntity(id, &primitives.Entity{
			Type:     "tool_error",
			Identity: ident,
			Label:    label,
			Fields: []primitives.EntityField{
				{Key: "error", Value: "true"},
				{Key: "detail", Value: clip(preview, maxFieldValue)},
			},
		})
	case preview == "":
		s = schema.NewEntity(id, &primitives.Entity{Type: "tool_result", Identity: ident, Label: label})
	default:
		if obj, ok := parseJSONObject(preview); ok {
			s = entityFromObject(id, ident, label, obj)
		} else if arr, ok := parseJSONArray(preview); ok {
			s = structureFromArray(id, arr)
		} else if isNumeric(preview) {
			s = schema.NewMetric(id, &primitives.Metric{Label: label, Value: preview})
		} else {
			s = schema.NewEntity(id, &primitives.Entity{
				Type:     "tool_result",
				Identity: ident,
				Label:    label,
				Fields:   []primitives.EntityField{{Key: "result", Value: clip(preview, maxFieldValue)}},
			})
		}
	}
	return []*schema.Surface{s.WithSeq(ev.Seq)}
}

// entityFromObject projects a JSON object onto an Entity, one field per
// top-level key in deterministic (sorted) order, bounded to maxEntityFields.
func entityFromObject(id, ident, label string, obj map[string]interface{}) *schema.Surface {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxEntityFields {
		keys = keys[:maxEntityFields]
	}
	fields := make([]primitives.EntityField, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, primitives.EntityField{Key: k, Value: clip(valueToString(obj[k]), maxFieldValue)})
	}
	return schema.NewEntity(id, &primitives.Entity{
		Type:     "tool_result",
		Identity: ident,
		Label:    label,
		Fields:   fields,
	})
}

// structureFromArray projects a JSON array onto a Structure(list), one record
// per element in order, bounded to maxListRecords.
func structureFromArray(id string, arr []interface{}) *schema.Surface {
	if len(arr) > maxListRecords {
		arr = arr[:maxListRecords]
	}
	records := make([]primitives.StructureNode, 0, len(arr))
	for i, el := range arr {
		records = append(records, primitives.StructureNode{
			ID:    strconv.Itoa(i),
			Label: clip(valueToString(el), maxFieldValue),
		})
	}
	return schema.NewStructure(id, &primitives.Structure{Shape: primitives.ShapeList, Records: records})
}

// --- small pure helpers -----------------------------------------------------

func parseJSONObject(s string) (map[string]interface{}, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '{' {
		return nil, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func parseJSONArray(s string) ([]interface{}, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '[' {
		return nil, false
	}
	var a []interface{}
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return nil, false
	}
	return a, true
}

func isNumeric(s string) bool {
	_, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil
}

// valueToString renders a decoded JSON value as a compact display string,
// formatting whole-number floats as integers so block heights / counts don't
// surface as "19042906.000000".
func valueToString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// humanizeTool turns a tool ref/URI ("matrix://tool/mcp/paxeer-net/chain_info@0.1.0"
// or "alias__name") into a plain label ("chain info").
func humanizeTool(ref string) string {
	s := strings.TrimSpace(ref)
	if s == "" {
		return "tool"
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "__"); i >= 0 {
		s = s[i+2:]
	}
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return "tool"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
