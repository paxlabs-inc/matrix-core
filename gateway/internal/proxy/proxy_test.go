// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/gateway/internal/auth"
	"matrix/gateway/internal/ledger"
	"matrix/gateway/internal/rates"
	"matrix/gateway/internal/routing"
	"matrix/gateway/internal/types"
)

// upstreamFake stands in for Fireworks/Together. It echoes a fixed
// chat-completion JSON shape with a `usage` block, allowing the
// gateway's debit path to exercise without external dependencies.
func upstreamFake(t *testing.T, promptTokens, completionTokens int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"id":    "chatcmpl-test",
			"model": "echo",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": completionTokens,
				"total_tokens":      promptTokens + completionTokens,
			},
		})
		_, _ = w.Write(body)
	}))
}

func newTestServer(t *testing.T, fakeURL string, freeTierOnly bool) *Server {
	t.Helper()
	a, err := auth.New(auth.Options{Token: "shh"})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	router := routing.New(routing.Options{
		FreeTierOnly:           freeTierOnly,
		FireworksChatURL:       fakeURL,
		FireworksEmbeddingsURL: fakeURL,
		TogetherChatURL:        fakeURL,
		TogetherEmbeddingsURL:  fakeURL,
	})
	lg := ledger.NewMemory("10")
	// Pin the ledger's clock to the SAME fixed instant the server uses.
	// Entries written directly via mem.Record (bypassing the proxy's
	// s.now() debit stamp) default OccurredAt to the ledger clock; if it
	// diverges from the server's DailySpend clock the rows fall outside
	// the queried day bucket and the budget gate silently reads 0 spent.
	// Sharing one fixed clock removes that divergence (the prior harness
	// only passed when the wall clock happened to be 2026-05-27).
	fixedNow := func() time.Time { return time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC) }
	lg.SetClock(fixedNow)
	srv, err := New(Options{
		Auth:           a,
		Router:         router,
		Ledger:         lg,
		Provider:       ProviderKeys{FireworksKey: "test_fw_key"},
		PreEstimatePax: "0.0001",
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return srv
}

func newGatewayRequest(method, path string, body []byte, hdrs map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	r.Header.Set(types.HeaderActorDID, "did:pax:tester")
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	return r
}

func TestProxyForwardsAndDebits(t *testing.T) {
	upstream := upstreamFake(t, 1_000_000, 500_000)
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()

	body, _ := json.Marshal(map[string]any{
		"model":    rates.ModelCompilerFreeTier,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot:     "compiler",
		types.HeaderIntentID: "intent_a",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	cost := w.Header().Get(types.HeaderCostPax)
	if cost == "" {
		t.Fatalf("cost header missing; headers=%+v", w.Header())
	}
	// gpt-oss-120b (v3 rate card, 1 PAX = $11.43):
	//   in  = $0.60/Mtoken → 0.052493438 PAX/Mtoken
	//   out = $1.20/Mtoken → 0.104986877 PAX/Mtoken
	// 1M in + 0.5M out →
	//   (1e6*0.052493438 + 5e5*0.104986877) / 1e6
	//   = (52493.438 + 52493.4385) / 1e6
	//   = 0.1049868765 PAX (≈ $1.20).
	if cost != "0.104986876500" {
		t.Fatalf("expected 0.104986876500, got %q", cost)
	}
}

func TestProxyRejectsNonWhitelistedFreeTier(t *testing.T) {
	upstream := upstreamFake(t, 100, 100)
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()

	body, _ := json.Marshal(map[string]any{"model": "accounts/fireworks/models/gpt-oss-20b"})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: "compiler",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestProxyBYOBypassesWhitelistAndLedger(t *testing.T) {
	// Capture upstream Authorization to confirm BYO key reaches it.
	var captured string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()

	body, _ := json.Marshal(map[string]any{"model": "Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8"})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot:       "executor",
		types.HeaderBYOAPIKey:  "true",
		types.HeaderUserAPIKey: "byo_secret",
		types.HeaderKindRoute:  "code",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get(types.HeaderCostPax) != "" {
		t.Fatalf("BYO must not stamp cost header; got %q", w.Header().Get(types.HeaderCostPax))
	}
	if captured != "Bearer byo_secret" {
		t.Fatalf("upstream Authorization=%q expected BYO key", captured)
	}
}

func TestProxyAuthRejectsBadToken(t *testing.T) {
	upstream := upstreamFake(t, 100, 100)
	defer upstream.Close()
	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()

	body := []byte(`{"model":"accounts/fireworks/models/gpt-oss-120b"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set(types.HeaderAuthorization, "Bearer wrong")
	r.Header.Set(types.HeaderActorDID, "did:pax:tester")
	r.Header.Set(types.HeaderSlot, "compiler")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestProxyBudgetExhaustedReturns429(t *testing.T) {
	upstream := upstreamFake(t, 100, 100)
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()

	// Pre-charge the actor over the cap.
	mem := srv.ledger.(*ledger.Memory)
	mem.SetCap("did:pax:tester", "1")
	_ = mem.Record(context.Background(), ledger.Entry{
		ActorDID: "did:pax:tester",
		CostPax:  "0.95",
	})

	body, _ := json.Marshal(map[string]any{"model": rates.ModelCompilerFreeTier})
	// PreEstimate=0.0001 so we won't hit. Bump it via raising spent.
	_ = mem.Record(context.Background(), ledger.Entry{
		ActorDID: "did:pax:tester",
		CostPax:  "0.06", // total now 1.01 > cap 1
	})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: "compiler",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d body=%s", w.Code, w.Body.String())
	}
	var resp types.BudgetExhaustedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Error != "budget_exhausted" {
		t.Fatalf("error=%q", resp.Error)
	}
}

func TestProxyForwardsUpstreamErrorVerbatimNoDebit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream busted"}}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()
	body, _ := json.Marshal(map[string]any{"model": rates.ModelCompilerFreeTier})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: "compiler",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected upstream 400 to pass through; got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upstream busted") {
		t.Fatalf("body did not pass through: %q", w.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 0 {
		t.Fatalf("upstream-error path must not debit ledger; rows=%d", len(rows))
	}
}

func TestProxyHealthz(t *testing.T) {
	upstream := upstreamFake(t, 1, 1)
	defer upstream.Close()
	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz: %d", w.Code)
	}
}

func TestProxyKillSwitch503(t *testing.T) {
	upstream := upstreamFake(t, 1, 1)
	defer upstream.Close()
	a, _ := auth.New(auth.Options{Token: "shh"})
	srv, err := New(Options{
		Auth:     a,
		Router:   routing.New(routing.Options{FireworksChatURL: upstream.URL}),
		Ledger:   ledger.NewMemory(""),
		Disabled: func() bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := srv.Mux()
	body, _ := json.Marshal(map[string]any{"model": rates.ModelCompilerFreeTier})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: "compiler",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch: status=%d", w.Code)
	}
	// healthz also should report disabled.
	rh := httptest.NewRequest("GET", "/healthz", nil)
	wh := httptest.NewRecorder()
	mux.ServeHTTP(wh, rh)
	if wh.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz under kill switch: %d", wh.Code)
	}
}

// ensures that response body bytes come through verbatim.
func TestProxyForwardsBodyVerbatim(t *testing.T) {
	exact := `{"id":"abc","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(exact))
	}))
	defer upstream.Close()
	srv := newTestServer(t, upstream.URL, false)
	mux := srv.Mux()
	body, _ := json.Marshal(map[string]any{"model": rates.ModelCompilerFreeTier})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: "compiler",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got := strings.TrimSpace(w.Body.String()); got != exact {
		t.Fatalf("body verbatim failed:\n want %q\n got  %q", exact, got)
	}
}

// TestEnsureStreamUsage covers the pure body-rewrite helper: it must add
// stream_options.include_usage=true, preserve sibling fields, merge into
// an existing stream_options object, and fail open on a non-JSON body.
func TestEnsureStreamUsage(t *testing.T) {
	// 1. Plain streaming body gains include_usage; model survives.
	out := ensureStreamUsage([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("result not JSON: %v (%s)", err, out)
	}
	if string(top["model"]) != `"m"` {
		t.Fatalf("model clobbered: %s", top["model"])
	}
	var so struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(top["stream_options"], &so); err != nil || !so.IncludeUsage {
		t.Fatalf("include_usage not set: %s (err=%v)", top["stream_options"], err)
	}

	// 2. Existing stream_options is merged, not replaced.
	var top2 map[string]json.RawMessage
	if err := json.Unmarshal(ensureStreamUsage([]byte(`{"model":"m","stream_options":{"continuous_usage_stats":true}}`)), &top2); err != nil {
		t.Fatalf("merge result not JSON: %v", err)
	}
	var opts map[string]json.RawMessage
	if err := json.Unmarshal(top2["stream_options"], &opts); err != nil {
		t.Fatalf("stream_options not an object: %v", err)
	}
	if string(opts["include_usage"]) != "true" {
		t.Fatalf("include_usage missing after merge: %s", top2["stream_options"])
	}
	if string(opts["continuous_usage_stats"]) != "true" {
		t.Fatalf("existing stream_options field dropped: %s", top2["stream_options"])
	}

	// 3. Fail-open: non-JSON body returned byte-identical.
	junk := []byte("not json at all")
	if got := ensureStreamUsage(junk); !bytes.Equal(got, junk) {
		t.Fatalf("non-JSON body must be returned untouched; got %q", got)
	}
}

// TestProxyStreamingForcesUsageAndDebits is the regression for the
// executor-metering bug: a stream=true call must (a) reach the upstream
// with stream_options.include_usage=true forced on, (b) pipe content
// deltas through to the client, and (c) debit the ledger from the
// trailing usage chunk. Before the fix the executor slot streamed and
// billed nothing, slipping past the daily budget cap.
func TestProxyStreamingForcesUsageAndDebits(t *testing.T) {
	var captured string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		emit := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		emit("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		emit("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n")
		// Fireworks emits the usage trailer ONLY when include_usage was set.
		emit(fmt.Sprintf("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", 1000, 500, 1500))
		emit("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, true) // free-tier-only
	mux := srv.Mux()

	body, _ := json.Marshal(map[string]any{
		"model":    rates.ModelKimiK26,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	r := newGatewayRequest("POST", "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot:     "executor",
		types.HeaderIntentID: "intent_stream",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// (a) Gateway forced include_usage onto the upstream request body.
	var up map[string]json.RawMessage
	if err := json.Unmarshal([]byte(captured), &up); err != nil {
		t.Fatalf("captured upstream body not JSON: %v (%s)", err, captured)
	}
	var so struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(up["stream_options"], &so); err != nil || !so.IncludeUsage {
		t.Fatalf("upstream missing stream_options.include_usage: %s", captured)
	}
	if string(up["stream"]) != "true" {
		t.Fatalf("stream flag lost on upstream body: %s", captured)
	}

	// (b) Content deltas piped through to the client verbatim.
	if !strings.Contains(w.Body.String(), "Hello") || !strings.Contains(w.Body.String(), "world") {
		t.Fatalf("streamed content not forwarded: %q", w.Body.String())
	}

	// (c) Trailing usage chunk debited exactly once at the right cost.
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 streamed-usage debit row; got %d (%+v)", len(rows), rows)
	}
	if rows[0].Model != rates.ModelKimiK26 || rows[0].TokensInput != 1000 || rows[0].TokensOutput != 500 {
		t.Fatalf("debit row mismatch: %+v", rows[0])
	}
	// kimi-k2.6 (v3 rate card, 1 PAX = $11.43):
	//   in  = $0.80/Mtoken → 0.069991251 PAX/Mtoken
	//   out = $1.60/Mtoken → 0.139982502 PAX/Mtoken
	// 1000 in + 500 out →
	//   (1000*0.069991251 + 500*0.139982502) / 1e6
	//   = (69.991251 + 69.991251) / 1e6
	//   = 1.39982502e-4 PAX.
	if rows[0].CostPax != "0.000139982502" {
		t.Fatalf("expected 0.000139982502 PAX debit, got %q", rows[0].CostPax)
	}
}

// ctxAwareLedger wraps the in-memory ledger but, unlike it, honours
// context cancellation on Record — letting the test prove maybeDebit
// detaches the debit from a cancelled request context.
type ctxAwareLedger struct {
	*ledger.Memory
}

func (l *ctxAwareLedger) Record(ctx context.Context, e ledger.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.Memory.Record(ctx, e)
}

// TestMaybeDebitSurvivesCanceledRequestCtx is the regression for the
// streamed-debit race: the daemon closes the connection the instant it
// reads `data: [DONE]`, cancelling r.Context() before the post-response
// ledger insert lands (seen in prod as record_err "insert: context
// canceled"). The debit must persist the row regardless.
func TestMaybeDebitSurvivesCanceledRequestCtx(t *testing.T) {
	a, _ := auth.New(auth.Options{Token: "shh"})
	lg := &ctxAwareLedger{Memory: ledger.NewMemory("10")}
	srv, err := New(Options{
		Auth:   a,
		Router: routing.New(routing.Options{}),
		Ledger: lg,
		Now:    func() time.Time { return time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Already-cancelled context: mirrors the daemon having closed the
	// stream by the time the post-response debit runs.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	dec := &routing.Decision{FreeTier: true, Model: rates.ModelKimiK26, Slot: "executor"}
	usage := &types.UpstreamUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

	cost, err := srv.maybeDebit(dec, usage, ledgerCtx{ctx: canceled, actor: "did:pax:tester", intentID: "i1"})
	if err != nil {
		t.Fatalf("maybeDebit errored despite detach: %v", err)
	}
	if cost == "" {
		t.Fatalf("expected non-empty cost")
	}
	rows := lg.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("debit must persist despite cancelled request ctx; rows=%d", len(rows))
	}
	if rows[0].Model != rates.ModelKimiK26 || rows[0].TokensInput != 1000 {
		t.Fatalf("row mismatch: %+v", rows[0])
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.