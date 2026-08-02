package agentwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type StreamNormalizer struct {
	choices  map[int]*streamChoice
	template map[string]json.RawMessage
}

type streamChoice struct {
	content   streamChannel
	reasoning streamChannel
	native    map[int]*nativeStreamCall
	finalized bool
}

type nativeStreamCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

type streamChannel struct {
	pending strings.Builder
	capture bool
}

func NewStreamNormalizer() *StreamNormalizer {
	return &StreamNormalizer{choices: map[int]*streamChoice{}}
}

func (normalizer *StreamNormalizer) PushLine(line []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return [][]byte{append([]byte(nil), line...)}, nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		flushed, err := normalizer.Flush()
		if err != nil {
			return nil, err
		}
		return append(flushed, append([]byte(nil), line...)), nil
	}
	if len(payload) == 0 {
		return [][]byte{append([]byte(nil), line...)}, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("agentwire: decode MiMo stream event: %w", err)
	}
	normalizer.rememberTemplate(envelope)

	var choices []json.RawMessage
	if raw, ok := envelope["choices"]; ok {
		if err := json.Unmarshal(raw, &choices); err != nil {
			return nil, fmt.Errorf("agentwire: decode MiMo stream choices: %w", err)
		}
	}
	if len(choices) == 0 {
		return [][]byte{encodeSSEEnvelope(envelope)}, nil
	}

	outputChoices := make([]json.RawMessage, 0, len(choices))
	for _, rawChoice := range choices {
		choice, emit, err := normalizer.normalizeChoice(rawChoice)
		if err != nil {
			return nil, err
		}
		if emit {
			outputChoices = append(outputChoices, choice)
		}
	}
	if len(outputChoices) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(outputChoices)
	if err != nil {
		return nil, fmt.Errorf("agentwire: encode MiMo stream choices: %w", err)
	}
	envelope["choices"] = encoded
	return [][]byte{encodeSSEEnvelope(envelope)}, nil
}

func (normalizer *StreamNormalizer) normalizeChoice(raw json.RawMessage) (json.RawMessage, bool, error) {
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, false, fmt.Errorf("agentwire: decode MiMo stream choice: %w", err)
	}
	index := 0
	_ = json.Unmarshal(choice["index"], &index)
	state := normalizer.choices[index]
	if state == nil {
		state = &streamChoice{native: map[int]*nativeStreamCall{}}
		normalizer.choices[index] = state
	}

	delta := map[string]json.RawMessage{}
	if rawDelta, ok := choice["delta"]; ok && !bytes.Equal(bytes.TrimSpace(rawDelta), []byte("null")) {
		if err := json.Unmarshal(rawDelta, &delta); err != nil {
			return nil, false, fmt.Errorf("agentwire: decode MiMo stream delta: %w", err)
		}
	}

	if rawCalls, ok := delta["tool_calls"]; ok {
		if err := state.appendNative(rawCalls); err != nil {
			return nil, false, err
		}
		delete(delta, "tool_calls")
	}
	for _, current := range []struct {
		field   string
		channel *streamChannel
	}{
		{field: "content", channel: &state.content},
		{field: "reasoning_content", channel: &state.reasoning},
	} {
		field, channel := current.field, current.channel
		text, ok := rawString(delta[field])
		if !ok {
			continue
		}
		safe := channel.push(text)
		if safe == "" {
			delete(delta, field)
		} else {
			delta[field], _ = json.Marshal(safe)
		}
	}

	finish, hasFinish := rawString(choice["finish_reason"])
	if hasFinish {
		recovered, err := state.finalize(delta, index)
		if err != nil {
			return nil, false, fmt.Errorf("agentwire: MiMo stream choice %d: %w", index, err)
		}
		native := state.nativeCalls(index)
		if len(recovered) > 0 && len(native) > 0 && !sameToolCalls(native, recovered) {
			return nil, false, fmt.Errorf("agentwire: MiMo stream choice %d native and textual calls disagree", index)
		}
		switch {
		case len(native) > 0:
			delta["tool_calls"], _ = json.Marshal(wireNativeStreamCalls(native))
			choice["finish_reason"] = json.RawMessage(`"tool_calls"`)
		case len(recovered) > 0:
			delta["tool_calls"], _ = json.Marshal(wireToolCalls(recovered, true))
			choice["finish_reason"] = json.RawMessage(`"tool_calls"`)
		default:
			_ = finish
		}
		state.finalized = true
	}

	if len(delta) > 0 {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return nil, false, fmt.Errorf("agentwire: encode MiMo stream delta: %w", err)
		}
		choice["delta"] = encoded
	} else {
		choice["delta"] = json.RawMessage(`{}`)
	}
	if len(delta) == 0 && !hasFinish {
		return nil, false, nil
	}
	encoded, err := json.Marshal(choice)
	if err != nil {
		return nil, false, fmt.Errorf("agentwire: encode MiMo stream choice: %w", err)
	}
	return encoded, true, nil
}

