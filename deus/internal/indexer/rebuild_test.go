package indexer_test

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/store"
)

func TestMirrorRebuildDeterministic(t *testing.T) {
	if os.Getenv("DEUS_RUN_ANVIL_TESTS") != "1" {
		t.Skip("set DEUS_RUN_ANVIL_TESTS=1 to run anvil integration tests")
	}
	ctx := context.Background()
	anvil := startAnvil(t)
	defer anvil.Cmd.Process.Kill()

	registryAddr := deployRegistry(t, anvil.RPC)
	key, _ := crypto.GenerateKey()
	owner := crypto.PubkeyToAddress(key.PublicKey)
	privBytes := crypto.FromECDSA(key)
	priv := "0x" + hex.EncodeToString(privBytes)

	dbURI := os.Getenv("DEUS_POSTGRES_URI")
	if dbURI == "" {
		dbURI = "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable"
	}
	st, err := store.New(ctx, dbURI)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	mig := filepath.Join("..", "..", "migrations")
	if err := st.Migrate(ctx, mig); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	c, err := chain.New(ctx, anvil.RPC, 31337)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer c.Close()
	reg, err := chain.NewRegistry(c, registryAddr)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ix := indexer.New(reg, st)

	var mh, ph [32]byte
	copy(mh[:], crypto.Keccak256([]byte("manifest-a")))
	copy(ph[:], crypto.Keccak256([]byte("pricing-a")))

	_, err = reg.Register(ctx, chain.RegisterRequest{
		Payout:        owner,
		ManifestHash:  mh,
		PricingHash:   ph,
		Hosted:        false,
		Confidential:  false,
		PrivateKeyHex: priv,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := ix.ReplayFrom(ctx, 0); err != nil {
		t.Fatalf("replay: %v", err)
	}
	n1, err := indexer.MirrorCount(ctx, st)
	if err != nil || n1 < 1 {
		t.Fatalf("mirror count after replay: n=%d err=%v", n1, err)
	}
	if err := ix.ReplayFrom(ctx, 0); err != nil {
		t.Fatalf("replay second: %v", err)
	}
	n2, err := indexer.MirrorCount(ctx, st)
	if err != nil {
		t.Fatalf("mirror count: %v", err)
	}
	if n2 < n1 {
		t.Fatalf("mirror count decreased: %d -> %d", n1, n2)
	}
}

type anvilProc struct {
	Cmd *exec.Cmd
	RPC string
}

func startAnvil(t *testing.T) anvilProc {
	t.Helper()
	port := "8545"
	rpc := "http://127.0.0.1:" + port
	cmd := exec.Command("anvil", "--port", port, "--chain-id", "31337")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("anvil start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := chain.New(context.Background(), rpc, 31337)
		if err == nil {
			c.Close()
			return anvilProc{Cmd: cmd, RPC: rpc}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatal("anvil not ready")
	return anvilProc{}
}

func deployRegistry(t *testing.T, rpc string) string {
	t.Helper()
	gov := "0x00000000000000000000000000000000000000a1"
	cmd := exec.Command("forge", "create",
		"src/ServiceRegistry.sol:ServiceRegistry",
		"--broadcast",
		"--rpc-url", rpc,
		"--private-key", "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
		"--constructor-args", gov,
	)
	cmd.Dir = filepath.Join("..", "..", "contracts")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deploy registry: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Deployed to:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Deployed to:"))
		}
	}
	t.Fatalf("deploy output missing address: %s", out)
	return ""
}
