package wallet

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/paxlabs-inc/tachyon-tools/internal/config"
)

// anvil account #0
const (
	anvilKey  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilAddr = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
)

func TestNewLocalSignerRaw(t *testing.T) {
	s, err := NewLocalSigner(config.WalletConfig{Signer: config.SignerRaw, PrivateKey: "0x" + anvilKey})
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := s.Address(nil)
	if !strings.EqualFold(addr.Hex(), anvilAddr) {
		t.Fatalf("addr = %s, want %s", addr.Hex(), anvilAddr)
	}
}

func TestNewLocalSignerErrors(t *testing.T) {
	if _, err := NewLocalSigner(config.WalletConfig{Signer: config.SignerRaw}); err == nil {
		t.Error("expected error for missing private key")
	}
	if _, err := NewLocalSigner(config.WalletConfig{Signer: "bogus"}); err == nil {
		t.Error("expected error for unknown signer")
	}
	if _, err := NewLocalSigner(config.WalletConfig{Signer: config.SignerKeystore}); err == nil {
		t.Error("expected error for missing keystore path")
	}
}

func TestAuthorizeProfiles(t *testing.T) {
	g := &Gate{
		Signer: &LocalSigner{},
		Profiles: map[string]Policy{
			"default": {Name: "default", SpendCapWei: big.NewInt(1000), AllowedChains: []string{"paxeer-mainnet"}},
		},
	}

	if _, err := g.Authorize("", nil, "paxeer-mainnet"); err == nil {
		t.Error("empty token should be denied when profiles exist")
	}
	if _, err := g.Authorize("nope", nil, "paxeer-mainnet"); err == nil {
		t.Error("unknown token should be denied")
	}
	p, err := g.Authorize("default", nil, "paxeer-mainnet")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if p.SpendCapWei.Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("spend cap = %v", p.SpendCapWei)
	}

	// request cap can only tighten
	p, _ = g.Authorize("default", big.NewInt(500), "paxeer-mainnet")
	if p.SpendCapWei.Cmp(big.NewInt(500)) != 0 {
		t.Errorf("tightened cap = %v, want 500", p.SpendCapWei)
	}
	p, _ = g.Authorize("default", big.NewInt(5000), "paxeer-mainnet")
	if p.SpendCapWei.Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("cap should not raise above profile: %v", p.SpendCapWei)
	}
}

func TestAuthorizeNoProfiles(t *testing.T) {
	g := &Gate{Signer: &LocalSigner{}}
	p, err := g.Authorize("anything", big.NewInt(7), "chain")
	if err != nil {
		t.Fatalf("no-profile authorize should pass: %v", err)
	}
	if p.SpendCapWei.Cmp(big.NewInt(7)) != 0 {
		t.Errorf("cap = %v", p.SpendCapWei)
	}
}

func TestValidatePolicy(t *testing.T) {
	cap := big.NewInt(1000)
	allowed := common.HexToAddress("0x1111111111111111111111111111111111111111")
	other := "0x2222222222222222222222222222222222222222"

	cases := []struct {
		name   string
		policy Policy
		intent TxIntent
		wantOK bool
	}{
		{"value within cap", Policy{SpendCapWei: cap}, TxIntent{Value: big.NewInt(500)}, true},
		{"value over cap", Policy{SpendCapWei: cap}, TxIntent{Value: big.NewInt(2000)}, false},
		{"chain allowed", Policy{AllowedChains: []string{"a"}, ChainID: "a"}, TxIntent{}, true},
		{"chain denied", Policy{AllowedChains: []string{"a"}, ChainID: "b"}, TxIntent{}, false},
		{"contract allowed", Policy{AllowedContracts: []common.Address{allowed}}, TxIntent{To: allowed.Hex()}, true},
		{"contract denied", Policy{AllowedContracts: []common.Address{allowed}}, TxIntent{To: other}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePolicy(c.policy, c.intent)
			if c.wantOK && err != nil {
				t.Errorf("want ok, got %v", err)
			}
			if !c.wantOK && err == nil {
				t.Error("want denial, got ok")
			}
		})
	}
}

func TestGateNotConfigured(t *testing.T) {
	g := &Gate{}
	if g.Configured() {
		t.Fatal("empty gate should not be configured")
	}
	_, err := g.Sign(nil, nil, TxIntent{}, Policy{})
	if err == nil {
		t.Fatal("expected WALLET_NOT_CONFIGURED")
	}
}

func TestBuildProfiles(t *testing.T) {
	in := map[string]config.PolicyProfile{
		"trader": {SpendCapWei: "100", Allow: []string{"0xabc"}, Chains: []string{"x"}},
	}
	out := buildProfiles(in)
	p, ok := out["trader"]
	if !ok {
		t.Fatal("missing profile")
	}
	if p.SpendCapWei.Cmp(big.NewInt(100)) != 0 || len(p.AllowedContracts) != 1 || len(p.AllowedChains) != 1 {
		t.Errorf("bad profile: %+v", p)
	}
}

func TestNewEmbeddedSignerDID(t *testing.T) {
	dir := t.TempDir()
	keyfile := filepath.Join(dir, "executor.key")
	seed := strings.Repeat("ab", 32) // 64-hex, 32 bytes
	if err := os.WriteFile(keyfile, []byte(seed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewEmbeddedSigner(config.WalletConfig{Keyfile: keyfile, Label: "executor"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.DID(), "did:matrix:executor:") {
		t.Errorf("did = %s", s.DID())
	}
	if fp := strings.TrimPrefix(s.DID(), "did:matrix:executor:"); len(fp) != 16 {
		t.Errorf("keyfp len = %d, want 16", len(fp))
	}
	// deterministic
	s2, _ := NewEmbeddedSigner(config.WalletConfig{Keyfile: keyfile})
	if s.DID() != s2.DID() {
		t.Error("DID not deterministic")
	}
}

func TestNewEmbeddedSignerErrors(t *testing.T) {
	if _, err := NewEmbeddedSigner(config.WalletConfig{}); err == nil {
		t.Error("expected error for missing keyfile")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.key")
	_ = os.WriteFile(bad, []byte("not-hex"), 0o600)
	if _, err := NewEmbeddedSigner(config.WalletConfig{Keyfile: bad}); err == nil {
		t.Error("expected error for invalid seed")
	}
}
