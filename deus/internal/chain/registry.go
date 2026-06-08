package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/paxlabs-inc/deus/internal/chain/bindings"
)

// Registry submits ServiceRegistry transactions.
type Registry struct {
	client   *ethclient.Client
	contract *bindings.ServiceRegistry
	address  common.Address
	chainID  *big.Int
}

// NewRegistry binds a deployed ServiceRegistry at addr.
func NewRegistry(c *Client, addr string) (*Registry, error) {
	if c == nil || c.Eth() == nil {
		return nil, fmt.Errorf("chain: nil client")
	}
	if addr == "" {
		return nil, fmt.Errorf("chain: empty registry address")
	}
	contract, err := bindings.NewServiceRegistry(common.HexToAddress(addr), c.Eth())
	if err != nil {
		return nil, fmt.Errorf("chain: bind registry: %w", err)
	}
	return &Registry{
		client:   c.Eth(),
		contract: contract,
		address:  common.HexToAddress(addr),
		chainID:  c.ChainID(),
	}, nil
}

// Address returns the registry contract address.
func (r *Registry) Address() common.Address {
	return r.address
}

// RegisterRequest is the on-chain register() payload.
type RegisterRequest struct {
	Payout        common.Address
	ManifestHash  [32]byte
	PricingHash   [32]byte
	Hosted        bool
	Confidential  bool
	PrivateKeyHex string
}

// RegisterResult is the mined register transaction outcome.
type RegisterResult struct {
	ChainServiceID uint64
	TxHash         string
	BlockNumber    uint64
}

// Register sends register() from the owner key and waits for the receipt.
func (r *Registry) Register(ctx context.Context, req RegisterRequest) (RegisterResult, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(req.PrivateKeyHex, "0x"))
	if err != nil {
		return RegisterResult{}, fmt.Errorf("chain: private key: %w", err)
	}
	auth, err := bind.NewKeyedTransactorWithChainID(key, r.chainID)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("chain: transactor: %w", err)
	}
	auth.Context = ctx

	tx, err := r.contract.Register(auth, req.Payout, req.ManifestHash, req.PricingHash, req.Hosted, req.Confidential)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("chain: register tx: %w", err)
	}

	receipt, err := bind.WaitMined(ctx, r.client, tx)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("chain: wait mined: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return RegisterResult{}, fmt.Errorf("chain: register reverted")
	}

	parsed, err := bindings.NewServiceRegistry(r.address, r.client)
	if err != nil {
		return RegisterResult{}, err
	}
	var result RegisterResult
	result.TxHash = tx.Hash().Hex()
	result.BlockNumber = receipt.BlockNumber.Uint64()

	for _, lg := range receipt.Logs {
		if lg.Address != r.address {
			continue
		}
		ev, err := parsed.ParseServiceRegistered(*lg)
		if err != nil {
			continue
		}
		result.ChainServiceID = ev.Id.Uint64()
		return result, nil
	}
	return RegisterResult{}, fmt.Errorf("chain: ServiceRegistered event not found")
}

// FilterRegistered returns ServiceRegistered logs from fromBlock through latest.
func (r *Registry) FilterRegistered(ctx context.Context, fromBlock int64) ([]bindings.ServiceRegistryServiceRegistered, error) {
	opts := &bind.FilterOpts{Start: uint64(fromBlock), Context: ctx}
	it, err := r.contract.FilterServiceRegistered(opts, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: filter registered: %w", err)
	}
	defer it.Close()
	var out []bindings.ServiceRegistryServiceRegistered
	for it.Next() {
		out = append(out, *it.Event)
	}
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("chain: filter iter: %w", err)
	}
	return out, nil
}
