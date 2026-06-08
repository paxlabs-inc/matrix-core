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
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

func TestInvokeFlowDirectRail(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 for integration flow")
	}
	ctx := context.Background()
	anvilCmd := startAnvil(t)
	defer anvilCmd.Process.Kill()

	regAddr := deployRegistry(t)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tempC":   14.2,
			"summary": "Partly cloudy",
		})
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
	regSvc := registry.NewService(db, chainReg, ix)
	discSvc := discovery.New(db)

	signer, err := receipts.NewSignerFromHex(cfg.ChainID, cfg.ServiceRegistryAddr, cfg.GatewaySigningKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	devWallet := &wallet.DevClient{MaxPerCallWei: "1000000000000000000"}
	gw := gateway.New(gateway.Config{
		Store:   db,
		Pricing: pricing.New(db),
		Meter:   metering.New(db),
		Wallet:  devWallet,
		Signer:  signer,
		Quality: quality.New(db),
		ChainID: cfg.ChainID,
	})

	srv := server.New(server.Deps{
		Log:               telemetry.NewLogger(),
		Store:             db,
		Chain:             chainClient,
		Registry:          regSvc,
		Discovery:         discSvc,
		Gateway:           gw,
		DevMode:           true,
		PublishPrivateKey: cfg.PublishPrivateKey,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	manifestRaw, err := os.ReadFile(filepath.Join("..", "fixtures", "proxy-weather.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var manifest map[string]any
	_ = json.Unmarshal(manifestRaw, &manifest)
	manifest["owner"] = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	manifest["payout_address"] = manifest["owner"]
	manifest["slug"] = "weather.flow." + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
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
		req.Header.Set("X-Caller-DID", "did:matrix:e2e:caller")
		req.Header.Set("X-Caller-Wallet", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	}

	quoteBody, _ := json.Marshal(map[string]any{"operation": "forecast", "estimated_units": "1"})
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
		QuoteID     string `json:"quote_id"`
		MaxTotalWei string `json:"max_total_wei"`
		EIP712      struct {
			Digest    string `json:"digest"`
			Signature string `json:"signature"`
		} `json:"eip712"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&quote)
	if quote.QuoteID == "" || quote.EIP712.Digest == "" {
		t.Fatalf("quote incomplete: %+v", quote)
	}

	invokeBody, _ := json.Marshal(map[string]any{
		"operation":       "forecast",
		"args":            map[string]any{"lat": 37.77, "lng": -122.41},
		"quote_id":        quote.QuoteID,
		"idempotency_key": "e2e-" + time.Now().Format("150405.000000"),
		"payment":         map[string]any{"rail": "direct"},
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
		InvocationID string `json:"invocation_id"`
		Outcome      string `json:"outcome"`
		ChargedWei   string `json:"charged_wei"`
		Receipt      struct {
			Digest     string `json:"digest"`
			GatewaySig string `json:"gateway_sig"`
		} `json:"receipt"`
		Result map[string]any `json:"result"`
	}
	_ = json.NewDecoder(iResp.Body).Decode(&invoke)
	if invoke.Outcome != "ok" {
		t.Fatalf("invoke outcome %q", invoke.Outcome)
	}
	if invoke.ChargedWei != "200000000000000" {
		t.Fatalf("charged %s", invoke.ChargedWei)
	}
	if invoke.Receipt.Digest == "" || invoke.Receipt.GatewaySig == "" {
		t.Fatalf("missing receipt: %+v", invoke.Receipt)
	}
	if invoke.Result["tempC"] == nil {
		t.Fatalf("result missing: %+v", invoke.Result)
	}
	if len(devWallet.Sends) != 1 {
		t.Fatalf("expected 1 wallet send, got %d", len(devWallet.Sends))
	}
	if devWallet.Sends[0].AmountWei != "200000000000000" {
		t.Fatalf("send amount %s", devWallet.Sends[0].AmountWei)
	}

	svc, err := db.GetServiceByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if svc.QualityScore == nil {
		t.Fatal("quality score not updated")
	}

	recReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/receipts/"+invoke.InvocationID, nil)
	callerHeaders(recReq)
	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	recResp.Body.Close()
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("receipt status %d", recResp.StatusCode)
	}
}
