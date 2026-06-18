// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
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
- metric: a single named value. payload {label, value, unit?, magnitude?, trend?: up|down|flat}
- entity: a referenceable object (a tx, token, file, account). payload {type, identity, label?, fields?: [{key,value}], affordances?: [{id,label,kind?: link|copy|ask, href?, ask_ref?}]}
- structure: a collection. payload {shape: list|table|tree, columns?: [..], records: [{id?,label?,ref?,cells?:{col:val},children?:[..]}]}
- stream: append-only text (logs, a trace). payload {source?, title?, chunks: [{seq,text,channel?}], closed?}
- timeline: stateful steps over time (a plan, jobs, sub-agents). payload {title?, steps: [{id,label,status: pending|running|done|failed, detail?, ref?}]}
- canvas: a media blob. payload {media: {kind: image|video|audio|page|chart, url?, mime?, alt?}, caption?}
- ask: a question that needs a typed human answer. payload {ask_kind: choose|input|confirm|sign|upload, prompt, options?: [{id,label}], expected?, required?}

Reuse the same "id" to UPDATE a surface you already rendered (e.g. flip a timeline step to done). Optional "attributes" decorate any surface: {stakes: fact|hypothesis|decision|irreversible, confidence: 0..1, cost: {amount, unit, cap}, temporality: point|stream|persistent}. Tag an irreversible action with stakes=irreversible so it renders with the right weight.`

// renderToolParams is the JSON-Schema object for the construct_render function
// arguments. payload is left an open object (its shape depends on kind, taught
// in the description); the typed contract is enforced by ParseRender +
// schema.Validate, which is the i2 security gate.
func renderToolParams() map[string]interface{} {
	kinds := make([]string, 0, len(schema.Kinds))
	for _, k := range schema.Kinds {
		kinds = append(kinds, string(k))
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"kind": map[string]interface{}{
				"type":        "string",
				"enum":        kinds,
				"description": "Which of the 8 surface primitives to render.",
			},
			"payload": map[string]interface{}{
				"type":        "object",
				"description": "The primitive's content; its shape depends on kind (see the field guide above).",
			},
			"id": map[string]interface{}{
				"type":        "string",
				"description": "Stable id for this surface. Reuse it to update a surface you already rendered; omit to create a new one.",
			},
			"ref": map[string]interface{}{
				"type":        "string",
				"description": "Optional id of another surface this one links to.",
			},
			"attributes": map[string]interface{}{
				"type":        "object",
				"description": "Optional decoration: stakes, confidence (0..1), cost {amount,unit,cap}, temporality.",
			},
		},
		"required": []string{"kind", "payload"},
	}
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
	payload, err := json.Marshal(args["payload"])
	if err != nil {
		return nil, fmt.Errorf("construct: render payload not encodable: %w", err)
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
