//go:build integration

package e2e_test

import (
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/internal/metering"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/quality"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/settlement"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

func mustBigInt(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("invalid big int: " + s)
	}
	return v
}

func callerDevKey() string {
	// Anvil mnemonic index 1 (test junk mnemonic).
	return "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
}

func buildGateway(db *store.Store, signer *receipts.Signer, wal wallet.Client) (*gateway.Gateway, *settlement.Settler, *settlement.DevPayer) {
	chSvc := channels.New(db, wal)
	vSvc := channels.NewVoucherService(db, signer)
	gw := gateway.New(gateway.Config{
		Store:    db,
		Pricing:  pricing.New(db),
		Meter:    metering.New(db),
		Wallet:   wal,
		Signer:   signer,
		Quality:  quality.New(db),
		Channels: chSvc,
		Vouchers: vSvc,
		ChainID:  31337,
	})
	payer := &settlement.DevPayer{}
	return gw, settlement.NewSettler(db, payer, chSvc), payer
}

func signDigestHex(digestHex, privateKeyHex string) (string, error) {
	keyHex := strings.TrimPrefix(privateKeyHex, "0x")
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return "", err
	}
	digestHex = strings.TrimPrefix(digestHex, "0x")
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return "", err
	}
	sig, err := crypto.Sign(digest, key)
	if err != nil {
		return "", err
	}
	if sig[64] < 27 {
		sig[64] += 27
	}
	return "0x" + hex.EncodeToString(sig), nil
}
