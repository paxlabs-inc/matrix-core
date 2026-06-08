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
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/server"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/telemetry"
)

func TestDiscoverySemanticRanking(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 for integration flow")
	}
	ctx := context.Background()
	anvilCmd := startAnvil(t)
	defer anvilCmd.Process.Kill()

	regAddr := deployRegistry(t)
	os.Setenv("DEUS_POSTGRES_URI", testDBURI())
	os.Setenv("DEUS_DEV", "1")
	os.Setenv("PAXEER_RPC_URL", "http://127.0.0.1:8545")
	os.Setenv("DEUS_CHAIN_ID", "31337")
	os.Setenv("DEUS_SERVICE_REGISTRY_ADDR", regAddr)
	os.Setenv("DEUS_PUBLISH_PRIVATE_KEY", anvilDevKey())

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
	discSvc := discovery.New(db)
	regSvc := registry.NewService(db, chainReg, ix)
	regSvc.SetManifestIndexer(discSvc)

	srv := server.New(server.Deps{
		Log: telemetry.NewLogger(), Store: db, Chain: chainClient,
		Registry: regSvc, Discovery: discSvc, DevMode: true,
		PublishPrivateKey: cfg.PublishPrivateKey,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	suffix := strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	weatherSlug := "disc.weather." + suffix
	financeSlug := "disc.finance." + suffix
	createAndPublish(t, ts.URL, "proxy-weather.json", weatherSlug)
	createAndPublish(t, ts.URL, "proxy-finance.json", financeSlug)

	discBody, _ := json.Marshal(map[string]any{
		"query": "weather forecast current conditions",
		"limit": 5,
	})
	discReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/discover", bytes.NewReader(discBody))
	discReq.Header.Set("Content-Type", "application/json")
	discResp, _ := http.DefaultClient.Do(discReq)
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("discover status %d", discResp.StatusCode)
	}
	var disc struct {
		Results []struct {
			Slug  string  `json:"slug"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	_ = json.NewDecoder(discResp.Body).Decode(&disc)
	discResp.Body.Close()
	if len(disc.Results) == 0 {
		t.Fatal("discover empty")
	}
	if len(disc.Results) == 0 {
		t.Fatal("discover empty")
	}
	first := disc.Results[0].Slug
	if !strings.Contains(first, "weather") {
		t.Fatalf("expected weather-ranked result first, got %+v", disc.Results)
	}
	for _, r := range disc.Results {
		if r.Slug == financeSlug {
			t.Fatalf("finance should not rank for weather query: %+v", disc.Results)
		}
	}
	if disc.Results[0].Score <= 0.35 {
		t.Fatalf("expected blended score above lexical baseline, got %+v", disc.Results[0])
	}
}

func createAndPublish(t *testing.T, baseURL, fixture, slug string) {
	t.Helper()
	manifestRaw, _ := os.ReadFile(filepath.Join("..", "fixtures", fixture))
	var manifest map[string]any
	_ = json.Unmarshal(manifestRaw, &manifest)
	manifest["owner"] = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	manifest["payout_address"] = manifest["owner"]
	manifest["slug"] = slug
	body, _ := json.Marshal(map[string]any{"manifest": manifest})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, _ := http.DefaultClient.Do(req)
	var created struct{ ID string `json:"id"` }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	pubReq, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/services/"+created.ID+"/publish", nil)
	pubReq.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	pubResp, _ := http.DefaultClient.Do(pubReq)
	pubResp.Body.Close()
}
