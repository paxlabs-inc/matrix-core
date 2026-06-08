package receipts_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/paxlabs-inc/deus/internal/receipts"
)

func TestVoucherCallerVerify(t *testing.T) {
	const callerKey = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	const callerWallet = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

	signer, err := receipts.NewSignerFromHex(31337, "0x5FbDB2315678afecb367f032d93F642f64180aa3", "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := signer.VoucherDigest(receipts.VoucherFields{
		ChannelID:       "550e8400-e29b-41d4-a716-446655440000",
		CumulativeWei:   "200000000000000",
		Nonce:           1,
		LastReceiptHash: "0x1234567890123456789012345678901234567890123456789012345678901234",
	})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signDigest(digest, callerKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.VerifyVoucherCaller(digest, sig, callerWallet); err != nil {
		t.Fatalf("verify: %v digest=%s sig=%s", err, digest, sig)
	}
}

func signDigest(digestHex, privateKeyHex string) (string, error) {
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
