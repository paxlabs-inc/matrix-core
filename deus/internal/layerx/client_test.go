package layerx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/pkg/lxtest"
)

// newHarness spins the REAL layerxd handler over the real Postgres store
// (skipped without LAYERX_TEST_POSTGRES_URI) and returns it with an httptest
// server in front.
func newHarness(t *testing.T) (*lxtest.Harness, *httptest.Server, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping layerx client integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	h, err := lxtest.New(ctx, lxtest.Config{
		PostgresURI:   uri,
		MigrationsDir: filepath.Join("..", "..", "..", "layerx", "migrations"),
	})
	if err != nil {
		t.Fatalf("lxtest.New: %v", err)
	}
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h.Handler)
	t.Cleanup(srv.Close)
	return h, srv, ctx
}

func newPayer(t *testing.T) (string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	did := fmt.Sprintf("did:matrix:payer-%d:%s", time.Now().UnixNano(), hex.EncodeToString(pub)[:16])
	return did, pub, priv
}

// TestClientRoundTrips drives every client seam against the real layerxd
// handler: challenge prefetch, payer-signed pay with ref, payer-signed hold,
// captor capture/release under the gateway's own DID (principal-token lane),
// receipt + account reads, and typed error mapping.
func TestClientRoundTrips(t *testing.T) {
	h, srv, ctx := newHarness(t)

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{BaseURL: srv.URL, KeyHex: hex.EncodeToString(seed)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.DID() == "" {
		t.Fatal("gateway DID not derived")
	}

	payer, payerPub, payerPriv := newPayer(t)
	payee, _, _ := newPayer(t)
	if err := h.CreditDeposit(ctx, payer, "0xabc", "0xdep-"+payer, 10_000_000); err != nil {
		t.Fatalf("fund payer: %v", err)
	}

	// Challenge prefetch (the 402 nonce lane).
	ch, err := c.Challenge(ctx, payer)
	if err != nil || ch.Nonce == "" {
		t.Fatalf("Challenge: %v (%+v)", err, ch)
	}

	// Payer-signed pay with ref, submitted by the gateway as transporter.
	ref := "0x4444444444444444444444444444444444444444444444444444444444444444"
	pre := lxtest.IntentMessage("pay", payer, ch.Nonce, payee, "1.500000", ref)
	receipt, err := c.SubmitPay(ctx, PayIntent{
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      ch.Nonce,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte(pre))),
		ToDID:      payee,
		AmountUSDX: "1.500000",
		Ref:        ref,
	})
	if err != nil {
		t.Fatalf("SubmitPay: %v", err)
	}
	if receipt.Seq <= 0 || receipt.Ref != ref {
		t.Fatalf("bad pay receipt: %+v", receipt)
	}

	// Public receipt read round-trips the ref.
	got, err := c.Receipt(ctx, receipt.Seq)
	if err != nil || got.Ref != ref {
		t.Fatalf("Receipt: %v (%+v)", err, got)
	}

	// Payer-signed hold naming the GATEWAY as captor.
	ch2, err := c.Challenge(ctx, payer)
	if err != nil {
		t.Fatalf("Challenge 2: %v", err)
	}
	pre = lxtest.IntentMessage("hold", payer, ch2.Nonce, payee, "3.000000", "60", ref, c.DID())
	hold, err := c.SubmitHold(ctx, HoldIntent{
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      ch2.Nonce,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte(pre))),
		ToDID:      payee,
		AmountUSDX: "3.000000",
		CaptorDID:  c.DID(),
		TTLSeconds: 60,
		Ref:        ref,
	})
	if err != nil {
		t.Fatalf("SubmitHold: %v", err)
	}
	if hold.Status != "open" || hold.CaptorDID != c.DID() {
		t.Fatalf("bad hold: %+v", hold)
	}

	// Public hold read.
	if hv, err := c.GetHold(ctx, hold.HoldID); err != nil || hv.HoldID != hold.HoldID {
		t.Fatalf("GetHold: %v (%+v)", err, hv)
	}

	// Captor capture under the gateway DID (challenge/verify token lane).
	capRes, err := c.Capture(ctx, hold.HoldID, "2.000000")
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if capRes.Receipt.Seq <= 0 || capRes.Receipt.Ref != ref || capRes.Hold.Status != "captured" {
		t.Fatalf("bad capture: %+v", capRes)
	}

	// Release of a captured hold maps to ErrConflict.
	if _, err := c.Release(ctx, hold.HoldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("Release captured = %v, want ErrConflict", err)
	}

	// A fresh hold releases cleanly as the captor.
	ch3, _ := c.Challenge(ctx, payer)
	pre = lxtest.IntentMessage("hold", payer, ch3.Nonce, payee, "1.000000", "60", "", c.DID())
	hold2, err := c.SubmitHold(ctx, HoldIntent{
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      ch3.Nonce,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte(pre))),
		ToDID:      payee,
		AmountUSDX: "1.000000",
		CaptorDID:  c.DID(),
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("SubmitHold 2: %v", err)
	}
	if rel, err := c.Release(ctx, hold2.HoldID); err != nil || rel.Status != "released" {
		t.Fatalf("Release: %v (%+v)", err, rel)
	}

	// Account read reflects the payee's captured + paid funds.
	acct, err := c.Account(ctx, payee)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acct.BalanceUSDX != "3.500000" { // 1.5 pay + 2.0 capture
		t.Fatalf("payee balance = %q, want 3.500000", acct.BalanceUSDX)
	}

	// Typed error mapping: underfunded pay -> ErrInsufficientFunds.
	ch4, _ := c.Challenge(ctx, payer)
	pre = lxtest.IntentMessage("pay", payer, ch4.Nonce, payee, "1000.000000")
	_, err = c.SubmitPay(ctx, PayIntent{
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      ch4.Nonce,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte(pre))),
		ToDID:      payee,
		AmountUSDX: "1000.000000",
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("underfunded pay = %v, want ErrInsufficientFunds", err)
	}

	// Bad signature -> ErrUnauthorized (and unknown hold -> ErrNotFound).
	ch5, _ := c.Challenge(ctx, payer)
	_, err = c.SubmitPay(ctx, PayIntent{
		FromDID:    payer,
		PublicKey:  hex.EncodeToString(payerPub),
		Nonce:      ch5.Nonce,
		Signature:  hex.EncodeToString(ed25519.Sign(payerPriv, []byte("wrong bytes"))),
		ToDID:      payee,
		AmountUSDX: "0.100000",
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad-signature pay = %v, want ErrUnauthorized", err)
	}
	if _, err := c.GetHold(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hold = %v, want ErrNotFound", err)
	}

	// Transport failure -> ErrUnavailable (rail down must never mean free call).
	down, _ := New(Config{BaseURL: "http://127.0.0.1:1"})
	if _, err := down.Account(ctx, payee); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("down rail = %v, want ErrUnavailable", err)
	}
}
