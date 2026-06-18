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
	CodeRateLimited       = "rate_limited" // per-client request rate exceeded
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

// PayRequest is POST /v1/pay (and the layerx_pay tool args).
//
// Two authorization paths (PHASE 4): present the X-LayerX-Agent principal token
// (convenience), OR authorize the write by a DID-signed intent ALONE — set
// from_did/public_key/nonce/signature where the signature is ed25519 over the
// canonical pay-intent bytes (invariant i6: the signature IS the authorization).
type PayRequest struct {
	ToDID      string `json:"to_did"`
	AmountUSDX string `json:"amount_usdx"` // decimal USDX
	FromDID    string `json:"from_did,omitempty"`
	PublicKey  string `json:"public_key,omitempty"` // hex(ed25519 pubkey) for the signed-intent path
	Nonce      string `json:"nonce,omitempty"`      // server-issued challenge nonce (single-use)
	Signature  string `json:"signature,omitempty"`  // hex(ed25519) over the canonical pay intent
}

// WithdrawRequest is POST /v1/withdraw. Same dual authorization as PayRequest.
type WithdrawRequest struct {
	AmountUSDX string `json:"amount_usdx"`
	SwapOut    string `json:"swap_out,omitempty"` // "" = USDL, else target asset symbol
	FromDID    string `json:"from_did,omitempty"`
	PublicKey  string `json:"public_key,omitempty"`
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

// BindEVMRequest is POST /v1/account/evm: register/update the caller DID's
// mapped Paxeer payout address (spec [usdx.account].binding).
type BindEVMRequest struct {
	EVMAddress string `json:"evm_address"`
}

// BindEVMResponse confirms the DID -> payout-address binding.
type BindEVMResponse struct {
	DID        string `json:"did"`
	EVMAddress string `json:"evm_address"`
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

// ─── public RPC / explorer shapes (PHASE 4) ──────────────────────────────────

// InfoResponse is GET /v1/info: the public "RPC info" call describing the
// sequencer + its on-chain wiring so a client can verify receipts independently.
type InfoResponse struct {
	Service            string `json:"service"`
	Version            string `json:"version"`
	ChainID            int64  `json:"chain_id"`
	VaultAddress       string `json:"vault_address,omitempty"`
	AnchorAddress      string `json:"anchor_address,omitempty"`
	USDLAddress        string `json:"usdl_address,omitempty"`
	DEXRouter          string `json:"dex_router,omitempty"`
	ReserveAsset       string `json:"reserve_asset"`
	SequencerPubkey    string `json:"sequencer_pubkey"`
	WindowSeconds      int64  `json:"window_seconds"`
	MicroThresholdUSDX string `json:"micro_threshold_usdx"`
	MicroPerUSDX       int64  `json:"micro_per_usdx"`
	ChainConfigured    bool   `json:"chain_configured"`
	TransportAuthGated bool   `json:"transport_auth_required"`
}

// SupplyResponse is GET /v1/supply: the public reserve proof (invariant i1).
// circulating_usdx is the off-chain sum of balances; reserve_usdl is the USDL
// held on-chain in the vault; drift_usdx is circulating - reserve (zero when the
// invariant holds). reserve fields are absent when the chain is not configured.
type SupplyResponse struct {
	CirculatingUSDX string `json:"circulating_usdx"`
	ReserveUSDL     string `json:"reserve_usdl,omitempty"`
	DriftUSDX       string `json:"drift_usdx,omitempty"`
	Reserved        bool   `json:"fully_reserved"` // true when drift == 0
	ReserveKnown    bool   `json:"reserve_known"`  // false in dev / chain unconfigured
	Accounts        int64  `json:"accounts"`
	Transfers       int64  `json:"transfers"`
}

// BatchView is the public view of a settlement batch.
type BatchView struct {
	ID            string    `json:"id"`
	RootHex       string    `json:"root"`
	Status        string    `json:"status"`
	AnchorTxHash  string    `json:"anchor_tx,omitempty"`
	TransferCount int       `json:"transfer_count"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	CreatedAt     time.Time `json:"created_at"`
}

// BatchesResponse is GET /v1/batches (paginated).
type BatchesResponse struct {
	Batches []BatchView `json:"batches"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
	Count   int         `json:"count"`
}

// AnchorResponse is GET /v1/anchor/{root}: the Paxeer settlement tx for a root.
type AnchorResponse struct {
	RootHex      string `json:"root"`
	BatchID      string `json:"batch_id"`
	Status       string `json:"status"`
	AnchorTxHash string `json:"anchor_tx,omitempty"`
	Anchored     bool   `json:"anchored"`
}

// TransferView is the public explorer view of a transfer.
type TransferView struct {
	Seq          int64     `json:"seq"`
	BatchID      string    `json:"batch_id,omitempty"`
	FromDID      string    `json:"from_did"`
	ToDID        string    `json:"to_did"`
	AmountUSDX   string    `json:"amount_usdx"`
	Tier         string    `json:"tier"`
	LeafHashHex  string    `json:"leaf_hash"`
	BatchRootHex string    `json:"batch_root,omitempty"`
	AnchorTxHash string    `json:"anchor_tx,omitempty"`
	Settled      bool      `json:"settled"`
	TS           time.Time `json:"ts"`
}

// TransfersResponse is GET /v1/transfers (KEYSET-paginated, optional ?did=).
// NextBefore is the cursor for the next older page: pass it as ?before= to
// continue. It is 0/absent when the last page has been reached.
type TransfersResponse struct {
	Transfers  []TransferView `json:"transfers"`
	DID        string         `json:"did,omitempty"`
	Limit      int            `json:"limit"`
	NextBefore int64          `json:"next_before,omitempty"`
	Count      int            `json:"count"`
}

// AccountResponse is GET /v1/account/{did}: the public account view.
type AccountResponse struct {
	DID         string         `json:"did"`
	EVMAddress  string         `json:"evm_address,omitempty"`
	BalanceUSDX string         `json:"balance_usdx"`
	EscrowUSDX  string         `json:"escrow_usdx"`
	History     []TransferView `json:"history"`
}
