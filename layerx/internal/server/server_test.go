package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

const testTransport = "transport-secret"

func newTestServer() (*Server, *auth.Tokens) {
	challenges := auth.NewChallenges(time.Minute)
	tokens := auth.NewTokens("agent-secret", time.Hour)
	// Store/Ledger/Settler are nil: these tests only hit auth-lane + middleware
	// + info paths that never touch them. Default public mode (transport not
	// required), matching the PHASE 4 rollup posture.
	srv := New(Deps{
		Challenges:     challenges,
		Tokens:         tokens,
		TransportToken: testTransport,
		ChainID:        125,
	})
	return srv, tokens
}

// newFleetServer is the legacy private-fleet posture: the transport bearer is
// enforced on the write/principal endpoints.
func newFleetServer() (*Server, *auth.Tokens) {
	challenges := auth.NewChallenges(time.Minute)
	tokens := auth.NewTokens("agent-secret", time.Hour)
	srv := New(Deps{
		Challenges:       challenges,
		Tokens:           tokens,
		TransportToken:   testTransport,
		RequireTransport: true,
	})
	return srv, tokens
}

func do(h http.Handler, method, path, bearer, agent string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if agent != "" {
		req.Header.Set("X-LayerX-Agent", agent)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRootIsPublic(t *testing.T) {
	srv, _ := newTestServer()
	rr := do(srv.Handler(), http.MethodGet, "/", "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("root status = %d, want 200", rr.Code)
	}
}

func TestTransportFleetModeEnforced(t *testing.T) {
	srv, _ := newFleetServer()
	h := srv.Handler()

	// No transport bearer -> 401 before any handler logic.
	if rr := do(h, http.MethodGet, "/v1/balance", "", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer status = %d, want 401", rr.Code)
	}
	// Wrong transport bearer -> 401.
	if rr := do(h, http.MethodGet, "/v1/balance", "wrong", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer status = %d, want 401", rr.Code)
	}
	// Valid transport bearer but missing principal token -> 401 (principal lane).
	if rr := do(h, http.MethodGet, "/v1/balance", testTransport, "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal status = %d, want 401", rr.Code)
	}
}

func TestPrincipalRequiredInPublicMode(t *testing.T) {
	srv, _ := newTestServer() // public mode: transport not required
	h := srv.Handler()
	// /v1/balance is still a principal endpoint: no X-LayerX-Agent token -> 401,
	// even though the transport bearer is no longer required.
	if rr := do(h, http.MethodGet, "/v1/balance", "", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal status = %d, want 401", rr.Code)
	}
}

func TestInfoIsPublicNoAuth(t *testing.T) {
	srv, _ := newTestServer()
	rr := do(srv.Handler(), http.MethodGet, "/v1/info", "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("info status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env struct {
		Ok   bool               `json:"ok"`
		Data types.InfoResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if !env.Ok || env.Data.ChainID != 125 || env.Data.Service != "layerxd" {
		t.Fatalf("bad info response: %+v", env)
	}
}

// TestInfoPublicEvenInFleetMode proves the public read surface is never gated by
// the transport bearer, even when RequireTransport is set.
func TestInfoPublicEvenInFleetMode(t *testing.T) {
	srv, _ := newFleetServer()
	rr := do(srv.Handler(), http.MethodGet, "/v1/info", "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("info in fleet mode status = %d, want 200 (public read must bypass transport)", rr.Code)
	}
}

func TestPayRejectsBadIntentSignature(t *testing.T) {
	srv, _ := newTestServer()
	h := srv.Handler()

	pub, _, _ := ed25519.GenerateKey(nil)
	did := "did:matrix:payer:" + hex.EncodeToString(pub)[:16]

	// Garbage signature on the signed-intent path -> 401 BEFORE any ledger touch
	// (the nil ledger is never reached, proving auth fails closed).
	req := types.PayRequest{
		ToDID:      "did:matrix:payee:0123456789abcdef",
		AmountUSDX: "1.0",
		FromDID:    did,
		PublicKey:  hex.EncodeToString(pub),
		Nonce:      "nonce-1",
		Signature:  hex.EncodeToString([]byte("not-a-valid-signature-not-a-valid-signatur")),
	}
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-signature pay status = %d, want 401", rr.Code)
	}
}

func TestPayRejectsWrongDIDSignature(t *testing.T) {
	srv, _ := newTestServer()
	h := srv.Handler()

	pub, _, _ := ed25519.GenerateKey(nil)
	_, attacker, _ := ed25519.GenerateKey(nil)
	did := "did:matrix:payer:" + hex.EncodeToString(pub)[:16]
	nonce := "nonce-2"
	amount := "1.000000"
	preimage := auth.IntentMessage("pay", did, nonce, "did:matrix:payee:0123456789abcdef", amount)

	// Sign with an unrelated key — the pubkey still matches the DID fp, but the
	// signature does not verify under it, so authorization must fail closed.
	forged := ed25519.Sign(attacker, []byte(preimage))
	req := types.PayRequest{
		ToDID:      "did:matrix:payee:0123456789abcdef",
		AmountUSDX: amount,
		FromDID:    did,
		PublicKey:  hex.EncodeToString(pub),
		Nonce:      nonce,
		Signature:  hex.EncodeToString(forged),
	}
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-DID-signature pay status = %d, want 401", rr.Code)
	}
}

func TestPayRejectsMissingAuthorization(t *testing.T) {
	srv, _ := newTestServer()
	// No X-LayerX-Agent token and no signed-intent fields -> 401.
	req := types.PayRequest{ToDID: "did:matrix:payee:0123456789abcdef", AmountUSDX: "1.0"}
	if rr := do(srv.Handler(), http.MethodPost, "/v1/pay", "", "", req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized pay status = %d, want 401", rr.Code)
	}
}

func TestChallengeRejectsBadDID(t *testing.T) {
	srv, _ := newTestServer()
	rr := do(srv.Handler(), http.MethodPost, "/v1/agent/auth/challenge", testTransport, "", types.ChallengeRequest{DID: "not-a-did"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad did status = %d, want 400", rr.Code)
	}
}

func TestAuthLaneRoundTrip(t *testing.T) {
	srv, tokens := newTestServer()
	h := srv.Handler()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := hex.EncodeToString(pub)[:16]
	did := "did:matrix:user-1:" + fp

	// 1. challenge
	rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", testTransport, "", types.ChallengeRequest{DID: did})
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d body=%s", rr.Code, rr.Body.String())
	}
	var chEnv struct {
		Ok   bool                    `json:"ok"`
		Data types.ChallengeResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &chEnv); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if !chEnv.Ok || chEnv.Data.Nonce == "" || chEnv.Data.Message == "" {
		t.Fatalf("bad challenge response: %+v", chEnv)
	}

	// 2. sign the exact message and verify
	sig := ed25519.Sign(priv, []byte(chEnv.Data.Message))
	rr = do(h, http.MethodPost, "/v1/agent/auth/verify", testTransport, "", types.VerifyRequest{
		DID:       did,
		PublicKey: hex.EncodeToString(pub),
		Nonce:     chEnv.Data.Nonce,
		Signature: hex.EncodeToString(sig),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("verify status = %d body=%s", rr.Code, rr.Body.String())
	}
	var vEnv struct {
		Ok   bool                 `json:"ok"`
		Data types.VerifyResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &vEnv); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if !vEnv.Ok || vEnv.Data.Token == "" {
		t.Fatalf("bad verify response: %+v", vEnv)
	}

	// 3. the minted token must carry the DID
	claims, err := tokens.Verify(vEnv.Data.Token)
	if err != nil {
		t.Fatalf("minted token must verify: %v", err)
	}
	if claims.DID != did {
		t.Fatalf("token DID = %q, want %q", claims.DID, did)
	}

	// 4. nonce is single-use: replaying verify must now fail
	rr = do(h, http.MethodPost, "/v1/agent/auth/verify", testTransport, "", types.VerifyRequest{
		DID:       did,
		PublicKey: hex.EncodeToString(pub),
		Nonce:     chEnv.Data.Nonce,
		Signature: hex.EncodeToString(sig),
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("nonce replay status = %d, want 401", rr.Code)
	}
}

func TestVerifyRejectsForgedSignature(t *testing.T) {
	srv, _ := newTestServer()
	h := srv.Handler()

	pub, _, _ := ed25519.GenerateKey(nil)
	_, attacker, _ := ed25519.GenerateKey(nil)
	fp := hex.EncodeToString(pub)[:16]
	did := "did:matrix:user-1:" + fp

	rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", testTransport, "", types.ChallengeRequest{DID: did})
	var chEnv struct {
		Data types.ChallengeResponse `json:"data"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &chEnv)

	// Sign with an unrelated key — must be rejected even though pubkey matches fp.
	forged := ed25519.Sign(attacker, []byte(chEnv.Data.Message))
	rr = do(h, http.MethodPost, "/v1/agent/auth/verify", testTransport, "", types.VerifyRequest{
		DID:       did,
		PublicKey: hex.EncodeToString(pub),
		Nonce:     chEnv.Data.Nonce,
		Signature: hex.EncodeToString(forged),
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature status = %d, want 401", rr.Code)
	}
}
