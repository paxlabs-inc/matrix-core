package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/engine"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
	"github.com/paxlabs-inc/layerx/internal/perps/mode"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// These tests drive the REAL perps HTTP surface over the real Postgres store,
// real engine, real auth, and real signer. No fakes: the offline subset stages
// ledger state through production store methods, and the live subset
// (CROSSVERSE_TEST_URL) exercises the full order path against the live feed.

type perpsHarness struct {
	srv        *Server
	st         *store.Store
	challenges *auth.Challenges
	tokens     *auth.Tokens
	eng        *engine.Engine
	ctx        context.Context
}

func newPerpsServer(t *testing.T, global mode.Mode, marketModes map[string]mode.Mode, feed PerpsFeed) *perpsHarness {
	t.Helper()
	uri := os.Getenv("LAYERX_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("LAYERX_TEST_POSTGRES_URI not set; skipping perps API integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	st, err := store.New(ctx, uri)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.SyncPerpMarkets(ctx, market.All()); err != nil {
		t.Fatalf("sync markets: %v", err)
	}
	signer, _, err := sig.New("")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	modes, err := mode.NewRegistry(global, marketModes)
	if err != nil {
		t.Fatalf("modes: %v", err)
	}
	var engFeed engine.Feed
	if f, ok := feed.(engine.Feed); ok {
		engFeed = f
	}
	eng := &engine.Engine{Store: st, Feed: engFeed, Modes: modes, Signer: signer,
		LiquidatorDID: "did:layerx:perps:liquidator"}
	challenges := auth.NewChallenges(time.Minute)
	tokens := auth.NewTokens("agent-secret", time.Hour)
	srv := New(Deps{
		Store:           st,
		Ledger:          ledger.New(st, signer, 1_000_000),
		Challenges:      challenges,
		Tokens:          tokens,
		ChainID:         125,
		SequencerPubHex: signer.PublicHex(),
		Perps:           &PerpsDeps{Engine: eng, Feed: feed, Modes: modes},
	})
	return &perpsHarness{srv: srv, st: st, challenges: challenges, tokens: tokens, eng: eng, ctx: ctx}
}

func perpsDID(t *testing.T, label string) (string, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	did := fmt.Sprintf("did:matrix:%s-%d:%s", label, time.Now().UnixNano(), hex.EncodeToString(pub)[:16])
	return did, pub, priv
}

// signedBody builds a DID-signed perps intent body: base fields + envelope.
func signedBody(t *testing.T, h *perpsHarness, priv ed25519.PrivateKey, pub ed25519.PublicKey,
	op, did string, fields []string, base map[string]any) (map[string]any, string) {
	t.Helper()
	nonce, _ := h.challenges.Create(did)
	preimage := auth.IntentMessage("perps."+op, did, nonce, fields...)
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(preimage)))
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["from_did"] = did
	body["public_key"] = hex.EncodeToString(pub)
	body["nonce"] = nonce
	body["signature"] = sig
	return body, nonce
}

func perpsDo(h http.Handler, method, path, agent string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if agent != "" {
		req.Header.Set("X-LayerX-Agent", agent)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func decodeData(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, rr.Body.String())
	}
	if !env.OK {
		t.Fatalf("expected ok response, got %s", rr.Body.String())
	}
	if v != nil {
		if err := json.Unmarshal(env.Data, v); err != nil {
			t.Fatalf("decode data: %v body=%s", err, rr.Body.String())
		}
	}
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, rr.Body.String())
	}
	return env.Error.Code
}

