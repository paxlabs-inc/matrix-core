// Package types holds the wire contracts shared across layerxd: the uniform
// response envelope, error codes, the agent-DID auth lane, and the value
// request/response shapes the MCP proxy (tools/layerx/layerx.mjs) speaks.
//
// The envelope mirrors chronosd / tachyond / uwacd ({ok,data,error}) so the MCP
// proxy can branch on ok=false without bespoke parsing. See layerx.frozen.kvx.
package types

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Envelope is the uniform {ok,data,error} response shape.
type Envelope struct {
	Ok    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error is a structured, machine-branchable failure.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// Error codes (stable; the agent may branch on these).
const (
	CodeInvalidRequest    = "invalid_request"
	CodeUnauthorized      = "unauthorized"       // transport/principal auth failed
	CodeNotFound          = "not_found"          // unknown account / receipt seq
	CodeInsufficientFunds = "insufficient_funds" // escrow-bounded spend exceeded
	CodeConflict          = "conflict"
	CodeInternal          = "internal"
)

// OK wraps a success payload.
func OK(data any) Envelope { return Envelope{Ok: true, Data: data} }

// Fail wraps an error.
func Fail(err *Error) Envelope { return Envelope{Ok: false, Error: err} }

// NewError constructs an *Error.
func NewError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// ─── USDX amount ────────────────────────────────────────────────────────────

// MicroPerUSDX is the fixed-point scale: balances are stored as int64 counts of
// micro-USDX (1 USDX = 1_000_000 micro-USDX). Integer math avoids float drift in
// the ledger (the exact precision is a [deferred] decision in the frozen spec;
// micro is the v1 scaffold choice).
const MicroPerUSDX int64 = 1_000_000

// FormatUSDX renders a micro-USDX amount as a decimal USDX string.
func FormatUSDX(micro int64) string {
	neg := micro < 0
	if neg {
		micro = -micro
	}
	whole := micro / MicroPerUSDX
	frac := micro % MicroPerUSDX
	s := fmt.Sprintf("%d.%06d", whole, frac)
	if neg {
		s = "-" + s
	}
	return s
}

// ParseUSDX parses a decimal USDX string into micro-USDX. It is deliberately
// strict — this is a value-bearing parse on agent-supplied input, so it rejects
// trailing garbage, embedded signs, excess precision, and any result that would
// overflow int64. Accepts an optional leading sign, an optional integer part,
// an optional fractional part of up to six digits (e.g. "1", "1.5", ".5",
// "-0.000001").
func ParseUSDX(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	wholeStr, fracStr, hasFrac := strings.Cut(s, ".")
	if wholeStr == "" && (!hasFrac || fracStr == "") {
		return 0, fmt.Errorf("invalid amount %q", s)
	}

	var whole int64
	if wholeStr != "" {
		if !isAllDigits(wholeStr) {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		w, err := strconv.ParseInt(wholeStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("amount %q is out of range", s)
		}
		whole = w
	}

	var frac int64
	if hasFrac && fracStr != "" {
		if len(fracStr) > 6 {
			return 0, fmt.Errorf("amount %q exceeds micro-USDX precision (6 dp)", s)
		}
		if !isAllDigits(fracStr) {
			return 0, fmt.Errorf("invalid fractional amount %q", s)
		}
		f, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid fractional amount %q", s)
		}
		for i := len(fracStr); i < 6; i++ {
			f *= 10
		}
		frac = f
	}

	// Overflow-safe: whole*MicroPerUSDX + frac must fit in int64.
	if whole > (math.MaxInt64-frac)/MicroPerUSDX {
		return 0, fmt.Errorf("amount %q is out of range", s)
	}
	micro := whole*MicroPerUSDX + frac
	if neg {
		micro = -micro
	}
	return micro, nil
}

// isAllDigits reports whether s is non-empty and every rune is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Settlement tiers (layerx.frozen.kvx [settlement.tiers]).
const (
	TierMicropayment = "micropayment" // < micro threshold: net-batched on the window
	TierMaterial     = "material"     // >= threshold: force-settled
)

// ─── domain records ──────────────────────────────────────────────────────────

// Account is an agent's LayerX account, keyed by DID (the DID IS the account).
type Account struct {
	DID         string    `json:"did"`
	EVMAddress  string    `json:"evm_address,omitempty"`
	BalanceUSDX int64     `json:"-"` // micro-USDX
	EscrowUSDX  int64     `json:"-"` // micro-USDX
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Transfer is one accepted value movement — a Merkle leaf in its batch.
type Transfer struct {
	Seq         int64     `json:"seq"` // strict monotonic sequencer ordering key
	BatchID     string    `json:"batch_id,omitempty"`
	FromDID     string    `json:"from_did"`
	ToDID       string    `json:"to_did"`
	AmountUSDX  int64     `json:"-"` // micro-USDX
	Tier        string    `json:"tier"`
	LeafHashHex string    `json:"leaf_hash"`
	SigHex      string    `json:"sig,omitempty"`
	TS          time.Time `json:"ts"`
}

// Receipt is the signed, Merkle-anchored proof of a transfer
// (layerx.frozen.kvx [receipts]).
type Receipt struct {
	Seq           int64     `json:"seq"`
	BatchID       string    `json:"batch_id,omitempty"`
	FromDID       string    `json:"from_did"`
	ToDID         string    `json:"to_did"`
	AmountUSDX    string    `json:"amount_usdx"` // decimal USDX
	Tier          string    `json:"tier"`
	TS            time.Time `json:"ts"`
	LeafHashHex   string    `json:"leaf_hash"`
	SequencerSig  string    `json:"sequencer_sig"`
	SequencerKey  string    `json:"sequencer_pubkey"`
	BatchRootHex  string    `json:"batch_root,omitempty"`     // set once the batch is sealed
	InclusionPath []string  `json:"inclusion_path,omitempty"` // set once the batch is sealed
	AnchorTxHash  string    `json:"anchor_tx,omitempty"`      // set once anchored on Paxeer
	Settled       bool      `json:"settled"`
}

// ─── auth lane ───────────────────────────────────────────────────────────────

// ChallengeRequest opens the agent-DID principal-auth lane.
type ChallengeRequest struct {
	DID string `json:"did"`
}

// ChallengeResponse carries the exact bytes the agent must ed25519-sign.
type ChallengeResponse struct {
	DID       string `json:"did"`
	Nonce     string `json:"nonce"`
	Message   string `json:"message"`
	ExpiresIn int    `json:"expires_in"`
}

// VerifyRequest proves possession of the DID's key over the challenge.
type VerifyRequest struct {
	DID       string `json:"did"`
	PublicKey string `json:"public_key"` // hex(ed25519 pubkey)
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"` // hex(ed25519 signature)
}

// VerifyResponse returns a short-lived principal token bound to the account DID.
type VerifyResponse struct {
	Token     string `json:"token"`
	DID       string `json:"did"`
	ExpiresIn int    `json:"expires_in"`
}

// ─── value API request/response shapes ───────────────────────────────────────

// BalanceResponse is GET /v1/balance.
type BalanceResponse struct {
	DID         string `json:"did"`
	EVMAddress  string `json:"evm_address,omitempty"`
	BalanceUSDX string `json:"balance_usdx"`
	EscrowUSDX  string `json:"escrow_usdx"`
}

// PayRequest is POST /v1/pay (and the layerx_pay tool args). Signed by the
// payer DID; the sig authorizes the debit.
type PayRequest struct {
	ToDID      string `json:"to_did"`
	AmountUSDX string `json:"amount_usdx"` // decimal USDX
	Nonce      string `json:"nonce,omitempty"`
	Signature  string `json:"signature,omitempty"` // hex(ed25519) over the canonical intent
}

// WithdrawRequest is POST /v1/withdraw.
type WithdrawRequest struct {
	AmountUSDX string `json:"amount_usdx"`
	SwapOut    string `json:"swap_out,omitempty"` // "" = USDL, else target asset symbol
	Nonce      string `json:"nonce,omitempty"`
	Signature  string `json:"signature,omitempty"`
}

// WithdrawResponse acknowledges a queued withdrawal.
type WithdrawResponse struct {
	WithdrawalID string `json:"withdrawal_id"`
	AmountUSDX   string `json:"amount_usdx"`
	Tier         string `json:"settlement_tier"`
	Status       string `json:"status"`
}

// DepositResponse tells the agent where + how to fund its account on-chain.
type DepositResponse struct {
	VaultAddress string `json:"vault_address"`
	ReserveAsset string `json:"reserve_asset"` // USDL
	DIDClaim     string `json:"did_claim"`     // payload to include in the on-chain deposit
	Note         string `json:"note"`
}

// SettleResponse acknowledges a force-settle request.
type SettleResponse struct {
	BatchID string `json:"batch_id"`
	Status  string `json:"status"`
	Note    string `json:"note"`
}
