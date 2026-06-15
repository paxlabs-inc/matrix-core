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
	// paths that never touch them.
	srv := New(Deps{
		Challenges:     challenges,
		Tokens:         tokens,
		TransportToken: testTransport,
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

func TestTransportAuthEnforced(t *testing.T) {
	srv, _ := newTestServer()
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