func activateMarket(t *testing.T, h *perpsHarness, symbol string) {
	t.Helper()
	row, err := h.st.GetPerpMarket(h.ctx, symbol)
	if err != nil {
		t.Fatal(err)
	}
	if row.Mode != "ACTIVE" {
		if _, err := h.st.SetPerpMarketMode(h.ctx, symbol, "ACTIVE", "", "did:op", row.Mode == "PAUSED"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPerpsPublicMarketsAndAuthTenancy(t *testing.T) {
	h := newPerpsServer(t, mode.Off, nil, nil)
	handler := h.srv.Handler()

	rr := perpsDo(handler, http.MethodGet, "/v1/perps/markets", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("public markets status = %d body=%s", rr.Code, rr.Body.String())
	}
	var data struct {
		Markets []struct {
			Symbol        string `json:"symbol"`
			Mode          string `json:"mode"`
			EffectiveMode string `json:"effective_mode"`
			Health        string `json:"health"`
		} `json:"markets"`
		Count int `json:"count"`
	}
	decodeData(t, rr, &data)
	if data.Count != market.Count() {
		t.Fatalf("markets = %d, want %d", data.Count, market.Count())
	}
	for _, m := range data.Markets {
		if m.EffectiveMode != "OFF" {
			t.Fatalf("global OFF must force effective OFF, got %s for %s", m.EffectiveMode, m.Symbol)
		}
	}

	// Unauthenticated account read is rejected; owner derives from the token.
	if rr := perpsDo(handler, http.MethodGet, "/v1/perps/account", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("account without token = %d", rr.Code)
	}

	ownerA, _, _ := perpsDID(t, "tenant-a")
	ownerB, _, _ := perpsDID(t, "tenant-b")
	if err := h.st.CreditDeposit(h.ctx, ownerA, "0xaaa", "0xdep-"+ownerA, 7_000_000); err != nil {
		t.Fatal(err)
	}
	tokA, _ := h.tokens.Mint(ownerA)
	tokB, _ := h.tokens.Mint(ownerB)

	var acctA struct {
		OwnerDID  string `json:"owner_did"`
		Spendable int64  `json:"spendable_micro_usdx"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/account", tokA, nil), &acctA)
	if acctA.OwnerDID != ownerA || acctA.Spendable != 7_000_000 {
		t.Fatalf("account A = %+v", acctA)
	}
	var acctB struct {
		OwnerDID  string `json:"owner_did"`
		Spendable int64  `json:"spendable_micro_usdx"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/account", tokB, nil), &acctB)
	if acctB.OwnerDID != ownerB || acctB.Spendable != 0 {
		t.Fatalf("account B must be scoped to B: %+v", acctB)
	}

	// Owner cannot be widened by query parameters: B's order list stays empty
	// even when it names A.
	var listB struct {
		Items []json.RawMessage `json:"items"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/orders?owner="+ownerA, tokB, nil), &listB)
	if len(listB.Items) != 0 {
		t.Fatalf("owner B sees %d foreign orders", len(listB.Items))
	}

	// Orderbook under OFF mode fails closed with the stable code.
	rr = perpsDo(handler, http.MethodGet, "/v1/perps/orderbook/BTC", "", nil)
	if rr.Code != http.StatusServiceUnavailable || errCode(t, rr) != "PERPS_DISABLED" {
		t.Fatalf("orderbook under OFF = %d %s", rr.Code, rr.Body.String())
	}
}

func TestPerpsOrderRejectsClientEconomics(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	handler := h.srv.Handler()
	rr := perpsDo(handler, http.MethodPost, "/v1/perps/orders", "", map[string]any{
		"symbol": "BTC", "side": "BUY", "type": "MARKET", "contracts": 1,
		"time_in_force": "IOC", "idempotency_key": "k1",
		"execution_price_cents": 1234,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("client price field must be rejected, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "execution_price_cents") {
		t.Fatalf("rejection must name the offending field: %s", rr.Body.String())
	}
}

func TestPerpsCancelExactlyOnceReplay(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	handler := h.srv.Handler()
	activateMarket(t, h, "BTC")

	owner, pub, priv := perpsDID(t, "cancel")
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 10_000_000); err != nil {
		t.Fatal(err)
	}
	// Stage a real RESTING order with held margin through production store paths.
	order, err := h.st.InsertPerpOrder(h.ctx, store.PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 1, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "stage-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReservePerpMargin(h.ctx, owner, order.ID, 2_500_000); err != nil {
		t.Fatal(err)
	}
	balBefore, err := h.st.GetPerpAccount(h.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if balBefore.SpendableMicro != 7_500_000 || balBefore.OpenOrderMarginMicro != 2_500_000 {
		t.Fatalf("staging = %+v", balBefore)
	}

	idem := "cancel-" + owner
	body, _ := signedBody(t, h, priv, pub, "cancel", owner,
		[]string{order.ID, idem}, map[string]any{"idempotency_key": idem})
	rr := perpsDo(handler, http.MethodDelete, "/v1/perps/orders/"+order.ID, "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rr.Code, rr.Body.String())
	}
	var first struct {
		Order struct {
			Status string `json:"status"`
		} `json:"order"`
		Released int64 `json:"released_micro_usdx"`
		Receipt  struct {
			SequencerSignature string `json:"sequencer_signature"`
			EventSeqLo         int64  `json:"event_seq_lo"`
		} `json:"receipt"`
		Replayed bool `json:"replayed"`
	}
	decodeData(t, rr, &first)
	if first.Order.Status != "CANCELLED" || first.Released != 2_500_000 || first.Replayed {
		t.Fatalf("first cancel = %+v", first)
	}
	if first.Receipt.SequencerSignature == "" || first.Receipt.EventSeqLo == 0 {
		t.Fatalf("cancel receipt incomplete: %+v", first.Receipt)
	}

	// Response-loss retry: the EXACT same signed request (nonce now consumed)
	// must replay the original result without a second release.
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/orders/"+order.ID, "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel replay = %d %s", rr.Code, rr.Body.String())
	}
	var second struct {
		Released int64 `json:"released_micro_usdx"`
		Replayed bool  `json:"replayed"`
	}
	decodeData(t, rr, &second)
	if !second.Replayed || second.Released != 2_500_000 {
		t.Fatalf("replay = %+v", second)
	}
	after, err := h.st.GetPerpAccount(h.ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpendableMicro != 10_000_000 || after.OpenOrderMarginMicro != 0 {
		t.Fatalf("released exactly once, got %+v", after)
	}

	// Same key, different request: IDEMPOTENCY_CONFLICT.
	conflictBody, _ := signedBody(t, h, priv, pub, "cancel", owner,
		[]string{"00000000-0000-0000-0000-000000000000", idem}, map[string]any{"idempotency_key": idem})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/orders/00000000-0000-0000-0000-000000000000", "", conflictBody)
	if rr.Code != http.StatusConflict || errCode(t, rr) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict = %d %s", rr.Code, rr.Body.String())
	}

	// Another owner cannot cancel this order (tenancy).
	intruder, ipub, ipriv := perpsDID(t, "intruder")
	if err := h.st.CreditDeposit(h.ctx, intruder, "0xint", "0xdep-"+intruder, 1_000_000); err != nil {
		t.Fatal(err)
	}
	iBody, _ := signedBody(t, h, ipriv, ipub, "cancel", intruder,
		[]string{order.ID, "i-" + intruder}, map[string]any{"idempotency_key": "i-" + intruder})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/orders/"+order.ID, "", iBody)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign cancel = %d %s", rr.Code, rr.Body.String())
	}
}

func TestPerpsMarginAddAndStaleRemove(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	handler := h.srv.Handler()
	activateMarket(t, h, "BTC")

	owner, pub, priv := perpsDID(t, "margin")
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 20_000_000); err != nil {
		t.Fatal(err)
	}
	stage, err := h.st.InsertPerpOrder(h.ctx, store.PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 2, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "mstage-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.st.ReservePerpMargin(h.ctx, owner, stage.ID, 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	pos, err := h.st.OpenPerpPositionFromReservation(h.ctx, res.ID, "BTC", "LONG", 2, 6_000_000,
		store.PerpSnapshotRef{SnapshotID: "stage", OrderbookSeq: 1, StatsSeq: 1, SourceTimestampMs: 1})
	if err != nil {
		t.Fatal(err)
	}

	idem := "madd-" + owner
	body, _ := signedBody(t, h, priv, pub, "margin", owner,
		[]string{pos.ID, "ADD", "1000000", idem},
		map[string]any{"operation": "ADD", "amount_micro_usdx": 1_000_000, "idempotency_key": idem})
	rr := perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+pos.ID+"/margin", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("margin add = %d %s", rr.Code, rr.Body.String())
	}
	var addResp struct {
		Position struct {
			Margin int64 `json:"margin_micro_usdx"`
		} `json:"position"`
		Account struct {
			Spendable int64 `json:"spendable_micro_usdx"`
		} `json:"account"`
		Replayed bool `json:"replayed"`
	}
	decodeData(t, rr, &addResp)
	if addResp.Position.Margin != 5_000_000 || addResp.Account.Spendable != 15_000_000 {
		t.Fatalf("margin add moved wrong value: %+v", addResp)
	}

	// Retry the same signed add: exactly-once.
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+pos.ID+"/margin", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("margin add replay = %d %s", rr.Code, rr.Body.String())
	}
	decodeData(t, rr, &addResp)
	if !addResp.Replayed {
		t.Fatalf("margin retry must replay: %+v", addResp)
	}
	acct, err := h.st.GetPerpAccount(h.ctx, owner)
	if err != nil || acct.SpendableMicro != 15_000_000 || acct.PositionMarginMicro != 5_000_000 {
		t.Fatalf("margin add applied twice? %+v %v", acct, err)
	}

	// REMOVE requires a live healthy mark; with no feed it fails closed.
	rIdem := "mrem-" + owner
	rBody, _ := signedBody(t, h, priv, pub, "margin", owner,
		[]string{pos.ID, "REMOVE", "1000000", rIdem},
		map[string]any{"operation": "REMOVE", "amount_micro_usdx": 1_000_000, "idempotency_key": rIdem})
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+pos.ID+"/margin", "", rBody)
	if rr.Code != http.StatusServiceUnavailable || errCode(t, rr) != "MARKET_STALE" {
		t.Fatalf("stale margin remove = %d %s", rr.Code, rr.Body.String())
	}
}

func TestPerpsDelegationLifecycle(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	handler := h.srv.Handler()

	owner, pub, priv := perpsDID(t, "delegator")
	delegate, dpub, dpriv := perpsDID(t, "delegate")
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 10_000_000); err != nil {
		t.Fatal(err)
	}

	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	base := map[string]any{
		"owner_did": owner, "delegate_did": delegate, "membership_tier": "pro",
		"allowed_markets": []string{"BTC"}, "allowed_order_types": []string{"MARKET", "LIMIT"},
		"maximum_order_notional_micro_usdx":      100_000_000,
		"maximum_position_notional_micro_usdx":   200_000_000,
		"maximum_leverage_x":                     3,
		"maximum_daily_notional_micro_usdx":      500_000_000,
		"maximum_daily_realized_loss_micro_usdx": 50_000_000,
		"expires_at_ms":                          expires,
	}
	grantFields := []string{
		owner, delegate, "pro", "BTC", "MARKET,LIMIT",
		"100000000", "200000000", "3", "500000000", "50000000",
		fmt.Sprintf("%d", expires),
	}
	grantSig := hex.EncodeToString(ed25519.Sign(priv, []byte(auth.PerpGrantMessage(grantFields...))))
	base["grant_signature"] = grantSig

	idem := "dcreate-" + owner
	base["idempotency_key"] = idem
	intentFields := append(append([]string{}, grantFields...), grantSig, idem)
	body, _ := signedBody(t, h, priv, pub, "delegation.create", owner, intentFields, base)
	rr := perpsDo(handler, http.MethodPost, "/v1/perps/delegations", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("delegation create = %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		Delegation struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"delegation"`
	}
	decodeData(t, rr, &created)
	if created.Delegation.Status != "ACTIVE" || created.Delegation.ID == "" {
		t.Fatalf("created = %+v", created)
	}

	// A tampered grant signature is rejected.
	tampered := map[string]any{}
	for k, v := range base {
		tampered[k] = v
	}
	tampered["maximum_leverage_x"] = 50
	tamperedFields := append([]string{}, intentFields...)
	tamperedFields[7] = "50"
	tBody, _ := signedBody(t, h, priv, pub, "delegation.create", owner, tamperedFields, tampered)
	tBody["idempotency_key"] = "dtamper-" + owner
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/delegations", "", tBody)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered grant = %d %s", rr.Code, rr.Body.String())
	}

	// Owner-scoped list via principal token.
	tok, _ := h.tokens.Mint(owner)
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/delegations", tok, nil), &list)
	if len(list.Items) == 0 {
		t.Fatal("owner list must include the delegation")
	}

	// The delegate cannot revoke its own grant.
	dIdem := "drevoke-bad-" + delegate
	dBody, _ := signedBody(t, h, dpriv, dpub, "delegation.revoke", delegate,
		[]string{created.Delegation.ID, dIdem}, map[string]any{"idempotency_key": dIdem})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/delegations/"+created.Delegation.ID, "", dBody)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("delegate self-revoke = %d %s", rr.Code, rr.Body.String())
	}

	// Owner revoke succeeds and is idempotent.
	rIdem := "drevoke-" + owner
	rBody, _ := signedBody(t, h, priv, pub, "delegation.revoke", owner,
		[]string{created.Delegation.ID, rIdem}, map[string]any{"idempotency_key": rIdem})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/delegations/"+created.Delegation.ID, "", rBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner revoke = %d %s", rr.Code, rr.Body.String())
	}
	var revoked struct {
		Delegation struct {
			Status string `json:"status"`
		} `json:"delegation"`
		Cancelled []string `json:"cancelled_order_ids"`
	}
	decodeData(t, rr, &revoked)
	if revoked.Delegation.Status != "REVOKED" {
		t.Fatalf("revoked = %+v", revoked)
	}
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/delegations/"+created.Delegation.ID, "", rBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent revoke = %d %s", rr.Code, rr.Body.String())
	}
}

