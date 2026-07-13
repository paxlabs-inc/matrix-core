package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// TestRefCarryingPayRoundTrip proves the row-level ref binding (DEUS-LAYERX
// req.2): a ref-carrying pay round-trips intent -> transfer row -> public
// receipt + explorer views, a tampered ref fails intent signature verification
// (and does NOT burn the nonce), and the Merkle leaf preimage is unchanged by
// the ref (the v2 domain-bump deferral, pinned).
func TestRefCarryingPayRoundTrip(t *testing.T) {
	srv, st, _, ctx := newExplorerServer(t)
	h := srv.Handler()

	payer, payerPub, payerPriv := uniqueDID(t)
	payee, _, _ := uniqueDID(t)
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	challenge := func() string {
		rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", "", "", types.ChallengeRequest{DID: payer})
		if rr.Code != http.StatusOK {
			t.Fatalf("challenge status = %d body=%s", rr.Code, rr.Body.String())
		}
		var env struct {
			Data types.ChallengeResponse `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode challenge: %v", err)
		}
		return env.Data.Nonce
	}

	const amount = "1.500000"
	ref := "0x1111111111111111111111111111111111111111111111111111111111111111"
	tampered := "0x2222222222222222222222222222222222222222222222222222222222222222"

	// 1. Tampered ref: the signature covers ref, the request carries a different
	//    ref -> 401, and the nonce survives (consumed only after verification).
	nonce := challenge()
	preimage := auth.IntentMessage("pay", payer, nonce, payee, amount, ref)
	sigHex := hex.EncodeToString(ed25519.Sign(payerPriv, []byte(preimage)))
	req := types.PayRequest{
		ToDID:      payee,
		AmountUSDX: amount,
		Ref:        tampered,
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      nonce,
		Signature:  sigHex,
	}
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered-ref pay status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 2. The same signed intent with the SIGNED ref succeeds on the same nonce.
	req.Ref = ref
	rr := do(h, http.MethodPost, "/v1/pay", "", "", req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ref pay status = %d body=%s", rr.Code, rr.Body.String())
	}
	var payEnv struct {
		Data types.Receipt `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payEnv); err != nil {
		t.Fatalf("decode pay: %v", err)
	}
	if payEnv.Data.Ref != ref {
		t.Fatalf("pay receipt ref = %q, want %q", payEnv.Data.Ref, ref)
	}
	seq := payEnv.Data.Seq

	// 3. Public receipt read carries the ref.
	rr = do(h, http.MethodGet, fmt.Sprintf("/v1/receipt/%d", seq), "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("public receipt status = %d", rr.Code)
	}
	var rcptEnv struct {
		Data types.Receipt `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rcptEnv); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if rcptEnv.Data.Ref != ref {
		t.Fatalf("public receipt ref = %q, want %q", rcptEnv.Data.Ref, ref)
	}

	// 4. Explorer transfer view carries the ref.
	rr = do(h, http.MethodGet, "/v1/transfers?did="+payee, "", "", nil)
	var txEnv struct {
		Data types.TransfersResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &txEnv); err != nil {
		t.Fatalf("decode transfers: %v", err)
	}
	if txEnv.Data.Count < 1 || txEnv.Data.Transfers[0].Ref != ref {
		t.Fatalf("explorer transfer ref missing: %+v", txEnv.Data)
	}

	// 5. Leaf/domain unchanged (v2 deferral pin): the leaf preimage stays
	//    (seq, from, to, amount, ts) — recomputing it WITHOUT the ref reproduces
	//    the stored leaf byte-for-byte. The pay response's ts is used because it
	//    carries the exact nanosecond timestamp the leaf committed to (the DB
	//    read truncates to microseconds).
	amountMicro, _ := types.ParseUSDX(amount)
	wantLeaf := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, payer, payee, amountMicro, payEnv.Data.TS.UnixNano()))
	if rcptEnv.Data.LeafHashHex != wantLeaf {
		t.Fatalf("leaf = %q, want ref-free canonical leaf %q", rcptEnv.Data.LeafHashHex, wantLeaf)
	}

	// 6. A ref-less pay still signs the unchanged legacy preimage (lockstep with
	//    existing layerx.mjs signers).
	nonce2 := challenge()
	legacyPre := auth.IntentMessage("pay", payer, nonce2, payee, amount)
	legacyReq := types.PayRequest{
		ToDID:      payee,
		AmountUSDX: amount,
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      nonce2,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte(legacyPre))),
	}
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", legacyReq); rr.Code != http.StatusOK {
		t.Fatalf("legacy ref-less pay status = %d body=%s", rr.Code, rr.Body.String())
	}

	// 7. Malformed ref is rejected before any signature work.
	badReq := req
	badReq.Ref = "0xnothex"
	if rr := do(h, http.MethodPost, "/v1/pay", "", "", badReq); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed ref status = %d, want 400", rr.Code)
	}
}