func (normalizer *StreamNormalizer) Flush() ([][]byte, error) {
	indexes := make([]int, 0, len(normalizer.choices))
	for index, state := range normalizer.choices {
		if !state.finalized {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)

	var output [][]byte
	for _, index := range indexes {
		state := normalizer.choices[index]
		delta := map[string]json.RawMessage{}
		recovered, err := state.finalize(delta, index)
		if err != nil {
			return nil, fmt.Errorf("agentwire: MiMo stream choice %d: %w", index, err)
		}
		native := state.nativeCalls(index)
		if len(recovered) > 0 && len(native) > 0 && !sameToolCalls(native, recovered) {
			return nil, fmt.Errorf("agentwire: MiMo stream choice %d native and textual calls disagree", index)
		}
		finish := any(nil)
		switch {
		case len(native) > 0:
			delta["tool_calls"], _ = json.Marshal(wireNativeStreamCalls(native))
			finish = "tool_calls"
		case len(recovered) > 0:
			delta["tool_calls"], _ = json.Marshal(wireToolCalls(recovered, true))
			finish = "tool_calls"
		}
		state.finalized = true
		if len(delta) == 0 {
			continue
		}
		choice := map[string]any{
			"index":         index,
			"delta":         delta,
			"finish_reason": finish,
		}
		envelope := cloneEnvelope(normalizer.template)
		envelope["choices"], _ = json.Marshal([]any{choice})
		output = append(output, append(encodeSSEEnvelope(envelope), '\n'))
	}
	return output, nil
}

func (normalizer *StreamNormalizer) rememberTemplate(envelope map[string]json.RawMessage) {
	template := cloneEnvelope(envelope)
	delete(template, "choices")
	delete(template, "usage")
	if len(template) > 0 {
		normalizer.template = template
	}
}

func (state *streamChoice) appendNative(raw json.RawMessage) error {
	var fragments []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &fragments); err != nil {
		return fmt.Errorf("agentwire: decode MiMo native stream calls: %w", err)
	}
	for _, fragment := range fragments {
		call := state.native[fragment.Index]
		if call == nil {
			call = &nativeStreamCall{}
			state.native[fragment.Index] = call
		}
		if fragment.ID != "" {
			call.ID = fragment.ID
		}
		call.Name += fragment.Function.Name
		call.Arguments.WriteString(fragment.Function.Arguments)
	}
	return nil
}

func (state *streamChoice) nativeCalls(choiceIndex int) []ToolCall {
	indexes := make([]int, 0, len(state.native))
	for index := range state.native {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]ToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := state.native[index]
		arguments := call.Arguments.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		id := call.ID
		if strings.TrimSpace(id) == "" {
			id = mintToolCallID(
				fmt.Sprintf("stream.choice.%d.native", choiceIndex),
				index,
				nativeToolCallMaterial(call.Name, json.RawMessage(arguments)),
			)
		}
		calls = append(calls, ToolCall{
			ID:        id,
			Name:      call.Name,
			Arguments: json.RawMessage(arguments),
		})
	}
	return calls
}

func (state *streamChoice) finalize(delta map[string]json.RawMessage, choiceIndex int) ([]ToolCall, error) {
	var recovered []ToolCall
	for _, current := range []struct {
		field   string
		channel *streamChannel
	}{
		{field: "content", channel: &state.content},
		{field: "reasoning_content", channel: &state.reasoning},
	} {
		field, channel := current.field, current.channel
		cleaned, calls, err := channel.finish(fmt.Sprintf("stream.choice.%d.%s", choiceIndex, field))
		if err != nil {
			return nil, err
		}
		if cleaned != "" {
			if existing, ok := rawString(delta[field]); ok {
				cleaned = existing + cleaned
			}
			delta[field], _ = json.Marshal(cleaned)
		}
		recovered = append(recovered, calls...)
	}
	return recovered, nil
}

func (channel *streamChannel) push(fragment string) string {
	channel.pending.WriteString(fragment)
	if channel.capture {
		return ""
	}
	value := channel.pending.String()
	markerAt := firstMarker(value)
	if markerAt >= 0 {
		safe := value[:markerAt]
		captured := value[markerAt:]
		channel.pending.Reset()
		channel.pending.WriteString(captured)
		channel.capture = true
		return safe
	}
	hold := partialMarkerSuffix(value)
	safeEnd := len(value) - hold
	safe := value[:safeEnd]
	remaining := value[safeEnd:]
	channel.pending.Reset()
	channel.pending.WriteString(remaining)
	return safe
}

func (channel *streamChannel) finish(context string) (string, []ToolCall, error) {
	value := channel.pending.String()
	channel.pending.Reset()
	if value == "" {
		return "", nil, nil
	}
	if !channel.capture {
		if partialMarkerSuffix(value) == len(value) && strings.HasPrefix(value, "<") {
			return "", nil, nil
		}
		return value, nil, nil
	}
	cleaned, calls, _, err := parseMiMoToolCalls(value, context)
	return cleaned, calls, err
}

func firstMarker(value string) int {
	best := -1
	for _, marker := range []string{mimoToolOpen, mimoFunctionOpen} {
		if at := strings.Index(value, marker); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

func partialMarkerSuffix(value string) int {
	longest := 0
	for _, marker := range []string{mimoToolOpen, mimoFunctionOpen} {
		limit := len(marker) - 1
		if len(value) < limit {
			limit = len(value)
		}
		for length := 1; length <= limit; length++ {
			if strings.HasSuffix(value, marker[:length]) && length > longest {
				longest = length
			}
		}
	}
	return longest
}

func wireNativeStreamCalls(calls []ToolCall) []map[string]any {
	wire := wireToolCalls(calls, true)
	for index := range wire {
		wire[index]["id"] = calls[index].ID
	}
	return wire
}

func cloneEnvelope(source map[string]json.RawMessage) map[string]json.RawMessage {
	clone := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		clone[key] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func encodeSSEEnvelope(envelope map[string]json.RawMessage) []byte {
	payload, _ := json.Marshal(envelope)
	return append(append([]byte("data: "), payload...), '\n')
}
