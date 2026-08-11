// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package projection

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
)

// ConstructRenderTool is the name of the single agent-facing render function.
// The projecting agent (Neo) advertises it as a synthetic tool — like
// core_execute / spawn_subagents — so it never enters the agents/*.json
// manifest and cannot break the daemon-boot tool-bijection check. Calling it is
// how the agent AUTHORS its own projection: it chooses a primitive and fills it.
const ConstructRenderTool = "construct_render"

// ToolSpec is one agent-facing render tool the Construct active tier advertises.
// The construct module OWNS this contract (the agent's projection vocabulary);
// the hosting agent converts each ToolSpec into its own function-tool type and
// routes calls back through ParseRender. Keeping the spec here makes the active
// surface single-source and unit-testable, and keeps the agent a thin adapter.
type ToolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Params      map[string]interface{} `json:"params"`
}

// RenderTools returns the agent-facing tool specs for the active projection
// tier. v1 exposes a single construct_render tool keyed on `kind`, keeping the
// agent's function list uncluttered; per-primitive convenience tools can be
// added later without changing ParseRender (it dispatches on kind).
func RenderTools() []ToolSpec {
	return []ToolSpec{{
		Name:        ConstructRenderTool,
		Description: renderToolDescription,
		Params:      renderToolParams(),
	}}
}

const renderToolDescription = `Render a rich visual surface onto the user's screen while you work. Use this to SHOW the user structured results instead of only describing them in chat — a value you found, an object you fetched, a list/table, your live progress, a media artifact, or a question you need answered.

Only render when you are actually doing a task or have something concrete to show. If the user is just chatting (a greeting, small talk, a question about you), do NOT render — keep the screen to chat.

Pick ONE primitive per call via "kind" and fill "payload" for that kind:
- narration: your reasoning/answer text. payload {text, role?: thinking|intent|answer}
- metric: a single named value. payload {label, value, unit?, magnitude?, scale?: human-readable magnitude hint e.g. "K"|"M"|"B", trend?: up|down|flat, display?: plain|bar|gauge (gauge renders a radial dial — use with magnitude+threshold), threshold?: {warn?, limit?, direction?: above|below}}
- entity: a referenceable object (a tx, token, file, account). payload {type, identity, label?, fields?: [{key,value,ref?}], affordances?: [{id,label,kind?: link|copy|ask, href?, ask_ref?}]}
- structure: a collection. payload {shape: list|table|tree, columns?: [..], records: [{id?,label?,ref?,cells?:{col:val},children?:[..]}]}
- stream: append-only text (logs, a trace, a code block — set channel:"command" for shell, or a language for code). payload {source?, title?, chunks: [{seq,text,channel?}], closed?}
- timeline: stateful steps over time (a plan, jobs, sub-agents, a to-do/workflow). payload {title?, steps: [{id,label,status: pending|running|done|failed, detail?, ref?}]}
- canvas: a media blob OR a data-driven chart. For media: payload {media: {kind: image|video|audio|page, url, mime?, alt?}, caption?}. For a CHART, set media.kind="chart" and fill "chart": payload {media: {kind: "chart"}, chart: {kind: area|bar|line|pie|radar|scatter, series?: [{key, name?, color?}], points: [{label?, values: {seriesKey: number}}], x_label?, y_label?, stacked?}, caption?}. Charts are drawn from DATA (always prefer this over a chart image).
- ask: a question that needs a typed human answer. payload {ask_kind: choose|input|confirm|sign|upload, prompt, options?: [{id,label}], expected?, required?}

Reuse the same "id" to UPDATE a surface you already rendered (e.g. flip a timeline step to done). Optional "attributes" decorate any surface: {stakes: fact|hypothesis|decision|irreversible, confidence: 0..1, cost: {amount, unit, cap}, temporality: point|stream|persistent}. Tag an irreversible action with stakes=irreversible so it renders with the right weight.`

