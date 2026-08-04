// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	// Pin EVERY provider lane to the fake upstream: routing has moved
	// models between providers over the versions (v7 bare "<vendor>/<model>"
	// → Baseten; v9 "grok-*" → xAI) and a lane left on its real default URL
	// makes a test silently dial the live provider.
	router := routing.New(routing.Options{
		FreeTierOnly:           freeTierOnly,
		FireworksChatURL:       fakeURL,
		FireworksEmbeddingsURL: fakeURL,
		TogetherChatURL:        fakeURL,
		TogetherEmbeddingsURL:  fakeURL,
		BasetenChatURL:         fakeURL,
		BasetenEmbeddingsURL:   fakeURL,
		XaiChatURL:             fakeURL,
		XaiEmbeddingsURL:       fakeURL,
		XiaomiChatURL:          fakeURL,
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
		Provider:       ProviderKeys{FireworksKey: "test_fw_key", XiaomiKey: "test_mimo_key"},
		PreEstimatePax: "0.0001",
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return srv
}

func configureScopedAgentCoreAuth(t *testing.T, srv *Server, now time.Time) (token string, key []byte) {
	t.Helper()
	key = bytes.Repeat([]byte("agentcore-test-key-material-"), 2)
	authenticator, err := auth.New(auth.Options{
		Token:                     "shh",
		AgentCoreVerificationKeys: map[string][]byte{"active-test": key},
		Now:                       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("auth.New scoped: %v", err)
	}
	issuer, err := auth.NewAgentCoreIssuer(auth.AgentCoreIssuerOptions{
		KeyID: "active-test",
		Key:   key,
		Now:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewAgentCoreIssuer: %v", err)
	}
	token, _, err = issuer.Mint("did:matrix:user-scoped:cody", auth.AgentCoreTokenTTL)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	srv.auth = authenticator
	return token, key
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
	r := httptest.NewRequest("GET", "/healthz", http.NoBody)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz: %d", w.Code)
	}
}

func TestAgentCoreTokenEndpointMintsOnlyFromLegacyAuthority(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1", false)
	now := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte("agentcore-issuer-test-key-"), 2)
	authenticator, err := auth.New(auth.Options{
		Token: "shh", AgentCoreVerificationKeys: map[string][]byte{"issuer-test": key},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := auth.NewAgentCoreIssuer(auth.AgentCoreIssuerOptions{
		KeyID: "issuer-test", Key: key, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.auth = authenticator
	srv.agentCoreIssuer = issuer

	request := httptest.NewRequest(http.MethodPost, "/internal/agentcore/token", http.NoBody)
	request.Header.Set(types.HeaderAuthorization, "Bearer shh")
	request.Header.Set(types.HeaderActorDID, "did:matrix:user-token:cody")
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("mint = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var response struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Token == "" {
		t.Fatalf("decode mint: %v body=%s", err, recorder.Body.String())
	}
	check := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	check.Header.Set(types.HeaderAuthorization, "Bearer "+response.Token)
	check.Header.Set(types.HeaderActorDID, "did:matrix:user-token:cody")
	check.Header.Set(types.HeaderSlot, auth.AgentCoreSlot)
	principal, err := authenticator.Authenticate(check)
	if err != nil || !principal.Scoped || principal.Actor != "did:matrix:user-token:cody" || principal.Model != auth.AgentCoreModel {
		t.Fatalf("minted principal = (%+v, %v)", principal, err)
	}

	scoped := httptest.NewRequest(http.MethodPost, "/internal/agentcore/token", http.NoBody)
	scoped.Header = check.Header.Clone()
	denied := httptest.NewRecorder()
	srv.Mux().ServeHTTP(denied, scoped)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("scoped remint = %d: %s", denied.Code, denied.Body.String())
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
	rh := httptest.NewRequest("GET", "/healthz", http.NoBody)
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

// TestProxyXiaomiUpstreamHop pins the only chat upstream in use: Xiaomi MiMo.
// The gateway forwards a metered mimo chat call to Xiaomi's OpenAI-compatible
// upstream with (a) the request body BYTE-IDENTICAL (no native-id rewrite, no
// thinking-block translation on the gateway hop), and (b) the gateway's
// MIMO_API_KEY bearer — while the ledger keeps metering.
func TestProxyXiaomiUpstreamHop(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-mimo",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
			},
		})
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	a, err := auth.New(auth.Options{Token: "shh"})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	router := routing.New(routing.Options{XiaomiChatURL: upstream.URL})
	lg := ledger.NewMemory("10")
	fixedNow := func() time.Time { return time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC) }
	lg.SetClock(fixedNow)
	srv, err := New(Options{
		Auth:           a,
		Router:         router,
		Ledger:         lg,
		Provider:       ProviderKeys{XiaomiKey: "test_mimo_key"},
		PreEstimatePax: "0.0001",
		Now:            fixedNow,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// A non-streaming mimo call: the gateway forwards the body verbatim.
	reqBody := []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`)
	r := newGatewayRequest("POST", "/v1/chat/completions", reqBody, map[string]string{
		types.HeaderSlot: "neo",
	})
	w := httptest.NewRecorder()
	srv.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer test_mimo_key" {
		t.Fatalf("upstream auth: %q", gotAuth)
	}
	// The whole body must reach Xiaomi byte-identical (no rewrite on the hop).
	if !bytes.Equal(gotBody, reqBody) {
		t.Fatalf("upstream body was rewritten:\n want %s\n got  %s", reqBody, gotBody)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, gotBody)
	}
	if string(sent["model"]) != `"mimo-v2.5-pro"` {
		t.Fatalf("upstream model: %s (mimo id must pass through unchanged)", sent["model"])
	}
	// The MiMo thinking block set by the caller survives the hop verbatim.
	if string(sent["thinking"]) != `{"type":"enabled"}` {
		t.Fatalf("thinking block must survive verbatim: %s", gotBody)
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

// ctxAwareLedger wraps the in-memory ledger but, unlike it, honors
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
// reads `data: [DONE]`, canceling r.Context() before the post-response
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

func TestProxyNormalizesBufferedMiMoToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-mimo","choices":[{"index":0,"message":{"role":"assistant","content":"<tool_call><function=read_file><parameter=path>/workspace/main.go</parameter></function></tool_call>","reasoning_content":"inspect"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	body := []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"inspect"}]}`)
	r := newGatewayRequest(http.MethodPost, "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	w := httptest.NewRecorder()
	srv.Mux().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<tool_call>") {
		t.Fatalf("MiMo XML leaked: %s", w.Body.String())
	}
	var response struct {
		Choices []struct {
			Message struct {
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage types.UpstreamUsage `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choice := response.Choices[0]
	if choice.Message.Reasoning != "inspect" || choice.FinishReason != "tool_calls" {
		t.Fatalf("reasoning/finish mismatch: %+v", choice)
	}
	if len(choice.Message.ToolCalls) != 1 ||
		choice.Message.ToolCalls[0].Function.Name != "read_file" ||
		choice.Message.ToolCalls[0].Function.Arguments != `{"path":"/workspace/main.go"}` ||
		!strings.HasPrefix(choice.Message.ToolCalls[0].ID, "mimo-") {
		t.Fatalf("tool call mismatch: %+v", choice.Message.ToolCalls)
	}
	if response.Usage.TotalTokens != 15 || len(srv.ledger.(*ledger.Memory).Snapshot()) != 1 {
		t.Fatalf("usage/debit lost: %+v rows=%+v", response.Usage, srv.ledger.(*ledger.Memory).Snapshot())
	}
}

func TestProxyNormalizesSplitMiMoStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(value string) {
			_, _ = io.WriteString(w, value)
			if flusher != nil {
				flusher.Flush()
			}
		}
		emit("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"inspect \"},\"finish_reason\":null}]}\n\n")
		emit("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"<tool_\"},\"finish_reason\":null}]}\n\n")
		emit("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"call><function=read_file><parameter=path>/workspace/main.go</parameter>\"},\"finish_reason\":null}]}\n\n")
		emit("data: {\"id\":\"chatcmpl-mimo\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		emit("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		emit("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	body := []byte(`{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"inspect"}],"stream":true}`)
	r := newGatewayRequest(http.MethodPost, "/v1/chat/completions", body, map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	w := httptest.NewRecorder()
	srv.Mux().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	output := w.Body.String()
	if strings.Contains(output, "<tool_") || strings.Contains(output, "<function=") {
		t.Fatalf("split MiMo XML leaked: %s", output)
	}
	if !strings.Contains(output, `"reasoning_content":"inspect `) ||
		!strings.Contains(output, `"name":"read_file"`) ||
		!strings.Contains(output, `"finish_reason":"tool_calls"`) ||
		!strings.Contains(output, "data: [DONE]") {
		t.Fatalf("normalized stream incomplete: %s", output)
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 10 || rows[0].TokensOutput != 5 {
		t.Fatalf("stream usage debit mismatch: %+v", rows)
	}
}

func TestProxyMiMoDuplicatedNativeTextualCallKeepsNativeID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"<tool_call><function=read_file><parameter=path>/same</parameter></function></tool_call>","tool_calls":[{"id":"native-stable","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/same\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":8,"total_tokens":29}}`)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[]}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "<tool_call>") || strings.Count(recorder.Body.String(), `"type":"function"`) != 1 {
		t.Fatalf("duplicated native/textual call was not coalesced: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"id":"native-stable"`) {
		t.Fatalf("native id was not retained: %s", recorder.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 21 || rows[0].TokensOutput != 8 {
		t.Fatalf("debit mismatch: %+v", rows)
	}
}

func TestProxyMiMoIdenticalParallelTextualCallsHaveStableUniqueIDs(t *testing.T) {
	call := `<tool_call><function=read_file><parameter=path>/same</parameter></function></tool_call>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, call+call)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	request := func() []string {
		t.Helper()
		req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[]}`), map[string]string{
			types.HeaderSlot: types.SlotCody,
		})
		recorder := httptest.NewRecorder()
		srv.Mux().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Choices []struct {
				Message struct {
					ToolCalls []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 2 {
			t.Fatalf("parallel calls missing: %s", recorder.Body.String())
		}
		return []string{response.Choices[0].Message.ToolCalls[0].ID, response.Choices[0].Message.ToolCalls[1].ID}
	}
	first := request()
	second := request()
	if first[0] == "" || first[1] == "" || first[0] == first[1] {
		t.Fatalf("parallel ids are not unique: %+v", first)
	}
	if first[0] != second[0] || first[1] != second[1] {
		t.Fatalf("parallel ids are not stable: first=%+v second=%+v", first, second)
	}
}

func TestProxyMiMoTruncatedTextualCallIsRecovered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"prefix <tool_call><function=read_file><parameter=path>/truncated"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[]}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "<tool_call>") {
		t.Fatalf("truncated call not normalized: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"prefix"`) ||
		!strings.Contains(recorder.Body.String(), `"arguments":"{\"path\":\"/truncated\"}"`) {
		t.Fatalf("truncated call recovery mismatch: %s", recorder.Body.String())
	}
}

func TestProxyMiMoBufferedMismatchStillDebitsRawUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"<tool_call><function=read_file><parameter=path>/textual</parameter></function></tool_call>","tool_calls":[{"id":"native","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/native\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,"completion_tokens":12,"total_tokens":43}}`)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[]}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "upstream_tool_protocol") {
		t.Fatalf("expected protocol 502, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 31 || rows[0].TokensOutput != 12 {
		t.Fatalf("raw mismatch usage was not debited: %+v", rows)
	}
}

func TestProxyMiMoStreamingMismatchDrainsUsageWithoutDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		emit := func(event string) {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
		emit(`data: {"choices":[{"index":0,"delta":{"content":"<tool_call><function=read_file><parameter=path>/textual</parameter></function></tool_call>","tool_calls":[{"index":0,"id":"native","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/native\"}"}}]},"finish_reason":"stop"}]}` + "\n\n")
		emit(`data: {"choices":[],"usage":{"prompt_tokens":41,"completion_tokens":14,"total_tokens":55}}` + "\n\n")
		emit("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[],"stream":true}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if strings.Contains(recorder.Body.String(), "[DONE]") || strings.Contains(recorder.Body.String(), "<tool_call>") {
		t.Fatalf("failed stream leaked terminal/tool data: %s", recorder.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 41 || rows[0].TokensOutput != 14 {
		t.Fatalf("drained mismatch usage was not debited: %+v", rows)
	}
}

func TestProxyMiMoStreamEOFFlushesTruncatedCallAndDebits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"<tool_call><function=read_file><parameter=path>/eof"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}`+"\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[],"stream":true}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if strings.Contains(recorder.Body.String(), "<tool_call>") || strings.Contains(recorder.Body.String(), "[DONE]") ||
		!strings.Contains(recorder.Body.String(), `"name":"read_file"`) {
		t.Fatalf("EOF normalization mismatch: %s", recorder.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 17 || rows[0].TokensOutput != 9 {
		t.Fatalf("EOF debit mismatch: %+v", rows)
	}
}

type failingStreamWriter struct {
	header    http.Header
	body      bytes.Buffer
	status    int
	writes    int
	failAfter int
}

func (writer *failingStreamWriter) Header() http.Header {
	return writer.header
}

func (writer *failingStreamWriter) WriteHeader(status int) {
	writer.status = status
}

func (writer *failingStreamWriter) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes > writer.failAfter {
		return 0, io.ErrClosedPipe
	}
	return writer.body.Write(value)
}

func (writer *failingStreamWriter) Flush() {}

func TestProxyStreamClientCancellationDrainsAndDebits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":19,"completion_tokens":11,"total_tokens":30}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"mimo-v2.5-pro","messages":[],"stream":true}`), map[string]string{
		types.HeaderSlot: types.SlotCody,
	})
	writer := &failingStreamWriter{header: make(http.Header), failAfter: 1}
	srv.Mux().ServeHTTP(writer, req)
	if strings.Contains(writer.body.String(), "[DONE]") {
		t.Fatalf("DONE was forwarded after client write failure: %s", writer.body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 19 || rows[0].TokensOutput != 11 {
		t.Fatalf("cancellation drain did not debit: %+v", rows)
	}
}

func TestProxyNonXiaomiStreamPassesThroughByteIdentical(t *testing.T) {
	exact := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<tool_call>opaque\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, exact)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, true)
	req := newGatewayRequest(http.MethodPost, "/v1/chat/completions", []byte(`{"model":"accounts/fireworks/models/deepseek-v4-flash","messages":[],"stream":true}`), map[string]string{
		types.HeaderSlot: "executor",
	})
	recorder := httptest.NewRecorder()
	srv.Mux().ServeHTTP(recorder, req)
	if recorder.Body.String() != exact {
		t.Fatalf("non-Xiaomi SSE was rewritten:\nwant %q\n got %q", exact, recorder.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].TokensInput != 7 || rows[0].TokensOutput != 3 {
		t.Fatalf("non-Xiaomi stream debit mismatch: %+v", rows)
	}
}

func TestProxyScopedAgentCoreCredentialBindsActorSlotAndProtectsProviderKey(t *testing.T) {
	const providerKey = "mimo-provider-key-sentinel"
	var upstreamCalls atomic.Int32
	var upstreamAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		upstreamAuthorization = r.Header.Get(types.HeaderAuthorization)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(types.HeaderAuthorization, "Bearer "+providerKey)
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18}}`)
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL, false)
	now := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)
	token, key := configureScopedAgentCoreAuth(t, srv, now)
	srv.provider.XiaomiKey = providerKey
	requestModel := func(path, currentToken, actor, slot, model string, extra map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "build"}},
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		r := newGatewayRequest(http.MethodPost, path, body, map[string]string{
			types.HeaderSlot: slot,
		})
		r.Header.Set(types.HeaderAuthorization, "Bearer "+currentToken)
		r.Header.Set(types.HeaderActorDID, actor)
		for name, value := range extra {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		srv.Mux().ServeHTTP(w, r)
		return w
	}
	request := func(path, currentToken, actor, slot string, extra map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		return requestModel(path, currentToken, actor, slot, auth.AgentCoreModel, extra)
	}

	valid := request(
		"/v1/chat/completions",
		token,
		"did:matrix:user-scoped:cody",
		types.SlotCody,
		nil,
	)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid scoped status=%d body=%s", valid.Code, valid.Body.String())
	}
	if token == providerKey || upstreamAuthorization != "Bearer "+providerKey {
		t.Fatalf("inbound token crossed provider boundary: token_equal=%v upstream_auth=%q", token == providerKey, upstreamAuthorization)
	}
	if valid.Header().Get(types.HeaderAuthorization) != "" ||
		strings.Contains(valid.Body.String(), providerKey) || strings.Contains(valid.Body.String(), token) {
		t.Fatalf("credential leaked downstream: headers=%v body=%s", valid.Header(), valid.Body.String())
	}
	rows := srv.ledger.(*ledger.Memory).Snapshot()
	if len(rows) != 1 || rows[0].ActorDID != "did:matrix:user-scoped:cody" || rows[0].Slot != types.SlotCody {
		t.Fatalf("scoped ledger attribution=%+v", rows)
	}
	for _, deniedModel := range []string{"xiaomimimo/mimo-v2.5-pro", "grok-4.3"} {
		response := requestModel(
			"/v1/chat/completions",
			token,
			"did:matrix:user-scoped:cody",
			types.SlotCody,
			deniedModel,
			nil,
		)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "scoped_model_mismatch") {
			t.Fatalf("model %q bypass status=%d body=%s", deniedModel, response.Code, response.Body.String())
		}
	}

	expiredIssuer, err := auth.NewAgentCoreIssuer(auth.AgentCoreIssuerOptions{
		KeyID: "active-test", Key: key, Now: func() time.Time { return now.Add(-time.Hour) },
	})
	if err != nil {
		t.Fatalf("expired issuer: %v", err)
	}
	expired, _, err := expiredIssuer.Mint("did:matrix:user-scoped:cody", auth.AgentCoreTokenTTL)
	if err != nil {
		t.Fatalf("expired token: %v", err)
	}
	parts := strings.Split(token, ".")
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	signature[0] ^= 1
	tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)

	tests := []struct {
		name  string
		path  string
		token string
		actor string
		slot  string
		extra map[string]string
	}{
		{name: "actor spoof", path: "/v1/chat/completions", token: token, actor: "did:matrix:other:cody", slot: types.SlotCody},
		{name: "slot spoof", path: "/v1/chat/completions", token: token, actor: "did:matrix:user-scoped:cody", slot: types.SlotNeo},
		{name: "wrong path", path: "/v1/embeddings", token: token, actor: "did:matrix:user-scoped:cody", slot: types.SlotCody},
		{name: "byo", path: "/v1/chat/completions", token: token, actor: "did:matrix:user-scoped:cody", slot: types.SlotCody, extra: map[string]string{
			types.HeaderBYOAPIKey: "true", types.HeaderUserAPIKey: "user-provider-key",
		}},
		{name: "expired", path: "/v1/chat/completions", token: expired, actor: "did:matrix:user-scoped:cody", slot: types.SlotCody},
		{name: "tampered", path: "/v1/chat/completions", token: tampered, actor: "did:matrix:user-scoped:cody", slot: types.SlotCody},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := request(test.path, test.token, test.actor, test.slot, test.extra)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), providerKey) || strings.Contains(response.Body.String(), test.token) {
				t.Fatalf("credential leaked in denial: %s", response.Body.String())
			}
		})
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("denied requests reached upstream: calls=%d", upstreamCalls.Load())
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
