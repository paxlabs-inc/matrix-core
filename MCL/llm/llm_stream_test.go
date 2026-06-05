// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"matrix/mcl/mtx/interpreter"
)

// ---------------------------------------------------------------------------
// (*Client).Stream — Session 31c (model router · P3a)
// ---------------------------------------------------------------------------

// streamServer builds an httptest server that emits an OpenAI-compatible
// text/event-stream response made of the given content chunks. Each
// chunk becomes a `data: <json>\n\n` frame; the server closes with
// `data: [DONE]\n\n` when terminate is true.
func streamServer(t *testing.T, chunks []string, terminate bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify this is actually a streaming request — the request
		// body should set "stream": true (matches buildRequest path).
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server: decode request: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		if !req.Stream {
			t.Errorf("server: stream=true not set on request")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			frame := streamFrame{
				ID: "test-stream-id",
				Choices: []streamChoice{
					{Index: 0, Delta: streamDelta{Role: "assistant", Content: c}},
				},
			}
			payload, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if terminate {
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := NewChatClient(&Config{
		Model:    "test/model",
		APIKey:   "test-key",
		Endpoint: endpoint,
	})
	if err != nil {
		t.Fatalf("NewChatClient: %v", err)
	}
	return c
}

func TestStream_AccumulatesAndInvokesDelta(t *testing.T) {
	chunks := []string{"Hello", ", ", "world", "!"}
	srv := streamServer(t, chunks, true)
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	var mu sync.Mutex
	var seen []string
	full, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "hi"},
	}, "", func(d string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, d)
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if full != "Hello, world!" {
		t.Errorf("full = %q, want %q", full, "Hello, world!")
	}
	if len(seen) != len(chunks) {
		t.Errorf("delta count = %d, want %d (got %v)", len(seen), len(chunks), seen)
	}
	for i, c := range chunks {
		if i >= len(seen) {
			break
		}
		if seen[i] != c {
			t.Errorf("delta[%d] = %q, want %q", i, seen[i], c)
		}
	}
}

func TestStream_NilDeltaCallbackOK(t *testing.T) {
	srv := streamServer(t, []string{"a", "b", "c"}, true)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	full, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err != nil {
		t.Fatalf("Stream(nil callback): %v", err)
	}
	if full != "abc" {
		t.Errorf("full = %q, want %q", full, "abc")
	}
}

func TestStream_HandlesMissingDONESentinel(t *testing.T) {
	// Some proxies / vLLM builds close the stream without [DONE].
	// Stream must still return whatever was accumulated.
	srv := streamServer(t, []string{"part1 ", "part2"}, false)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	full, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if full != "part1 part2" {
		t.Errorf("full = %q, want %q", full, "part1 part2")
	}
}

func TestStream_RoleOnlyOpeningChunkIgnored(t *testing.T) {
	// Many providers send a first delta with role="assistant" and no
	// content. parseSSEStream must skip it (no spurious empty delta).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		// Role-only opener.
		open := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Role: "assistant"}}}}
		b, _ := json.Marshal(open)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		// Content frame.
		body := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: "tok"}}}}
		b2, _ := json.Marshal(body)
		fmt.Fprintf(w, "data: %s\n\n", b2)
		flusher.Flush()

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	var deltas []string
	full, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if full != "tok" {
		t.Errorf("full = %q, want %q", full, "tok")
	}
	if len(deltas) != 1 || deltas[0] != "tok" {
		t.Errorf("deltas = %v, want [\"tok\"] (role-only opener leaked)", deltas)
	}
}

func TestStream_HTTPErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(chatResponse{
			Error: &chatErrorBody{Message: "rate limited", Type: "rate_limit"},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v, want to contain 'rate limited'", err)
	}
}

func TestStream_InlineErrorFrameReturnsPartial(t *testing.T) {
	// A provider may emit a few content frames and then an error frame
	// before [DONE]. Stream MUST return both the partial text AND the
	// error so the caller can render the partial then surface the
	// failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		ok := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: "before-error"}}}}
		b, _ := json.Marshal(ok)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		bad := streamFrame{Error: &chatErrorBody{Message: "context length exceeded"}}
		b2, _ := json.Marshal(bad)
		fmt.Fprintf(w, "data: %s\n\n", b2)
		flusher.Flush()
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err == nil {
		t.Fatal("expected error from inline error frame")
	}
	if !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("err = %v, want to contain provider message", err)
	}
	if got != "before-error" {
		t.Errorf("partial = %q, want %q (must surface text before error)", got, "before-error")
	}
}

func TestStream_TolerantOfNonDataFrames(t *testing.T) {
	// Comments (`: keepalive`), event:, id:, retry: lines must be skipped
	// without aborting the stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		// Comment / heartbeat.
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()

		// event: type prefix (some proxies).
		fmt.Fprint(w, "event: message\n")
		fr := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: "hi"}}}}
		b, _ := json.Marshal(fr)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		// id: + retry: framing (allowed by the SSE spec, ignored here).
		fmt.Fprint(w, "id: 42\n")
		fmt.Fprint(w, "retry: 1000\n")
		fr2 := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: "!"}}}}
		b2, _ := json.Marshal(fr2)
		fmt.Fprintf(w, "data: %s\n\n", b2)
		flusher.Flush()

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	full, err := c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "x"},
	}, "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if full != "hi!" {
		t.Errorf("full = %q, want %q (non-data frames must be ignored)", full, "hi!")
	}
}

