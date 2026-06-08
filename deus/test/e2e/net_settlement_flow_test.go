//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
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
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/server"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

func TestNetSettlementFlow(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 for integration flow")
	}
	ctx := context.Background()
	anvilCmd := startAnvil(t)
	defer anvilCmd.Process.Kill()

	regAddr := deployRegistry(t)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "value": 42})
	}))
	defer proxySrv.Close()

	os.Setenv("DEUS_POSTGRES_URI", testDBURI())
	os.Setenv("DEUS_DEV", "1")
	os.Setenv("PAXEER_RPC_URL", "http://127.0.0.1:8545")
	os.Setenv("DEUS_CHAIN_ID", "31337")
	os.Setenv("DEUS_SERVICE_REGISTRY_ADDR", regAddr)
	os.Setenv("DEUS_PUBLISH_PRIVATE_KEY", anvilDevKey())
	os.Setenv("DEUS_GATEWAY_SIGNING_KEY", anvilDevKey())

	cfg, _ := config.Load()
	db, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.Migrate(ctx, filepath.Join("..", "..", "migrations"))

	chainClient, _ := chain.New(ctx, cfg.RPCURL, cfg.ChainID)
	defer chainClient.Close()
	chainReg, _ := chain.NewRegistry(chainClient, cfg.ServiceRegistryAddr)
	ix := indexer.New(chainReg, db)
	regSvc := registry.NewService(db, chainReg, ix)
	discSvc := discovery.New(db)

	signer, _ := receipts.NewSignerFromHex(cfg.ChainID, cfg.ServiceRegistryAddr, cfg.GatewaySigningKey)
	devWallet := &wallet.DevClient{}
	gw, settler, payer := buildGateway(db, signer, devWallet)

	srv := server.New(server.Deps{
		Log: telemetry.NewLogger(), Store: db, Chain: chainClient,
		Registry: regSvc, Discovery: discSvc, Gateway: gw, Settler: settler, DevMode: true,
		PublishPrivateKey: cfg.PublishPrivateKey,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	manifestRaw, _ := os.ReadFile(filepath.Join("..", "fixtures", "proxy-weather.json"))
	var manifest map[string]any
	_ = json.Unmarshal(manifestRaw, &manifest)
	manifest["owner"] = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	manifest["payout_address"] = manifest["owner"]
	manifest["slug"] = "net.flow." + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	manifest["endpoint"] = map[string]any{"proxy_url": proxySrv.URL}
	body, _ := json.Marshal(map[string]any{"manifest": manifest})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, _ := http.DefaultClient.Do(req)
	var created struct{ ID string `json:"id"` }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	pubReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services/"+created.ID+"/publish", nil)
	pubReq.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	pubResp, _ := http.DefaultClient.Do(pubReq)
	pubResp.Body.Close()

	svc, _ := db.GetServiceByID(ctx, created.ID)
	beforeUnsettled, err := db.UnsettledInvocations(ctx, svc.DeveloperID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTotal := big.NewInt(0)
	for _, inv := range beforeUnsettled {
		amt, ok := new(big.Int).SetString(inv.PriceWei, 10)
		if !ok {
			t.Fatalf("invalid price %s", inv.PriceWei)
		}
		beforeTotal.Add(beforeTotal, amt)
	}
	chargeWei := "200000000000000"
	callerHeaders := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer test-agent")
		req.Header.Set("X-Caller-DID", "did:matrix:net:caller")
		req.Header.Set("X-Caller-Wallet", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	}

	chBody, _ := json.Marshal(map[string]any{"cap_wei": "1000000000000000000", "fund_tx": "0xdevfund"})
	chReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/channels", bytes.NewReader(chBody))
	chReq.Header.Set("Content-Type", "application/json")
	callerHeaders(chReq)
	chResp, _ := http.DefaultClient.Do(chReq)
	if chResp.StatusCode != http.StatusCreated {
		t.Fatalf("channel status %d", chResp.StatusCode)
	}
	chResp.Body.Close()

	quoteBody, _ := json.Marshal(map[string]any{"operation": "forecast", "estimated_units": "1"})
	qReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/quote/"+created.ID, bytes.NewReader(quoteBody))
	qReq.Header.Set("Content-Type", "application/json")
	callerHeaders(qReq)
	qResp, _ := http.DefaultClient.Do(qReq)
	var quote struct{ QuoteID string `json:"quote_id"` }
	_ = json.NewDecoder(qResp.Body).Decode(&quote)
	qResp.Body.Close()

	// First invoke without sig to get voucher digest
	idem := "net-e2e-" + time.Now().Format("150405.000000")
	invokeBody, _ := json.Marshal(map[string]any{
		"operation": "forecast", "args": map[string]any{"lat": 1.0, "lng": 2.0},
		"quote_id": quote.QuoteID, "idempotency_key": idem,
		"payment": map[string]any{"rail": "net"},
	})
	iReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/invoke/"+created.ID, bytes.NewReader(invokeBody))
	iReq.Header.Set("Content-Type", "application/json")
	callerHeaders(iReq)
	iResp, _ := http.DefaultClient.Do(iReq)
	if iResp.StatusCode != http.StatusOK {
		t.Fatalf("invoke status %d", iResp.StatusCode)
	}
	var invoke1 struct {
		Voucher *struct {
			Digest        string `json:"digest"`
			CumulativeWei string `json:"cumulative_wei"`
			Nonce         int64  `json:"nonce"`
			ChannelID     string `json:"channel_id"`
			LastReceiptHash string `json:"last_receipt_hash"`
		} `json:"voucher"`
	}
	_ = json.NewDecoder(iResp.Body).Decode(&invoke1)
	iResp.Body.Close()
	if invoke1.Voucher == nil || invoke1.Voucher.Digest == "" {
		t.Fatal("missing pending voucher")
	}

	sig, err := signDigestHex(invoke1.Voucher.Digest, callerDevKey())
	if err != nil {
		t.Fatal(err)
	}
	cosignBody, _ := json.Marshal(map[string]any{
		"channel_id": invoke1.Voucher.ChannelID,
		"cumulative_wei": invoke1.Voucher.CumulativeWei,
		"charge_wei": chargeWei,
		"nonce": invoke1.Voucher.Nonce,
		"last_receipt_hash": invoke1.Voucher.LastReceiptHash,
		"digest": invoke1.Voucher.Digest,
		"caller_sig": sig,
	})
	cReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/vouchers/cosign", bytes.NewReader(cosignBody))
	cReq.Header.Set("Content-Type", "application/json")
	callerHeaders(cReq)
	cResp, _ := http.DefaultClient.Do(cReq)
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("cosign status %d", cResp.StatusCode)
	}
	cResp.Body.Close()

	settleBody, _ := json.Marshal(map[string]any{
		"developer_id": svc.DeveloperID,
		"payout_address": "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
	})
	sReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/settle/run", bytes.NewReader(settleBody))
	sReq.Header.Set("Content-Type", "application/json")
	sResp, _ := http.DefaultClient.Do(sReq)
	if sResp.StatusCode != http.StatusOK {
		t.Fatalf("settle status %d", sResp.StatusCode)
	}
	var settleRes struct {
		TotalWei string `json:"total_wei"`
		Count    int    `json:"count"`
	}
	_ = json.NewDecoder(sResp.Body).Decode(&settleRes)
	sResp.Body.Close()
	wantCount := len(beforeUnsettled) + 1
	wantTotal := new(big.Int).Add(beforeTotal, mustBigInt(chargeWei))
	if settleRes.Count != wantCount || settleRes.TotalWei != wantTotal.Text(10) {
		t.Fatalf("settle result %+v want count=%d total=%s", settleRes, wantCount, wantTotal)
	}
	if len(payer.Payouts) == 0 || payer.Payouts[len(payer.Payouts)-1].AmountWei != wantTotal.Text(10) {
		t.Fatalf("dev payer %+v", payer.Payouts)
	}
	if len(payer.Anchors) != 1 {
		t.Fatalf("expected anchor record")
	}
}