// TestPerpsStreamReplayAndTenancy proves durable owner-scoped SSE replay with
// Last-Event-ID and that another owner's stream never exposes the events.
func TestPerpsStreamReplayAndTenancy(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	ts := httptest.NewServer(h.srv.Handler())
	t.Cleanup(ts.Close)

	owner, _, _ := perpsDID(t, "stream")
	other, _, _ := perpsDID(t, "stream-other")
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 10_000_000); err != nil {
		t.Fatal(err)
	}
	// Stage durable owner events through real store paths.
	stage, err := h.st.InsertPerpOrder(h.ctx, store.PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 1, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "sstage-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := h.st.ReservePerpMargin(h.ctx, owner, stage.ID, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReleasePerpMarginReservation(h.ctx, r1.ID); err != nil {
		t.Fatal(err)
	}

	readEvents := func(did string, lastEventID string, wait time.Duration) []string {
		tok, _ := h.tokens.Mint(did)
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/perps/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		req = req.WithContext(ctx)
		req.Header.Set("X-LayerX-Agent", tok)
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d", resp.StatusCode)
		}
		var events []string
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				events = append(events, strings.TrimPrefix(line, "event: "))
			}
		}
		return events
	}

	got := readEvents(owner, "", 2*time.Second)
	if len(got) < 3 {
		t.Fatalf("owner replay = %v, want order.accepted + reserve + release", got)
	}
	for _, ev := range got {
		if ev != "balance.updated" && ev != "order.accepted" {
			t.Fatalf("unexpected event type %q in %v", ev, got)
		}
	}

	// Resuming after the last event replays nothing.
	resumed := readEvents(owner, fmt.Sprintf("%d", len(got)), 1500*time.Millisecond)
	if len(resumed) != 0 {
		t.Fatalf("resume replayed %v", resumed)
	}

	// Tenancy: the other owner's stream never carries these events.
	foreign := readEvents(other, "", 1500*time.Millisecond)
	if len(foreign) != 0 {
		t.Fatalf("foreign stream leaked %v", foreign)
	}
}

