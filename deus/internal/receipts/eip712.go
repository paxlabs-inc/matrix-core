// Package receipts builds and verifies EIP-712 signed quotes and receipts (docs/04-onchain.md §4.5).
package receipts

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Signer holds gateway signing material for quotes and receipts.
type Signer struct {
	ChainID            int64
	VerifyingContract  common.Address
	PrivateKey         *ecdsa.PrivateKey
}

// NewSignerFromHex parses a gateway private key and registry address.
func NewSignerFromHex(chainID int64, registryAddr, keyHex string) (*Signer, error) {
	keyHex = strings.TrimPrefix(strings.TrimSpace(keyHex), "0x")
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return nil, fmt.Errorf("receipts: invalid signing key: %w", err)
	}
	return &Signer{
		ChainID:           chainID,
		VerifyingContract: common.HexToAddress(registryAddr),
		PrivateKey:        key,
	}, nil
}

// GatewayAddress returns the signer address.
func (s *Signer) GatewayAddress() common.Address {
	return crypto.PubkeyToAddress(s.PrivateKey.PublicKey)
}

// QuoteFields are the EIP-712 DeusQuote struct fields.
type QuoteFields struct {
	ServiceID       string
	EndpointID      string
	PricingVersion  int
	UnitPriceWei    string
	MaxUnits        string
	Caller          string
	ExpiresAt       time.Time
}

// SignQuote returns digest hex and signature hex.
func (s *Signer) SignQuote(f QuoteFields) (digest string, sig string, err error) {
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"DeusQuote": {
				{Name: "serviceId", Type: "string"},
				{Name: "endpoint", Type: "string"},
				{Name: "pricingVersion", Type: "uint256"},
				{Name: "unitPriceWei", Type: "uint256"},
				{Name: "maxUnits", Type: "uint256"},
				{Name: "caller", Type: "string"},
				{Name: "expiresAt", Type: "uint256"},
			},
		},
		PrimaryType: "DeusQuote",
		Domain: apitypes.TypedDataDomain{
			Name:              "DeusQuote",
			Version:           "1",
			ChainId:           math.NewHexOrDecimal256(s.ChainID),
			VerifyingContract: s.VerifyingContract.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"serviceId":      f.ServiceID,
			"endpoint":       f.EndpointID,
			"pricingVersion": fmt.Sprintf("%d", f.PricingVersion),
			"unitPriceWei":   f.UnitPriceWei,
			"maxUnits":       f.MaxUnits,
			"caller":         f.Caller,
			"expiresAt":      fmt.Sprintf("%d", f.ExpiresAt.Unix()),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", "", err
	}
	signature, err := crypto.Sign(hash, s.PrivateKey)
	if err != nil {
		return "", "", err
	}
	if signature[64] < 27 {
		signature[64] += 27
	}
	return "0x" + hex.EncodeToString(hash), "0x" + hex.EncodeToString(signature), nil
}

// VerifyQuote checks a quote signature recovers to the gateway address.
func (s *Signer) VerifyQuote(digestHex, sigHex string) error {
	got, err := RecoverSigner(digestHex, sigHex)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got.Hex(), s.GatewayAddress().Hex()) {
		return fmt.Errorf("receipts: quote signer mismatch")
	}
	return nil
}

// ReceiptFields are the EIP-712 DeusReceipt struct fields.
type ReceiptFields struct {
	InvocationID string
	ServiceID    string
	Caller       string
	ArgsHash     string
	ResultHash   string
	PriceWei     string
	Units        string
	Outcome      string
	Timestamp    time.Time
}

// SignReceipt returns digest hex and signature hex.
func (s *Signer) SignReceipt(f ReceiptFields) (digest string, sig string, err error) {
	typed := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"DeusReceipt": {
				{Name: "invocationId", Type: "string"},
				{Name: "serviceId", Type: "string"},
				{Name: "caller", Type: "string"},
				{Name: "argsHash", Type: "bytes32"},
				{Name: "resultHash", Type: "bytes32"},
				{Name: "priceWei", Type: "uint256"},
				{Name: "units", Type: "uint256"},
				{Name: "outcome", Type: "string"},
				{Name: "ts", Type: "uint256"},
			},
		},
		PrimaryType: "DeusReceipt",
		Domain: apitypes.TypedDataDomain{
			Name:              "DeusReceipt",
			Version:           "1",
			ChainId:           math.NewHexOrDecimal256(s.ChainID),
			VerifyingContract: s.VerifyingContract.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"invocationId": f.InvocationID,
			"serviceId":    f.ServiceID,
			"caller":       f.Caller,
			"argsHash":     f.ArgsHash,
			"resultHash":   f.ResultHash,
			"priceWei":     f.PriceWei,
			"units":        f.Units,
			"outcome":      f.Outcome,
			"ts":           fmt.Sprintf("%d", f.Timestamp.Unix()),
		},
	}
	hash, _, err := apitypes.TypedDataAndHash(typed)
	if err != nil {
		return "", "", err
	}
	signature, err := crypto.Sign(hash, s.PrivateKey)
	if err != nil {
		return "", "", err
	}
	if signature[64] < 27 {
		signature[64] += 27
	}
	return "0x" + hex.EncodeToString(hash), "0x" + hex.EncodeToString(signature), nil
}

// RecoverSigner returns the address that signed digest with sig.
func RecoverSigner(digestHex, sigHex string) (common.Address, error) {
	digestHex = strings.TrimPrefix(digestHex, "0x")
	sigHex = strings.TrimPrefix(sigHex, "0x")
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return common.Address{}, err
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return common.Address{}, err
	}
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("receipts: invalid signature length")
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pub, err := crypto.SigToPub(digest, sig)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// HashPayload returns keccak256(canonical_json(v)) as 0x-prefixed hex.
func HashPayload(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := crypto.Keccak256Hash(b)
	return sum.Hex(), nil
}

// WeiString formats *big.Int as decimal string for EIP-712 uint256 fields.
func WeiString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.Text(10)
}
