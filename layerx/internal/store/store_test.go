package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
)

// newTestStore connects to LAYERX_TEST_POSTGRES_URI (a disposable DB) and runs
// migrations. Without that env var the integration test is skipped, so the
// default `go test` run stays hermetic.
func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping store integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	st, err := New(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, ctx
}

func uniqueDID(label string) string {
	return fmt.Sprintf("did:matrix:%s-%d:0123456789abcdef", label, time.Now().UnixNano())
}

func finalize(from, to string, amount int64) func(int64, time.Time) (string, string) {
	return func(seq int64, ts time.Time) (string, string) {
		leaf := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, from, to, amount, ts.UnixNano()))
		return leaf, "testsig"
	}
}

func TestStoreLedgerFlow(t *testing.T) {
	st, ctx := newTestStore(t)
	payer := uniqueDID("payer")
	payee := uniqueDID("payee")

	// Fund the payer.
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdeposit-"+payer, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	// Idempotent on deposit_tx: a replay must not double-credit.
	if err := st.CreditDeposit(ctx, payer, "0xabc", "0xdeposit-"+payer, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit replay: %v", err)
	}
	acct, err := st.GetAccount(ctx, payer)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acct.BalanceUSDX != 5_000_000 || acct.EscrowUSDX != 5_000_000 {
		t.Fatalf("after idempotent deposit balance=%d escrow=%d, want 5_000_000 each", acct.BalanceUSDX, acct.EscrowUSDX)
	}

	// Pay 2 USDX.
	res, err := st.Pay(ctx, payer, payee, 2_000_000, "material", finalize(payer, payee, 2_000_000))
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Seq <= 0 || res.LeafHex == "" {
		t.Fatalf("bad pay result: %+v", res)
	}

	payerAcct, _ := st.GetAccount(ctx, payer)
	payeeAcct, _ := st.GetAccount(ctx, payee)
	if payerAcct.BalanceUSDX != 3_000_000 {
		t.Errorf("payer balance = %d, want 3_000_000", payerAcct.BalanceUSDX)
	}
	if payeeAcct.BalanceUSDX != 2_000_000 {
		t.Errorf("payee balance = %d, want 2_000_000", payeeAcct.BalanceUSDX)
	}

	// Overspend must be rejected and leave balances intact.
	if _, err := st.Pay(ctx, payer, payee, 9_000_000, "material", finalize(payer, payee, 9_000_000)); err != ErrInsufficientFunds {
		t.Fatalf("overspend err = %v, want ErrInsufficientFunds", err)
	}
	if a, _ := st.GetAccount(ctx, payer); a.BalanceUSDX != 3_000_000 {
		t.Fatalf("balance changed after failed pay: %d", a.BalanceUSDX)
	}

	// Self-pay rejected.
	if _, err := st.Pay(ctx, payer, payer, 1, "micropayment", finalize(payer, payer, 1)); err == nil {
		t.Fatal("self-pay must be rejected")
	}

	// Receipt scoping: payee can read, a stranger cannot.
	if _, err := st.GetTransfer(ctx, res.Seq, payee); err != nil {
		t.Fatalf("payee GetTransfer: %v", err)
	}
	if _, err := st.GetTransfer(ctx, res.Seq, uniqueDID("stranger")); err != ErrNotFound {
		t.Fatalf("stranger GetTransfer err = %v, want ErrNotFound", err)
	}

	// Settle: our transfer must appear unsettled, then seal+anchor it.
	unsettled, err := st.ListUnsettled(ctx, 0)
	if err != nil {
		t.Fatalf("ListUnsettled: %v", err)
	}
	leaves := make([][32]byte, 0, len(unsettled))
	seqs := make([]int64, 0, len(unsettled))
	found := false
	for _, u := range unsettled {
		raw, _ := hex.DecodeString(u.LeafHex)
		var l [32]byte
		copy(l[:], raw)
		leaves = append(leaves, l)
		seqs = append(seqs, u.Seq)
		if u.Seq == res.Seq {
			found = true
		}
	}
	if !found {
		t.Fatal("our transfer should be in the unsettled set")
	}
	root := accumulator.Root(leaves)
	now := time.Now().UTC()
	batchID, err := st.SealBatch(ctx, hex.EncodeToString(root[:]), seqs, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("SealBatch: %v", err)
	}
	if err := st.MarkAnchored(ctx, batchID, "0xanchor"); err != nil {
		t.Fatalf("MarkAnchored: %v", err)
	}

	// Now the transfer is settled with a root + proof reconstructable.
	row, err := st.GetTransfer(ctx, res.Seq, payee)
	if err != nil {
		t.Fatalf("GetTransfer post-settle: %v", err)
	}
	if !row.Settled || row.BatchRootHex == "" || row.AnchorTx != "0xanchor" {
		t.Fatalf("transfer not settled correctly: %+v", row)
	}
	bl, err := st.ListBatchLeaves(ctx, batchID)
	if err != nil || len(bl) == 0 {
		t.Fatalf("ListBatchLeaves: %v (n=%d)", err, len(bl))
	}

	// Withdrawal: escrow clamps at 0, balance is the bound.
	if _, err := st.QueueWithdrawal(ctx, payee, 2_000_000, "", "material"); err != nil {
		t.Fatalf("QueueWithdrawal: %v", err)
	}
	pa, _ := st.GetAccount(ctx, payee)
	if pa.BalanceUSDX != 0 {
		t.Errorf("payee balance after withdraw = %d, want 0", pa.BalanceUSDX)
	}
	if pa.EscrowUSDX != 0 {
		t.Errorf("payee escrow after withdraw = %d, want 0 (clamped)", pa.EscrowUSDX)
	}
	if _, err := st.QueueWithdrawal(ctx, payee, 1, "", "material"); err != ErrInsufficientFunds {
		t.Fatalf("over-withdraw err = %v, want ErrInsufficientFunds", err)
	}
}
