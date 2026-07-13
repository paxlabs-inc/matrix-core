package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// signedIntent mints a challenge nonce for did and signs the given intent
// fields, returning (nonce, signature hex).
func signedIntent(t *testing.T, h http.Handler, did string, priv ed25519.PrivateKey, op string, fields ...string) (string, string) {
	t.Helper()
	rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", "", "", types.ChallengeRequest{DID: did})
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env struct {
		Data types.ChallengeResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	preimage := auth.IntentMessage(op, did, env.Data.Nonce, fields...)
	return env.Data.Nonce, hex.EncodeToString(ed25519.Sign(priv, []byte(preimage)))
}

func decodeHold(t *testing.T, body []byte) types.HoldView {
	t.Helper()
	var env struct {
		Data types.HoldView `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode hold: %v", err)
	}
	return env.Data
}

// TestHoldHTTPLifecycle drives the full hold surface over real HTTP + the real
// store: create (signed intent), captor-only capture with bounds, receipt
// emission, public reads, release, expiry, double-capture, and 402 parity.
func TestHoldHTTPLifecycle(t *testing.T) {
	srv, st, _, ctx := newExplorerServer(t)
	h := srv.Handler()

	payer, payerPub, payerPriv := uniqueDID(t)
	payee, _, _ := uniqueDID(t)
	captor, captorPub, captorPriv := uniqueDID(t)
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 10_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	ref := "0x3333333333333333333333333333333333333333333333333333333333333333"
	holdReq := func(amount string, ttl int64, ref string) types.HoldRequest {
		nonce, sig := signedIntent(t, h, payer, payerPriv, "hold",
			payee, amount, fmt.Sprintf("%d", ttl), ref, captor)
		return types.HoldRequest{
			ToDID:      payee,
			AmountUSDX: amount,
			CaptorDID:  captor,
			TTLSeconds: ttl,
			Ref:        ref,
			FromDID:    payer,
			PublicKey:  hex.EncodeToString(payerPub),
			Nonce:      nonce,
			Signature:  sig,
		}
	}

	// 1. Create a hold: 3 USDX for 60s.
	rr := do(h, http.MethodPost, "/v1/hold", "", "", holdReq("3.000000", 60, ref))
	if rr.Code != http.StatusOK {
		t.Fatalf("hold status = %d body=%s", rr.Code, rr.Body.String())
	}
	hv := decodeHold(t, rr.Body.Bytes())
	if hv.Status != "open" || hv.AmountUSDX != "3.000000" || hv.CaptorDID != captor || hv.Ref != ref {
		t.Fatalf("bad hold view: %+v", hv)
	}

	// 2. Public read, no auth.
	rr = do(h, http.MethodGet, "/v1/hold/"+hv.HoldID, "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("public hold read = %d", rr.Code)
	}

	// 3. Over-balance hold -> 402 parity with pay.
	rr = do(h, http.MethodPost, "/v1/hold", "", "", holdReq("100.000000", 60, ""))
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("over-balance hold = %d, want 402; body=%s", rr.Code, rr.Body.String())
	}

	// 4. Capture by the PAYER (not the captor) is rejected at the ledger.
	nonce, sig := signedIntent(t, h, payer, payerPriv, "capture", hv.HoldID, "1.000000")
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", "", types.CaptureRequest{
		AmountUSDX: "1.000000", FromDID: payer, PublicKey: hex.EncodeToString(payerPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("non-captor capture = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}

	// 5. Captor capture over the held amount is rejected.
	nonce, sig = signedIntent(t, h, captor, captorPriv, "capture", hv.HoldID, "3.000001")
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", "", types.CaptureRequest{
		AmountUSDX: "3.000001", FromDID: captor, PublicKey: hex.EncodeToString(captorPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("over-amount capture = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}

	// 6. Partial capture by the captor: 2 USDX; remainder auto-returns.
	nonce, sig = signedIntent(t, h, captor, captorPriv, "capture", hv.HoldID, "2.000000")
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", "", types.CaptureRequest{
		AmountUSDX: "2.000000", FromDID: captor, PublicKey: hex.EncodeToString(captorPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("capture status = %d body=%s", rr.Code, rr.Body.String())
	}
	var capEnv struct {
		Data types.CaptureResponse `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &capEnv); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	if capEnv.Data.Receipt.Seq <= 0 || capEnv.Data.Receipt.Ref != ref ||
		capEnv.Data.Receipt.FromDID != payer || capEnv.Data.Receipt.ToDID != payee ||
		capEnv.Data.Receipt.AmountUSDX != "2.000000" {
		t.Fatalf("bad capture receipt: %+v", capEnv.Data.Receipt)
	}
	if capEnv.Data.Hold.Status != "captured" || capEnv.Data.Hold.CapturedUSDX != "2.000000" {
		t.Fatalf("bad captured hold: %+v", capEnv.Data.Hold)
	}
	// The capture's transfer is publicly receipt-readable with the ref.
	rr = do(h, http.MethodGet, fmt.Sprintf("/v1/receipt/%d", capEnv.Data.Receipt.Seq), "", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("capture receipt read = %d", rr.Code)
	}
	// Payer got the 1 USDX remainder back: 10 - 3 + 1 = 8.
	acct, _ := st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 8_000_000 {
		t.Fatalf("payer balance = %d, want 8_000_000", acct.BalanceUSDX)
	}

	// 7. Double capture with a different amount -> 409 conflict.
	nonce, sig = signedIntent(t, h, captor, captorPriv, "capture", hv.HoldID, "1.000000")
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", "", types.CaptureRequest{
		AmountUSDX: "1.000000", FromDID: captor, PublicKey: hex.EncodeToString(captorPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("double capture = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}

	// 8. Release path: new hold, released by the payer, funds restored.
	rr = do(h, http.MethodPost, "/v1/hold", "", "", holdReq("4.000000", 60, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("hold 2 status = %d body=%s", rr.Code, rr.Body.String())
	}
	hv2 := decodeHold(t, rr.Body.Bytes())
	nonce, sig = signedIntent(t, h, payer, payerPriv, "release", hv2.HoldID)
	rr = do(h, http.MethodPost, "/v1/hold/"+hv2.HoldID+"/release", "", "", types.ReleaseRequest{
		FromDID: payer, PublicKey: hex.EncodeToString(payerPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("release status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := decodeHold(t, rr.Body.Bytes()); got.Status != "released" {
		t.Fatalf("hold status after release = %q", got.Status)
	}
	acct, _ = st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 8_000_000 {
		t.Fatalf("payer balance after release = %d, want 8_000_000", acct.BalanceUSDX)
	}

	// 9. Expiry: 1s hold, wait past expiry, capture -> 409; sweep refunds.
	rr = do(h, http.MethodPost, "/v1/hold", "", "", holdReq("1.000000", 1, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("hold 3 status = %d body=%s", rr.Code, rr.Body.String())
	}
	hv3 := decodeHold(t, rr.Body.Bytes())
	time.Sleep(1100 * time.Millisecond)
	nonce, sig = signedIntent(t, h, captor, captorPriv, "capture", hv3.HoldID, "1.000000")
	rr = do(h, http.MethodPost, "/v1/hold/"+hv3.HoldID+"/capture", "", "", types.CaptureRequest{
		AmountUSDX: "1.000000", FromDID: captor, PublicKey: hex.EncodeToString(captorPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expired capture = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := st.SweepExpiredHolds(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	acct, _ = st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 8_000_000 {
		t.Fatalf("payer balance after expiry sweep = %d, want 8_000_000", acct.BalanceUSDX)
	}

	// 10. A tampered hold intent (signed captor != submitted captor) is 401.
	nonce, sig = signedIntent(t, h, payer, payerPriv, "hold", payee, "1.000000", "60", "", captor)
	rr = do(h, http.MethodPost, "/v1/hold", "", "", types.HoldRequest{
		ToDID: payee, AmountUSDX: "1.000000", CaptorDID: payee, TTLSeconds: 60,
		FromDID: payer, PublicKey: hex.EncodeToString(payerPub), Nonce: nonce, Signature: sig,
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered captor hold = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHoldPrincipalTokenLane proves the X-LayerX-Agent token authorizes hold
// lifecycle ops in place of a signed intent (the writeCaller pattern), with the
// token DID as the acting principal.
func TestHoldPrincipalTokenLane(t *testing.T) {
	srv, st, _, ctx := newExplorerServer(t)
	h := srv.Handler()

	payer, payerPub, payerPriv := uniqueDID(t)
	payee, _, _ := uniqueDID(t)
	captor, captorPub, captorPriv := uniqueDID(t)
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	mintToken := func(did string, pub ed25519.PublicKey, priv ed25519.PrivateKey) string {
		rr := do(h, http.MethodPost, "/v1/agent/auth/challenge", "", "", types.ChallengeRequest{DID: did})
		var chEnv struct {
			Data types.ChallengeResponse `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &chEnv); err != nil {
			t.Fatalf("decode challenge: %v", err)
		}
		sig := hex.EncodeToString(ed25519.Sign(priv, []byte(chEnv.Data.Message)))
		rr = do(h, http.MethodPost, "/v1/agent/auth/verify", "", "", types.VerifyRequest{
			DID: did, PublicKey: hex.EncodeToString(pub), Nonce: chEnv.Data.Nonce, Signature: sig,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("verify status = %d body=%s", rr.Code, rr.Body.String())
		}
		var vEnv struct {
			Data types.VerifyResponse `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &vEnv); err != nil {
			t.Fatalf("decode verify: %v", err)
		}
		return vEnv.Data.Token
	}

	payerTok := mintToken(payer, payerPub, payerPriv)
	captorTok := mintToken(captor, captorPub, captorPriv)

	rr := do(h, http.MethodPost, "/v1/hold", "", payerTok, types.HoldRequest{
		ToDID: payee, AmountUSDX: "2.500000", CaptorDID: captor, TTLSeconds: 60,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("token hold status = %d body=%s", rr.Code, rr.Body.String())
	}
	hv := decodeHold(t, rr.Body.Bytes())

	// The payer's token cannot capture (token DID != captor).
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", payerTok, types.CaptureRequest{AmountUSDX: "2.500000"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("payer-token capture = %d, want 401", rr.Code)
	}
	// The captor's token captures in full.
	rr = do(h, http.MethodPost, "/v1/hold/"+hv.HoldID+"/capture", "", captorTok, types.CaptureRequest{AmountUSDX: "2.500000"})
	if rr.Code != http.StatusOK {
		t.Fatalf("captor-token capture = %d body=%s", rr.Code, rr.Body.String())
	}
	// Full capture leaves no remainder; payee credited.
	payeeAcct, _ := st.GetAccount(ctx, payee)
	if payeeAcct.BalanceUSDX != 2_500_000 {
		t.Fatalf("payee balance = %d, want 2_500_000", payeeAcct.BalanceUSDX)
	}
}
