package store

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

func TestHoldLifecycle(t *testing.T) {
	st, ctx := newTestStore(t)
	payer := uniqueDID("hold-payer")
	payee := uniqueDID("hold-payee")
	captor := uniqueDID("hold-captor")

	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 10_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	ref := "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	h, err := st.CreateHold(ctx, payer, payee, captor, 3_000_000, ref, time.Minute)
	if err != nil {
		t.Fatalf("CreateHold: %v", err)
	}
	if h.Status != "open" || h.AmountMicro != 3_000_000 || h.CaptorDID != captor || h.Ref != ref {
		t.Fatalf("bad hold: %+v", h)
	}

	// Held funds left the spendable balance.
	acct, _ := st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 7_000_000 {
		t.Fatalf("payer balance after hold = %d, want 7_000_000", acct.BalanceUSDX)
	}
	// Held funds are unspendable by pay and withdraw.
	if _, err := st.Pay(ctx, payer, payee, 8_000_000, "material", "", finalize(payer, payee, 8_000_000)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("pay over spendable = %v, want ErrInsufficientFunds", err)
	}
	if _, err := st.QueueWithdrawal(ctx, payer, 8_000_000, "", "material"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("withdraw over spendable = %v, want ErrInsufficientFunds", err)
	}

	// Non-captor cannot capture; over-amount rejected; wrong payee impossible by
	// construction (payee fixed on the hold row).
	if _, _, err := st.CaptureHold(ctx, h.ID, payer, 1_000_000, "micropayment", finalize(payer, payee, 1_000_000)); !errors.Is(err, ErrNotCaptor) {
		t.Fatalf("non-captor capture = %v, want ErrNotCaptor", err)
	}
	if _, _, err := st.CaptureHold(ctx, h.ID, captor, 3_000_001, "material", finalize(payer, payee, 3_000_001)); !errors.Is(err, ErrCaptureExceedsHold) {
		t.Fatalf("over-amount capture = %v, want ErrCaptureExceedsHold", err)
	}

	// Partial capture: payee credited, remainder back to payer, standard transfer
	// row emitted with the hold's ref.
	res, hc, err := st.CaptureHold(ctx, h.ID, captor, 2_000_000, "material", finalize(payer, payee, 2_000_000))
	if err != nil {
		t.Fatalf("CaptureHold: %v", err)
	}
	if res.Seq <= 0 || res.LeafHex == "" || hc.Status != "captured" || hc.CaptureSeq != res.Seq {
		t.Fatalf("bad capture: res=%+v hold=%+v", res, hc)
	}
	payeeAcct, _ := st.GetAccount(ctx, payee)
	if payeeAcct.BalanceUSDX != 2_000_000 {
		t.Fatalf("payee balance = %d, want 2_000_000", payeeAcct.BalanceUSDX)
	}
	acct, _ = st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 8_000_000 {
		t.Fatalf("payer balance after partial capture = %d, want 8_000_000 (7M + 1M remainder)", acct.BalanceUSDX)
	}
	row, err := st.GetTransferPublic(ctx, res.Seq)
	if err != nil {
		t.Fatalf("GetTransferPublic: %v", err)
	}
	if row.Ref != ref || row.FromDID != payer || row.ToDID != payee || row.AmountMicro != 2_000_000 {
		t.Fatalf("bad capture transfer row: %+v", row)
	}

	// Idempotent replay: same amount returns the SAME transfer, no double credit.
	res2, _, err := st.CaptureHold(ctx, h.ID, captor, 2_000_000, "material", finalize(payer, payee, 2_000_000))
	if err != nil {
		t.Fatalf("idempotent capture replay: %v", err)
	}
	if res2.Seq != res.Seq {
		t.Fatalf("replay seq = %d, want %d", res2.Seq, res.Seq)
	}
	payeeAcct, _ = st.GetAccount(ctx, payee)
	if payeeAcct.BalanceUSDX != 2_000_000 {
		t.Fatalf("payee balance after replay = %d, want 2_000_000 (no double credit)", payeeAcct.BalanceUSDX)
	}
	// Different-amount re-capture is a conflict, not a second charge.
	if _, _, err := st.CaptureHold(ctx, h.ID, captor, 1_000_000, "micropayment", finalize(payer, payee, 1_000_000)); !errors.Is(err, ErrHoldClosed) {
		t.Fatalf("double capture = %v, want ErrHoldClosed", err)
	}
	// Releasing a captured hold is rejected.
	if _, err := st.ReleaseHold(ctx, h.ID, captor); !errors.Is(err, ErrHoldClosed) {
		t.Fatalf("release captured = %v, want ErrHoldClosed", err)
	}
}

