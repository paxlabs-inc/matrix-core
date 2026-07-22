// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"encoding/json"
	"strconv"
	"strings"
)

// mimo.go is Neo's native adapter for Xiaomi MiMo (MiMo-V2.5-Pro, the current
// main/cheap/consolidation model). It mirrors, on the client side, the two
// server flags Xiaomi's vLLM recipe recommends for MiMo:
//
//	--tool-call-parser mimo     → parseMimoToolCalls   (the qwen3_xml tag grammar)
//	--reasoning-parser  mimo    → mimoReasoningContent (DeepSeek-compatible thinking)
//
// The provider (api.xiaomimimo.com, and any self-hosted vLLM) does not always
// parse tool calls server-side, so MiMo emits them inline in `content` using
// the qwen3_xml tag grammar. And MiMo uses DeepSeek-style thinking, which
// REQUIRES prior assistant reasoning_content to be replayed through a
// multi-turn tool conversation — dropping it (or omitting the empty string on a
// tool-call turn) degrades or 400s the next turn. This file owns both halves so
// MiMo compatibility lives in one legible place instead of scattered fallbacks.

// The qwen3_xml tag grammar MiMo uses when the server does not parse tool calls:
// a `<tool_call>` wrapper, a `<function=NAME>` header, then one
// `<parameter=key>value` span per argument. Closing tags (`</parameter>`,
// `</function>`, `</tool_call>`) are frequently omitted or lost to truncation,
// so the parser treats every closer as optional.
const (
	mimoToolCallOpen  = "<tool_call>"
	mimoToolCallClose = "</tool_call>"
	mimoFunctionOpen  = "<function="
	mimoFunctionClose = "</function>"
	mimoParamOpen     = "<parameter="
	mimoParamClose    = "</parameter>"
)

// parseMimoToolCalls is the native MiMo (qwen3_xml) tool-call parser: it pulls
// tag-grammar tool calls out of content and returns the content with those
// spans removed plus the structured calls. Parameter values run to the next
// parameter/closer (or end of content on truncation); each value is kept as a
// raw string unless it parses as standalone JSON. Returns (content, nil) when
// there is nothing to extract, so a server-parsed response is never disturbed.
func parseMimoToolCalls(content string) (string, []ToolCall) {
	if !strings.Contains(content, mimoFunctionOpen) {
		return content, nil
	}
	var b strings.Builder
	var calls []ToolCall
	rest := content
	for {
		fi := strings.Index(rest, mimoFunctionOpen)
		if fi < 0 {
			b.WriteString(rest)
			break
		}
		start := fi
		// Fold a preceding `<tool_call>` wrapper (only whitespace between it
		// and the function header) into the stripped span.
		if ti := strings.LastIndex(rest[:fi], mimoToolCallOpen); ti >= 0 &&
			strings.TrimSpace(rest[ti+len(mimoToolCallOpen):fi]) == "" {
			start = ti
		}
		b.WriteString(rest[:start])
		after := rest[fi+len(mimoFunctionOpen):]
		gt := strings.IndexByte(after, '>')
		if gt < 0 {
			// Truncated open tag: drop everything from the tag onward.
			break
		}
		name := strings.TrimSpace(strings.TrimSuffix(after[:gt], "/"))
		body := after[gt+1:]
		argEnd := len(body)
		fnEnd := strings.Index(body, mimoFunctionClose)
		tcEnd := strings.Index(body, mimoToolCallClose)
		if fnEnd >= 0 {
			argEnd = fnEnd
		}
		if tcEnd >= 0 && tcEnd < argEnd {
			argEnd = tcEnd
		}
		if name != "" {
			calls = append(calls, ToolCall{
				ID:       "call_" + strconv.Itoa(len(calls)),
				Type:     "function",
				Function: FunctionCall{Name: name, Arguments: mimoParamArgs(body[:argEnd])},
			})
		}
		switch {
		case tcEnd >= 0:
			rest = body[tcEnd+len(mimoToolCallClose):]
		case fnEnd >= 0:
			rest = body[fnEnd+len(mimoFunctionClose):]
		default:
			// Unterminated call (truncated generation): consume the remainder.
			rest = ""
		}
		if rest == "" {
			break
		}
	}
	if len(calls) == 0 {
		return content, nil
	}
	return strings.TrimSpace(b.String()), calls
}

// mimoParamArgs parses the `<parameter=key>value` spans of one call body into a
// JSON-object argument string. Values are trimmed raw strings; a value that is
// itself standalone JSON (object/array/number/bool/null) is kept typed so
// numeric and structured arguments survive the round trip.
func mimoParamArgs(body string) string {
	args := map[string]interface{}{}
	rest := body
	for {
		i := strings.Index(rest, mimoParamOpen)
		if i < 0 {
			break
		}
		after := rest[i+len(mimoParamOpen):]
		gt := strings.IndexByte(after, '>')
		if gt < 0 {
			break
		}
		key := strings.TrimSpace(strings.TrimSuffix(after[:gt], "/"))
		val := after[gt+1:]
		end := len(val)
		if j := strings.Index(val, mimoParamClose); j >= 0 && j < end {
			end = j
		}
		if j := strings.Index(val, mimoParamOpen); j >= 0 && j < end {
			end = j
		}
		if j := strings.Index(val, mimoFunctionClose); j >= 0 && j < end {
			end = j
		}
		if j := strings.Index(val, mimoToolCallClose); j >= 0 && j < end {
			end = j
		}
		if key != "" {
			args[key] = mimoParamValue(strings.TrimSpace(val[:end]))
		}
		rest = val[end:]
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// mimoParamValue keeps a parameter value as its raw string unless the whole
// value parses as standalone JSON, in which case the typed value is preserved.
func mimoParamValue(raw string) interface{} {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		if _, isString := v.(string); !isString {
			return v
		}
	}
	return raw
}

// mimoReasoningContent is the request-side half of the MiMo reasoning adapter
// (the `--reasoning-parser mimo` counterpart). MiMo uses DeepSeek-compatible
// thinking: prior assistant reasoning_content must be replayed through a
// multi-turn tool conversation, and on an assistant turn that is part of a
// tool-call chain the field must be present VERBATIM — including the empty
// string "" — or a strict deserializer 400s ("reasoning_content" required) and
// the model loses the thread it was mid-thought on.
//
// So for a MiMo request every assistant message carries reasoning_content
// (a non-nil pointer, possibly to ""); user / tool / system messages never do.
// The pointer lets an explicit "" ride the wire that `omitempty` would drop.
// A resumed conversation whose durable seed lost the reasoning (it is never
// persisted) degrades gracefully: the assistant turn ships reasoning_content:""
// which MiMo accepts, rather than an absent field it may reject.
func mimoReasoningContent(role, reasoning string) *string {
	if role != RoleAssistant {
		return nil
	}
	return &reasoning
}
