//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestRegistryPublishDiscoverFlow(t *testing.T) {
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

	srv := server.New(server.Deps{
		Log:               telemetry.NewLogger(),
		Store:             db,
		Chain:             chainClient,
		Registry:          regSvc,
		Discovery:         discSvc,
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
	wantSlug := "weather.e2e." + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	manifest["slug"] = wantSlug
	body, _ := json.Marshal(map[string]any{"manifest": manifest})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
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
	defer pubResp.Body.Close()
	if pubResp.StatusCode != http.StatusOK {
		t.Fatalf("publish status %d", pubResp.StatusCode)
	}

	discBody := []byte(`{"query":"` + wantSlug + `","limit":10}`)
	discReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/discover", bytes.NewReader(discBody))
	discReq.Header.Set("Content-Type", "application/json")
	discResp, err := http.DefaultClient.Do(discReq)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	defer discResp.Body.Close()
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("discover status %d", discResp.StatusCode)
	}
	var disc struct {
		Results []struct {
			Slug string `json:"slug"`
		} `json:"results"`
	}
	_ = json.NewDecoder(discResp.Body).Decode(&disc)
	if len(disc.Results) == 0 {
		t.Fatalf("discover empty: %+v", disc.Results)
	}
	found := false
	for _, r := range disc.Results {
		if r.Slug == wantSlug {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("discover missing slug %q: %+v", wantSlug, disc.Results)
	}
}

func testDBURI() string {
	if v := os.Getenv("DEUS_POSTGRES_URI"); v != "" {
		return v
	}
	return "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable"
}

func anvilDevKey() string {
	return "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
}

func startAnvil(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("anvil", "--port", "8545", "--chain-id", "31337")
	if err := cmd.Start(); err != nil {
		t.Fatalf("anvil: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := chain.New(context.Background(), "http://127.0.0.1:8545", 31337)
		if err == nil {
			c.Close()
			return cmd
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatal("anvil timeout")
	return nil
}

func deployRegistry(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("forge", "create", "src/ServiceRegistry.sol:ServiceRegistry",
		"--broadcast",
		"--rpc-url", "http://127.0.0.1:8545",
		"--private-key", anvilDevKey(),
		"--constructor-args", "0x00000000000000000000000000000000000000A1",
	)
	cmd.Dir = filepath.Join("..", "..", "contracts")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Deployed to:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Deployed to:"))
		}
	}
	t.Fatalf("no deploy address in %s", out)
	return ""
}
