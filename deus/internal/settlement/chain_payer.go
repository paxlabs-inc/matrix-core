package settlement

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/paxlabs-inc/deus/internal/chain"
)

// ChainPayer signs payout and anchor transactions with the settler key.
type ChainPayer struct {
	eth        *ethclient.Client
	chainID    *big.Int
	settlerKey *ecdsa.PrivateKey
	anchorAddr common.Address
	channels   *chain.PaymentChannel
	anchor     *chain.SettlementAnchor
}

// NewChainPayer wires on-chain settlement.
func NewChainPayer(c *chain.Client, settlerKeyHex, anchorAddr string) (*ChainPayer, error) {
	keyHex := strings.TrimPrefix(strings.TrimSpace(settlerKeyHex), "0x")
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return nil, fmt.Errorf("settlement: settler key: %w", err)
	}
	ch, err := chain.NewPaymentChannel(c)
	if err != nil {
		return nil, err
	}
	anc, err := chain.NewSettlementAnchor()
	if err != nil {
		return nil, err
	}
	return &ChainPayer{
		eth:        c.Eth(),
		chainID:    c.ChainID(),
		settlerKey: key,
		anchorAddr: common.HexToAddress(anchorAddr),
		channels:   ch,
		anchor:     anc,
	}, nil
}

// PayoutDeveloper is a no-op aggregate hook; use PayoutFromEscrow per caller channel.
func (p *ChainPayer) PayoutDeveloper(ctx context.Context, payoutAddr, amountWei string) (string, error) {
	_ = ctx
	_ = payoutAddr
	_ = amountWei
	return "", fmt.Errorf("settlement: use PayoutFromEscrow for net rail")
}

// PayoutFromEscrow calls PaymentChannel.payout from a caller escrow.
func (p *ChainPayer) PayoutFromEscrow(ctx context.Context, escrowAddr, payee, amountWei string) (string, error) {
	amt, ok := new(big.Int).SetString(amountWei, 10)
	if !ok {
		return "", fmt.Errorf("settlement: invalid payout amount")
	}
	data, err := p.channels.EncodePayout(common.HexToAddress(payee), amt)
	if err != nil {
		return "", err
	}
	return p.send(ctx, common.HexToAddress(escrowAddr), big.NewInt(0), data)
}

// AnchorSettlement emits SettlementAnchor.anchor.
func (p *ChainPayer) AnchorSettlement(ctx context.Context, developerAddr, merkleRoot, totalWei string, count int) (string, error) {
	total, ok := new(big.Int).SetString(totalWei, 10)
	if !ok {
		return "", fmt.Errorf("settlement: invalid anchor total")
	}
	root, err := decodeRoot(merkleRoot)
	if err != nil {
		return "", err
	}
	data, err := p.anchor.EncodeAnchor(common.HexToAddress(developerAddr), root, total, count)
	if err != nil {
		return "", err
	}
	return p.send(ctx, p.anchorAddr, big.NewInt(0), data)
}

func (p *ChainPayer) send(ctx context.Context, to common.Address, value *big.Int, data []byte) (string, error) {
	from := crypto.PubkeyToAddress(p.settlerKey.PublicKey)
	nonce, err := p.eth.PendingNonceAt(ctx, from)
	if err != nil {
		return "", fmt.Errorf("settlement: nonce: %w", err)
	}
	gasPrice, err := p.eth.SuggestGasPrice(ctx)
	if err != nil {
		return "", fmt.Errorf("settlement: gas price: %w", err)
	}
	msg := ethereum.CallMsg{From: from, To: &to, Value: value, Data: data}
	gas, err := p.eth.EstimateGas(ctx, msg)
	if err != nil {
		gas = 300_000
	}
	tx := types.NewTransaction(nonce, to, value, gas, gasPrice, data)
	signed, err := types.SignTx(tx, types.NewEIP155Signer(p.chainID), p.settlerKey)
	if err != nil {
		return "", fmt.Errorf("settlement: sign tx: %w", err)
	}
	if err := p.eth.SendTransaction(ctx, signed); err != nil {
		return "", fmt.Errorf("settlement: broadcast: %w", err)
	}
	return signed.Hash().Hex(), nil
}

func decodeRoot(s string) ([32]byte, error) {
	var root [32]byte
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	b, err := hex.DecodeString(s)
	if err != nil {
		return root, fmt.Errorf("settlement: merkle root: %w", err)
	}
	if len(b) != 32 {
		return root, fmt.Errorf("settlement: merkle root len %d", len(b))
	}
	copy(root[:], b)
	return root, nil
}
