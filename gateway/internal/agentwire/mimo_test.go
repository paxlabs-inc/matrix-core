package agentwire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMiMoToolCallsRecordedGrammar(t *testing.T) {
	input := "before\n<tool_call><function=read_file><parameter=path>/workspace/a.go</parameter></function></tool_call>\n" +
		"<tool_call><function=search><parameter=query>{\"symbol\":\"Run\"}</parameter>"
	cleaned, calls, markup, err := ParseMiMoToolCalls(input)
	if err != nil {
		t.Fatalf("ParseMiMoToolCalls: %v", err)
	}
	if !markup || cleaned != "before" {
		t.Fatalf("markup=%v cleaned=%q", markup, cleaned)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%d (%+v)", len(calls), calls)
	}
	if calls[0].Name != "read_file" || string(calls[0].Arguments) != `{"path":"/workspace/a.go"}` {
		t.Fatalf("first call=%+v", calls[0])
	}
	if calls[1].Name != "search" || string(calls[1].Arguments) != `{"query":{"symbol":"Run"}}` {
		t.Fatalf("second call=%+v", calls[1])
	}
	if !strings.HasPrefix(calls[0].ID, "mimo-") || len(calls[0].ID) != len("mimo-")+24 {
		t.Fatalf("stable id shape=%q", calls[0].ID)
	}
	_, repeated, _, err := ParseMiMoToolCalls(input)
	if err != nil || repeated[0].ID != calls[0].ID {
		t.Fatalf("id is not deterministic: %v %+v", err, repeated)
	}
}

func TestParseMiMoToolCallsIdenticalParallelIDsAreUniqueAndStable(t *testing.T) {
	call := `<tool_call><function=read_file><parameter=path>/same</parameter></function></tool_call>`
	input := call + call
	_, first, _, err := ParseMiMoToolCalls(input)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	_, second, _, err := ParseMiMoToolCalls(input)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("calls: first=%+v second=%+v", first, second)
	}
	if first[0].ID == first[1].ID {
		t.Fatalf("identical parallel calls collided: %+v", first)
	}
	if first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("minted ids are not stable: first=%+v second=%+v", first, second)
	}
}

func TestNormalizeChatCompletionReasoningAndNativeAuthority(t *testing.T) {
	textual := `<tool_call><function=read_file><parameter=path>/workspace/a.go</parameter></function></tool_call>`
	body := []byte(`{"id":"chatcmpl-mimo","choices":[{"index":0,"message":{"role":"assistant","content":"` + textual + `","reasoning_content":"checking","tool_calls":[{"id":"native-7","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/workspace/a.go\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`)

	out, err := NormalizeChatCompletion(body)
	if err != nil {
		t.Fatalf("NormalizeChatCompletion: %v", err)
	}
	if bytes.Contains(out, []byte("<tool_call>")) {
		t.Fatalf("XML leaked: %s", out)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decode normalized: %v", err)
	}
	choice := envelope.Choices[0]
	if choice.Message.Content != "" || choice.Message.Reasoning != "checking" {
		t.Fatalf("channels not cleaned: %+v", choice.Message)
	}
	if len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0].ID != "native-7" {
		t.Fatalf("native call was not authoritative: %+v", choice.Message.ToolCalls)
	}
	if choice.FinishReason != "tool_calls" || envelope.Usage["total_tokens"] != 13 {
		t.Fatalf("finish/usage lost: %+v", envelope)
	}
}

func TestNormalizeChatCompletionRejectsNativeTextualDisagreement(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"<tool_call><function=read_file><parameter=path>/a</parameter></function></tool_call>","tool_calls":[{"id":"native","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/b\"}"}}]},"finish_reason":"stop"}]}`)
	if _, err := NormalizeChatCompletion(body); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("expected disagreement, got %v", err)
	}
}

func TestNormalizeChatCompletionMintsOnlyMissingNativeIDsStably(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"content":"","tool_calls":[{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/same\"}"}},{"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/same\"}"}},{"id":"native-kept","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"/a\"}"}}]},"finish_reason":"stop"}]}`)
	first, err := NormalizeChatCompletion(body)
	if err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	second, err := NormalizeChatCompletion(first)
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	type response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	var left, right response
	if err := json.Unmarshal(first, &left); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second, &right); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	leftCalls := left.Choices[0].Message.ToolCalls
	rightCalls := right.Choices[0].Message.ToolCalls
	if len(leftCalls) != 3 || len(rightCalls) != 3 {
		t.Fatalf("native calls missing: left=%+v right=%+v", leftCalls, rightCalls)
	}
	if leftCalls[0].ID == "" || leftCalls[1].ID == "" || leftCalls[0].ID == leftCalls[1].ID {
		t.Fatalf("missing native ids were not uniquely minted: %+v", leftCalls)
	}
	if leftCalls[2].ID != "native-kept" {
		t.Fatalf("valid native id changed: %+v", leftCalls)
	}
	for index := range leftCalls {
		if leftCalls[index].ID != rightCalls[index].ID {
			t.Fatalf("native id %d is unstable: left=%q right=%q", index, leftCalls[index].ID, rightCalls[index].ID)
		}
	}
}

