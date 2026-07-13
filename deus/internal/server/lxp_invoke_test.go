package server

import (
	"bytes"
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

	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/pkg/lxp"
	"github.com/paxlabs-inc/layerx/pkg/lxtest"
	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

const lxpTestSignerKey = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

// lxpTestRig assembles the REAL stack end to end: deus store + gateway +
// HTTP handler over a real Postgres, the real layerxd handler over its own
// real Postgres, and a live proxy backend counting executions. No fakes.
type lxpTestRig struct {
	ts         *httptest.Server // deus HTTP surface
	lxd        *httptest.Server // layerxd HTTP surface
	harness    *lxtest.Harness
	db         *store.Store
	serviceID  string
	payeeDID   string
	devWallet  string
	gatewayDID string
	executions *int64
}

// lxpRigOpts shapes the registered service: proxy (default) or hosted mode,
// exact (default) or hold settlement, an optional operation timeout, and an
// optional backend handler override (the default backend answers a weather
// JSON; hosted rigs speak the runner protocol through it either way).
type lxpRigOpts struct {
	hosted         bool
	settlementMode string
	holdTTLS       int64
	timeoutMS      int
	backend        http.HandlerFunc
}

func newLXPRig(t *testing.T, opts lxpRigOpts) (*lxpTestRig, context.Context) {
	t.Helper()
	lxURI := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if lxURI == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping LXP rail integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	deusURI := os.Getenv("DEUS_POSTGRES_URI")
	if deusURI == "" {
		deusURI = "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable"
	}
	db, err := store.New(ctx, deusURI)
	if err != nil {
		t.Skipf("deus postgres unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("deus migrate: %v", err)
	}

	harness, err := lxtest.New(ctx, lxtest.Config{
		PostgresURI:   lxURI,
		MigrationsDir: filepath.Join("..", "..", "..", "layerx", "migrations"),
	})
	if err != nil {
		t.Fatalf("lxtest: %v", err)
	}
	t.Cleanup(harness.Close)
	lxd := httptest.NewServer(harness.Handler)
	t.Cleanup(lxd.Close)

	var executions int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&executions, 1)
		if opts.backend != nil {
			opts.backend(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tempC": 14.2, "summary": "Partly cloudy"})
	}))
	t.Cleanup(backend.Close)

	// Register the service directly through the store: USDX-only pricing,
	// payee_did set — the LXP-native listing shape.
	svcMode := "proxy"
	if opts.hosted {
		svcMode = "hosted"
	}
	payeePub, _, _ := ed25519.GenerateKey(rand.Reader)
	payeeDID := fmt.Sprintf("did:matrix:lxp-payee-%d:%s", time.Now().UnixNano(), hex.EncodeToString(payeePub)[:16])
	walletBytes := make([]byte, 20)
	if _, err := rand.Read(walletBytes); err != nil {
		t.Fatal(err)
	}
	devWallet := "0x" + hex.EncodeToString(walletBytes)
	slug := fmt.Sprintf("lxp-weather-%d", time.Now().UnixNano())
	operation := map[string]any{
		"name": "forecast", "method": "POST",
		"input_schema": map[string]any{"type": "object"}, "output_schema": map[string]any{"type": "object"},
	}
	if opts.timeoutMS > 0 {
		operation["timeout_ms"] = opts.timeoutMS
	}
	manifest := map[string]any{
		"schema_version": "1",
		"slug":           slug,
		"kind":           "data",
		"display_name":   "LXP Weather",
		"summary":        "lxp rail test",
		"owner":          devWallet,
		"payout_address": devWallet,
		"payee_did":      payeeDID,
		"mode":           svcMode,
		"operations":     []map[string]any{operation},
		"pricing": []map[string]any{{
			"operation": "forecast", "model": "per_call", "unit": "call",
			"unit_price_usdx": "0.031500", "min_charge_usdx": "0.031500",
		}},
	}
	if opts.settlementMode != "" {
		manifest["settlement_mode"] = opts.settlementMode
		if opts.holdTTLS > 0 {
			manifest["hold_ttl_s"] = opts.holdTTLS
		}
	}
	if !opts.hosted {
		manifest["endpoint"] = map[string]any{"proxy_url": backend.URL}
	}
	manifestRaw, _ := json.Marshal(manifest)
	devID, err := db.UpsertDeveloperByWallet(ctx, devWallet, devWallet, "lxp-dev")
	if err != nil {
		t.Fatalf("developer: %v", err)
	}
	if err := db.SetDeveloperPayeeDID(ctx, devID, payeeDID); err != nil {
		t.Fatalf("payee did: %v", err)
	}
	svcID, err := db.InsertDraftService(ctx, store.ServiceRow{
		DeveloperID: devID, Slug: slug, Kind: "data", Mode: svcMode,
		DisplayName: "LXP Weather", Summary: "lxp rail test",
		Manifest: manifestRaw, ManifestHash: "0xdead", Status: "active",
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	endpoint := store.EndpointRow{
		Operation: "forecast", Method: "POST",
		InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`),
	}
	if !opts.hosted {
		proxyURL := backend.URL
		endpoint.ProxyURL = &proxyURL
	}
	if err := db.InsertEndpoints(ctx, svcID, []store.EndpointRow{endpoint}); err != nil {
		t.Fatalf("endpoints: %v", err)
	}
	if err := db.InsertPricingPlans(ctx, svcID, []store.PricingRow{{
		Model: "per_call", Unit: "call", PriceUSDX: "0.031500", MinChargeUSDX: "0.031500", Version: 1,
	}}); err != nil {
		t.Fatalf("pricing: %v", err)
	}

	var hostingRouter gateway.HostingRouter
	if opts.hosted {
		execURL := backend.URL
		if _, err := db.InsertDeployment(ctx, store.DeploymentRow{
			ServiceID: svcID, Runtime: "node-22", ExecEndpoint: &execURL, Status: "active",
		}); err != nil {
			t.Fatalf("deployment: %v", err)
		}
		hostingRouter = hosting.NewOrchestrator(db, nil, nil, hosting.Limits{})
	}

	signer, err := receipts.NewSignerFromHex(31337, "0x0000000000000000000000000000000000000001", lxpTestSignerKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	lxpSrv, err := lxp.New(lxp.Config{LayerXURL: lxd.URL, KeyHex: hex.EncodeToString(seed)})
	if err != nil {
		t.Fatalf("lxp: %v", err)
	}
	gw := gateway.New(gateway.Config{
		Store:   db,
		Pricing: pricing.New(db),
		Meter:   metering.New(db),
		Signer:  signer,
		Quality: quality.New(db),
		Hosting: hostingRouter,
		ChainID: 31337,
		LXP:     lxpSrv,
	})
	srv := New(Deps{Log: telemetry.NewLogger(), Store: db, Gateway: gw, DevMode: true})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &lxpTestRig{
		ts: ts, lxd: lxd, harness: harness, db: db,
		serviceID: svcID, payeeDID: payeeDID, devWallet: devWallet,
		gatewayDID: lxpSrv.DID(), executions: &executions,
	}, ctx
}

type lxpCaller struct {
	did  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newLXPCaller(t *testing.T) lxpCaller {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return lxpCaller{
		did:  fmt.Sprintf("did:matrix:lxp-caller-%d:%s", time.Now().UnixNano(), hex.EncodeToString(pub)[:16]),
		pub:  pub,
		priv: priv,
	}
}

func (c lxpCaller) invoke(t *testing.T, rig *lxpTestRig, idem, paymentHeader string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"operation":       "forecast",
		"args":            map[string]any{"city": "berlin"},
		"idempotency_key": idem,
	})
	req, _ := http.NewRequest(http.MethodPost, rig.ts.URL+"/v1/invoke/"+rig.serviceID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-agent")
	req.Header.Set("X-Caller-DID", c.did)
	if paymentHeader != "" {
		req.Header.Set(lxp.HeaderPayment, paymentHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (c lxpCaller) sign(t *testing.T, terms lxp.Terms) string {
	t.Helper()
	p := lxp.Payment{
		FromDID:    c.did,
		PublicKey:  hex.EncodeToString(c.pub),
		Nonce:      terms.Nonce,
		ToDID:      terms.PayTo,
		AmountUSDX: terms.AmountUSDX,
		Mode:       terms.Mode,
		Ref:        terms.Ref,
	}
	preimage := lxp.PayPreimage(p)
	if terms.Mode == lxp.ModeHold {
		preimage = lxp.HoldPreimage(p, terms.TTLSeconds, terms.CaptorDID)
	}
	p.Signature = hex.EncodeToString(ed25519.Sign(c.priv, []byte(preimage)))
	return lxp.EncodePayment(p)
}

// getHold reads the public hold view straight off the layerxd test server.
func getHold(t *testing.T, rig *lxpTestRig, id string) lxtypes.HoldView {
	t.Helper()
	resp, err := http.Get(rig.lxd.URL + "/v1/hold/" + id)
	if err != nil {
		t.Fatalf("get hold: %v", err)
	}
	defer resp.Body.Close()
	var env struct {
		Ok   bool             `json:"ok"`
		Data lxtypes.HoldView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || !env.Ok {
		t.Fatalf("hold view (%d): ok=%v err=%v", resp.StatusCode, env.Ok, err)
	}
	return env.Data
}

func decodeTerms(t *testing.T, resp *http.Response) (string, lxp.Terms) {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Error  string     `json:"error"`
		Reason string     `json:"reason"`
		LXP    *lxp.Terms `json:"lxp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode terms: %v", err)
	}
	if body.LXP == nil {
		t.Fatalf("402 carried no lxp terms (reason=%q)", body.Reason)
	}
	return body.Reason, *body.LXP
}

// TestGatewayLXPRailExactMode drives the full invoke over the LayerX rail:
// 402 challenge (equivalent terms, quote optional) -> signed retry -> execute
// -> settle -> 200 + X-LayerX-Receipt with cross-bound execution receipt, then
// idempotent replay without a second charge, and no-free-calls when layerxd
// dies.
func TestGatewayLXPRailExactMode(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	// 1. Unpaid invoke -> 402 with lxp/1 terms carrying the USDX price, the
	//    payee DID, a prefetched nonce, and the invocation-binding ref.
	idem := fmt.Sprintf("lxp-idem-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid invoke = %d, want 402", resp.StatusCode)
	}
	_, terms := decodeTerms(t, resp)
	if terms.Protocol != lxp.Protocol || terms.AmountUSDX != "0.031500" || terms.Mode != lxp.ModeExact ||
		terms.Nonce == "" || terms.Ref == "" || terms.LayerX != rig.lxd.URL {
		t.Fatalf("bad challenge terms: %+v", terms)
	}
	if n := atomic.LoadInt64(rig.executions); n != 0 {
		t.Fatalf("executions after challenge = %d, want 0", n)
	}

	// 2. Signed retry -> 200 + receipt header + cross-bound response fields.
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusOK {
		raw, _ := json.Marshal(resp.Header)
		t.Fatalf("paid invoke = %d (headers %s)", resp.StatusCode, raw)
	}
	rcptHdr, err := lxp.DecodeReceipt(resp.Header.Get(lxp.HeaderReceipt))
	if err != nil || rcptHdr.Seq <= 0 || rcptHdr.AmountUSDX != "0.031500" || rcptHdr.Ref != terms.Ref {
		t.Fatalf("bad receipt header: %+v (%v)", rcptHdr, err)
	}
	var inv struct {
		InvocationID string         `json:"invocation_id"`
		Outcome      string         `json:"outcome"`
		Result       map[string]any `json:"result"`
		ChargedUSDX  string         `json:"charged_usdx"`
		LayerXSeq    int64          `json:"layerx_seq"`
		Ref          string         `json:"ref"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatalf("decode invoke: %v", err)
	}
	resp.Body.Close()
	if inv.Outcome != "ok" || inv.ChargedUSDX != "0.031500" || inv.LayerXSeq != rcptHdr.Seq ||
		inv.Ref != terms.Ref || inv.Result["summary"] != "Partly cloudy" {
		t.Fatalf("bad invoke response: %+v", inv)
	}
	if n := atomic.LoadInt64(rig.executions); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	// The payment settled payer -> payee on LayerX; deus never touched it.
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 31_500 {
		t.Fatalf("payee balance = %d, want 31500 micro", bal)
	}
	// The metering row is cross-bound: rail layerx, layerx_seq, usdx charge.
	row, err := rig.db.GetInvocation(ctx, inv.InvocationID)
	if err != nil || row.Rail != "layerx" || row.LayerXSeq != rcptHdr.Seq || row.PriceUSDX != "0.031500" {
		t.Fatalf("bad metering row: %+v (%v)", row, err)
	}

	// 3. Replay: same idempotency key (fresh signed payment) returns the stored
	//    result with exactly ONE charge and no second execution.
	resp = caller.invoke(t, rig, idem, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("replay challenge = %d", resp.StatusCode)
	}
	_, terms2 := decodeTerms(t, resp)
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replayed invoke = %d", resp.StatusCode)
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
		t.Fatalf("executions after replay = %d, want 1", n)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 31_500 {
		t.Fatalf("payee balance after replay = %d, want 31500 (exactly one charge)", bal)
	}

	// 4. Tampered payment (amount mismatch) -> fresh 402 terms_mismatch.
	resp = caller.invoke(t, rig, "lxp-idem-tamper-"+idem, "")
	_, terms3 := decodeTerms(t, resp)
	pay, _ := lxp.ParsePayment(caller.sign(t, terms3))
	pay.AmountUSDX = "0.000001"
	resp = caller.invoke(t, rig, "lxp-idem-tamper-"+idem, lxp.EncodePayment(pay))
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("tampered payment = %d, want 402", resp.StatusCode)
	}
	reason, _ := decodeTerms(t, resp)
	if reason != lxp.ReasonTermsMismatch {
		t.Fatalf("tampered reason = %q, want terms_mismatch", reason)
	}

	// 5. No free calls: kill layerxd; a signed payment cannot settle -> 503
	//    payment_unavailable, the row voids, and the result is never served.
	idemDown := fmt.Sprintf("lxp-idem-down-%d", time.Now().UnixNano())
	resp = caller.invoke(t, rig, idemDown, "")
	_, termsDown := decodeTerms(t, resp)
	rig.lxd.Close()
	resp = caller.invoke(t, rig, idemDown, caller.sign(t, termsDown))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down invoke = %d, want 503", resp.StatusCode)
	}
	var down map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&down)
	resp.Body.Close()
	if down["error"] != "payment_unavailable" {
		t.Fatalf("rail-down body = %v, want payment_unavailable", down)
	}
	row, err = rig.db.GetInvocationByIdempotency(ctx, idemDown)
	if err != nil || row.Outcome != "voided" {
		t.Fatalf("rail-down row = %+v (%v), want voided", row, err)
	}
	// And a fully-unpaid request with the rail down is also 503 — never free.
	resp = caller.invoke(t, rig, "lxp-idem-down2-"+idemDown, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down challenge = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestGatewayLXPRailHoldMode drives the hold-mode pipeline on a real stack:
// 402 terms carry captor_did + ttl_s, the signed hold locks funds in the
// payer's own account, execution runs, the capture emits the standard transfer
// crediting the payee, and the metering row cross-binds seq + hold_id. Replay
// returns the stored result with exactly one charge.
func TestGatewayLXPRailHoldMode(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{settlementMode: "hold", holdTTLS: 60})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	idem := fmt.Sprintf("lxp-hold-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid invoke = %d, want 402", resp.StatusCode)
	}
	_, terms := decodeTerms(t, resp)
	if terms.Mode != lxp.ModeHold || terms.CaptorDID == "" || terms.TTLSeconds != 60 ||
		terms.AmountUSDX != "0.031500" || terms.Nonce == "" || terms.Ref == "" {
		t.Fatalf("bad hold challenge terms: %+v", terms)
	}

	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("paid hold invoke = %d (%v)", resp.StatusCode, body)
	}
	rcptHdr, err := lxp.DecodeReceipt(resp.Header.Get(lxp.HeaderReceipt))
	if err != nil || rcptHdr.Seq <= 0 || rcptHdr.AmountUSDX != "0.031500" || rcptHdr.Ref != terms.Ref {
		t.Fatalf("bad receipt header: %+v (%v)", rcptHdr, err)
	}
	var inv struct {
		InvocationID string         `json:"invocation_id"`
		Outcome      string         `json:"outcome"`
		Result       map[string]any `json:"result"`
		ChargedUSDX  string         `json:"charged_usdx"`
		LayerXSeq    int64          `json:"layerx_seq"`
		Ref          string         `json:"ref"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatalf("decode invoke: %v", err)
	}
	resp.Body.Close()
	if inv.Outcome != "ok" || inv.ChargedUSDX != "0.031500" || inv.LayerXSeq != rcptHdr.Seq || inv.Ref != terms.Ref {
		t.Fatalf("bad invoke response: %+v", inv)
	}
	if n := atomic.LoadInt64(rig.executions); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 31_500 {
		t.Fatalf("payee balance = %d, want 31500 micro", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, caller.did); bal != 1_000_000-31_500 {
		t.Fatalf("payer balance = %d, want %d", bal, 1_000_000-31_500)
	}
	row, err := rig.db.GetInvocation(ctx, inv.InvocationID)
	if err != nil || row.Rail != "layerx" || row.LayerXSeq != rcptHdr.Seq || row.HoldID == "" {
		t.Fatalf("bad metering row: %+v (%v)", row, err)
	}
	hold := getHold(t, rig, row.HoldID)
	if hold.Status != "captured" || hold.CaptureSeq != rcptHdr.Seq || hold.CaptorDID != terms.CaptorDID ||
		hold.PayerDID != caller.did || hold.PayeeDID != terms.PayTo || hold.Ref != terms.Ref {
		t.Fatalf("bad hold view: %+v", hold)
	}

	// Replay: stored result, no second hold or charge.
	resp = caller.invoke(t, rig, idem, "")
	_, terms2 := decodeTerms(t, resp)
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replayed hold invoke = %d", resp.StatusCode)
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
		t.Fatalf("executions after replay = %d, want 1", n)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 31_500 {
		t.Fatalf("payee balance after replay = %d, want 31500 (exactly one charge)", bal)
	}
}

// TestGatewayLXPRailHoldModeExecutionFailure proves fail-open-refund: the
// backend blows up after the hold locks funds, the gateway releases the full
// amount back to the payer, voids the row, and the payee earns nothing.
func TestGatewayLXPRailHoldModeExecutionFailure(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{
		settlementMode: "hold", holdTTLS: 60,
		backend: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 500_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	idem := fmt.Sprintf("lxp-holdfail-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	_, terms := decodeTerms(t, resp)
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed-execution invoke = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	if n := atomic.LoadInt64(rig.executions); n != 1 {
		t.Fatalf("executions = %d, want 1 (the failed attempt)", n)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, caller.did); bal != 500_000 {
		t.Fatalf("payer balance = %d, want 500000 (hold released in full)", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 0 {
		t.Fatalf("payee balance = %d, want 0 (no charge for failed execution)", bal)
	}
	row, err := rig.db.GetInvocationByIdempotency(ctx, idem)
	if err != nil || row.Outcome != "voided" {
		t.Fatalf("row = %+v (%v), want voided", row, err)
	}
}

// TestGatewayLXPRailHoldModeExpiry proves no-stranded-holds past the TTL: the
// backend outlives the 1s hold, the capture is rejected at the ledger
// (expired), the gateway's release returns the funds, and the payer answers a
// fresh 402 — never a silent charge.
func TestGatewayLXPRailHoldModeExpiry(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{
		settlementMode: "hold", holdTTLS: 1, timeoutMS: 5000,
		backend: func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1400 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{"late": true})
		},
	})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 500_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	idem := fmt.Sprintf("lxp-holdexp-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	_, terms := decodeTerms(t, resp)
	if terms.TTLSeconds != 1 {
		t.Fatalf("ttl_s = %d, want 1", terms.TTLSeconds)
	}
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expired-capture invoke = %d, want 402", resp.StatusCode)
	}
	resp.Body.Close()
	if bal, _ := rig.harness.BalanceMicro(ctx, caller.did); bal != 500_000 {
		t.Fatalf("payer balance = %d, want 500000 (expired hold released)", bal)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 0 {
		t.Fatalf("payee balance = %d, want 0", bal)
	}
	row, err := rig.db.GetInvocationByIdempotency(ctx, idem)
	if err != nil || row.Outcome != "voided" {
		t.Fatalf("row = %+v (%v), want voided", row, err)
	}
}

// TestGatewayLXPRailHostedHoldMode proves the LXP rail serves HOSTED services:
// the runner protocol executes behind a hold, the capture credits the payee,
// and the runner's co-signature lands on the stored receipt.
func TestGatewayLXPRailHostedHoldMode(t *testing.T) {
	runnerSig := "0xrunner-cosign"
	rig, ctx := newLXPRig(t, lxpRigOpts{
		hosted: true, settlementMode: "hold", holdTTLS: 60,
		backend: func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				InvocationID  string         `json:"invocation_id"`
				Operation     string         `json:"operation"`
				Args          map[string]any `json:"args"`
				CallerDID     string         `json:"caller_did"`
				ReceiptDigest string         `json:"receipt_digest"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
				req.InvocationID == "" || req.Operation != "forecast" || req.ReceiptDigest == "" {
				http.Error(w, "bad runner request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outcome":    "ok",
				"result":     map[string]any{"echo": req.Args["city"], "hosted": true},
				"units":      "1",
				"runner_sig": runnerSig,
			})
		},
	})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	idem := fmt.Sprintf("lxp-hosted-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid hosted invoke = %d, want 402", resp.StatusCode)
	}
	_, terms := decodeTerms(t, resp)
	if terms.Mode != lxp.ModeHold || terms.CaptorDID == "" {
		t.Fatalf("bad hosted hold terms: %+v", terms)
	}
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("paid hosted invoke = %d (%v)", resp.StatusCode, body)
	}
	rcptHdr, err := lxp.DecodeReceipt(resp.Header.Get(lxp.HeaderReceipt))
	if err != nil || rcptHdr.Seq <= 0 {
		t.Fatalf("bad receipt header: %+v (%v)", rcptHdr, err)
	}
	var inv struct {
		InvocationID string         `json:"invocation_id"`
		Outcome      string         `json:"outcome"`
		Result       map[string]any `json:"result"`
		Receipt      struct {
			RunnerSig *string `json:"runner_sig"`
		} `json:"receipt"`
		LayerXSeq int64 `json:"layerx_seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatalf("decode invoke: %v", err)
	}
	resp.Body.Close()
	if inv.Outcome != "ok" || inv.Result["hosted"] != true || inv.Result["echo"] != "berlin" ||
		inv.LayerXSeq != rcptHdr.Seq || inv.Receipt.RunnerSig == nil || *inv.Receipt.RunnerSig != runnerSig {
		t.Fatalf("bad hosted invoke response: %+v", inv)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, terms.PayTo); bal != 31_500 {
		t.Fatalf("payee balance = %d, want 31500", bal)
	}
	row, err := rig.db.GetInvocation(ctx, inv.InvocationID)
	if err != nil || row.Rail != "layerx" || row.HoldID == "" || row.LayerXSeq != rcptHdr.Seq {
		t.Fatalf("bad metering row: %+v (%v)", row, err)
	}
	if hold := getHold(t, rig, row.HoldID); hold.Status != "captured" {
		t.Fatalf("hold status = %q, want captured", hold.Status)
	}
}

// TestLXPEarnings proves req.8.3: /v1/me/earnings joins deus layerx-rail
// invocation aggregates with LIVE LayerX reads (the payee DID's real account
// balance on real layerxd) and links withdrawal out to layerxd — deus holds no
// payout code and never sits in the money path.
func TestLXPEarnings(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{})
	caller := newLXPCaller(t)
	if err := rig.harness.CreditDeposit(ctx, caller.did, "0xabc", "0xdep-"+caller.did, 1_000_000); err != nil {
		t.Fatalf("fund caller: %v", err)
	}

	idem := fmt.Sprintf("lxp-earn-%d", time.Now().UnixNano())
	resp := caller.invoke(t, rig, idem, "")
	_, terms := decodeTerms(t, resp)
	resp = caller.invoke(t, rig, idem, caller.sign(t, terms))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid invoke = %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, rig.ts.URL+"/v1/me/earnings", nil)
	req.Header.Set("X-Developer-Wallet", rig.devWallet)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("earnings = %d", resp.StatusCode)
	}
	var earnings struct {
		LayerX *struct {
			PayeeDID    string `json:"payee_did"`
			EarnedUSDX  string `json:"earned_usdx"`
			Invocations int    `json:"invocations"`
			BalanceUSDX string `json:"balance_usdx"`
			LayerXURL   string `json:"layerx"`
			WithdrawURL string `json:"withdraw_url"`
		} `json:"layerx"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&earnings); err != nil {
		t.Fatalf("decode earnings: %v", err)
	}
	lx := earnings.LayerX
	if lx == nil {
		t.Fatal("earnings carried no layerx block")
	}
	if lx.PayeeDID != rig.payeeDID || lx.EarnedUSDX != "0.031500" || lx.Invocations != 1 {
		t.Fatalf("bad layerx earnings: %+v", lx)
	}
	// The balance is a LIVE layerxd account read of the payee DID.
	if lx.BalanceUSDX != "0.031500" {
		t.Fatalf("payee live balance = %q, want 0.031500", lx.BalanceUSDX)
	}
	if lx.LayerXURL != rig.lxd.URL || lx.WithdrawURL != rig.lxd.URL+"/v1/withdraw" {
		t.Fatalf("bad link-out: %+v", lx)
	}
}