func TestHoldReleaseAndExpiry(t *testing.T) {
	st, ctx := newTestStore(t)
	payer := uniqueDID("rel-payer")
	payee := uniqueDID("rel-payee")
	captor := uniqueDID("rel-captor")

	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 10_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}

	// Release by payer, idempotent.
	h1, err := st.CreateHold(ctx, payer, payee, captor, 4_000_000, "", time.Minute)
	if err != nil {
		t.Fatalf("CreateHold: %v", err)
	}
	if _, err := st.ReleaseHold(ctx, h1.ID, payee); !errors.Is(err, ErrNotCaptor) {
		t.Fatalf("release by payee = %v, want ErrNotCaptor", err)
	}
	rel, err := st.ReleaseHold(ctx, h1.ID, payer)
	if err != nil || rel.Status != "released" {
		t.Fatalf("ReleaseHold: %v %+v", err, rel)
	}
	relAgain, err := st.ReleaseHold(ctx, h1.ID, captor)
	if err != nil || relAgain.Status != "released" {
		t.Fatalf("idempotent release: %v %+v", err, relAgain)
	}
	acct, _ := st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 10_000_000 {
		t.Fatalf("payer balance after release = %d, want 10_000_000 (full refund, once)", acct.BalanceUSDX)
	}

	// Expiry: capture past expiry rejected; sweep refunds; sweep idempotent.
	h2, err := st.CreateHold(ctx, payer, payee, captor, 2_500_000, "", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("CreateHold ttl: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, _, err := st.CaptureHold(ctx, h2.ID, captor, 2_500_000, "material", finalize(payer, payee, 2_500_000)); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("capture past expiry = %v, want ErrHoldExpired", err)
	}
	n, err := st.SweepExpiredHolds(ctx)
	if err != nil || n < 1 {
		t.Fatalf("SweepExpiredHolds = %d, %v; want >= 1", n, err)
	}
	got, _ := st.GetHold(ctx, h2.ID)
	if got.Status != "expired" {
		t.Fatalf("hold status after sweep = %q, want expired", got.Status)
	}
	acct, _ = st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 10_000_000 {
		t.Fatalf("payer balance after expiry sweep = %d, want 10_000_000", acct.BalanceUSDX)
	}
	if _, err := st.SweepExpiredHolds(ctx); err != nil {
		t.Fatalf("idempotent sweep: %v", err)
	}
	acct, _ = st.GetAccount(ctx, payer)
	if acct.BalanceUSDX != 10_000_000 {
		t.Fatalf("payer balance after re-sweep = %d, want 10_000_000 (no double refund)", acct.BalanceUSDX)
	}

	// Insufficient spendable balance rejects the hold in parity with pay.
	if _, err := st.CreateHold(ctx, payer, payee, captor, 10_000_001, "", time.Minute); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("over-balance hold = %v, want ErrInsufficientFunds", err)
	}
}