// renderToolParams is the discriminated JSON-Schema union for construct_render.
// The model cannot pair a kind with another primitive's payload shape: each
// oneOf branch fixes kind with const and carries that primitive's required
// fields. ParseRender + schema.Validate remain the authoritative i2 gate.
func renderToolParams() map[string]interface{} {
	variants := []interface{}{
		renderVariant(schema.KindNarration, objectSchema(
			map[string]interface{}{
				"text": stringSchema(),
				"role": enumSchema("thinking", "intent", "answer"),
			},
			"text",
		)),
		renderVariant(schema.KindMetric, objectSchema(
			map[string]interface{}{
				"label":     stringSchema(),
				"value":     stringSchema(),
				"unit":      stringSchema(),
				"magnitude": numberSchema(),
				"scale":     stringSchema(),
				"trend":     enumSchema("up", "down", "flat"),
				"display":   enumSchema("plain", "bar", "gauge"),
				"threshold": objectSchema(map[string]interface{}{
					"warn": numberSchema(), "limit": numberSchema(),
					"direction": enumSchema("above", "below"),
				}),
			},
			"label", "value",
		)),
		renderVariant(schema.KindEntity, objectSchema(
			map[string]interface{}{
				"type":     stringSchema(),
				"identity": stringSchema(),
				"label":    stringSchema(),
				"fields": arraySchema(objectSchema(
					map[string]interface{}{
						"key": stringSchema(), "value": stringSchema(), "ref": stringSchema(),
					},
					"key", "value",
				)),
				"affordances": arraySchema(objectSchema(
					map[string]interface{}{
						"id": stringSchema(), "label": stringSchema(),
						"kind": enumSchema("link", "copy", "ask"),
						"href": stringSchema(), "ask_ref": stringSchema(),
					},
					"id", "label",
				)),
			},
			"type", "identity",
		)),
		renderVariant(schema.KindStructure, objectSchema(
			map[string]interface{}{
				"shape":   enumSchema("list", "table", "tree"),
				"columns": arraySchema(stringSchema()),
				"records": arraySchema(structureRecordSchema()),
			},
			"shape", "records",
		)),
		renderVariant(schema.KindStream, objectSchema(
			map[string]interface{}{
				"source": stringSchema(), "title": stringSchema(), "closed": boolSchema(),
				"chunks": arraySchema(objectSchema(
					map[string]interface{}{
						"seq": numberSchema(), "text": stringSchema(), "channel": stringSchema(),
					},
					"seq", "text",
				)),
			},
			"chunks",
		)),
		renderVariant(schema.KindTimeline, objectSchema(
			map[string]interface{}{
				"title": stringSchema(),
				"steps": arraySchema(objectSchema(
					map[string]interface{}{
						"id": stringSchema(), "label": stringSchema(),
						"status": enumSchema("pending", "running", "done", "failed"),
						"detail": stringSchema(), "ref": stringSchema(),
					},
					"id", "label", "status",
				)),
			},
			"steps",
		)),
		renderVariant(schema.KindCanvas, canvasPayloadSchema()),
		renderVariant(schema.KindAsk, objectSchema(
			map[string]interface{}{
				"ask_kind": enumSchema("choose", "input", "confirm", "sign", "upload"),
				"prompt":   stringSchema(),
				"options": arraySchema(objectSchema(
					map[string]interface{}{"id": stringSchema(), "label": stringSchema()},
					"id", "label",
				)),
				"expected": stringSchema(), "required": boolSchema(),
			},
			"ask_kind", "prompt",
		)),
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"kind":       enumSchema("narration", "metric", "entity", "structure", "stream", "timeline", "canvas", "ask"),
			"payload":    map[string]interface{}{"type": "object"},
			"id":         stringSchema(),
			"ref":        stringSchema(),
			"attributes": attributesSchema(),
		},
		"required":             []string{"kind", "payload"},
		"additionalProperties": false,
		"oneOf":                variants,
	}
}

func renderVariant(kind schema.Kind, payload map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"kind":       map[string]interface{}{"type": "string", "const": string(kind)},
			"payload":    payload,
			"id":         stringSchema(),
			"ref":        stringSchema(),
			"attributes": attributesSchema(),
		},
		"required":             []string{"kind", "payload"},
		"additionalProperties": false,
	}
}

