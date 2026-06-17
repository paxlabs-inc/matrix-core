package chain

import (
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Operator is the loaded settlement EVM key: the secp256k1 key that signs the
// operator-only LayerXVault.settle batch txs and the EIP-712 force-exit balance
// proofs (frozen spec [config] LAYERX_OPERATOR_KEY). The key value is
// secret-sourced from the environment at use time and never logged or stored in
// config; only its derived address is exposed.
type Operator struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

// LoadOperator parses a secp256k1 private key from a hex string (with or without
// a 0x prefix). Returns an error on an empty/invalid key so prod boot fails
// closed rather than running keyless.
func LoadOperator(keyHex string) (*Operator, error) {
	h := strings.TrimPrefix(strings.TrimSpace(keyHex), "0x")
	if h == "" {
		return nil, fmt.Errorf("chain: empty operator key")
	}
	key, err := crypto.HexToECDSA(h)
	if err != nil {
		return nil, fmt.Errorf("chain: invalid operator key: %w", err)
	}
	return &Operator{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

// Key returns the underlying private key (for building keyed transactors).
func (o *Operator) Key() *ecdsa.PrivateKey { return o.key }

// Address returns the operator's EVM address (safe to log).
func (o *Operator) Address() common.Address { return o.addr }
