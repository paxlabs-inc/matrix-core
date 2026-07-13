package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/layerx"
	"github.com/paxlabs-inc/deus/pkg/lxp"
)

// newCaptorClient mints a fresh keyed layerx client — a DID that can run
// captor operations through the principal-token lane.
func newCaptorClient(t *testing.T, url, label string) *layerx.Client {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	cli, err := layerx.New(layerx.Config{BaseURL: url, KeyHex: hex.EncodeToString(seed), DIDLabel: label})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

// openDirectHold signs and submits a hold intent straight to layerxd: payer
// consents to captorDID capturing up to amountUSDX from a fixed payee before
// the TTL — the ledger-level surface the gateway rides.
func openDirectHold(t *testing.T, ctx context.Context, rig *lxpTestRig, payer lxpCaller, payeeDID, captorDID, amountUSDX string, ttl int64) string {
	t.Helper()
	cli, err := layerx.New(layerx.Config{BaseURL: rig.lxd.URL})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cli.Challenge(ctx, payer.did)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	p := lxp.Payment{
		FromDID:    payer.did,
		PublicKey:  hex.EncodeToString(payer.pub),
		Nonce:      ch.Nonce,
		ToDID:      payeeDID,
		AmountUSDX: amountUSDX,
	}
	preimage := lxp.HoldPreimage(p, ttl, captorDID)
	p.Signature = hex.EncodeToString(ed25519.Sign(payer.priv, []byte(preimage)))
	hold, err := cli.SubmitHold(ctx, layerx.HoldIntent{
		FromDID:    p.FromDID,
		PublicKey:  p.PublicKey,
		Nonce:      p.Nonce,
		Signature:  p.Signature,
		ToDID:      payeeDID,
		AmountUSDX: amountUSDX,
		CaptorDID:  captorDID,
		TTLSeconds: ttl,
	})
	if err != nil {
		t.Fatalf("submit hold: %v", err)
	}
	return hold.HoldID
}

// TestPropertyCustodyGatewayNeverTouchesTheMoney is the DEUS-LAYERX task-5.2
// custody property (reqs 12.2, 12.3), half one: across successful, failed,
// and replayed settlements in hold mode, the gateway DID's LayerX balance is
// byte-identical before and after — deus transports signatures, funds move
// payer -> payee only. And with layerxd down, every priced call is 503 with
// ZERO service executions.
func TestPropertyCustodyGatewayNeverTouchesTheMoney(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{
		settlementMode: "hold", holdTTLS: 60,
		backend: func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["city"] == "fail" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		},
	})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}
	gwBefore, err := rig.harness.BalanceMicro(ctx, rig.gatewayDID)
	if err != nil {
		t.Fatal(err)
	}

	invokeCity := func(idem, city, payment string) *http.Response {
		body, _ := json.Marshal(map[string]any{
			"operation": "forecast", "args": map[string]any{"city": city}, "idempotency_key": idem,
		})
		req, _ := http.NewRequest(http.MethodPost, rig.ts.URL+"/v1/invoke/"+rig.serviceID, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-agent")
		req.Header.Set("X-Caller-DID", caller.did)
		if payment != "" {
			req.Header.Set(lxp.HeaderPayment, payment)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Successful settlement.
	idemOK := fmt.Sprintf("custody-ok-%d", time.Now().UnixNano())
	resp := invokeCity(idemOK, "berlin", "")
	_, terms := decodeTerms(t, resp)
	resp = invokeCity(idemOK, "berlin", caller.sign(t, terms))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid invoke = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Failed execution (hold released).
	idemFail := fmt.Sprintf("custody-fail-%d", time.Now().UnixNano())
	resp = invokeCity(idemFail, "fail", "")
	_, failTerms := decodeTerms(t, resp)
	resp = invokeCity(idemFail, "fail", caller.sign(t, failTerms))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed invoke = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()

	// Replay of the success.
	resp = invokeCity(idemOK, "berlin", "")
	_, terms2 := decodeTerms(t, resp)
	resp = invokeCity(idemOK, "berlin", caller.sign(t, terms2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The gateway DID's balance is byte-identical: it captured as captor but
	// never held or received a micro-USDX.
	gwAfter, err := rig.harness.BalanceMicro(ctx, rig.gatewayDID)
	if err != nil {
		t.Fatal(err)
	}
	if gwAfter != gwBefore {
		t.Fatalf("gateway DID balance moved: %d -> %d", gwBefore, gwAfter)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
		t.Fatalf("payee = %d, want exactly one 31500 charge", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, caller.did); bal != 1_000_000-31_500 {
		t.Fatalf("payer = %d, want one charge + full refund of the failed hold", bal)
	}

	// layerxd down: prefetch live terms first, then kill the rail. Signed or
	// unpaid, every priced call is 503 and the service NEVER executes.
	idemDown := fmt.Sprintf("custody-down-%d", time.Now().UnixNano())
	resp = invokeCity(idemDown, "berlin", "")
	_, downTerms := decodeTerms(t, resp)
	execsBefore := atomic.LoadInt64(rig.executions)
	rig.lxd.Close()
	resp = invokeCity(idemDown, "berlin", caller.sign(t, downTerms))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down signed = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	resp = invokeCity("custody-down2-"+idemDown, "berlin", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down unpaid = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	if n := atomic.LoadInt64(rig.executions); n != execsBefore {
		t.Fatalf("rail-down executed the service (%d -> %d)", execsBefore, n)
	}
	row, err := rig.db.GetInvocationByIdempotency(ctx, idemDown)
	if err != nil || row.Outcome != "voided" {
		t.Fatalf("rail-down row = %+v (%v), want voided", row, err)
	}
}

// TestPropertyCustodyLedgerBounds is task-5.2 half two: the payer-consented
// capture bounds hold at the LEDGER regardless of transport — capture over
// amount, by a non-captor, past expiry, or redirecting the payee is
// impossible; expired holds release on the sweep.
func TestPropertyCustodyLedgerBounds(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{})
	payer := newLXPCaller(t)
	payee := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, payer.did, "0xabc", "0xdep-"+payer.did, 1_000_000); err != nil {
		t.Fatalf("fund payer: %v", err)
	}
	captor := newCaptorClient(t, rig.lxd.URL, "custody-captor")
	stranger := newCaptorClient(t, rig.lxd.URL, "custody-stranger")

	holdID := openDirectHold(t, ctx, rig, payer, payee.did, captor.DID(), "0.100000", 60)
	if bal, _ := rig.harness.BalanceMicro(ctx, payer.did); bal != 900_000 {
		t.Fatalf("payer after hold = %d, want 900000 (held funds unspendable)", bal)
	}

	// A non-captor cannot capture.
	if _, err := stranger.Capture(ctx, holdID, "0.050000"); err == nil {
		t.Fatal("non-captor capture succeeded")
	}
	// The captor cannot capture over the payer-consented amount.
	if _, err := captor.Capture(ctx, holdID, "0.200000"); err == nil {
		t.Fatal("over-amount capture succeeded")
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, payee.did); bal != 0 {
		t.Fatalf("payee credited by a rejected capture: %d", bal)
	}

	// A bounded capture pays the FIXED payee (the wire has no payee field to
	// redirect) and returns the remainder to the payer in the same tx.
	res, err := captor.Capture(ctx, holdID, "0.060000")
	if err != nil {
		t.Fatalf("bounded capture: %v", err)
	}
	if res.Receipt.ToDID != payee.did || res.Receipt.FromDID != payer.did || res.Receipt.AmountUSDX != "0.060000" {
		t.Fatalf("capture transfer not payer->fixed-payee: %+v", res.Receipt)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, payee.did); bal != 60_000 {
		t.Fatalf("payee after capture = %d, want 60000", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, payer.did); bal != 940_000 {
		t.Fatalf("payer after capture = %d, want 940000 (remainder returned)", bal)
	}
	// Double capture with a different amount is rejected.
	if _, err := captor.Capture(ctx, holdID, "0.010000"); err == nil {
		t.Fatal("double capture succeeded")
	}

	// Past expiry the captor gets nothing, and the sweep releases in full.
	expID := openDirectHold(t, ctx, rig, payer, payee.did, captor.DID(), "0.050000", 1)
	time.Sleep(1200 * time.Millisecond)
	if _, err := captor.Capture(ctx, expID, "0.050000"); err == nil {
		t.Fatal("past-expiry capture succeeded")
	}
	n, err := rig.harness.SweepExpiredHolds(ctx)
	if err != nil || n < 1 {
		t.Fatalf("sweep = %d (%v), want >=1", n, err)
	}
	if hold := getHold(t, rig, expID); hold.Status != "expired" {
		t.Fatalf("swept hold status = %q, want expired", hold.Status)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, payer.did); bal != 940_000 {
		t.Fatalf("payer after sweep = %d, want 940000 (expired hold fully released)", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, payee.did); bal != 60_000 {
		t.Fatalf("payee after sweep = %d, want 60000 (unchanged)", bal)
	}
}