func attributesSchema() map[string]interface{} {
	return objectSchema(map[string]interface{}{
		"stakes": enumSchema("fact", "hypothesis", "decision", "irreversible"),
		"confidence": map[string]interface{}{
			"type": "number", "minimum": 0, "maximum": 1,
		},
		"cost": objectSchema(
			map[string]interface{}{
				"amount": numberSchema(), "unit": stringSchema(), "cap": numberSchema(),
			},
			"amount",
		),
		"temporality": enumSchema("point", "stream", "persistent"),
	})
}

func canvasPayloadSchema() map[string]interface{} {
	common := map[string]interface{}{
		"caption": stringSchema(),
		"regions": arraySchema(objectSchema(
			map[string]interface{}{
				"id": stringSchema(), "label": stringSchema(),
				"x": numberSchema(), "y": numberSchema(),
				"w": numberSchema(), "h": numberSchema(),
				"ask_ref": stringSchema(),
			},
			"id", "x", "y", "w", "h",
		)),
	}
	mediaProperties := cloneProperties(common)
	mediaProperties["media"] = objectSchema(
		map[string]interface{}{
			"kind": enumSchema("image", "video", "audio", "page"),
			"url":  stringSchema(), "mime": stringSchema(), "alt": stringSchema(),
		},
		"kind", "url",
	)
	chartProperties := cloneProperties(common)
	chartProperties["media"] = objectSchema(
		map[string]interface{}{"kind": constStringSchema("chart")},
		"kind",
	)
	chartProperties["chart"] = objectSchema(
		map[string]interface{}{
			"kind": enumSchema("area", "bar", "line", "pie", "radar", "scatter"),
			"series": arraySchema(objectSchema(
				map[string]interface{}{
					"key": stringSchema(), "name": stringSchema(), "color": stringSchema(),
				},
				"key",
			)),
			"points": arraySchema(objectSchema(
				map[string]interface{}{
					"label": stringSchema(),
					"values": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": numberSchema(),
					},
				},
				"values",
			)),
			"x_label": stringSchema(), "y_label": stringSchema(), "stacked": boolSchema(),
		},
		"kind", "points",
	)
	return map[string]interface{}{
		"type":  "object",
		"oneOf": []interface{}{objectSchema(mediaProperties, "media"), objectSchema(chartProperties, "media", "chart")},
	}
}

func cloneProperties(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func objectSchema(properties map[string]interface{}, required ...string) map[string]interface{} {
	out := map[string]interface{}{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func structureRecordSchema() map[string]interface{} {
	return structureRecordSchemaAtDepth(6)
}

func structureRecordSchemaAtDepth(depth int) map[string]interface{} {
	properties := map[string]interface{}{
		"id": stringSchema(), "label": stringSchema(), "ref": stringSchema(),
		"cells": map[string]interface{}{"type": "object", "additionalProperties": scalarSchema()},
	}
	if depth > 0 {
		properties["children"] = arraySchema(structureRecordSchemaAtDepth(depth - 1))
	}
	return objectSchema(
		properties,
	)
}

func stringSchema() map[string]interface{} { return map[string]interface{}{"type": "string"} }
func numberSchema() map[string]interface{} { return map[string]interface{}{"type": "number"} }
func boolSchema() map[string]interface{}   { return map[string]interface{}{"type": "boolean"} }
func scalarSchema() map[string]interface{} {
	return map[string]interface{}{"anyOf": []interface{}{
		stringSchema(),
		numberSchema(),
		boolSchema(),
		map[string]interface{}{"type": "null"},
	}}
}
func enumSchema(values ...string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "enum": values}
}
func constStringSchema(value string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "const": value}
}
func arraySchema(items map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"type": "array", "items": items}
}

