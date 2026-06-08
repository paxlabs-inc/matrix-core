package receipts

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// VoucherFields are EIP-712 DeusVoucher struct fields.
type VoucherFields struct {
	ChannelID       string
	CumulativeWei   string
	Nonce           uint64
	LastReceiptHash string
}

// VoucherDigest returns the EIP-712 digest for a voucher (unsigned).
func (s *Signer) VoucherDigest(f VoucherFields) (string, error) {
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"DeusVoucher": {
				{Name: "channelId", Type: "string"},
				{Name: "cumulativeWei", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "lastReceiptHash", Type: "bytes32"},
			},
		},
		PrimaryType: "DeusVoucher",
		Domain: apitypes.TypedDataDomain{
			Name:              "DeusVoucher",
			Version:           "1",
			ChainId:           math.NewHexOrDecimal256(s.ChainID),
			VerifyingContract: s.VerifyingContract.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"channelId":       f.ChannelID,
			"cumulativeWei":   f.CumulativeWei,
			"nonce":           fmt.Sprintf("%d", f.Nonce),
			"lastReceiptHash": f.LastReceiptHash,
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(hash), nil
}

// VerifyVoucherCaller checks caller signature over voucher digest.
func (s *Signer) VerifyVoucherCaller(digestHex, callerSigHex, callerWallet string) error {
	addr, err := RecoverSigner(digestHex, callerSigHex)
	if err != nil {
		return err
	}
	if !strings.EqualFold(addr.Hex(), callerWallet) {
		return fmt.Errorf("receipts: voucher caller mismatch")
	}
	return nil
}