func TestStreamNormalizerHoldsSplitXMLAndEmitsParallelCalls(t *testing.T) {
	normalizer := NewStreamNormalizer()
	lines := [][]byte{
		[]byte("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"checking \"},\"finish_reason\":null}]}\n"),
		[]byte("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"<tool_\"},\"finish_reason\":null}]}\n"),
		[]byte("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"call><function=read_file><parameter=path>/a</parameter></function></tool_call><tool_call><function=read_file><parameter=path>/b</parameter>\"},\"finish_reason\":null}]}\n"),
		[]byte("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n"),
		[]byte("data: [DONE]\n"),
	}
	var output []byte
	for index, line := range lines {
		events, err := normalizer.PushLine(line)
		if err != nil {
			t.Fatalf("PushLine %d: %v", index, err)
		}
		for _, event := range events {
			output = append(output, event...)
		}
	}
	if bytes.Contains(output, []byte("<tool_")) || bytes.Contains(output, []byte("<function=")) {
		t.Fatalf("split XML leaked: %s", output)
	}
	if !bytes.Contains(output, []byte(`"reasoning_content":"checking `)) {
		t.Fatalf("ordinary reasoning was not preserved: %s", output)
	}
	if bytes.Count(output, []byte(`"type":"function"`)) != 2 {
		t.Fatalf("parallel calls missing: %s", output)
	}
	if !bytes.Contains(output, []byte(`"finish_reason":"tool_calls"`)) ||
		!bytes.Contains(output, []byte(`data: [DONE]`)) {
		t.Fatalf("stream terminal events wrong: %s", output)
	}
}

func TestStreamNormalizerRetainsMatchingNativeIDAndRejectsMismatch(t *testing.T) {
	matching := NewStreamNormalizer()
	lines := [][]byte{
		[]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"native-9","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]},"finish_reason":null}]}` + "\n"),
		[]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/a\"}"}}],"content":"<tool_call><function=read_file><parameter=path>/a</parameter></function></tool_call>"},"finish_reason":null}]}` + "\n"),
		[]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"),
	}
	var output []byte
	for _, line := range lines {
		events, err := matching.PushLine(line)
		if err != nil {
			t.Fatalf("matching native stream: %v", err)
		}
		for _, event := range events {
			output = append(output, event...)
		}
	}
	if !bytes.Contains(output, []byte(`"id":"native-9"`)) ||
		bytes.Count(output, []byte(`"type":"function"`)) != 1 ||
		bytes.Contains(output, []byte("<tool_call>")) {
		t.Fatalf("native stream was not authoritative: %s", output)
	}

	mismatch := NewStreamNormalizer()
	for _, line := range lines[:1] {
		if _, err := mismatch.PushLine(line); err != nil {
			t.Fatalf("mismatch native setup: %v", err)
		}
	}
	_, err := mismatch.PushLine([]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/a\"}"}}],"content":"<tool_call><function=read_file><parameter=path>/b</parameter></function></tool_call>"},"finish_reason":null}]}` + "\n"))
	if err != nil {
		t.Fatalf("mismatch textual setup: %v", err)
	}
	_, err = mismatch.PushLine(lines[2])
	if err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("expected streaming disagreement, got %v", err)
	}
}

func TestStreamNormalizerMintsStableUniqueMissingNativeIDs(t *testing.T) {
	lines := [][]byte{
		[]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/same\"}"}},{"index":1,"type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/same\"}"}},{"index":2,"id":"native-kept","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"/a\"}"}}]},"finish_reason":null}]}` + "\n"),
		[]byte(`data: {"id":"chatcmpl-mimo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"),
	}
	normalize := func() []string {
		t.Helper()
		normalizer := NewStreamNormalizer()
		var output []byte
		for _, line := range lines {
			events, err := normalizer.PushLine(line)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			for _, event := range events {
				output = append(output, event...)
			}
		}
		var ids []string
		for _, line := range bytes.Split(output, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var envelope struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &envelope); err != nil {
				t.Fatalf("decode event: %v", err)
			}
			for _, choice := range envelope.Choices {
				for _, call := range choice.Delta.ToolCalls {
					ids = append(ids, call.ID)
				}
			}
		}
		return ids
	}
	first := normalize()
	second := normalize()
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("native stream ids missing: first=%+v second=%+v", first, second)
	}
	if first[0] == "" || first[1] == "" || first[0] == first[1] {
		t.Fatalf("missing stream ids were not uniquely minted: %+v", first)
	}
	if first[2] != "native-kept" {
		t.Fatalf("valid stream id changed: %+v", first)
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("stream id %d is unstable: first=%q second=%q", index, first[index], second[index])
		}
	}
}