func TestStream_ContextCancellation(t *testing.T) {
	// A slow server that never finishes; verify ctx cancel returns
	// promptly with ctx.Err() (not a generic read error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		fr := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: "first"}}}}
		b, _ := json.Marshal(fr)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()

		// Then sleep forever (until the client gives up).
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Stream(ctx, []interpreter.Message{{Role: "user", Content: "x"}}, "", nil)
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	// Context-cancelled requests can return either ctx.Err() (caught
	// in the read loop) OR a wrapped net error from http.Client. Both
	// are acceptable; what matters is the request returned promptly.
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want context cancellation", err)
	}
}

func TestStream_GrammarConstraintForwarded(t *testing.T) {
	// Streaming must still apply response_format when the grammar mode
	// supports it. Fireworks accepts json_schema with stream=true.
	var captured *chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		captured = &req

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		fr := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: `{"verb":"build"}`}}}}
		b, _ := json.Marshal(fr)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := NewChatClient(&Config{
		Model:       "test/model",
		APIKey:      "test-key",
		Endpoint:    srv.URL,
		GrammarMode: GrammarJSONSchema,
		Grammars:    DefaultGrammars(),
	})
	if err != nil {
		t.Fatalf("NewChatClient: %v", err)
	}
	_, err = c.Stream(context.Background(), []interpreter.Message{
		{Role: "user", Content: "find my wallet"},
	}, "intent_frame@1", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if captured == nil || captured.ResponseFormat == nil {
		t.Fatalf("response_format not forwarded on stream request: %+v", captured)
	}
	if captured.ResponseFormat.Type != "json_schema" {
		t.Errorf("type = %q, want json_schema", captured.ResponseFormat.Type)
	}
	if !captured.Stream {
		t.Errorf("stream flag must be set on streaming request")
	}
}

func TestStream_DecodeAndStreamReturnSameFinalText(t *testing.T) {
	// The contract: Stream's accumulated text == Decode's full text on
	// equivalent requests. Asserts streaming is purely a delivery-mode
	// change, not a content change (replay invariant safety).
	const want = "deterministic full payload"

	streamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		// Split on word boundaries to mimic real tokenization.
		for _, tok := range []string{"deter", "ministic", " full", " payload"} {
			fr := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: tok}}}}
			b, _ := json.Marshal(fr)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	decodeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: want}}},
		})
	})
	streamSrv := httptest.NewServer(streamHandler)
	defer streamSrv.Close()
	decodeSrv := httptest.NewServer(decodeHandler)
	defer decodeSrv.Close()

	cs := newTestClient(t, streamSrv.URL)
	cd := newTestClient(t, decodeSrv.URL)

	streamed, err := cs.Stream(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	decoded, err := cd.Decode(context.Background(), []interpreter.Message{{Role: "user", Content: "x"}}, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if streamed != decoded {
		t.Errorf("streamed=%q decoded=%q (must match for replay invariant)", streamed, decoded)
	}
	if streamed != want {
		t.Errorf("streamed=%q want=%q", streamed, want)
	}
}

func TestStream_ClientImplementsStreamingLLM(t *testing.T) {
	// Compile-time + runtime guard: type-asserting (*Client) to
	// interpreter.StreamingLLM must succeed. Otherwise the step
	// handler's capability detection silently falls back to Decode.
	c := &Client{}
	if _, ok := interface{}(c).(interpreter.StreamingLLM); !ok {
		t.Fatal("*Client does not implement interpreter.StreamingLLM")
	}
}

// -----------------------------------------------------------------------
// concurrency: parallel streams to the same Client must not interleave
// each other's deltas in the caller's onDelta callback.
// -----------------------------------------------------------------------

func TestStream_ParallelRequestsIndependent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the user message char-by-char so each test can
		// assert it received its own payload.
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		var content string
		for _, m := range req.Messages {
			if m.Role == "user" {
				content = m.Content
				break
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, ch := range content {
			fr := streamFrame{Choices: []streamChoice{{Delta: streamDelta{Content: string(ch)}}}}
			b, _ := json.Marshal(fr)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	var wg sync.WaitGroup
	var ok int32
	for _, payload := range []string{"alpha", "beta-charlie", "delta1234"} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			got, err := c.Stream(context.Background(), []interpreter.Message{
				{Role: "user", Content: p},
			}, "", nil)
			if err != nil {
				t.Errorf("Stream(%q): %v", p, err)
				return
			}
			if got != p {
				t.Errorf("got = %q, want %q (deltas leaked across goroutines)", got, p)
				return
			}
			atomic.AddInt32(&ok, 1)
		}(payload)
	}
	wg.Wait()
	if ok != 3 {
		t.Errorf("only %d/3 concurrent streams succeeded", ok)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
