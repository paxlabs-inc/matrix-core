package lxp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/pkg/lxtest"
)

func newLayerxd(t *testing.T) (*lxtest.Harness, *httptest.Server, context.Context) {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping lxp integration test")
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

func newServer(t *testing.T, layerxURL string) *Server {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{LayerXURL: layerxURL, KeyHex: hex.EncodeToString(seed), DIDLabel: "lxp-test-svc"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

type testPayer struct {
	did  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestPayer(t *testing.T) testPayer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testPayer{
		did:  fmt.Sprintf("did:matrix:lxp-payer-%d:%s", time.Now().UnixNano(), hex.EncodeToString(pub)[:16]),
		pub:  pub,
		priv: priv,
	}
}

// signPayment turns terms into a signed X-LayerX-Payment header value — the
// exact handshake deus.mjs performs.
func (tp testPayer) signPayment(t *testing.T, terms Terms) string {
	t.Helper()
	p := Payment{
		FromDID:    tp.did,
		PublicKey:  hex.EncodeToString(tp.pub),
		Nonce:      terms.Nonce,
		ToDID:      terms.PayTo,
		AmountUSDX: terms.AmountUSDX,
		Mode:       terms.Mode,
		Ref:        terms.Ref,
	}
	var preimage string
	switch terms.Mode {
	case ModeExact:
		preimage = PayPreimage(p)
	case ModeHold:
		preimage = HoldPreimage(p, terms.TTLSeconds, terms.CaptorDID)
	default:
		t.Fatalf("unknown terms mode %q", terms.Mode)
	}
	p.Signature = hex.EncodeToString(ed25519.Sign(tp.priv, []byte(preimage)))
	return EncodePayment(p)
}

func decodeChallenge(t *testing.T, resp *http.Response) (string, Terms) {
	t.Helper()
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		LXP    *Terms `json:"lxp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode challenge body: %v", err)
	}
	if body.LXP == nil {
		return body.Reason, Terms{}
	}
	return body.Reason, *body.LXP
}

// TestMiddlewareExactMode drives the full lxp/1 wire flow in exact mode
// against the REAL layerxd handler: 402 challenge shape, signed retry, 200 +
// X-LayerX-Receipt, and fresh-402 behaviors for replayed and tampered
// payments.
func TestMiddlewareExactMode(t *testing.T) {
	h, lxd, ctx := newLayerxd(t)
	s := newServer(t, lxd.URL)
	payer := newTestPayer(t)
	payee, _, _ := newTestPayer(t).did, "", ""
	if err := h.CreditDeposit(ctx, payer.did, "0xabc", "0xdep-"+payer.did, 5_000_000); err != nil {
		t.Fatalf("fund payer: %v", err)
	}

	var executions int64
	ref := "0x5555555555555555555555555555555555555555555555555555555555555555"
	price := Price{AmountUSDX: "0.031500", PayTo: payee, Mode: ModeExact, Ref: ref}
	app := s.Middleware(func(*http.Request) (Price, bool, error) { return price, true, nil })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&executions, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":"served"}`))
		}))
	svc := httptest.NewServer(app)
	defer svc.Close()

	// 1. Unpaid request without a caller DID -> 402 identify_payer, no terms.
	resp, err := http.Get(svc.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("bare request = %d, want 402", resp.StatusCode)
	}
	reason, _ := decodeChallenge(t, resp)
	resp.Body.Close()
	if reason != ReasonIdentifyPayer {
		t.Fatalf("bare reason = %q, want identify_payer", reason)
	}

	// 2. Unpaid request with X-Caller-DID -> 402 with full lxp/1 terms.
	req, _ := http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderCallerDID, payer.did)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("challenge status = %d, want 402", resp.StatusCode)
	}
	reason, terms := decodeChallenge(t, resp)
	resp.Body.Close()
	if reason != ReasonPaymentRequired {
		t.Fatalf("challenge reason = %q", reason)
	}
	if terms.Protocol != Protocol || terms.Asset != "USDX" || terms.AmountUSDX != "0.031500" ||
		terms.PayTo != payee || terms.Mode != ModeExact || terms.Nonce == "" ||
		terms.Ref != ref || terms.LayerX != lxd.URL || terms.ExpiresAt.IsZero() {
		t.Fatalf("bad terms: %+v", terms)
	}

	// 3. Signed retry -> 200 + X-LayerX-Receipt; the resource executed once.
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, payer.signPayment(t, terms))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid request = %d", resp.StatusCode)
	}
	rcpt, err := DecodeReceipt(resp.Header.Get(HeaderReceipt))
	resp.Body.Close()
	if err != nil || rcpt.Seq <= 0 || rcpt.AmountUSDX != "0.031500" || rcpt.Ref != ref ||
		rcpt.LeafHash == "" || rcpt.SequencerSig == "" {
		t.Fatalf("bad receipt header: %+v (%v)", rcpt, err)
	}
	if n := atomic.LoadInt64(&executions); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	// The receipt is publicly verifiable at layerxd.
	pub, err := s.Client().Receipt(ctx, rcpt.Seq)
	if err != nil || pub.Ref != ref {
		t.Fatalf("public receipt: %v (%+v)", err, pub)
	}

	// 4. REPLAY of the same payment (consumed nonce) -> fresh 402 with a NEW
	//    nonce and a machine-readable reason; no execution.
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, payer.signPayment(t, terms)) // same nonce, re-signed
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("replayed payment = %d, want 402", resp.StatusCode)
	}
	reason, fresh := decodeChallenge(t, resp)
	resp.Body.Close()
	if reason != ReasonPaymentRejected {
		t.Fatalf("replay reason = %q, want payment_rejected", reason)
	}
	if fresh.Nonce == "" || fresh.Nonce == terms.Nonce {
		t.Fatalf("replay challenge did not carry a fresh nonce")
	}
	if n := atomic.LoadInt64(&executions); n != 1 {
		t.Fatalf("executions after replay = %d, want 1 (never execution)", n)
	}

	// 5. Tampered amount (signed 0.0315, submitted 0.02) -> 402 terms_mismatch.
	pay, _ := ParsePayment(payer.signPayment(t, fresh))
	pay.AmountUSDX = "0.020000"
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, EncodePayment(pay))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reason, _ = decodeChallenge(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || reason != ReasonTermsMismatch {
		t.Fatalf("tampered amount = %d/%q, want 402/terms_mismatch", resp.StatusCode, reason)
	}

	// 6. Garbage signature -> 402 invalid_signature, no execution.
	pay, _ = ParsePayment(payer.signPayment(t, fresh))
	pay.Signature = hex.EncodeToString(ed25519.Sign(payer.priv, []byte("garbage")))
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, EncodePayment(pay))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reason, _ = decodeChallenge(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired || reason != ReasonInvalidSignature {
		t.Fatalf("bad signature = %d/%q, want 402/invalid_signature", resp.StatusCode, reason)
	}
	if n := atomic.LoadInt64(&executions); n != 1 {
		t.Fatalf("executions after invalid attempts = %d, want 1", n)
	}
}

