package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/pkg/types"
)

func TestDIDClaimRegistry(t *testing.T) {
	st, ctx := newTestStore(t)
	did := uniqueDID("claimant")
	claim := fmt.Sprintf("deadbeef%d", time.Now().UnixNano())

	if _, err := st.ResolveDIDClaim(ctx, claim); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered claim must be ErrNotFound, got %v", err)
	}
	if err := st.RegisterDIDClaim(ctx, claim, did); err != nil {
		t.Fatalf("RegisterDIDClaim: %v", err)
	}
	// Re-register is idempotent.
	if err := st.RegisterDIDClaim(ctx, claim, did); err != nil {
		t.Fatalf("RegisterDIDClaim (repeat): %v", err)
	}
	got, err := st.ResolveDIDClaim(ctx, claim)
	if err != nil {
		t.Fatalf("ResolveDIDClaim: %v", err)
	}
	if got != did {
		t.Fatalf("resolved %q, want %q", got, did)
	}
}

func TestChainCursor(t *testing.T) {
	st, ctx := newTestStore(t)
	name := "test_cursor_" + uniqueDID("c")[12:24]

	if _, ok, err := st.GetCursor(ctx, name); err != nil || ok {
		t.Fatalf("fresh cursor must be absent, ok=%v err=%v", ok, err)
	}
	if err := st.SetCursor(ctx, name, 42); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	block, ok, err := st.GetCursor(ctx, name)
	if err != nil || !ok || block != 42 {
		t.Fatalf("GetCursor = (%d,%v,%v), want (42,true,nil)", block, ok, err)
	}
	if err := st.SetCursor(ctx, name, 100); err != nil {
		t.Fatalf("SetCursor advance: %v", err)
	}
	if block, _, _ := st.GetCursor(ctx, name); block != 100 {
		t.Fatalf("cursor not advanced, got %d", block)
	}
}

func TestWithdrawalPayoutLifecycle(t *testing.T) {
	st, ctx := newTestStore(t)
	did := uniqueDID("wd")

	// Fund + bind a payout address, then queue a withdrawal.
	if err := st.CreditDeposit(ctx, did, "0x000000000000000000000000000000000000aaaa", "0xdep:"+did, 5_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	wid, err := st.QueueWithdrawal(ctx, did, 2_000_000, "", types.TierMaterial)
	if err != nil {
		t.Fatalf("QueueWithdrawal: %v", err)
	}

	queued, err := st.ListQueuedWithdrawals(ctx, 0)
	if err != nil {
		t.Fatalf("ListQueuedWithdrawals: %v", err)
	}
	var found *QueuedWithdrawal
	for i := range queued {
		if queued[i].ID == wid {
			found = &queued[i]
		}
	}
	if found == nil {
		t.Fatal("queued withdrawal not listed")
	}
	if found.EVMAddress == "" {
		t.Fatal("queued withdrawal must carry the mapped EVM payout address")
	}

	// Seal -> submitted, with a frozen payout root + recipient.
	root := "00aa" + wid[:8]
	if err := st.SealWithdrawals(ctx, root, []SealedWithdrawal{{ID: wid, EVMAddress: found.EVMAddress}}); err != nil {
		t.Fatalf("SealWithdrawals: %v", err)
	}
	// No longer queued.
	for _, q := range mustQueued(t, st, ctx) {
		if q.ID == wid {
			t.Fatal("sealed withdrawal must leave the queued set")
		}
	}
	// Re-sealing the same (now submitted) row must fail (not in 'queued').
	if err := st.SealWithdrawals(ctx, root, []SealedWithdrawal{{ID: wid, EVMAddress: found.EVMAddress}}); err == nil {
		t.Fatal("re-sealing a submitted withdrawal must fail")
	}

	submitted, err := st.ListSubmittedWithdrawals(ctx)
	if err != nil {
		t.Fatalf("ListSubmittedWithdrawals: %v", err)
	}
	var sub *QueuedWithdrawal
	for i := range submitted {
		if submitted[i].ID == wid {
			sub = &submitted[i]
		}
	}
	if sub == nil {
		t.Fatal("sealed withdrawal must appear as submitted")
	}
	if sub.PayoutRoot != root || sub.EVMAddress != found.EVMAddress {
		t.Fatalf("submitted row lost frozen root/recipient: %+v", sub)
	}

	// Record a transient error (stays submitted), then confirm settled.
	if err := st.RecordWithdrawalError(ctx, root, "chain timeout"); err != nil {
		t.Fatalf("RecordWithdrawalError: %v", err)
	}
	if err := st.MarkWithdrawalSettled(ctx, wid, "0xpayouttx"); err != nil {
		t.Fatalf("MarkWithdrawalSettled: %v", err)
	}
	for _, s := range mustSubmitted(t, st, ctx) {
		if s.ID == wid {
			t.Fatal("settled withdrawal must leave the submitted set")
		}
	}
}

func TestCirculatingUSDX(t *testing.T) {
	st, ctx := newTestStore(t)
	before, err := st.CirculatingUSDX(ctx)
	if err != nil {
		t.Fatalf("CirculatingUSDX: %v", err)
	}
	did := uniqueDID("circ")
	if err := st.CreditDeposit(ctx, did, "0x000000000000000000000000000000000000bbbb", "0xdep:"+did, 7_000_000); err != nil {
		t.Fatalf("CreditDeposit: %v", err)
	}
	after, err := st.CirculatingUSDX(ctx)
	if err != nil {
		t.Fatalf("CirculatingUSDX: %v", err)
	}
	if after-before != 7_000_000 {
		t.Fatalf("circulating delta = %d, want 7_000_000", after-before)
	}
}

func mustQueued(t *testing.T, st *Store, ctx context.Context) []QueuedWithdrawal {
	t.Helper()
	q, err := st.ListQueuedWithdrawals(ctx, 0)
	if err != nil {
		t.Fatalf("ListQueuedWithdrawals: %v", err)
	}
	return q
}

func mustSubmitted(t *testing.T, st *Store, ctx context.Context) []QueuedWithdrawal {
	t.Helper()
	s, err := st.ListSubmittedWithdrawals(ctx)
	if err != nil {
		t.Fatalf("ListSubmittedWithdrawals: %v", err)
	}
	return s
}
