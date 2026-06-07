package wallet

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/evm"
	ttypes "github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// TxIntent is the chain-agnostic description of a transaction to sign or broadcast.
type TxIntent struct {
	From  string // optional; signers may fill it in
	To    string // "" => contract creation
	Data  []byte
	Value *big.Int
	Gas   uint64 // 0 => estimate
}

// SignResult is returned by a Signer. Exactly one of RawTx / TxHash is populated:
//   - RawTx: a locally signed transaction the caller must broadcast.
//   - TxHash: the signer already broadcast (remote send); caller waits for receipt.
type SignResult struct {
	RawTx  []byte
	TxHash string
	From   common.Address
}

// Policy is the effective capability profile enforced before signing.
type Policy struct {
	Name             string
	SpendCapWei      *big.Int
	AllowedContracts []common.Address
	AllowedChains    []string
	ChainID          string
}

// Signer produces a broadcastable result for a transaction intent.
type Signer interface {
	// Sign may use client (nonce/gas/fees) for local signing; remote signers
	// that own custody and broadcast server-side may ignore it.
	Sign(ctx context.Context, client *evm.Client, intent TxIntent) (SignResult, error)
	// Address returns the signer's address when known (best effort).
	Address(ctx context.Context) (common.Address, error)
}

// Gate enforces a named policy profile before delegating to the signer.
type Gate struct {
	Signer   Signer
	Profiles map[string]Policy
}

// Configured reports whether a signer is available.
func (g *Gate) Configured() bool { return g != nil && g.Signer != nil }

// Authorize resolves a capability token to an effective policy. When profiles
// are configured, an unknown token is denied. requestCap can only tighten the
// profile's spend cap, never raise it.
func (g *Gate) Authorize(token string, requestCap *big.Int, chainID string) (Policy, *ttypes.Error) {
	p := Policy{ChainID: chainID}
	if g != nil && len(g.Profiles) > 0 {
		if token == "" {
			return Policy{}, ttypes.NewError(ttypes.CodeWalletDenied, "capability_token required: no profile specified", false, nil)
		}
		prof, ok := g.Profiles[token]
		if !ok {
			return Policy{}, ttypes.NewError(ttypes.CodeWalletDenied, "unknown capability profile: "+token, false, nil)
		}
		p = prof
		p.ChainID = chainID
	}
	p.Name = token
	if requestCap != nil && (p.SpendCapWei == nil || requestCap.Cmp(p.SpendCapWei) < 0) {
		p.SpendCapWei = requestCap
	}
	return p, nil
}

// Sign validates the policy against the intent and delegates to the signer.
func (g *Gate) Sign(ctx context.Context, client *evm.Client, intent TxIntent, policy Policy) (SignResult, *ttypes.Error) {
	if !g.Configured() {
		return SignResult{}, ttypes.NewError(ttypes.CodeWalletNotConfigured, "no signer configured", false, nil)
	}
	if err := validatePolicy(policy, intent); err != nil {
		return SignResult{}, ttypes.NewError(ttypes.CodeWalletDenied, err.Error(), false, nil)
	}
	res, signErr := g.Signer.Sign(ctx, client, intent)
	if signErr != nil {
		return SignResult{}, ttypes.NewError(ttypes.CodeWalletDenied, signErr.Error(), true, nil)
	}
	return res, nil
}

