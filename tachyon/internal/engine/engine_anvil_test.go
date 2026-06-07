package engine

import (
	"context"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paxlabs-inc/tachyon-tools/internal/compiler"
	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/registry"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Anvil's first well-known dev account (deterministic mnemonic). Funded with
// 10000 ETH on a fresh chain; never used outside a throwaway local node.
const (
	anvilDevKey  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilChainID = 31337
)

// boxRuntime is a minimal SimpleStorage contract supporting:
//
//	store(uint256)  selector 0x6057361d -> SSTORE slot 0
//	retrieve()      selector 0x2e64cec1 -> SLOAD slot 0, return 32 bytes
//
// boxCreation is the init code that CODECOPYs the runtime and returns it.
const (
	boxRuntime  = "60003560e01c80636057361d14601a57632e64cec114602257005b600435600055005b60005460005260206000f3"
	boxCreation = "602e600c600039602e6000f3" + boxRuntime
)

const boxABI = `[
	{"type":"function","name":"store","inputs":[{"name":"v","type":"uint256"}],"outputs":[]},
	{"type":"function","name":"retrieve","inputs":[],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"}
]`

// TestAnvilDeployCallReceipt exercises the full broadcast path against a live
// local Anvil node: deploy a contract, broadcast a state-changing call encoded
// from method+args, then read the value back via a simulated (eth_call) read.
// Skips when the `anvil` binary is not on PATH.
func TestAnvilDeployCallReceipt(t *testing.T) {
	anvilBin, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil not installed; skipping live broadcast integration test")
	}

	port := freePort(t)
	rpcURL := "http://127.0.0.1:" + port

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, anvilBin,
		"--port", port,
		"--chain-id", "31337",
		"--silent",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start anvil: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	root := t.TempDir()
	cfg := config.Config{
		ProjectRoot:  root,
		ArtifactsDir: "artifacts",
		RegistryPath: filepath.Join(root, "registry.json"),
		ForgePath:    "forge",
		Wallet: config.WalletConfig{
			Mode:       config.WalletModeSelfHosted,
			Signer:     config.SignerRaw,
			PrivateKey: anvilDevKey,
		},
		Chains: []config.ChainConfig{
			{ID: "anvil-it", Name: "Anvil IT", RPCURL: rpcURL, ChainID: anvilChainID},
		},
	}

	e, err := New(cfg)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if !e.Wallet.Configured() {
		t.Fatalf("expected a configured signer in self_hosted raw mode")
	}

	waitForRPC(t, rpcURL)

	// Inject the precompiled Box artifact the deployer will look up by name.
	projectID := compiler.ProjectID(root)
	if err := e.Reg.PutArtifact(registry.ArtifactRecord{
		ProjectID: projectID,
		Name:      "Box",
		ABI:       []byte(boxABI),
		Bytecode:  "0x" + boxCreation,
	}); err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	// 1) Deploy -> receipt (creation tx, contract address from receipt).
	depEnv := e.Deploy(ctx, types.DeployRequest{
		IdempotencyKey: "anvil-it-box-1",
		ChainID:        "anvil-it",
		Contract:       "Box",
	})
	if !depEnv.Ok {
		t.Fatalf("deploy failed: %+v", depEnv.Error)
	}
	addr := depEnv.Data.Address
	if addr == "" || depEnv.Data.TxHash == "" {
		t.Fatalf("deploy returned empty address/txhash: %+v", depEnv.Data)
	}

	// 2) Broadcast a state-changing call, calldata ABI-encoded from method+args.
	const want = "424242"
	callEnv := e.Call(ctx, types.CallRequest{
		ChainID: "anvil-it",
		To:      addr,
		ABI:     []byte(boxABI),
		Method:  "store",
		Args:    []interface{}{want},
	})
	if !callEnv.Ok {
		t.Fatalf("broadcast call failed: %+v", callEnv.Error)
	}
	if callEnv.Data.TxHash == "" {
		t.Fatalf("broadcast call returned no tx hash: %+v", callEnv.Data)
	}

	// 3) Read the value back via a simulated (eth_call) read; poll for the
	// mined state. Anvil auto-mines instantly, so this settles quickly.
	wantBig, _ := new(big.Int).SetString(want, 10)
	deadline := time.Now().Add(15 * time.Second)
	var lastResult string
	for {
		readEnv := e.Call(ctx, types.CallRequest{
			ChainID:      "anvil-it",
			To:           addr,
			ABI:          []byte(boxABI),
			Method:       "retrieve",
			SimulateOnly: true,
		})
		if readEnv.Ok {
			lastResult = readEnv.Data.Result
			if got, ok := new(big.Int).SetString(strings.TrimPrefix(lastResult, "0x"), 16); ok && got.Cmp(wantBig) == 0 {
				return // success: round-trip deploy -> broadcast -> read confirmed
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("retrieve did not return stored value %s; last result=%q (ok=%v err=%+v)",
				want, lastResult, readEnv.Ok, readEnv.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// freePort reserves an ephemeral TCP port and returns it as a string. The
// listener is closed immediately; anvil rebinds it.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	return port
}

// waitForRPC blocks until the JSON-RPC endpoint answers eth_chainId, or the
// test times out.
func waitForRPC(t *testing.T, rpcURL string) {
	t.Helper()
	const body = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Post(rpcURL, "application/json", strings.NewReader(body))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("anvil RPC at %s never became ready", rpcURL)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