// ParseRender maps a construct_render tool-call's arguments into a validated
// schema.Surface. It is the active-tier counterpart to ProjectEvent: the agent
// fills a trusted primitive and this function rejects anything that does not
// satisfy the frozen contract (invariant i2 — no arbitrary UI). A returned
// error is meant to flow back to the model so it can correct and retry.
func ParseRender(args map[string]interface{}) (*schema.Surface, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("construct: render arguments are empty")
	}
	kind := schema.Kind(asString(args["kind"]))
	if !schema.ValidKind(kind) {
		return nil, fmt.Errorf("construct: render kind %q is not one of the 8 primitives", kind)
	}
	payload, err := encodePayload(args["payload"])
	if err != nil {
		return nil, err
	}
	s, err := buildSurface(kind, asString(args["id"]), payload)
	if err != nil {
		return nil, err
	}
	if ref := asString(args["ref"]); ref != "" {
		s.WithRef(ref)
	}
	if attrs := parseAttributes(args["attributes"]); attrs != nil {
		s.WithAttributes(attrs)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("construct: rendered surface invalid: %w", err)
	}
	return s, nil
}

// buildSurface constructs the kind-matched surface from the payload bytes. An
// empty/null payload leaves a zero-value primitive, which Validate then rejects
// if the primitive has required fields — surfacing a clear error to the model.
func buildSurface(kind schema.Kind, id string, payload []byte) (*schema.Surface, error) {
	if id == "" {
		id = autoID(kind, payload)
	}
	switch kind {
	case schema.KindNarration:
		var p primitives.Narration
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewNarration(id, &p), nil
	case schema.KindMetric:
		var p primitives.Metric
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewMetric(id, &p), nil
	case schema.KindEntity:
		var p primitives.Entity
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewEntity(id, &p), nil
	case schema.KindStructure:
		var p primitives.Structure
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewStructure(id, &p), nil
	case schema.KindStream:
		var p primitives.Stream
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewStream(id, &p), nil
	case schema.KindTimeline:
		var p primitives.Timeline
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewTimeline(id, &p), nil
	case schema.KindCanvas:
		var p primitives.Canvas
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewCanvas(id, &p), nil
	case schema.KindAsk:
		var p primitives.Ask
		if err := unmarshalPayload(payload, &p); err != nil {
			return nil, err
		}
		return schema.NewAsk(id, &p), nil
	}
	return nil, fmt.Errorf("construct: unsupported render kind %q", kind)
}

// encodePayload turns the render arguments' `payload` into the JSON bytes the
// kind-matched primitive is decoded from. The payload is normally a nested JSON
// object, but models frequently DOUBLE-ENCODE it as a JSON string (the whole
// object stringified). In that case args["payload"] arrives as a Go string, and
// re-marshaling it would quote it AGAIN — yielding "cannot unmarshal string into
// Go value of type primitives.X" when buildSurface decodes it. Detect the string
// case and use its bytes directly so a double-encoded payload still parses.
func encodePayload(v interface{}) ([]byte, error) {
	if s, ok := v.(string); ok {
		return []byte(s), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("construct: render payload not encodable: %w", err)
	}
	return b, nil
}

// unmarshalPayload decodes the payload into dst, tolerating an empty or null
// payload (left as the zero value for Validate to judge).
func unmarshalPayload(payload []byte, dst interface{}) error {
	s := string(payload)
	if len(payload) == 0 || s == "null" {
		return nil
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("construct: render payload does not match kind: %w", err)
	}
	return nil
}

// parseAttributes decodes the optional attribute decoration; nil when absent or
// empty so an undecorated surface carries no attribute block.
func parseAttributes(v interface{}) *schema.Attributes {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var a schema.Attributes
	if err := json.Unmarshal(b, &a); err != nil {
		return nil
	}
	if a == (schema.Attributes{}) {
		return nil
	}
	return &a
}

// autoID derives a stable id from the kind + payload content when the agent
// did not supply one, so an identical re-render coalesces onto one surface.
func autoID(kind schema.Kind, payload []byte) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write(payload)
	return fmt.Sprintf("%s:%08x", kind, h.Sum32())
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}