// TestHoldReserveInvariantProperty drives a seeded random interleaving of
// hold/capture/release/pay/withdraw/sweep against the REAL store and asserts,
// after every operation, (1) value conservation — deposits == balances + open
// holds + withdrawn + captured-away-to-payees' balances (payee balances are in
// the model) — and (2) the reserve invariant: the store's circulating supply
// (balances + open holds) moved exactly as the model predicts.
func TestHoldReserveInvariantProperty(t *testing.T) {
	st, ctx := newTestStore(t)
	rng := rand.New(rand.NewSource(42))

	const nAccounts = 4
	const deposit = int64(100_000_000) // 100 USDX each
	dids := make([]string, nAccounts)
	model := map[string]int64{}
	for i := range dids {
		dids[i] = uniqueDID("prop")
		if err := st.CreditDeposit(ctx, dids[i], "0xabc", "0xdep-"+dids[i], deposit); err != nil {
			t.Fatalf("CreditDeposit: %v", err)
		}
		model[dids[i]] = deposit
	}
	captor := uniqueDID("prop-captor")

	type openHold struct {
		id     string
		payer  string
		amount int64
	}
	var open []openHold
	var withdrawn int64

	supply0, err := st.Supply(ctx)
	if err != nil {
		t.Fatalf("Supply: %v", err)
	}
	circulating := func() int64 {
		var c int64
		for _, d := range dids {
			c += model[d]
		}
		for _, h := range open {
			c += h.amount
		}
		return c
	}
	circ0 := circulating()

	check := func(step int) {
		t.Helper()
		for _, d := range dids {
			acct, gerr := st.GetAccount(ctx, d)
			if gerr != nil {
				t.Fatalf("step %d: GetAccount(%s): %v", step, d, gerr)
			}
			if acct.BalanceUSDX != model[d] {
				t.Fatalf("step %d: balance(%s) = %d, model = %d", step, d, acct.BalanceUSDX, model[d])
			}
		}
		sup, serr := st.Supply(ctx)
		if serr != nil {
			t.Fatalf("step %d: Supply: %v", step, serr)
		}
		// The DB is shared across tests, so assert on the DELTA of circulating
		// supply, which isolates this test's own value movements (withdrawals are
		// the only ops that remove circulating USDX).
		if got, want := sup.CirculatingMicroUSDX-supply0.CirculatingMicroUSDX, circulating()-circ0; got != want {
			t.Fatalf("step %d: circulating delta = %d, model delta = %d", step, got, want)
		}
		var total int64
		for _, d := range dids {
			total += model[d]
		}
		for _, h := range open {
			total += h.amount
		}
		if total+withdrawn != int64(nAccounts)*deposit {
			t.Fatalf("step %d: conservation broken: balances+holds+withdrawn = %d, want %d",
				step, total+withdrawn, int64(nAccounts)*deposit)
		}
	}

	for step := 0; step < 120; step++ {
		payer := dids[rng.Intn(nAccounts)]
		payee := dids[rng.Intn(nAccounts)]
		amount := int64(rng.Intn(3_000_000) + 1)
		switch op := rng.Intn(6); op {
		case 0: // pay
			_, err := st.Pay(ctx, payer, payee, amount, "micropayment", "", finalize(payer, payee, amount))
			switch {
			case payer == payee:
				if err == nil {
					t.Fatalf("step %d: self-pay accepted", step)
				}
			case model[payer] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: pay underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: pay: %v", step, err)
			default:
				model[payer] -= amount
				model[payee] += amount
			}
		case 1: // hold
			h, err := st.CreateHold(ctx, payer, payee, captor, amount, "", time.Minute)
			switch {
			case payer == payee:
				if err == nil {
					t.Fatalf("step %d: self-hold accepted", step)
				}
			case model[payer] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: hold underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: hold: %v", step, err)
			default:
				model[payer] -= amount
				open = append(open, openHold{id: h.ID, payer: payer, amount: amount})
			}
		case 2: // capture (partial or full) on a random open hold
			if len(open) == 0 {
				continue
			}
			i := rng.Intn(len(open))
			h := open[i]
			capAmt := int64(rng.Intn(int(h.amount)) + 1)
			hh, err := st.GetHold(ctx, h.id)
			if err != nil {
				t.Fatalf("step %d: GetHold: %v", step, err)
			}
			_, _, err = st.CaptureHold(ctx, h.id, captor, capAmt, "micropayment",
				finalize(hh.PayerDID, hh.PayeeDID, capAmt))
			if err != nil {
				t.Fatalf("step %d: capture: %v", step, err)
			}
			model[hh.PayeeDID] += capAmt
			model[h.payer] += h.amount - capAmt
			open = append(open[:i], open[i+1:]...)
		case 3: // release a random open hold
			if len(open) == 0 {
				continue
			}
			i := rng.Intn(len(open))
			h := open[i]
			if _, err := st.ReleaseHold(ctx, h.id, captor); err != nil {
				t.Fatalf("step %d: release: %v", step, err)
			}
			model[h.payer] += h.amount
			open = append(open[:i], open[i+1:]...)
		case 4: // withdraw
			_, err := st.QueueWithdrawal(ctx, payer, amount, "", "material")
			switch {
			case model[payer] < amount:
				if !errors.Is(err, ErrInsufficientFunds) {
					t.Fatalf("step %d: withdraw underfunded = %v", step, err)
				}
			case err != nil:
				t.Fatalf("step %d: withdraw: %v", step, err)
			default:
				model[payer] -= amount
				withdrawn += amount
			}
		case 5: // sweep (no holds here are expired — TTL 1m — so it must be a no-op for this test's holds)
			if _, err := st.SweepExpiredHolds(ctx); err != nil {
				t.Fatalf("step %d: sweep: %v", step, err)
			}
		default:
			_ = op
		}
		check(step)
	}

	if len(open) > 0 {
		// Close out: release everything and re-check conservation one last time.
		for _, h := range open {
			if _, err := st.ReleaseHold(ctx, h.id, captor); err != nil {
				t.Fatalf("final release: %v", err)
			}
			model[h.payer] += h.amount
		}
		open = nil
		check(-1)
	}
}
