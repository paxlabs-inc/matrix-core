package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// These tests exercise the PHASE 4 public RPC / explorer surface end-to-end
// against a real Postgres ledger. They are skipped unless LAYERX_TEST_POSTGRES_URI
// points at a disposable DB, so the default `go test` run stays hermetic (the
// auth/middleware paths are covered by the hermetic tests in server_test.go).

func newExplorerServer(t *testing.T) (*Server, *store.Store, *auth.Challenges, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping public-surface integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	st, err := store.New(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	signer, _, err := sig.New("")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	challenges := auth.NewChallenges(time.Minute)
	srv := New(Deps{
		Store:           st,
		Ledger:          ledger.New(st, signer, 1_000_000),
		Challenges:      challenges,
		Tokens:          auth.NewTokens("agent-secret", time.Hour),
		ChainID:         125,
		SequencerPubHex: signer.PublicHex(),
	})
	return srv, st, challenges, ctx
}

func uniqueDID(t *testing.T) (string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	did := fmt.Sprintf("did:matrix:test-%d:%s", time.Now().UnixNano(), hex.EncodeToString(pub)[:16])
	return did, pub, priv
}

// TestPublicSurfaceWithDIDSignedPay drives a DID-signed pay accepted WITHOUT any
// transport bearer, then reads the whole public surface unauthenticated.
func TestPublicSurfaceWithDIDSignedPay(t *testing.T) {
	srv, st, _, ctx := newExplorerServer(t)
	h := srv.Handler()

	payer, payerPub, payerPriv := uniqueDID(t)
	payee, _, _ := uniqueDID(t)

	// Fund the payer (5 USDX) directly via the store (simulates a credited deposit).
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	// 1. Get a challenge nonce (public, no auth).
	rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", "", "", types.ChallengeRequest{DID: payer})
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d body=%s", rr.Code, rr.Body.String())
	}
	var chEnv struct {
		Data types.ChallengeResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &chEnv); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}

	// 2. Sign the canonical pay intent and submit WITHOUT a transport bearer or
	//    principal token — the signature IS the authorization (i6).
	amount := "2.000000"
	preimage := auth.IntentMessage("pay", payer, chEnv.Data.Nonce, payee, amount)
	sigHex := hex.EncodeToString(ed25519.Sign(payerPriv, []byte(preimage)))
	payReq := types.PayRequest{
		ToDID:      payee,
		AmountUSDX: amount,
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      chEnv.Data.Nonce,
		Signature:  sigHex,
	}
	rr = do(h, http.MethodPost, "/v1/pay", "", "", payReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("DID-signed pay status = %d body=%s", rr.Code, rr.Body.String())
	}
	var payEnv struct {
		Data types.Receipt `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payEnv); err != nil {
		t.Fatalf("decode pay: %v", err)
	}
	if payEnv.Data.Seq <= 0 || payEnv.Data.FromDID != payer || payEnv.Data.ToDID != payee {
		t.Fatalf("bad pay receipt: %+v", payEnv.Data)
	}
	seq := payEnv.Data.Seq

	// 2b. Replaying the same signed intent must fail (single-use nonce).
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", payReq); rr.Code != http.StatusUnauthorized {
		t.Fatalf("replayed pay status = %d, want 401 (nonce already consumed)", rr.Code)
	}

	// 3. Public receipt read (no auth, no ownership scoping).
	rr = do(h, http.MethodGet, fmt.Sprintf("/v1/receipt/%d", seq), "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("public receipt status = %d body=%s", rr.Code, rr.Body.String())
	}

	// 4. Supply (the reserve proof). Chain unconfigured -> reserve unknown, but
	//    circulating must equal the funded total (payer 3 + payee 2 = 5 USDX of
	//    this test's accounts; other rows may exist, so assert >= 5 USDX).
	rr = do(h, http.MethodGet, "/v1/supply", "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("supply status = %d", rr.Code)
	}
	var supEnv struct {
		Data types.SupplyResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &supEnv); err != nil {
		t.Fatalf("decode supply: %v", err)
	}
	if supEnv.Data.ReserveKnown {
		t.Fatalf("reserve should be unknown without a chain client")
	}
	if got, err := types.ParseUSDX(supEnv.Data.CirculatingUSDX); err != nil || got < 5_000_000 {
		t.Fatalf("circulating = %q (parsed %d), want >= 5 USDX", supEnv.Data.CirculatingUSDX, got)
	}

	// 5. Transfers feed (public) + ?did= filter.
	rr = do(h, http.MethodGet, "/v1/transfers?did="+payee, "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("transfers status = %d", rr.Code)
	}
	var txEnv struct {
		Data types.TransfersResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &txEnv); err != nil {
		t.Fatalf("decode transfers: %v", err)
	}
	if txEnv.Data.Count < 1 || txEnv.Data.Transfers[0].ToDID != payee {
		t.Fatalf("payee transfer feed wrong: %+v", txEnv.Data)
	}

	// 6. Public account read shows the payee balance.
	rr = do(h, http.MethodGet, "/v1/account/"+payee, "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("account status = %d body=%s", rr.Code, rr.Body.String())
	}
	var acctEnv struct {
		Data types.AccountResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &acctEnv); err != nil {
		t.Fatalf("decode account: %v", err)
	}
	if acctEnv.Data.BalanceUSDX != "2.000000" {
		t.Fatalf("payee balance = %q, want 2.000000", acctEnv.Data.BalanceUSDX)
	}
	if len(acctEnv.Data.History) < 1 {
		t.Fatalf("payee history empty")
	}

	// 7. Pagination clamp: limit=1 returns at most one row.
	rr = do(h, http.MethodGet, "/v1/transfers?limit=1", "", "", nil)
	var pageEnv struct {
		Data types.TransfersResponse `json:"data"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &pageEnv)
	if pageEnv.Data.Count != 1 || pageEnv.Data.Limit != 1 {
		t.Fatalf("pagination limit=1 returned count=%d limit=%d", pageEnv.Data.Count, pageEnv.Data.Limit)
	}
}