// newLiveFeed starts the real Crossverse manager for BTC and waits until risk
// increase is allowed (healthy per-symbol feed + fresh aggregate lane).
func newLiveFeed(t *testing.T, baseURL string) (*crossverse.Manager, error) {
	t.Helper()
	mgr, err := crossverse.New(crossverse.Config{BaseURL: baseURL, Symbols: []string{"BTC"}, Logf: t.Logf})
	if err != nil {
		return nil, err
	}
	feedCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		mgr.Stop()
	})
	if err := mgr.Start(feedCtx); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for live BTC feed")
		}
		allowed, err := mgr.RiskIncreaseAllowed("BTC")
		if err != nil {
			return nil, err
		}
		if allowed {
			return mgr, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// fundPerpPool funds protocol liquidity through the ONLY legal path: an
// ordinary signed USDX transfer from a funded account.
func fundPerpPool(t *testing.T, h *perpsHarness, fromDID string, amountMicro int64) {
	t.Helper()
	if _, err := h.st.FundPerpPool(h.ctx, fromDID, "liquidity", amountMicro, "material",
		func(seq int64, ts time.Time) (string, string) {
			leaf := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, fromDID, store.PerpPoolDID("liquidity"), amountMicro, ts.UnixNano()))
			return leaf, "testsig"
		}); err != nil {
		t.Fatalf("FundPerpPool: %v", err)
	}
}

