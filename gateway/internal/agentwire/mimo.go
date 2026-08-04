package agentwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const (
	mimoToolOpen       = "<tool_call>"
	mimoToolClose      = "</tool_call>"
	mimoFunctionOpen   = "<function="
	mimoFunctionClose  = "</function>"
	mimoParameterOpen  = "<parameter="
	mimoParameterClose = "</parameter>"
)

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

func ParseMiMoToolCalls(text string) (cleanedText string, parsedCalls []ToolCall, found bool, parseErr error) {
	return parseMiMoToolCalls(text, "standalone")
}

func parseMiMoToolCalls(text, context string) (cleanedText string, parsedCalls []ToolCall, found bool, parseErr error) {
	hasMarkup := strings.Contains(text, mimoToolOpen) ||
		strings.Contains(text, mimoToolClose) ||
		strings.Contains(text, mimoFunctionOpen)
	if !hasMarkup {
		return text, nil, false, nil
	}
	if !strings.Contains(text, mimoFunctionOpen) {
		return "", nil, true, fmt.Errorf("agentwire: MiMo function tag is missing")
	}

	var cleaned strings.Builder
	var calls []ToolCall
	remainder := text
	for {
		functionAt := strings.Index(remainder, mimoFunctionOpen)
		if functionAt < 0 {
			if strings.Contains(remainder, mimoToolOpen) || strings.Contains(remainder, mimoToolClose) {
				return "", nil, true, fmt.Errorf("agentwire: unmatched MiMo tool tag")
			}
			cleaned.WriteString(remainder)
			break
		}

		start := functionAt
		if toolAt := strings.LastIndex(remainder[:functionAt], mimoToolOpen); toolAt >= 0 &&
			strings.TrimSpace(remainder[toolAt+len(mimoToolOpen):functionAt]) == "" {
			start = toolAt
		}
		cleaned.WriteString(remainder[:start])

		afterFunction := remainder[functionAt+len(mimoFunctionOpen):]
		headerEnd := strings.IndexByte(afterFunction, '>')
		if headerEnd < 0 {
			return strings.TrimSpace(cleaned.String()), nil, true, fmt.Errorf("agentwire: truncated MiMo function tag")
		}
		name := strings.TrimSpace(strings.TrimSuffix(afterFunction[:headerEnd], "/"))
		if !validMiMoName(name) {
			return "", nil, true, fmt.Errorf("agentwire: invalid MiMo function name")
		}

		body := afterFunction[headerEnd+1:]
		bodyEnd := len(body)
		functionEnd := strings.Index(body, mimoFunctionClose)
		toolEnd := strings.Index(body, mimoToolClose)
		if functionEnd >= 0 {
			bodyEnd = functionEnd
		}
		if toolEnd >= 0 && toolEnd < bodyEnd {
			bodyEnd = toolEnd
		}
		arguments, err := parseMiMoParameters(body[:bodyEnd])
		if err != nil {
			return "", nil, true, err
		}

		consumed := len(body)
		switch {
		case toolEnd >= 0:
			consumed = toolEnd + len(mimoToolClose)
		case functionEnd >= 0:
			consumed = functionEnd + len(mimoFunctionClose)
		}
		rawEnd := functionAt + len(mimoFunctionOpen) + headerEnd + 1 + consumed
		if rawEnd > len(remainder) {
			rawEnd = len(remainder)
		}
		raw := remainder[start:rawEnd]
		calls = append(calls, ToolCall{
			ID:        mintToolCallID(context, len(calls), []byte(raw)),
			Name:      name,
			Arguments: arguments,
		})
		remainder = body[consumed:]
		if remainder == "" {
			break
		}
	}

	return strings.TrimSpace(cleaned.String()), calls, true, nil
}

func parseMiMoParameters(body string) (json.RawMessage, error) {
	parameters := map[string]any{}
	remainder := body
	for {
		parameterAt := strings.Index(remainder, mimoParameterOpen)
		if parameterAt < 0 {
			break
		}
		afterParameter := remainder[parameterAt+len(mimoParameterOpen):]
		headerEnd := strings.IndexByte(afterParameter, '>')
		if headerEnd < 0 {
			return nil, fmt.Errorf("agentwire: truncated MiMo parameter tag")
		}
		name := strings.TrimSpace(strings.TrimSuffix(afterParameter[:headerEnd], "/"))
		if !validMiMoName(name) {
			return nil, fmt.Errorf("agentwire: invalid MiMo parameter name")
		}
		if _, duplicate := parameters[name]; duplicate {
			return nil, fmt.Errorf("agentwire: duplicate MiMo parameter")
		}

		valueAndRest := afterParameter[headerEnd+1:]
		valueEnd := len(valueAndRest)
		for _, marker := range []string{
			mimoParameterClose,
			mimoParameterOpen,
			mimoFunctionClose,
			mimoToolClose,
		} {
			if markerAt := strings.Index(valueAndRest, marker); markerAt >= 0 && markerAt < valueEnd {
				valueEnd = markerAt
			}
		}
		parameters[name] = mimoParameterValue(strings.TrimSpace(valueAndRest[:valueEnd]))
		remainder = valueAndRest[valueEnd:]
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return nil, fmt.Errorf("agentwire: encode MiMo parameters: %w", err)
	}
	return encoded, nil
}

