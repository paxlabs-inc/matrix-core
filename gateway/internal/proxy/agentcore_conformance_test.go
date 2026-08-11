// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"matrix/gateway/internal/ledger"
	"matrix/gateway/internal/types"
)

type conformanceToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type conformanceStream struct {
	Content       string
	Reasoning     string
	ToolCalls     []conformanceToolCall
	FinishReasons []string
	Done          bool
}

func decodeConformanceStream(t *testing.T, body string) conformanceStream {
	t.Helper()
	var result conformanceStream
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			result.Done = true
			continue
		}
		var envelope struct {
			Choices []struct {
				Delta struct {
					Content   string                `json:"content"`
					Reasoning string                `json:"reasoning_content"`
					ToolCalls []conformanceToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			t.Fatalf("decode SSE payload %q: %v", payload, err)
		}
		for _, choice := range envelope.Choices {
			result.Content += choice.Delta.Content
			result.Reasoning += choice.Delta.Reasoning
			result.ToolCalls = append(result.ToolCalls, choice.Delta.ToolCalls...)
			if choice.FinishReason != nil {
				result.FinishReasons = append(result.FinishReasons, *choice.FinishReason)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE response: %v", err)
	}
	return result
}

func TestAgentCoreTwoTurnParallelToolConformance(t *testing.T) {
	var (
		mu       sync.Mutex
		requests [][]byte
		call     int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, append([]byte(nil), body...))
		call++
		current := call
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(event string) {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
		switch current {
		case 1:
			emit(`data: {"id":"turn-1","model":"mimo-v2.5-pro","choices":[{"index":0,"delta":{"reasoning_content":"Inspect both files. <tool_call><function=read_file><parameter=path>web/app.tsx</parameter></function></tool_call><tool_call><function=read_file><parameter=path>api/main.go</parameter></function></tool_call>"},"finish_reason":"stop"}]}` + "\n\n")
			emit(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n")
			emit("data: [DONE]\n\n")
		case 2:
			emit(`data: {"id":"turn-2","model":"mimo-v2.5-pro","choices":[{"index":0,"delta":{"reasoning_content":"Both results are available. ","content":"The frontend and API are consistent."},"finish_reason":"stop"}]}` + "\n\n")
			emit(`data: {"choices":[],"usage":{"prompt_tokens":23,"completion_tokens":5,"total_tokens":28}}` + "\n\n")
			emit("data: [DONE]\n\n")
		default:
			http.Error(w, "unexpected conformance turn", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()
	turnOneBody, err := json.Marshal(map[string]any{
		"model":  "mimo-v2.5-pro",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "Inspect the frontend and API."},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnOneRequest := newGatewayRequest(http.MethodPost, "/v1/chat/completions", turnOneBody, map[string]string{
		types.HeaderSlot:     types.SlotCody,
		types.HeaderIntentID: "agentcore-turn-1",
	})
	turnOneRecorder := httptest.NewRecorder()
	mux.ServeHTTP(turnOneRecorder, turnOneRequest)
	if turnOneRecorder.Code != http.StatusOK {
		t.Fatalf("turn one status=%d body=%s", turnOneRecorder.Code, turnOneRecorder.Body.String())
	}
	if strings.Contains(turnOneRecorder.Body.String(), "<tool_call>") || strings.Contains(turnOneRecorder.Body.String(), "<function=") {
		t.Fatalf("turn one leaked MiMo XML: %s", turnOneRecorder.Body.String())
	}
	turnOne := decodeConformanceStream(t, turnOneRecorder.Body.String())
	if turnOne.Reasoning != "Inspect both files. " || len(turnOne.ToolCalls) != 2 || !turnOne.Done {
		t.Fatalf("turn one normalization mismatch: %+v body=%s", turnOne, turnOneRecorder.Body.String())
	}
	if len(turnOne.FinishReasons) != 1 || turnOne.FinishReasons[0] != "tool_calls" {
		t.Fatalf("turn one finish state=%+v", turnOne.FinishReasons)
	}
	if turnOne.ToolCalls[0].ID == "" || turnOne.ToolCalls[1].ID == "" || turnOne.ToolCalls[0].ID == turnOne.ToolCalls[1].ID {
		t.Fatalf("turn one call ids are not usable: %+v", turnOne.ToolCalls)
	}

	assistantCalls := make([]any, 0, len(turnOne.ToolCalls))
	toolResults := make([]any, 0, len(turnOne.ToolCalls))
	for index, toolCall := range turnOne.ToolCalls {
		assistantCalls = append(assistantCalls, map[string]any{
			"id": toolCall.ID, "type": toolCall.Type,
			"function": map[string]any{"name": toolCall.Function.Name, "arguments": toolCall.Function.Arguments},
		})
		toolResults = append(toolResults, map[string]any{
			"role": "tool", "tool_call_id": toolCall.ID,
			"content": fmt.Sprintf("file-%d contents", index+1),
		})
	}
	turnTwoMessages := []any{
		map[string]any{"role": "user", "content": "Inspect the frontend and API."},
		map[string]any{
			"role": "assistant", "content": "", "reasoning_content": turnOne.Reasoning,
			"tool_calls": assistantCalls,
		},
	}
	turnTwoMessages = append(turnTwoMessages, toolResults...)
	turnTwoBody, err := json.Marshal(map[string]any{
		"model": "mimo-v2.5-pro", "stream": true, "messages": turnTwoMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	turnTwoRequest := newGatewayRequest(http.MethodPost, "/v1/chat/completions", turnTwoBody, map[string]string{
		types.HeaderSlot:     types.SlotCody,
		types.HeaderIntentID: "agentcore-turn-2",
	})
	turnTwoRecorder := httptest.NewRecorder()
	mux.ServeHTTP(turnTwoRecorder, turnTwoRequest)
	if turnTwoRecorder.Code != http.StatusOK {
		t.Fatalf("turn two status=%d body=%s", turnTwoRecorder.Code, turnTwoRecorder.Body.String())
	}
	if strings.Contains(turnTwoRecorder.Body.String(), "<tool_call>") || strings.Contains(turnTwoRecorder.Body.String(), "<function=") {
		t.Fatalf("turn two leaked MiMo XML: %s", turnTwoRecorder.Body.String())
	}
	turnTwo := decodeConformanceStream(t, turnTwoRecorder.Body.String())
	if turnTwo.Content != "The frontend and API are consistent." || turnTwo.Reasoning != "Both results are available. " || !turnTwo.Done {
		t.Fatalf("turn two response mismatch: %+v", turnTwo)
	}
	if len(turnTwo.FinishReasons) != 1 || turnTwo.FinishReasons[0] != "stop" || len(turnTwo.ToolCalls) != 0 {
		t.Fatalf("turn two terminal state mismatch: %+v", turnTwo)
	}

	mu.Lock()
	captured := append([][]byte(nil), requests...)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("upstream requests=%d, want 2", len(captured))
	}
	var replay struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []struct {
			Role       string                `json:"role"`
			Content    string                `json:"content"`
			Reasoning  string                `json:"reasoning_content"`
			ToolCallID string                `json:"tool_call_id"`
			ToolCalls  []conformanceToolCall `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured[1], &replay); err != nil {
		t.Fatalf("decode second upstream request: %v body=%s", err, captured[1])
	}
	if !replay.Stream || !replay.StreamOptions.IncludeUsage || len(replay.Messages) != 4 {
		t.Fatalf("second upstream request shape mismatch: %+v body=%s", replay, captured[1])
	}
	assistant := replay.Messages[1]
	if assistant.Role != "assistant" || assistant.Reasoning != turnOne.Reasoning || len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant replay mismatch: %+v", assistant)
	}
	for index := range turnOne.ToolCalls {
		if assistant.ToolCalls[index].ID != turnOne.ToolCalls[index].ID ||
			assistant.ToolCalls[index].Function.Name != turnOne.ToolCalls[index].Function.Name ||
			assistant.ToolCalls[index].Function.Arguments != turnOne.ToolCalls[index].Function.Arguments {
			t.Fatalf("assistant tool replay %d mismatch: got=%+v want=%+v", index, assistant.ToolCalls[index], turnOne.ToolCalls[index])
		}
		toolResult := replay.Messages[index+2]
		if toolResult.Role != "tool" || toolResult.ToolCallID != turnOne.ToolCalls[index].ID || toolResult.Content != fmt.Sprintf("file-%d contents", index+1) {
			t.Fatalf("tool result replay %d mismatch: %+v", index, toolResult)
		}
	}

	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 2 {
		t.Fatalf("ledger rows=%d, want 2: %+v", len(rows), rows)
	}
	for index, expected := range []struct {
		intent     string
		input      int
		completion int
	}{
		{intent: "agentcore-turn-1", input: 11, completion: 7},
		{intent: "agentcore-turn-2", input: 23, completion: 5},
	} {
		row := rows[index]
		if row.ActorDID != "did:pax:tester" || row.Slot != types.SlotCody || row.IntentID != expected.intent ||
			row.Model != "mimo-v2.5-pro" || row.TokensInput != expected.input || row.TokensOutput != expected.completion {
			t.Fatalf("ledger row %d mismatch: %+v", index, row)
		}
	}
}

func TestAgentCoreRealTCPDisconnectStillMetersFinalUsage(t *testing.T) {
	firstSent := make(chan struct{})
	usageSent := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"disconnect","choices":[{"index":0,"delta":{"content":"started"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(firstSent)

		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
			return
		case <-timer.C:
		}
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":29,"completion_tokens":13,"total_tokens":42}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(usageSent)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	gateway := httptest.NewServer(srv.Mux())
	defer gateway.Close()

	requestBody := []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"start"}],"stream":true}`)
	connection, err := net.DialTimeout("tcp", gateway.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	request := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer shh\r\nX-Matrix-Actor-DID: did:pax:tester\r\nX-Matrix-Slot: cody\r\nX-Matrix-Intent-ID: agentcore-disconnect\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", gateway.Listener.Addr().String(), len(requestBody), requestBody)
	if _, err := io.WriteString(connection, request); err != nil {
		_ = connection.Close()
		t.Fatalf("write raw request: %v", err)
	}

	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read gateway status: %v", err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		_ = connection.Close()
		t.Fatalf("gateway status line=%q", statusLine)
	}
	headers, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read gateway headers: %v", err)
	}
	var responseBody io.Reader = reader
	if strings.EqualFold(headers.Get("Transfer-Encoding"), "chunked") {
		responseBody = httputil.NewChunkedReader(reader)
	}
	responseReader := bufio.NewReader(responseBody)
	firstLine, err := responseReader.ReadString('\n')
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read first SSE line: %v", err)
	}
	if !strings.Contains(firstLine, `"content":"started"`) {
		_ = connection.Close()
		t.Fatalf("first SSE line=%q", firstLine)
	}
	select {
	case <-firstSent:
	case <-time.After(time.Second):
		_ = connection.Close()
		t.Fatal("upstream never emitted first event")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close downstream TCP connection: %v", err)
	}

	select {
	case <-usageSent:
	case <-upstreamCanceled:
		t.Fatal("downstream disconnect canceled the successful upstream call before final usage")
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not reach final usage after downstream disconnect")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := srv.ledger.(*ledger.Memory).Snapshot()
		if len(rows) == 1 {
			row := rows[0]
			if row.ActorDID != "did:pax:tester" || row.Slot != types.SlotCody || row.IntentID != "agentcore-disconnect" ||
				row.TokensInput != 29 || row.TokensOutput != 13 {
				t.Fatalf("disconnect ledger row mismatch: %+v", row)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect usage was not metered: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScopedAgentCoreOverlapDeniedUntilDisconnectedStreamFinishesDraining(t *testing.T) {
	firstSent := make(chan struct{})
	allowDrain := make(chan struct{})
	drainEventSent := make(chan struct{})
	allowUsage := make(chan struct{})
	usageSent := make(chan struct{})
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(event string) {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if current > 1 {
			emit(`data: {"id":"after-drain","choices":[{"index":0,"delta":{"content":"accepted"},"finish_reason":"stop"}]}` + "\n\n")
			emit(`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n")
			emit("data: [DONE]\n\n")
			return
		}

		emit(`data: {"id":"overlap","choices":[{"index":0,"delta":{"content":"started"},"finish_reason":null}]}` + "\n\n")
		close(firstSent)
		<-allowDrain
		emit(`data: {"id":"overlap","choices":[{"index":0,"delta":{"content":"still-running"},"finish_reason":null}]}` + "\n\n")
		close(drainEventSent)
		<-allowUsage
		emit(`data: {"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n")
		emit("data: [DONE]\n\n")
		close(usageSent)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	token, _ := configureScopedAgentCoreAuth(t, srv, srv.now())
	gateway := httptest.NewServer(srv.Mux())
	defer gateway.Close()

	requestBody := []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"start"}],"stream":true}`)
	connection, err := net.DialTimeout("tcp", gateway.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	rawRequest := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Matrix-Actor-DID: did:matrix:user-scoped:cody\r\nX-Matrix-Slot: cody\r\nX-Matrix-Intent-ID: scoped-overlap-first\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", gateway.Listener.Addr().String(), token, len(requestBody), requestBody)
	if _, err := io.WriteString(connection, rawRequest); err != nil {
		_ = connection.Close()
		t.Fatalf("write raw request: %v", err)
	}

	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, " 200 ") {
		_ = connection.Close()
		t.Fatalf("read first status line=%q err=%v", statusLine, err)
	}
	headers, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read first headers: %v", err)
	}
	var responseBody io.Reader = reader
	if strings.EqualFold(headers.Get("Transfer-Encoding"), "chunked") {
		responseBody = httputil.NewChunkedReader(reader)
	}
	firstLine, err := bufio.NewReader(responseBody).ReadString('\n')
	if err != nil || !strings.Contains(firstLine, `"content":"started"`) {
		_ = connection.Close()
		t.Fatalf("read first SSE line=%q err=%v", firstLine, err)
	}
	<-firstSent
	if err := connection.Close(); err != nil {
		t.Fatalf("close downstream: %v", err)
	}
	close(allowDrain)
	select {
	case <-drainEventSent:
	case <-time.After(time.Second):
		t.Fatal("upstream never entered the post-disconnect drain phase")
	}

	callScoped := func(intent string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(string(requestBody)))
		if err != nil {
			t.Fatalf("build scoped request: %v", err)
		}
		req.Header.Set(types.HeaderAuthorization, "Bearer "+token)
		req.Header.Set(types.HeaderActorDID, "did:matrix:user-scoped:cody")
		req.Header.Set(types.HeaderSlot, types.SlotCody)
		req.Header.Set(types.HeaderIntentID, intent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("scoped request: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read scoped response: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	status, body := callScoped("scoped-overlap-second")
	if status != http.StatusTooManyRequests || !strings.Contains(body, `"error":"agentcore_call_in_progress"`) {
		t.Fatalf("overlap denial status=%d body=%s", status, body)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("overlapping scoped call reached upstream: calls=%d", upstreamCalls.Load())
	}

	close(allowUsage)
	select {
	case <-usageSent:
	case <-time.After(3 * time.Second):
		t.Fatal("first scoped call never emitted final usage")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		status, body = callScoped("scoped-after-drain")
		if status == http.StatusOK {
			break
		}
		if status != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("post-drain call status=%d body=%s", status, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("post-drain accepted calls=%d, want 2", upstreamCalls.Load())
	}
}