// TestLivePerpsAPIFlow drives the COMPLETE authenticated flow against the live
// Crossverse feed: quote -> DID-signed market order -> response-loss replay ->
// fills list -> close -> stream carries the fill. Env-gated.
func TestLivePerpsAPIFlow(t *testing.T) {
	baseURL := os.Getenv("CROSSVERSE_TEST_URL")
	if baseURL == "" {
		t.Skip("CROSSVERSE_TEST_URL not set")
	}
	mgr, err := newLiveFeed(t, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, mgr)
	handler := h.srv.Handler()
	activateMarket(t, h, "BTC")

	owner, pub, priv := perpsDID(t, "live-api")
	// BTC activation needs usable capital >= 20,500 USDX (stress 2000bps +
	// liq fee 50bps over the 100k USDX configured OI cap).
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 30_000_000_000); err != nil {
		t.Fatal(err)
	}
	fundPerpPool(t, h, owner, 21_000_000_000)

	// Server-side quote via principal token.
	tok, _ := h.tokens.Mint(owner)
	rr := perpsDo(handler, http.MethodPost, "/v1/perps/quote", tok, map[string]any{
		"symbol": "BTC", "side": "BUY", "contracts": 2,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("quote = %d %s", rr.Code, rr.Body.String())
	}
	var quote struct {
		QuoteID        string `json:"quote_id"`
		ExecutionPrice int64  `json:"execution_price_cents"`
		RequiredMargin int64  `json:"required_margin_micro_usdx"`
		ExpiresAtMs    int64  `json:"expires_at_ms"`
	}
	decodeData(t, rr, &quote)
	if quote.QuoteID == "" || quote.ExecutionPrice <= 0 || quote.RequiredMargin <= 0 {
		t.Fatalf("quote = %+v", quote)
	}

	// DID-signed market order.
	idem := "order-" + owner
	orderBase := map[string]any{
		"symbol": "BTC", "side": "BUY", "type": "MARKET", "contracts": 2,
		"time_in_force": "IOC", "idempotency_key": idem,
	}
	fields := []string{"BTC", "BUY", "MARKET", "2", "0", "0", "IOC", "false", "", idem}
	body, _ := signedBody(t, h, priv, pub, "order", owner, fields, orderBase)
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("order = %d %s", rr.Code, rr.Body.String())
	}
	var placed struct {
		Order struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"order"`
		Fills []struct {
			ID          string `json:"id"`
			PriceCents  int64  `json:"price_cents"`
			SnapshotRef struct {
				SnapshotID string `json:"snapshot_id"`
			} `json:"snapshot_ref"`
		} `json:"fills"`
		Receipt struct {
			SequencerSignature string `json:"sequencer_signature"`
			SnapshotID         string `json:"snapshot_id"`
		} `json:"receipt"`
		Replayed bool `json:"replayed"`
	}
	decodeData(t, rr, &placed)
	if placed.Order.Status != "FILLED" || len(placed.Fills) != 1 || placed.Replayed {
		t.Fatalf("placed = %+v", placed)
	}
	if placed.Receipt.SequencerSignature == "" || placed.Receipt.SnapshotID == "" ||
		placed.Fills[0].SnapshotRef.SnapshotID == "" {
		t.Fatalf("receipt/fill must bind the snapshot: %+v", placed)
	}

	// Response-loss retry with the same signed body: identical replay.
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("order replay = %d %s", rr.Code, rr.Body.String())
	}
	var replay struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
		Replayed bool `json:"replayed"`
	}
	decodeData(t, rr, &replay)
	if !replay.Replayed || replay.Order.ID != placed.Order.ID {
		t.Fatalf("replay = %+v want order %s", replay, placed.Order.ID)
	}

	// Fills list is owner-scoped and carries the fill.
	var fills struct {
		Items []struct {
			OrderID string `json:"order_id"`
		} `json:"items"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/fills?symbol=BTC", tok, nil), &fills)
	found := false
	for _, f := range fills.Items {
		if f.OrderID == placed.Order.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("fills list missing order %s: %+v", placed.Order.ID, fills)
	}

	// Positions read computes live risk fields.
	var positions struct {
		Items []struct {
			ID             string `json:"id"`
			Contracts      int64  `json:"contracts"`
			MarkPriceCents int64  `json:"mark_price_cents"`
			LiqPriceCents  int64  `json:"liquidation_price_cents"`
		} `json:"items"`
	}
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/positions", tok, nil), &positions)
	if len(positions.Items) != 1 || positions.Items[0].Contracts != 2 ||
		positions.Items[0].MarkPriceCents <= 0 || positions.Items[0].LiqPriceCents <= 0 {
		t.Fatalf("positions = %+v", positions)
	}
	positionID := positions.Items[0].ID

	// Close everything via the close endpoint (server forces reduce-only).
	cIdem := "close-" + owner
	cBody, _ := signedBody(t, h, priv, pub, "close", owner,
		[]string{positionID, "0", cIdem}, map[string]any{"idempotency_key": cIdem})
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+positionID+"/close", "", cBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("close = %d %s", rr.Code, rr.Body.String())
	}
	var closed struct {
		Position struct {
			Status    string `json:"status"`
			Contracts int64  `json:"contracts"`
		} `json:"position"`
		Receipt struct {
			SequencerSignature string `json:"sequencer_signature"`
		} `json:"receipt"`
	}
	decodeData(t, rr, &closed)
	if closed.Position.Status != "CLOSED" || closed.Position.Contracts != 0 ||
		closed.Receipt.SequencerSignature == "" {
		t.Fatalf("closed = %+v", closed)
	}

	// Closing again replays the SAME result (idempotent, not an error).
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+positionID+"/close", "", cBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("close replay = %d %s", rr.Code, rr.Body.String())
	}

	// Conservation stays exact after the full API flow.
	if err := h.eng.ReconcileOnce(h.ctx); err != nil {
		t.Fatalf("reconciliation after API flow: %v", err)
	}
}

