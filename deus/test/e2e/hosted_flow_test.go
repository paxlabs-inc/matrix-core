//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
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
	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/objstore"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/server"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/telemetry"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

func TestHostedInvokeFlow(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 for integration flow")
	}
	ctx := context.Background()
	anvilCmd := startAnvil(t)
	defer anvilCmd.Process.Kill()

	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invoke" {
			http.NotFound(w, r)
			return
		}
		var body gateway.HostedInvokeRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(gateway.HostedInvokeResponse{
			Outcome: "ok",
			Result:  map[string]any{"echo": body.Args["message"]},
			Units:   "1",
		})
	}))
	defer runnerSrv.Close()

	regAddr := deployRegistry(t)
	os.Setenv("DEUS_POSTGRES_URI", testDBURI())
	os.Setenv("DEUS_DEV", "1")
	os.Setenv("PAXEER_RPC_URL", "http://127.0.0.1:8545")
	os.Setenv("DEUS_CHAIN_ID", "31337")
	os.Setenv("DEUS_SERVICE_REGISTRY_ADDR", regAddr)
	os.Setenv("DEUS_PUBLISH_PRIVATE_KEY", anvilDevKey())
	os.Setenv("DEUS_GATEWAY_SIGNING_KEY", anvilDevKey())
	os.Setenv("DEUS_HOSTING_DEV_EXEC_URL", runnerSrv.URL)

	cfg, _ := config.Load()
	db, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.Migrate(ctx, filepath.Join("..", "..", "migrations"))

	blobs := objstore.NewMem("deus-e2e")
	limits := hosting.LimitsFromEnv()
	hostOrchestrator := hosting.NewOrchestrator(db, blobs, &hosting.DevBackend{ExecURL: runnerSrv.URL}, limits)

	chainClient, _ := chain.New(ctx, cfg.RPCURL, cfg.ChainID)
	defer chainClient.Close()
	chainReg, _ := chain.NewRegistry(chainClient, cfg.ServiceRegistryAddr)
	ix := indexer.New(chainReg, db)
	regSvc := registry.NewService(db, chainReg, ix)
	discSvc := discovery.New(db)

	signer, _ := receipts.NewSignerFromHex(cfg.ChainID, cfg.ServiceRegistryAddr, cfg.GatewaySigningKey)
	gw := gateway.New(gateway.Config{
		Store: db, Pricing: pricing.New(db), Meter: metering.New(db),
		Wallet: &wallet.DevClient{}, Signer: signer, Quality: quality.New(db),
		Hosting: hostOrchestrator, ChainID: cfg.ChainID,
	})

	srv := server.New(server.Deps{
		Log: telemetry.NewLogger(), Store: db, Chain: chainClient,
		Registry: regSvc, Discovery: discSvc, Gateway: gw, Hosting: hostOrchestrator,
		BlobURL: blobs.URL, DevMode: true, PublishPrivateKey: cfg.PublishPrivateKey,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	manifestRaw, _ := os.ReadFile(filepath.Join("..", "fixtures", "hosted-echo.json"))
	var manifest map[string]any
	_ = json.Unmarshal(manifestRaw, &manifest)
	manifest["owner"] = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	manifest["payout_address"] = manifest["owner"]
	manifest["slug"] = "hosted.echo." + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	body, _ := json.Marshal(map[string]any{"manifest": manifest})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, _ := http.DefaultClient.Do(req)
	var created struct{ ID string `json:"id"` }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	artifactKey := uploadArtifact(t, ts.URL, created.ID)
	depBody, _ := json.Marshal(map[string]any{"artifact_key": artifactKey, "runtime": "node20"})
	depReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services/"+created.ID+"/deploy", bytes.NewReader(depBody))
	depReq.Header.Set("Content-Type", "application/json")
	depReq.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	depResp, _ := http.DefaultClient.Do(depReq)
	if depResp.StatusCode != http.StatusOK {
		t.Fatalf("deploy status %d", depResp.StatusCode)
	}
	depResp.Body.Close()

	pubReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/services/"+created.ID+"/publish", nil)
	pubReq.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	pubResp, _ := http.DefaultClient.Do(pubReq)
	if pubResp.StatusCode != http.StatusOK {
		t.Fatalf("publish status %d", pubResp.StatusCode)
	}
	pubResp.Body.Close()

	callerHeaders := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer test-agent")
		req.Header.Set("X-Caller-DID", "did:matrix:hosted:caller")
		req.Header.Set("X-Caller-Wallet", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	}
	quoteBody, _ := json.Marshal(map[string]any{"operation": "echo", "estimated_units": "1"})
	qReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/quote/"+created.ID, bytes.NewReader(quoteBody))
	qReq.Header.Set("Content-Type", "application/json")
	callerHeaders(qReq)
	qResp, _ := http.DefaultClient.Do(qReq)
	var quote struct{ QuoteID string `json:"quote_id"` }
	_ = json.NewDecoder(qResp.Body).Decode(&quote)
	qResp.Body.Close()

	idem := "hosted-e2e-" + time.Now().Format("150405.000000")
	invokeBody, _ := json.Marshal(map[string]any{
		"operation": "echo", "args": map[string]any{"message": "phase3"},
		"quote_id": quote.QuoteID, "idempotency_key": idem,
		"payment": map[string]any{"rail": "direct"},
	})
	iReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/invoke/"+created.ID, bytes.NewReader(invokeBody))
	iReq.Header.Set("Content-Type", "application/json")
	callerHeaders(iReq)
	iResp, _ := http.DefaultClient.Do(iReq)
	if iResp.StatusCode != http.StatusOK {
		t.Fatalf("invoke status %d", iResp.StatusCode)
	}
	var invoke struct {
		Result     map[string]any `json:"result"`
		ChargedWei string         `json:"charged_wei"`
		Receipt    struct {
			Digest string `json:"digest"`
		} `json:"receipt"`
	}
	_ = json.NewDecoder(iResp.Body).Decode(&invoke)
	iResp.Body.Close()
	if invoke.Result["echo"] != "phase3" {
		t.Fatalf("unexpected result %+v", invoke.Result)
	}
	if invoke.ChargedWei != "200000000000000" || invoke.Receipt.Digest == "" {
		t.Fatalf("invoke payload %+v", invoke)
	}
}

func uploadArtifact(t *testing.T, baseURL, serviceID string) string {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("artifact", "bundle.tar.gz")
	_, _ = io.WriteString(part, "fake-node20-bundle")
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/services/"+serviceID+"/artifacts", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Developer-Wallet", "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("artifact upload status %d", resp.StatusCode)
	}
	var out struct {
		ArtifactKey string `json:"artifact_key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ArtifactKey
}
