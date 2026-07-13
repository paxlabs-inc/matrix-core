package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/pkg/lxp"
	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

// getLXReceipt reads the public sequencer-signed transfer receipt off the
// layerxd test server.
func getLXReceipt(t *testing.T, rig *lxpTestRig, seq int64) lxtypes.Receipt {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/v1/receipt/%d", rig.lxd.URL, seq))
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	defer resp.Body.Close()
	var env struct {
		Ok   bool            `json:"ok"`
		Data lxtypes.Receipt `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || !env.Ok {
		t.Fatalf("receipt read (%d): ok=%v err=%v", resp.StatusCode, env.Ok, err)
	}
	return env.Data
}

// TestPropertyE2EFullEconomicLoop is the DEUS-LAYERX task-5.1 property (reqs
// 12.1, 12.3), the gate for the rail deletion: the REAL deus gateway + REAL
// layerxd over the REAL Postgres stores drive the full economic loop — 402
// challenge -> client signs -> settle -> execute -> 200 + X-LayerX-Receipt —
// in BOTH exact and hold modes; paid <-> served is cross-provable from signed
// artifacts (the payer-signed ref rides the sequencer-signed LayerX receipt
// AND the invocation row via layerx_seq); and a replayed idempotency key
// returns the stored result with exactly one charge. No fakes anywhere.
func TestPropertyE2EFullEconomicLoop(t *testing.T) {
	modes := []struct {
		name string
		opts lxpRigOpts
	}{
		{"exact", lxpRigOpts{}},
		{"hold", lxpRigOpts{settlementMode: "hold", holdTTLS: 60}},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			rig, ctx := newLXPRig(t, mode.opts)
			caller := newLXPCaller(t)
			if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
				t.Fatalf("fund caller: %v", err)
			}

			// 402 challenge: terms carry the priced amount, payee, a live
			// nonce, and the invocation-binding ref.
			idem := fmt.Sprintf("e2e-%s-%d", mode.name, time.Now().UnixNano())
			resp := caller.invoke(t, rig, idem, "")
			if resp.StatusCode != http.StatusPaymentRequired {
				t.Fatalf("unpaid = %d, want 402", resp.StatusCode)
			}
			_, terms := decodeTerms(t, resp)
			if terms.Protocol != lxp.Protocol || terms.Ref == "" || terms.Nonce == "" {
				t.Fatalf("bad terms: %+v", terms)
			}
			if n := atomic.LoadInt64(rig.executions); n != 0 {
				t.Fatalf("challenge executed the service (%d)", n)
			}

			// Client signs -> settle -> execute -> 200 + X-LayerX-Receipt.
			resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("paid = %d", resp.StatusCode)
			}
			rcptHdr, err := lxp.DecodeReceipt(resp.Header.Get(lxp.HeaderReceipt))
			if err != nil || rcptHdr.Seq <= 0 {
				t.Fatalf("bad receipt header: %+v (%v)", rcptHdr, err)
			}
			var inv struct {
				InvocationID string `json:"invocation_id"`
				Outcome      string `json:"outcome"`
				ChargedUSDX  string `json:"charged_usdx"`
				LayerXSeq    int64  `json:"layerx_seq"`
				Ref          string `json:"ref"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
				t.Fatalf("decode: %v", err)
			}
			resp.Body.Close()
			if inv.Outcome != "ok" || inv.LayerXSeq != rcptHdr.Seq || inv.Ref != terms.Ref {
				t.Fatalf("bad response cross-binding: %+v vs header %+v", inv, rcptHdr)
			}
			if n := atomic.LoadInt64(rig.executions); n != 1 {
				t.Fatalf("executions = %d, want 1", n)
			}

			// PAID: the public sequencer-signed LayerX receipt carries the
			// payer-signed ref, payer -> payee, exact amount.
			lxRcpt := getLXReceipt(t, rig, rcptHdr.Seq)
			if lxRcpt.Ref != terms.Ref || lxRcpt.FromDID != caller.did || lxRcpt.ToDID != rig.payeeDID ||
				lxRcpt.AmountUSDX != terms.AmountUSDX || lxRcpt.LeafHashHex == "" || lxRcpt.SequencerSig == "" {
				t.Fatalf("payment receipt not cross-bound: %+v (terms %+v)", lxRcpt, terms)
			}

			// SERVED: the metering row binds the same settlement seq (and the
			// hold, in hold mode); an execution receipt is stored against it.
			row, err := rig.db.GetInvocation(ctx, inv.InvocationID)
			if err != nil || row.Rail != "layerx" || row.LayerXSeq != rcptHdr.Seq || row.Outcome != "ok" {
				t.Fatalf("invocation row not cross-bound: %+v (%v)", row, err)
			}
			if mode.name == "hold" {
				if row.HoldID == "" {
					t.Fatal("hold-mode row carries no hold_id")
				}
				if hold := getHold(t, rig, row.HoldID); hold.Status != "captured" || hold.CaptureSeq != rcptHdr.Seq || hold.Ref != terms.Ref {
					t.Fatalf("hold not cross-bound: %+v", hold)
				}
			}
			if rec, err := rig.db.GetReceipt(ctx, inv.InvocationID); err != nil || rec.Digest == "" || rec.GatewaySig == "" {
				t.Fatalf("execution receipt missing: %+v (%v)", rec, err)
			}
			if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
				t.Fatalf("payee balance = %d, want 31500", bal)
			}

			// REPLAY: same idempotency key -> stored result, same seq, exactly
			// one charge and one execution.
			resp = caller.invoke(t, rig, idem, "")
			_, terms2 := decodeTerms(t, resp)
			resp = caller.invoke(t, rig, idem, caller.sign(t, terms2))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("replay = %d", resp.StatusCode)
			}
			var replay struct {
				Outcome   string         `json:"outcome"`
				Result    map[string]any `json:"result"`
				LayerXSeq int64          `json:"layerx_seq"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
				t.Fatalf("decode replay: %v", err)
			}
			resp.Body.Close()
			if replay.Outcome != "ok" || replay.Result["replayed"] != true || replay.LayerXSeq != rcptHdr.Seq {
				t.Fatalf("bad replay: %+v", replay)
			}
			if n := atomic.LoadInt64(rig.executions); n != 1 {
				t.Fatalf("replay executed again (%d)", n)
			}
			if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
				t.Fatalf("replay double-charged: payee = %d", bal)
			}
			if bal, _ := rig.harness.BalanceMicro(ctx, caller.did); bal != 1_000_000-31_500 {
				t.Fatalf("payer balance = %d, want %d (exactly one charge)", bal, 1_000_000-31_500)
			}
		})
	}
}