// TestPerpsDelegatedCancel proves the delegate cancel law offline: a delegate
// may cancel the owner's orders it placed (even without a live grant), while a
// stranger acting for the owner is rejected DELEGATION_REQUIRED.
func TestPerpsDelegatedCancel(t *testing.T) {
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, nil)
	handler := h.srv.Handler()
	activateMarket(t, h, "BTC")

	owner, _, _ := perpsDID(t, "dc-owner")
	delegate, dpub, dpriv := perpsDID(t, "dc-agent")
	stranger, spub, spriv := perpsDID(t, "dc-stranger")
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 10_000_000); err != nil {
		t.Fatal(err)
	}
	placed, err := h.st.InsertPerpOrder(h.ctx, store.PerpOrder{
		OwnerDID: owner, ActingDID: delegate, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 1, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "dstage-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReservePerpMargin(h.ctx, owner, placed.ID, 2_000_000); err != nil {
		t.Fatal(err)
	}
	ownOrder, err := h.st.InsertPerpOrder(h.ctx, store.PerpOrder{
		OwnerDID: owner, ActingDID: owner, MarketSymbol: "BTC", Side: "BUY",
		OrderType: "LIMIT", Contracts: 1, LimitPriceCents: 100, TimeInForce: "GTC",
		IdempotencyKey: "ostage-" + owner, Status: "RESTING",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The stranger (no grant, did not place the order) is rejected.
	sIdem := "scancel-" + stranger
	sBody, _ := signedBody(t, h, spriv, spub, "cancel", stranger,
		[]string{ownOrder.ID, sIdem, owner}, map[string]any{"idempotency_key": sIdem, "owner_did": owner})
	rr := perpsDo(handler, http.MethodDelete, "/v1/perps/orders/"+ownOrder.ID, "", sBody)
	if rr.Code != http.StatusForbidden || errCode(t, rr) != "DELEGATION_REQUIRED" {
		t.Fatalf("stranger cancel = %d %s", rr.Code, rr.Body.String())
	}

	// The delegate cancels its own placed order for the owner (no grant needed).
	dIdem := "dcancel-" + delegate
	dBody, _ := signedBody(t, h, dpriv, dpub, "cancel", delegate,
		[]string{placed.ID, dIdem, owner}, map[string]any{"idempotency_key": dIdem, "owner_did": owner})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/orders/"+placed.ID, "", dBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("delegate cancel = %d %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Order struct {
			Status    string `json:"status"`
			ActingDID string `json:"acting_did"`
		} `json:"order"`
		Released int64 `json:"released_micro_usdx"`
	}
	decodeData(t, rr, &res)
	if res.Order.Status != "CANCELLED" || res.Released != 2_000_000 {
		t.Fatalf("delegate cancel result = %+v", res)
	}
	// The released margin went to the OWNER's spendable balance.
	acct, err := h.st.GetPerpAccount(h.ctx, owner)
	if err != nil || acct.SpendableMicro != 10_000_000 {
		t.Fatalf("owner balance after delegate cancel = %+v %v", acct, err)
	}
}

// TestLivePerpsDelegatedFlow drives the full delegated trade path against the
// live feed: owner-signed grant -> delegate trades for the owner within
// bounds -> bound breach rejected -> revocation blocks new delegated risk.
func TestLivePerpsDelegatedFlow(t *testing.T) {
	baseURL := os.Getenv("CROSSVERSE_TEST_URL")
	if baseURL == "" {
		t.Skip("CROSSVERSE_TEST_URL not set")
	}
	mgr, err := newLiveFeed(t, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	h := newPerpsServer(t, mode.Active, map[string]mode.Mode{"BTC": mode.Active}, mgr)
	handler := h.srv.Handler()
	activateMarket(t, h, "BTC")

	owner, opub, opriv := perpsDID(t, "df-owner")
	delegate, _, _ := perpsDID(t, "df-agent")
	h.eng.Entitlements = engine.StaticEntitlements{owner: "pro"}
	if err := h.st.CreditDeposit(h.ctx, owner, "0xabc", "0xdep-"+owner, 30_000_000_000); err != nil {
		t.Fatal(err)
	}
	fundPerpPool(t, h, owner, 21_000_000_000)

	// Owner-signed grant: BTC MARKET orders up to 50 USDX notional.
	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	base := map[string]any{
		"owner_did": owner, "delegate_did": delegate, "membership_tier": "pro",
		"allowed_markets": []string{"BTC"}, "allowed_order_types": []string{"MARKET"},
		"maximum_order_notional_micro_usdx":      50_000_000,
		"maximum_position_notional_micro_usdx":   100_000_000,
		"maximum_leverage_x":                     5,
		"maximum_daily_notional_micro_usdx":      100_000_000,
		"maximum_daily_realized_loss_micro_usdx": 100_000_000,
		"expires_at_ms":                          expires,
	}
	grantFields := []string{
		owner, delegate, "pro", "BTC", "MARKET",
		"50000000", "100000000", "5", "100000000", "100000000",
		fmt.Sprintf("%d", expires),
	}
	grantSig := hex.EncodeToString(ed25519.Sign(opriv, []byte(auth.PerpGrantMessage(grantFields...))))
	base["grant_signature"] = grantSig
	idem := "dfcreate-" + owner
	base["idempotency_key"] = idem
	intentFields := append(append([]string{}, grantFields...), grantSig, idem)
	body, _ := signedBody(t, h, opriv, opub, "delegation.create", owner, intentFields, base)
	rr := perpsDo(handler, http.MethodPost, "/v1/perps/delegations", "", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("grant create = %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		Delegation struct {
			ID string `json:"id"`
		} `json:"delegation"`
	}
	decodeData(t, rr, &created)

	// Delegate trades for the owner via its principal token.
	dTok, _ := h.tokens.Mint(delegate)
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", dTok, map[string]any{
		"owner_did": owner, "symbol": "BTC", "side": "BUY", "type": "MARKET",
		"contracts": 2, "time_in_force": "IOC", "idempotency_key": "dford-" + delegate,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("delegated order = %d %s", rr.Code, rr.Body.String())
	}
	var placed struct {
		Order struct {
			OwnerDID  string `json:"owner_did"`
			ActingDID string `json:"acting_did"`
			Status    string `json:"status"`
		} `json:"order"`
		Receipt struct {
			OwnerDID  string `json:"owner_did"`
			ActingDID string `json:"acting_did"`
		} `json:"receipt"`
	}
	decodeData(t, rr, &placed)
	if placed.Order.Status != "FILLED" || placed.Order.OwnerDID != owner ||
		placed.Order.ActingDID != delegate || placed.Receipt.ActingDID != delegate {
		t.Fatalf("delegated order audit = %+v", placed)
	}

	// A 6-contract order (60 USDX) exceeds the 50 USDX order bound.
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", dTok, map[string]any{
		"owner_did": owner, "symbol": "BTC", "side": "BUY", "type": "MARKET",
		"contracts": 6, "time_in_force": "IOC", "idempotency_key": "dfbig-" + delegate,
	})
	if rr.Code != http.StatusForbidden || errCode(t, rr) != "DELEGATION_LIMIT" {
		t.Fatalf("over-bound delegated order = %d %s", rr.Code, rr.Body.String())
	}

	// Membership downgrade blocks delegated risk increase.
	h.eng.Entitlements = engine.StaticEntitlements{owner: "basic"}
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", dTok, map[string]any{
		"owner_did": owner, "symbol": "BTC", "side": "BUY", "type": "MARKET",
		"contracts": 1, "time_in_force": "IOC", "idempotency_key": "dfdown-" + delegate,
	})
	if rr.Code != http.StatusForbidden || errCode(t, rr) != "MEMBERSHIP_REQUIRED" {
		t.Fatalf("downgraded delegated order = %d %s", rr.Code, rr.Body.String())
	}
	h.eng.Entitlements = engine.StaticEntitlements{owner: "pro"}

	// Revocation blocks every new delegated action.
	rIdem := "dfrevoke-" + owner
	rBody, _ := signedBody(t, h, opriv, opub, "delegation.revoke", owner,
		[]string{created.Delegation.ID, rIdem}, map[string]any{"idempotency_key": rIdem})
	rr = perpsDo(handler, http.MethodDelete, "/v1/perps/delegations/"+created.Delegation.ID, "", rBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rr.Code, rr.Body.String())
	}
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/orders", dTok, map[string]any{
		"owner_did": owner, "symbol": "BTC", "side": "BUY", "type": "MARKET",
		"contracts": 1, "time_in_force": "IOC", "idempotency_key": "dfpost-" + delegate,
	})
	if rr.Code != http.StatusForbidden || errCode(t, rr) != "DELEGATION_REQUIRED" {
		t.Fatalf("post-revoke delegated order = %d %s", rr.Code, rr.Body.String())
	}

	// The owner still closes its own position freely.
	var positions struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	oTok, _ := h.tokens.Mint(owner)
	decodeData(t, perpsDo(handler, http.MethodGet, "/v1/perps/positions", oTok, nil), &positions)
	if len(positions.Items) != 1 {
		t.Fatalf("positions = %+v", positions)
	}
	cIdem := "dfclose-" + owner
	cBody, _ := signedBody(t, h, opriv, opub, "close", owner,
		[]string{positions.Items[0].ID, "0", cIdem}, map[string]any{"idempotency_key": cIdem})
	rr = perpsDo(handler, http.MethodPost, "/v1/perps/positions/"+positions.Items[0].ID+"/close", "", cBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner close = %d %s", rr.Code, rr.Body.String())
	}
	if err := h.eng.ReconcileOnce(h.ctx); err != nil {
		t.Fatalf("reconciliation after delegated flow: %v", err)
	}
}