// TestMiddlewareHoldMode proves the hold pipeline: reserve -> execute ->
// capture on success (receipt on the response), release on handler failure
// (no charge, no stranded funds).
func TestMiddlewareHoldMode(t *testing.T) {
	h, lxd, ctx := newLayerxd(t)
	s := newServer(t, lxd.URL)
	payer := newTestPayer(t)
	payee := newTestPayer(t).did
	if err := h.CreditDeposit(ctx, payer.did, "0xabc", "0xdep-"+payer.did, 5_000_000); err != nil {
		t.Fatalf("fund payer: %v", err)
	}

	var fail atomic.Bool
	price := Price{AmountUSDX: "1.000000", PayTo: payee, Mode: ModeHold, TTLSeconds: 60}
	app := s.Middleware(func(*http.Request) (Price, bool, error) { return price, true, nil })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("served"))
		}))
	svc := httptest.NewServer(app)
	defer svc.Close()

	challenge := func() Terms {
		req, _ := http.NewRequest(http.MethodGet, svc.URL, nil)
		req.Header.Set(HeaderCallerDID, payer.did)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("challenge status = %d", resp.StatusCode)
		}
		_, terms := decodeChallenge(t, resp)
		if terms.Mode != ModeHold || terms.CaptorDID != s.DID() || terms.TTLSeconds != 60 {
			t.Fatalf("bad hold terms: %+v", terms)
		}
		return terms
	}

	// Success path: hold -> execute -> capture; payee credited, receipt attached.
	terms := challenge()
	req, _ := http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, payer.signPayment(t, terms))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid hold request = %d", resp.StatusCode)
	}
	rcpt, err := DecodeReceipt(resp.Header.Get(HeaderReceipt))
	resp.Body.Close()
	if err != nil || rcpt.Seq <= 0 || rcpt.AmountUSDX != "1.000000" {
		t.Fatalf("bad hold receipt: %+v (%v)", rcpt, err)
	}
	if bal, _ := h.BalanceMicro(ctx, payee); bal != 1_000_000 {
		t.Fatalf("payee balance = %d, want 1_000_000", bal)
	}

	// Failure path: hold -> execute fails -> release; NO charge, payer whole.
	fail.Store(true)
	terms = challenge()
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, payer.signPayment(t, terms))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed execution = %d, want 500 passthrough", resp.StatusCode)
	}
	if resp.Header.Get(HeaderReceipt) != "" {
		t.Fatal("failed execution carried a receipt")
	}
	resp.Body.Close()
	if bal, _ := h.BalanceMicro(ctx, payee); bal != 1_000_000 {
		t.Fatalf("payee balance after failure = %d, want 1_000_000 (no second charge)", bal)
	}
	if bal, _ := h.BalanceMicro(ctx, payer.did); bal != 4_000_000 {
		t.Fatalf("payer balance after release = %d, want 4_000_000 (5 - 1 captured)", bal)
	}
}