func validatePolicy(p Policy, intent TxIntent) error {
	if p.SpendCapWei != nil && intent.Value != nil && intent.Value.Cmp(p.SpendCapWei) > 0 {
		return fmt.Errorf("tx value exceeds spend_cap_wei")
	}
	if len(p.AllowedChains) > 0 && p.ChainID != "" {
		ok := false
		for _, c := range p.AllowedChains {
			if c == p.ChainID {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("chain %q not in profile allow-list", p.ChainID)
		}
	}
	if len(p.AllowedContracts) > 0 && strings.TrimSpace(intent.To) != "" {
		to := common.HexToAddress(intent.To)
		ok := false
		for _, a := range p.AllowedContracts {
			if a == to {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("destination not in allowed_contracts")
		}
	}
	return nil
}

// NewGate builds a policy gate + signer from configuration. Returns a nil-signer
// gate (no error) when no wallet mode is configured, so read-only verbs still work.
func NewGate(cfg config.Config) (*Gate, error) {
	profiles := buildProfiles(cfg.Policies)
	switch cfg.Wallet.Mode {
	case config.WalletModeSelfHosted:
		s, err := NewLocalSigner(cfg.Wallet)
		if err != nil {
			return nil, err
		}
		return &Gate{Signer: s, Profiles: profiles}, nil
	case config.WalletModeEmbedded:
		s, err := NewEmbeddedSigner(cfg.Wallet)
		if err != nil {
			return nil, err
		}
		return &Gate{Signer: s, Profiles: profiles}, nil
	default:
		return &Gate{Profiles: profiles}, nil
	}
}

func buildProfiles(in map[string]config.PolicyProfile) map[string]Policy {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Policy, len(in))
	for name, pp := range in {
		p := Policy{Name: name, AllowedChains: pp.Chains}
		if cap, err := ParseSpendCap(pp.SpendCapWei); err == nil {
			p.SpendCapWei = cap
		}
		for _, a := range pp.Allow {
			if strings.TrimSpace(a) != "" {
				p.AllowedContracts = append(p.AllowedContracts, common.HexToAddress(a))
			}
		}
		out[name] = p
	}
	return out
}

// LocalSigner signs transactions with an operator-held key. Self-host mode.
// The key is sourced per config.WalletConfig.Signer: raw hex, an env-var
// reference (resolved upstream), or a decrypted web3 keystore.
type LocalSigner struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

// NewLocalSigner loads the signing key for self-hosted mode.
func NewLocalSigner(w config.WalletConfig) (*LocalSigner, error) {
	switch w.Signer {
	case config.SignerKeystore:
		if w.KeystorePath == "" {
			return nil, fmt.Errorf("wallet.self_hosted.keystore_path required for keystore signer")
		}
		blob, err := os.ReadFile(w.KeystorePath)
		if err != nil {
			return nil, fmt.Errorf("read keystore: %w", err)
		}
		k, err := keystore.DecryptKey(blob, w.KeystorePassword)
		if err != nil {
			return nil, fmt.Errorf("decrypt keystore: %w", err)
		}
		return &LocalSigner{key: k.PrivateKey, addr: crypto.PubkeyToAddress(k.PrivateKey.PublicKey)}, nil
	case config.SignerRaw, config.SignerEnv, "":
		hexKey := strings.TrimPrefix(strings.TrimSpace(w.PrivateKey), "0x")
		if hexKey == "" {
			return nil, fmt.Errorf("wallet.self_hosted.private_key required for %q signer", w.Signer)
		}
		key, err := crypto.HexToECDSA(hexKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private_key: %w", err)
		}
		return &LocalSigner{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
	default:
		return nil, fmt.Errorf("unknown self_hosted signer %q (want raw|keystore|env)", w.Signer)
	}
}

// Address returns the local signer's address.
func (s *LocalSigner) Address(context.Context) (common.Address, error) { return s.addr, nil }

// Sign builds the transaction from chain state and signs it locally.
func (s *LocalSigner) Sign(ctx context.Context, client *evm.Client, intent TxIntent) (SignResult, error) {
	if client == nil {
		return SignResult{}, fmt.Errorf("local signer requires a chain client")
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return SignResult{}, fmt.Errorf("chain id: %w", err)
	}
	var to *common.Address
	if strings.TrimSpace(intent.To) != "" {
		a := common.HexToAddress(intent.To)
		to = &a
	}
	tx, err := client.BuildTx(ctx, evm.TxParams{
		From:  s.addr,
		To:    to,
		Data:  intent.Data,
		Value: intent.Value,
		Gas:   intent.Gas,
	})
	if err != nil {
		return SignResult{}, err
	}
	raw, from, err := evm.SignTxKey(tx, chainID, s.key)
	if err != nil {
		return SignResult{}, err
	}
	return SignResult{RawTx: raw, From: from}, nil
}

// ParseSpendCap parses wei string for policy.
func ParseSpendCap(s string) (*big.Int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("invalid spend_cap_wei")
	}
	return v, nil
}
