package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/settlement"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/types"
)

// Tests run against a THROWAWAY Postgres only (set DEUS_TEST_POSTGRES_URI).
// They intentionally do NOT fall back to DEUS_POSTGRES_URI so they can never
// touch a real deployment's database.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	uri := os.Getenv("DEUS_TEST_POSTGRES_URI")
	if uri == "" {
		t.Skip("DEUS_TEST_POSTGRES_URI not set; skipping handler tests")
	}
	ctx := context.Background()
	st, err := store.New(ctx, uri)
	if err != nil {
		t.Skipf("test postgres unavailable: %v", err)
	}
	t.Cleanup(st.Close)
	dir := os.Getenv("DEUS_MIGRATIONS_DIR")
	if dir == "" {
		dir = "../../migrations"
	}
	if err := st.Migrate(ctx, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `TRUNCATE developers CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

type fixture struct {
	st          *store.Store
	srv         *httptest.Server
	devWallet   string
	callerDID   string
	serviceID   string
	slug        string
	endpointIDs map[string]string // operation -> endpoint id
	developerID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()

	f := &fixture{
		st:        st,
		devWallet: "0x1111111111111111111111111111111111111111",
		callerDID: "did:matrix:test:caller",
		slug:      "alpha-weather",
	}

	devID, err := st.UpsertDeveloperByWallet(ctx, f.devWallet, f.devWallet, "Alpha Dev")
	if err != nil {
		t.Fatalf("upsert developer: %v", err)
	}
	f.developerID = devID

	manifest := map[string]any{
		"schema_version": "2026-01",
		"slug":           f.slug,
		"kind":           "data",
		"mode":           "proxy",
		"display_name":   "Alpha Weather",
		"summary":        "Forecasts",
		"tags":           []string{"weather", "geo"},
		"pricing": []map[string]any{
			{"operation": "forecast", "model": "per_unit", "unit": "request", "price_wei": "100", "min_charge_wei": "100"},
		},
	}
	raw, _ := json.Marshal(manifest)
	svcID, err := st.InsertDraftService(ctx, store.ServiceRow{
		DeveloperID:  devID,
		Slug:         f.slug,
		Kind:         "data",
		Mode:         "proxy",
		DisplayName:  "Alpha Weather",
		Summary:      "Forecasts",
		Manifest:     raw,
		ManifestHash: "0x" + strings.Repeat("ab", 32),
		Status:       "draft",
	})
	if err != nil {
		t.Fatalf("insert service: %v", err)
	}
	f.serviceID = svcID
	if err := st.UpdateServiceStatus(ctx, svcID, "active"); err != nil {
		t.Fatalf("activate: %v", err)
	}

	ops := []store.EndpointRow{
		{Operation: "forecast", Method: "POST", InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`)},
		{Operation: "history", Method: "POST", InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`)},
	}
	if err := st.InsertEndpoints(ctx, svcID, ops); err != nil {
		t.Fatalf("insert endpoints: %v", err)
	}
	eps, err := st.EndpointsByService(ctx, svcID)
	if err != nil {
		t.Fatalf("endpoints: %v", err)
	}
	f.endpointIDs = map[string]string{}
	for _, ep := range eps {
		f.endpointIDs[ep.Operation] = ep.ID
	}

	s := New(Deps{
		Store:   st,
		Settler: settlement.NewSettler(st, &settlement.DevPayer{}),
		DevMode: true,
	})
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// addInvocation reserves + finalizes one ledger row and returns its id.
func (f *fixture) addInvocation(t *testing.T, op, outcome, priceWei string, latencyMS int) string {
	t.Helper()
	ctx := context.Background()
	id, err := f.st.InsertReservedInvocation(ctx, store.InvocationRow{
		IdempotencyKey: fmt.Sprintf("idem-%s-%d", op, time.Now().UnixNano()),
		ServiceID:      f.serviceID,
		EndpointID:     f.endpointIDs[op],
		CallerDID:      f.callerDID,
		CallerWallet:   "0x2222222222222222222222222222222222222222",
		Units:          "1",
		PriceWei:       priceWei,
		PricingVersion: 1,
		ArgsHash:       "0xargs",
	})
	if err != nil {
		t.Fatalf("reserve invocation: %v", err)
	}
	charged := priceWei
	if outcome != "ok" {
		charged = "0"
	}
	if err := f.st.FinalizeInvocation(ctx, id, outcome, "0xresult", "1", charged, latencyMS); err != nil {
		t.Fatalf("finalize invocation: %v", err)
	}
	return id
}

func (f *fixture) do(t *testing.T, method, path string, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, out
}

func (f *fixture) devHeaders() map[string]string {
	return map[string]string{"X-Developer-Wallet": f.devWallet, "Content-Type": "application/json"}
}

func (f *fixture) callerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer test", "X-Caller-DID": f.callerDID}
}

func TestGetServiceBySlugAndID(t *testing.T) {
	f := newFixture(t)

	res, body := f.do(t, "GET", "/v1/services/"+f.slug, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get by slug: status %d body %s", res.StatusCode, body)
	}
	var bySlug types.ServiceResponse
	if err := json.Unmarshal(body, &bySlug); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bySlug.ID != f.serviceID || bySlug.Slug != f.slug {
		t.Fatalf("slug lookup mismatch: %+v", bySlug)
	}

	res, body = f.do(t, "GET", "/v1/services/"+f.serviceID, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get by id: status %d body %s", res.StatusCode, body)
	}
	var byID types.ServiceResponse
	if err := json.Unmarshal(body, &byID); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if byID.ID != f.serviceID {
		t.Fatalf("id lookup mismatch: %+v", byID)
	}

	res, _ = f.do(t, "GET", "/v1/services/does-not-exist", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing slug: want 404 got %d", res.StatusCode)
	}
}

func TestCatalogShapeAndEnrichment(t *testing.T) {
	f := newFixture(t)

	res, body := f.do(t, "GET", "/v1/catalog?limit=10&offset=0", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("catalog: status %d body %s", res.StatusCode, body)
	}
	var cat types.CatalogResponse
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cat.Total != 1 || len(cat.Services) != 1 {
		t.Fatalf("catalog total/services: %+v", cat)
	}
	item := cat.Services[0]
	if item.Slug != f.slug || item.Status != "active" {
		t.Fatalf("catalog item: %+v", item)
	}
	if item.PriceWei != "100" || item.Unit != "request" {
		t.Fatalf("catalog enrichment price/unit missing: %+v", item)
	}
	if len(item.Tags) != 2 || item.Tags[0] != "weather" {
		t.Fatalf("catalog enrichment tags missing: %+v", item)
	}
}

func TestPauseDelistOwnership(t *testing.T) {
	f := newFixture(t)

	res, body := f.do(t, "POST", "/v1/services/"+f.serviceID+"/pause", "{}", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pause: status %d body %s", res.StatusCode, body)
	}
	var st types.ServiceStatusResponse
	_ = json.Unmarshal(body, &st)
	if st.Status != "paused" {
		t.Fatalf("pause status: %+v", st)
	}

	// Wrong wallet is rejected.
	other := map[string]string{"X-Developer-Wallet": "0x9999999999999999999999999999999999999999"}
	res, _ = f.do(t, "POST", "/v1/services/"+f.serviceID+"/delist", "{}", other)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("delist wrong owner: want 403 got %d", res.StatusCode)
	}

	// Slug works for lifecycle routes too.
	res, body = f.do(t, "POST", "/v1/services/"+f.slug+"/delist", "{}", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delist by slug: status %d body %s", res.StatusCode, body)
	}
	_ = json.Unmarshal(body, &st)
	if st.Status != "delisted" || st.ID != f.serviceID {
		t.Fatalf("delist status: %+v", st)
	}

	// No developer header at all → 401 from middleware.
	res, _ = f.do(t, "POST", "/v1/services/"+f.serviceID+"/pause", "{}", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pause unauthenticated: want 401 got %d", res.StatusCode)
	}
}

func TestMeAndSpend(t *testing.T) {
	f := newFixture(t)
	f.addInvocation(t, "forecast", "ok", "100", 120)
	f.addInvocation(t, "forecast", "ok", "100", 140)
	f.addInvocation(t, "history", "error", "100", 0)

	// /v1/me with developer identity.
	res, body := f.do(t, "GET", "/v1/me", "", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("me: status %d body %s", res.StatusCode, body)
	}
	var me types.MeResponse
	_ = json.Unmarshal(body, &me)
	if me.Wallet != f.devWallet || me.DisplayName != "Alpha Dev" {
		t.Fatalf("me response: %+v", me)
	}

	// /v1/me with caller identity.
	res, body = f.do(t, "GET", "/v1/me", "", f.callerHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("me caller: status %d body %s", res.StatusCode, body)
	}
	_ = json.Unmarshal(body, &me)
	if me.DID != f.callerDID {
		t.Fatalf("me caller did: %+v", me)
	}

	// /v1/me with no identity → 401.
	res, _ = f.do(t, "GET", "/v1/me", "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me anonymous: want 401 got %d", res.StatusCode)
	}

	// /v1/me/spend aggregates the caller's finalized ok spend.
	res, body = f.do(t, "GET", "/v1/me/spend", "", f.callerHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("spend: status %d body %s", res.StatusCode, body)
	}
	var spend types.SpendResponse
	_ = json.Unmarshal(body, &spend)
	if spend.TotalSpentWei != "200" {
		t.Fatalf("spend total: %+v", spend)
	}
	if len(spend.Entries) != 1 || spend.Entries[0].Invocations != 2 || spend.Entries[0].TotalWei != "200" {
		t.Fatalf("spend entries: %+v", spend.Entries)
	}
}

func TestMyServicesAndEarnings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addInvocation(t, "forecast", "ok", "100", 100)
	f.addInvocation(t, "forecast", "ok", "100", 110)
	settledID := f.addInvocation(t, "history", "ok", "100", 130)
	f.addInvocation(t, "history", "error", "100", 0)

	setID, err := f.st.InsertSettlement(ctx, store.SettlementRow{
		DeveloperID:     f.developerID,
		Rail:            "net",
		TotalWei:        "100",
		InvocationCount: 1,
		MerkleRoot:      "0x" + strings.Repeat("cd", 32),
		WindowStart:     time.Now().Add(-time.Hour),
		WindowEnd:       time.Now(),
	})
	if err != nil {
		t.Fatalf("insert settlement: %v", err)
	}
	if err := f.st.MarkInvocationsSettled(ctx, setID, []string{settledID}); err != nil {
		t.Fatalf("mark settled: %v", err)
	}

	res, body := f.do(t, "GET", "/v1/me/services", "", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("my services: status %d body %s", res.StatusCode, body)
	}
	var mine types.MyServicesResponse
	_ = json.Unmarshal(body, &mine)
	if len(mine.Services) != 1 {
		t.Fatalf("my services count: %+v", mine)
	}
	svc := mine.Services[0]
	if svc.ID != f.serviceID || svc.Invocations != 3 || svc.RevenueWei != "300" {
		t.Fatalf("my services aggregates: %+v", svc)
	}

	res, body = f.do(t, "GET", "/v1/me/earnings", "", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("earnings: status %d body %s", res.StatusCode, body)
	}
	var earn types.EarningsResponse
	_ = json.Unmarshal(body, &earn)
	if earn.TotalEarnedWei != "300" || earn.PendingWei != "200" || earn.AvailableWei != "100" {
		t.Fatalf("earnings totals: %+v", earn)
	}
	if len(earn.Settlements) != 1 || earn.Settlements[0].AmountWei != "100" {
		t.Fatalf("earnings settlements: %+v", earn.Settlements)
	}
	if earn.PayoutAddress != f.devWallet {
		t.Fatalf("earnings payout address: %+v", earn)
	}

	// Unknown developer → zeroed earnings, not an error.
	other := map[string]string{"X-Developer-Wallet": "0x9999999999999999999999999999999999999999"}
	res, body = f.do(t, "GET", "/v1/me/earnings", "", other)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("earnings unknown dev: status %d body %s", res.StatusCode, body)
	}
	_ = json.Unmarshal(body, &earn)
	if earn.TotalEarnedWei != "0" || len(earn.Settlements) != 0 {
		t.Fatalf("earnings unknown dev body: %+v", earn)
	}
}

func TestLogsAndAnalytics(t *testing.T) {
	f := newFixture(t)
	f.addInvocation(t, "forecast", "ok", "100", 100)
	f.addInvocation(t, "forecast", "ok", "100", 200)
	f.addInvocation(t, "history", "error", "100", 0)

	res, body := f.do(t, "GET", "/v1/services/"+f.serviceID+"/logs", "", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("logs: status %d body %s", res.StatusCode, body)
	}
	var logs types.LogsResponse
	_ = json.Unmarshal(body, &logs)
	if len(logs.Logs) != 3 {
		t.Fatalf("logs count: %+v", logs)
	}
	var sawError bool
	for _, l := range logs.Logs {
		if l.Level == "error" {
			sawError = true
		}
		if !strings.Contains(l.Message, "invoke op=") {
			t.Fatalf("log message format: %q", l.Message)
		}
	}
	if !sawError {
		t.Fatalf("expected an error-level log line: %+v", logs.Logs)
	}

	res, body = f.do(t, "GET", "/v1/services/"+f.serviceID+"/analytics", "", f.devHeaders())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("analytics: status %d body %s", res.StatusCode, body)
	}
	var an types.ServiceAnalyticsResponse
	_ = json.Unmarshal(body, &an)
	if an.ServiceID != f.serviceID || an.TotalInvocations != 2 || an.TotalRevenueWei != "200" {
		t.Fatalf("analytics totals: %+v", an)
	}
	if an.AvgLatencyMS != 150 {
		t.Fatalf("analytics avg latency: %+v", an)
	}
	if an.SuccessRate < 0.66 || an.SuccessRate > 0.67 {
		t.Fatalf("analytics success rate: %v", an.SuccessRate)
	}
	if len(an.Series) != 1 || an.Series[0].Invocations != 2 {
		t.Fatalf("analytics series: %+v", an.Series)
	}
	if len(an.TopOperations) != 1 || an.TopOperations[0].Operation != "forecast" || an.TopOperations[0].RevenueWei != "200" {
		t.Fatalf("analytics top operations: %+v", an.TopOperations)
	}

	// Analytics is owner-scoped.
	other := map[string]string{"X-Developer-Wallet": "0x9999999999999999999999999999999999999999"}
	res, _ = f.do(t, "GET", "/v1/services/"+f.serviceID+"/analytics", "", other)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("analytics wrong owner: want 403 got %d", res.StatusCode)
	}
}

func TestPayout(t *testing.T) {
	f := newFixture(t)

	// Missing payout address → 400.
	res, _ := f.do(t, "POST", "/v1/services/"+f.serviceID+"/payout", `{}`, f.devHeaders())
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("payout missing address: want 400 got %d", res.StatusCode)
	}

	// Nothing on the net rail to settle → 409 nothing_to_settle, but the payout
	// address update still lands.
	payout := `{"payout_address":"0x3333333333333333333333333333333333333333"}`
	res, body := f.do(t, "POST", "/v1/services/"+f.serviceID+"/payout", payout, f.devHeaders())
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("payout nothing to settle: want 409 got %d body %s", res.StatusCode, body)
	}
	dev, err := f.st.DeveloperByWallet(context.Background(), f.devWallet)
	if err != nil {
		t.Fatalf("developer by wallet: %v", err)
	}
	if dev.PayoutAddress != "0x3333333333333333333333333333333333333333" {
		t.Fatalf("payout address not updated: %+v", dev)
	}
}
