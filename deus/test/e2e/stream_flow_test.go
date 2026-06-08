//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/config"
	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/server"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/streams"
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

func TestStreamRailFlow(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 for integration flow")
	}
	ctx := context.Background()
	anvilCmd := startAnvil(t)
	defer anvilCmd.Process.Kill()

	regAddr := deployRegistry(t)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "step": "tick"})
	}))
	defer proxySrv.Close()

	os.Setenv("DEUS_POSTGRES_URI", testDBURI())
	os.Setenv("DEUS_DEV", "1")
	os.Setenv("PAXEER_RPC_URL", "http://127.0.0.1:8545")
	os.Setenv("DEUS_CHAIN_ID", "31337")
	os.Setenv("DEUS_SERVICE_REGISTRY_ADDR", regAddr)
	os.Setenv("DEUS_PUBLISH_PRIVATE_KEY", anvilDevKey())
	os.Setenv("DEUS_GATEWAY_SIGNING_KEY", anvilDevKey())

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer db.Close()
	_ = db.Migrate(ctx, filepath.Join("..", "..", "migrations"))

	chainClient, err := chain.New(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer chainClient.Close()
	chainReg, err := chain.NewRegistry(chainClient, cfg.ServiceRegistryAddr)
	if err != nil {
		t.Fatalf("registry bind: %v", err)
	}
	ix := indexer.New(chainReg, db)
	discSvc := discovery.New(db)
	regSvc := registry.NewService(db, chainReg, ix)
	regSvc.SetManifestIndexer(discSvc)

	signer, err := receipts.NewSignerFromHex(cfg.ChainID, cfg.ServiceRegistryAddr, cfg.GatewaySigningKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	devWallet := &wallet.DevClient{MaxPerCallWei: ""}
	devStreams := streams.NewDevBackend()
	pricingSvc := pricing.New(db)
	streamSvc := streams.New(streams.Config{
		Store:   db,
		Pricing: pricingSvc,
		Wallet:  devWallet,
		Backend: devStreams,
		Dev:     devStreams,
	})
	gw := gateway.New(gateway.Config{
		Store:   db,
		Pricing: pricingSvc,
		Meter:   metering.New(db),
		Wallet:  devWallet,
		Signer:  signer,
		Quality: quality.New(db),
		Streams: streamSvc,
		ChainID: cfg.ChainID,
	})

	srv := server.New(server.Deps{
		Log:               telemetry.NewLogger(),
		Store:             db,
		Chain:             chainClient,
		Registry:          regSvc,
		Discovery:         discSvc,
		Gateway:           gw,
		Streams:           streamSvc,
		DevMode:           true,
		PublishPrivateKey: cfg.PublishPrivateKey,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	manifestRaw, err := os.ReadFile(filepath.Join("..", "fixtures", "proxy-agent-stream.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var manifest map[string]any
	_ = json.Unmarshal(manifestRaw, &manifest)
	manifest["owner"] = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	manifest["payout_address"] = manifest["owner"]
	manifest["slug"] = "agent.stream." + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	manifest["endpoint"] = map[string]any{"proxy_url": proxySrv.URL}
	body, _ := json.Marshal(map[string]any{"manifest": manifest})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)

	pubReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services/"+created.ID+"/publish", nil)
	pubReq.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	pubResp, err := http.DefaultClient.Do(pubReq)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	pubResp.Body.Close()

	callerHeaders := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer test-agent")
		req.Header.Set("X-Caller-DID", "did:matrix:e2e:stream")
		req.Header.Set("X-Caller-Wallet", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	}

	capWei := "10000000000000000"
	openBody, _ := json.Marshal(map[string]any{
		"service_id": created.ID,
		"operation":  "run",
		"cap_wei":    capWei,
	})
	oReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/streams", bytes.NewReader(openBody))
	oReq.Header.Set("Content-Type", "application/json")
	callerHeaders(oReq)
	oResp, err := http.DefaultClient.Do(oReq)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer oResp.Body.Close()
	if oResp.StatusCode != http.StatusCreated {
		t.Fatalf("open stream status %d", oResp.StatusCode)
	}
	var opened struct {
		StreamID         string `json:"stream_id"`
		RatePerSecondWei string `json:"rate_per_second_wei"`
	}
	_ = json.NewDecoder(oResp.Body).Decode(&opened)
	if opened.StreamID == "" {
		t.Fatal("missing stream_id")
	}
	if opened.RatePerSecondWei != "1000000000000" {
		t.Fatalf("rate %s", opened.RatePerSecondWei)
	}
	if len(devWallet.StreamOpens) != 1 {
		t.Fatalf("expected 1 stream open, got %d", len(devWallet.StreamOpens))
	}

	time.Sleep(2 * time.Second)

	quoteBody, _ := json.Marshal(map[string]any{"operation": "run", "estimated_units": "2"})
	qReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/quote/"+created.ID, bytes.NewReader(quoteBody))
	qReq.Header.Set("Content-Type", "application/json")
	callerHeaders(qReq)
	qResp, err := http.DefaultClient.Do(qReq)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	defer qResp.Body.Close()
	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("quote status %d", qResp.StatusCode)
	}
	var quote struct {
		QuoteID string `json:"quote_id"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&quote)

	invokeBody, _ := json.Marshal(map[string]any{
		"operation":       "run",
		"args":            map[string]any{"step": "tick"},
		"quote_id":        quote.QuoteID,
		"idempotency_key": "stream-e2e-" + time.Now().Format("150405.000000"),
		"payment":         map[string]any{"rail": "stream", "stream_id": opened.StreamID},
	})
	iReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/invoke/"+created.ID, bytes.NewReader(invokeBody))
	iReq.Header.Set("Content-Type", "application/json")
	callerHeaders(iReq)
	iResp, err := http.DefaultClient.Do(iReq)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	defer iResp.Body.Close()
	if iResp.StatusCode != http.StatusOK {
		t.Fatalf("invoke status %d", iResp.StatusCode)
	}
	var invoke struct {
		Outcome    string `json:"outcome"`
		ChargedWei string `json:"charged_wei"`
	}
	_ = json.NewDecoder(iResp.Body).Decode(&invoke)
	if invoke.Outcome != "ok" {
		t.Fatalf("invoke outcome %q", invoke.Outcome)
	}
	if invoke.ChargedWei == "0" {
		t.Fatalf("expected stream-metered charge, got %s", invoke.ChargedWei)
	}
	if len(devWallet.Sends) != 0 {
		t.Fatalf("stream rail must not direct-send, got %d sends", len(devWallet.Sends))
	}

	sReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/streams/"+opened.StreamID+"/settle", nil)
	callerHeaders(sReq)
	sResp, err := http.DefaultClient.Do(sReq)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	defer sResp.Body.Close()
	if sResp.StatusCode != http.StatusOK {
		t.Fatalf("settle status %d", sResp.StatusCode)
	}
	var settled struct {
		SettledWei string `json:"settled_wei"`
		AccruedWei string `json:"accrued_wei"`
	}
	_ = json.NewDecoder(sResp.Body).Decode(&settled)
	if settled.SettledWei == "0" {
		t.Fatalf("expected settled > 0, got %s", settled.SettledWei)
	}

	cReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/streams/"+opened.StreamID+"/close", nil)
	callerHeaders(cReq)
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("close status %d", cResp.StatusCode)
	}
	var closed struct {
		Status    string `json:"status"`
		RefundWei string `json:"refund_wei"`
		CapWei    string `json:"cap_wei"`
	}
	_ = json.NewDecoder(cResp.Body).Decode(&closed)
	if closed.Status != "closed" {
		t.Fatalf("status %q", closed.Status)
	}
	if closed.RefundWei == "" || closed.RefundWei == "0" {
		t.Fatalf("expected refund of unspent cap, got %s", closed.RefundWei)
	}
	if closed.RefundWei == capWei {
		t.Fatalf("refund should be less than full cap after accrual, got %s", closed.RefundWei)
	}
}