func mimoParameterValue(raw string) any {
	var value any
	if json.Unmarshal([]byte(raw), &value) == nil {
		if _, isString := value.(string); !isString {
			return value
		}
	}
	return raw
}

func validMiMoName(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' ||
			current >= 'A' && current <= 'Z' ||
			current >= '0' && current <= '9' ||
			current == '_' || current == '-' || current == '.' {
			continue
		}
		return false
	}
	return true
}

func NormalizeChatCompletion(body []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("agentwire: decode chat completion: %w", err)
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(envelope["choices"], &choices); err != nil {
		return nil, fmt.Errorf("agentwire: decode chat completion choices: %w", err)
	}

	for index, rawChoice := range choices {
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(rawChoice, &choice); err != nil {
			return nil, fmt.Errorf("agentwire: decode choice %d: %w", index, err)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(choice["message"], &message); err != nil {
			return nil, fmt.Errorf("agentwire: decode choice %d message: %w", index, err)
		}

		var recovered []ToolCall
		for _, field := range []string{"content", "reasoning_content"} {
			text, ok := rawString(message[field])
			if !ok {
				continue
			}
			cleaned, calls, markup, err := parseMiMoToolCalls(text, fmt.Sprintf("chat.choice.%d.%s", index, field))
			if err != nil {
				return nil, fmt.Errorf("agentwire: choice %d %s: %w", index, field, err)
			}
			if markup {
				message[field], _ = json.Marshal(cleaned)
				recovered = append(recovered, calls...)
			}
		}

		native, normalizedNative, err := normalizeNativeToolCalls(message["tool_calls"], fmt.Sprintf("chat.choice.%d.native", index))
		if err != nil {
			return nil, fmt.Errorf("agentwire: decode choice %d native tool calls: %w", index, err)
		}
		if len(native) > 0 {
			message["tool_calls"] = normalizedNative
		}
		if len(recovered) > 0 && len(native) == 0 {
			encoded, err := json.Marshal(wireToolCalls(recovered, false))
			if err != nil {
				return nil, fmt.Errorf("agentwire: encode choice %d tool calls: %w", index, err)
			}
			message["tool_calls"] = encoded
		}
		if len(recovered) > 0 && len(native) > 0 && !sameToolCalls(native, recovered) {
			return nil, fmt.Errorf("agentwire: choice %d native and textual MiMo calls disagree", index)
		}
		if len(recovered) > 0 || len(native) > 0 {
			choice["finish_reason"] = json.RawMessage(`"tool_calls"`)
		}

		encodedMessage, err := json.Marshal(message)
		if err != nil {
			return nil, fmt.Errorf("agentwire: encode choice %d message: %w", index, err)
		}
		choice["message"] = encodedMessage
		choices[index], err = json.Marshal(choice)
		if err != nil {
			return nil, fmt.Errorf("agentwire: encode choice %d: %w", index, err)
		}
	}

	encodedChoices, err := json.Marshal(choices)
	if err != nil {
		return nil, fmt.Errorf("agentwire: encode chat completion choices: %w", err)
	}
	envelope["choices"] = encodedChoices
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("agentwire: encode chat completion: %w", err)
	}
	return out, nil
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func normalizeNativeToolCalls(raw json.RawMessage, context string) ([]ToolCall, json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, raw, nil
	}
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, nil, err
	}
	calls := make([]ToolCall, 0, len(wire))
	for index, item := range wire {
		var id string
		_ = json.Unmarshal(item["id"], &id)
		var function map[string]json.RawMessage
		if err := json.Unmarshal(item["function"], &function); err != nil {
			return nil, nil, err
		}
		var name string
		_ = json.Unmarshal(function["name"], &name)
		arguments := function["arguments"]
		var encoded string
		if json.Unmarshal(arguments, &encoded) == nil {
			arguments = json.RawMessage(encoded)
		}
		if len(bytes.TrimSpace(arguments)) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if strings.TrimSpace(id) == "" {
			id = mintToolCallID(context, index, nativeToolCallMaterial(name, arguments))
			item["id"], _ = json.Marshal(id)
		}
		calls = append(calls, ToolCall{
			ID:        id,
			Name:      name,
			Arguments: arguments,
		})
	}
	normalized, err := json.Marshal(wire)
	if err != nil {
		return nil, nil, err
	}
	return calls, normalized, nil
}

func mintToolCallID(context string, occurrence int, material []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(context))
	_, _ = digest.Write([]byte{0})
	_, _ = fmt.Fprintf(digest, "%d", occurrence)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(material)
	sum := digest.Sum(nil)
	return fmt.Sprintf("mimo-%x", sum[:12])
}

func nativeToolCallMaterial(name string, arguments json.RawMessage) []byte {
	canonical := bytes.TrimSpace(arguments)
	var value any
	if json.Unmarshal(canonical, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}
	material := make([]byte, 0, len(name)+1+len(canonical))
	material = append(material, name...)
	material = append(material, 0)
	return append(material, canonical...)
}

func sameToolCalls(left, right []ToolCall) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name ||
			!sameJSONValue(left[index].Arguments, right[index].Arguments) {
			return false
		}
	}
	return true
}

func sameJSONValue(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func wireToolCalls(calls []ToolCall, streaming bool) []map[string]any {
	wire := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		item := map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(call.Arguments),
			},
		}
		if streaming {
			item["index"] = index
		}
		wire = append(wire, item)
	}
	return wire
}