// TestMiddlewareLayerxDown proves no-free-calls: with layerxd unreachable
// every priced request is 503 payment_unavailable and the resource records
// ZERO executions.
func TestMiddlewareLayerxDown(t *testing.T) {
	payer := newTestPayer(t)
	payee := newTestPayer(t).did
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{LayerXURL: "http://127.0.0.1:1", KeyHex: hex.EncodeToString(seed)})
	if err != nil {
		t.Fatal(err)
	}

	var executions int64
	price := Price{AmountUSDX: "0.100000", PayTo: payee, Mode: ModeExact}
	app := s.Middleware(func(*http.Request) (Price, bool, error) { return price, true, nil })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&executions, 1)
		}))
	svc := httptest.NewServer(app)
	defer svc.Close()

	// Challenge path is down -> 503.
	req, _ := http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderCallerDID, payer.did)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("challenge with rail down = %d, want 503", resp.StatusCode)
	}

	// A syntactically-valid signed payment cannot settle -> 503, no execution.
	p := Payment{
		FromDID:    payer.did,
		PublicKey:  hex.EncodeToString(payer.pub),
		Nonce:      "stale-nonce",
		ToDID:      payee,
		AmountUSDX: "0.100000",
		Mode:       ModeExact,
	}
	p.Signature = hex.EncodeToString(ed25519.Sign(payer.priv, []byte(PayPreimage(p))))
	req, _ = http.NewRequest(http.MethodGet, svc.URL, nil)
	req.Header.Set(HeaderPayment, EncodePayment(p))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("settle with rail down = %d, want 503", resp.StatusCode)
	}
	if n := atomic.LoadInt64(&executions); n != 0 {
		t.Fatalf("executions with rail down = %d, want 0", n)
	}
}

// TestFreeRoutePassthrough proves unpriced routes bypass the paywall entirely.
func TestFreeRoutePassthrough(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{LayerXURL: "http://127.0.0.1:1", KeyHex: hex.EncodeToString(seed)})
	if err != nil {
		t.Fatal(err)
	}
	app := s.Middleware(func(*http.Request) (Price, bool, error) { return Price{}, false, nil })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("free route = %d, want passthrough 418", rr.Code)
	}
}
