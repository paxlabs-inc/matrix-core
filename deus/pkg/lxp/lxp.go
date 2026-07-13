// Package lxp implements LXP — HTTP-native payments on LayerX (lxp/1): the
// 402 challenge with prefetched-nonce terms, the X-LayerX-Payment retry, the
// settlement submit, and the X-LayerX-Receipt response. This is the ONE
// protocol implementation: the deus gateway consumes it, and any Go service
// can import it standalone to charge for an endpoint without the deus gateway
// in its data path.
//
// Trust posture: the service never custodies funds and never signs on a
// payer's behalf. A payment executes only on the payer's own ed25519 intent
// signature (LayerX invariant i6); layerxd unreachable is 503
// payment_unavailable, never a free call.
package lxp

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/paxlabs-inc/deus/internal/layerx"
	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

// Protocol identifies the wire dialect carried in the 402 terms body.
const Protocol = "lxp/1"

// Wire headers.
const (
	HeaderPayment   = "X-LayerX-Payment"
	HeaderReceipt   = "X-LayerX-Receipt"
	HeaderCallerDID = "X-Caller-DID" // identifies the payer for nonce prefetch
)

// Modes.
const (
	ModeExact = "exact" // settle the full amount, then serve
	ModeHold  = "hold"  // reserve -> serve -> capture on success / release on failure
)

// Machine-readable 402 reasons (the `reason` field beside a fresh challenge).
const (
	ReasonPaymentRequired   = "payment_required"
	ReasonIdentifyPayer     = "identify_payer" // no payer DID to prefetch a nonce for
	ReasonInvalidPayment    = "invalid_payment"
	ReasonInvalidSignature  = "invalid_signature"
	ReasonTermsMismatch     = "terms_mismatch"
	ReasonPaymentRejected   = "payment_rejected" // expired/replayed nonce or refused intent
	ReasonInsufficientFunds = "insufficient_funds"
)

var didRe = regexp.MustCompile(`^did:matrix:([^:]+):([0-9a-fA-F]{16})$`)

// Terms is the lxp/1 challenge body: everything the payer needs to sign the
// canonical LayerX intent and retry, including a live single-use nonce
// prefetched for its DID.
type Terms struct {
	Protocol   string    `json:"protocol"`
	Asset      string    `json:"asset"`
	AmountUSDX string    `json:"amount_usdx"`
	PayTo      string    `json:"pay_to"`
	Mode       string    `json:"mode"`
	CaptorDID  string    `json:"captor_did,omitempty"` // hold mode
	TTLSeconds int64     `json:"ttl_s,omitempty"`      // hold mode
	Nonce      string    `json:"nonce"`
	Ref        string    `json:"ref,omitempty"`
	LayerX     string    `json:"layerx"`
	QuoteID    string    `json:"quote_id,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Payment is the decoded X-LayerX-Payment header: the payer-signed intent the
// service transports to layerxd verbatim.
type Payment struct {
	FromDID    string `json:"from_did"`
	PublicKey  string `json:"public_key"`
	Nonce      string `json:"nonce"`
	Signature  string `json:"signature"`
	ToDID      string `json:"to_did"`
	AmountUSDX string `json:"amount_usdx"`
	Mode       string `json:"mode"`
	Ref        string `json:"ref,omitempty"`
}

// ReceiptHeader is the X-LayerX-Receipt payload: the settled payment's
// sequencer-signed proof (forever re-readable at GET /v1/receipt/{seq}).
type ReceiptHeader struct {
	Seq          int64  `json:"seq"`
	LeafHash     string `json:"leaf_hash"`
	SequencerSig string `json:"sequencer_sig"`
	AmountUSDX   string `json:"amount_usdx"`
	Ref          string `json:"ref,omitempty"`
}

// Price is what a resource costs — the input to a challenge.
type Price struct {
	AmountUSDX string // decimal USDX, normalized to 6dp in the terms
	PayTo      string // payee DID
	Mode       string // ModeExact | ModeHold (default exact)
	TTLSeconds int64  // hold lifetime (hold mode; default 120)
	Ref        string // optional 0x + 64 hex binding digest
	QuoteID    string // optional pre-flight quote reference
}

// Config wires a Server.
type Config struct {
	// LayerXURL is layerxd's base URL — also advertised in the terms body.
	LayerXURL string
	// Bearer is the optional shared transport token.
	Bearer string
	// KeyHex is the service's ed25519 identity (32-byte seed or 64-byte key,
	// hex). Required for hold mode (the service is the payer-authorized captor).
	KeyHex string
	// DIDLabel names the service identity (default "deus-gateway").
	DIDLabel string
	// HTTPClient overrides the layerxd transport (tests).
	HTTPClient *http.Client
}

// Server is one service's LXP half: challenge minting, payment verification,
// settlement, and receipts, all against one layerxd.
type Server struct {
	cli       *layerx.Client
	layerxURL string
}

// New builds a Server.
func New(cfg Config) (*Server, error) {
	cli, err := layerx.New(layerx.Config{
		BaseURL:    cfg.LayerXURL,
		Bearer:     cfg.Bearer,
		KeyHex:     cfg.KeyHex,
		DIDLabel:   cfg.DIDLabel,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &Server{cli: cli, layerxURL: strings.TrimRight(cfg.LayerXURL, "/")}, nil
}

// DID returns the service's LayerX identity ("" when no key is configured).
func (s *Server) DID() string { return s.cli.DID() }

// Client exposes the underlying layerxd client (receipt/account reads).
func (s *Server) Client() *layerx.Client { return s.cli }

// LayerXURL returns the layerxd base URL the terms advertise.
func (s *Server) LayerXURL() string { return s.layerxURL }

// Challenge prefetches a single-use nonce for payerDID and assembles the lxp/1
// terms. expires_at is clamped to the nonce TTL so signed-but-stale retries
// fail closed at layerxd.
func (s *Server) Challenge(ctx context.Context, payerDID string, p Price) (Terms, error) {
	amount, err := lxtypes.ParseUSDX(p.AmountUSDX)
	if err != nil || amount <= 0 {
		return Terms{}, fmt.Errorf("lxp: price amount %q invalid", p.AmountUSDX)
	}
	mode := p.Mode
	if mode == "" {
		mode = ModeExact
	}
	if mode != ModeExact && mode != ModeHold {
		return Terms{}, fmt.Errorf("lxp: unknown mode %q", p.Mode)
	}
	t := Terms{
		Protocol:   Protocol,
		Asset:      "USDX",
		AmountUSDX: lxtypes.FormatUSDX(amount),
		PayTo:      p.PayTo,
		Mode:       mode,
		Ref:        p.Ref,
		LayerX:     s.layerxURL,
		QuoteID:    p.QuoteID,
	}
	if mode == ModeHold {
		if s.DID() == "" {
			return Terms{}, errors.New("lxp: hold mode requires a service key (captor identity)")
		}
		t.CaptorDID = s.DID()
		t.TTLSeconds = p.TTLSeconds
		if t.TTLSeconds <= 0 {
			t.TTLSeconds = 120
		}
	}
	ch, err := s.cli.Challenge(ctx, payerDID)
	if err != nil {
		return Terms{}, err
	}
	t.Nonce = ch.Nonce
	t.ExpiresAt = time.Now().UTC().Add(time.Duration(ch.ExpiresIn) * time.Second)
	return t, nil
}

// ParsePayment decodes an X-LayerX-Payment header value.
func ParsePayment(header string) (Payment, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		// Tolerate padded encoders.
		if raw, err = base64.URLEncoding.DecodeString(strings.TrimSpace(header)); err != nil {
			return Payment{}, fmt.Errorf("lxp: payment header is not base64url")
		}
	}
	var p Payment
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payment{}, fmt.Errorf("lxp: payment header is not json: %w", err)
	}
	return p, nil
}

// EncodePayment renders a Payment as an X-LayerX-Payment header value (the
// client half; shared by tests and the Node middleware vectors).
func EncodePayment(p Payment) string {
	raw, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// EncodeReceipt renders the X-LayerX-Receipt header value for a settled
// payment.
func EncodeReceipt(r lxtypes.Receipt) string {
	raw, _ := json.Marshal(ReceiptHeader{
		Seq:          r.Seq,
		LeafHash:     r.LeafHashHex,
		SequencerSig: r.SequencerSig,
		AmountUSDX:   r.AmountUSDX,
		Ref:          r.Ref,
	})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeReceipt parses an X-LayerX-Receipt header value (client half).
func DecodeReceipt(header string) (ReceiptHeader, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return ReceiptHeader{}, fmt.Errorf("lxp: receipt header is not base64url")
	}
	var r ReceiptHeader
	if err := json.Unmarshal(raw, &r); err != nil {
		return ReceiptHeader{}, fmt.Errorf("lxp: receipt header is not json: %w", err)
	}
	return r, nil
}

// PayPreimage is the canonical LayerX pay-intent preimage for a payment
// against the given terms (auth.IntentMessage lockstep: the ref joins the
// signed fields only when present).
func PayPreimage(p Payment) string {
	fields := []string{p.ToDID, p.AmountUSDX}
	if p.Ref != "" {
		fields = append(fields, p.Ref)
	}
	return intentMessage("pay", p.FromDID, p.Nonce, fields...)
}

// HoldPreimage is the canonical hold-intent preimage (payee, amount, ttl, ref,
// captor — the payer consents to every capture bound).
func HoldPreimage(p Payment, ttlSeconds int64, captorDID string) string {
	return intentMessage("hold", p.FromDID, p.Nonce,
		p.ToDID, p.AmountUSDX, fmt.Sprintf("%d", ttlSeconds), p.Ref, captorDID)
}

// intentMessage mirrors layerxd's auth.IntentMessage byte-for-byte.
func intentMessage(op, did, nonce string, fields ...string) string {
	return "matrix-layerx-intent:" + op + ":" + did + ":" + strings.Join(fields, ":") + ":" + nonce
}

// VerifyAgainstTerms checks a payment's SHAPE against the priced terms and its
// ed25519 signature locally, BEFORE anything executes or is submitted: the
// amount/payee/mode/ref must match what the service priced, the public key
// must match the DID fingerprint, and the signature must verify over the
// canonical preimage. Nonce liveness/replay is layerxd's job at submit (the
// nonce is consumed there, single-use).
func (s *Server) VerifyAgainstTerms(p Payment, price Price) (reason string, err error) {
	amount, aerr := lxtypes.ParseUSDX(price.AmountUSDX)
	if aerr != nil {
		return ReasonTermsMismatch, fmt.Errorf("lxp: price amount: %w", aerr)
	}
	mode := price.Mode
	if mode == "" {
		mode = ModeExact
	}
	if p.FromDID == "" || p.PublicKey == "" || p.Nonce == "" || p.Signature == "" {
		return ReasonInvalidPayment, errors.New("lxp: payment missing intent fields")
	}
	got, gerr := lxtypes.ParseUSDX(p.AmountUSDX)
	if gerr != nil || got != amount || p.AmountUSDX != lxtypes.FormatUSDX(amount) {
		return ReasonTermsMismatch, fmt.Errorf("lxp: payment amount %q does not match terms %q", p.AmountUSDX, lxtypes.FormatUSDX(amount))
	}
	if p.ToDID != price.PayTo {
		return ReasonTermsMismatch, errors.New("lxp: payment payee does not match terms")
	}
	if p.Mode != mode {
		return ReasonTermsMismatch, fmt.Errorf("lxp: payment mode %q does not match terms mode %q", p.Mode, mode)
	}
	if p.Ref != price.Ref {
		return ReasonTermsMismatch, errors.New("lxp: payment ref does not match terms")
	}
	var preimage string
	switch mode {
	case ModeExact:
		preimage = PayPreimage(p)
	case ModeHold:
		ttl := price.TTLSeconds
		if ttl <= 0 {
			ttl = 120
		}
		preimage = HoldPreimage(p, ttl, s.DID())
	}
	if err := verifyDIDSig(p.FromDID, p.PublicKey, p.Signature, preimage); err != nil {
		return ReasonInvalidSignature, err
	}
	return "", nil
}

// verifyDIDSig mirrors layerxd's check: pubkey matches the DID fingerprint and
// signs the exact preimage bytes.
func verifyDIDSig(didStr, pubHex, sigHex, preimage string) error {
	m := didRe.FindStringSubmatch(strings.TrimSpace(didStr))
	if m == nil {
		return fmt.Errorf("lxp: malformed did %q", didStr)
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(pubHex, "0x"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("lxp: invalid public key")
	}
	if hex.EncodeToString(pub)[:16] != strings.ToLower(m[2]) {
		return errors.New("lxp: public key does not match did fingerprint")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil {
		return errors.New("lxp: invalid signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(preimage), sig) {
		return errors.New("lxp: signature verification failed")
	}
	return nil
}

// SettleExact submits the payer-signed pay intent and returns the receipt.
func (s *Server) SettleExact(ctx context.Context, p Payment) (lxtypes.Receipt, error) {
	return s.cli.SubmitPay(ctx, layerx.PayIntent{
		FromDID:    p.FromDID,
		PublicKey:  p.PublicKey,
		Nonce:      p.Nonce,
		Signature:  p.Signature,
		ToDID:      p.ToDID,
		AmountUSDX: p.AmountUSDX,
		Ref:        p.Ref,
	})
}

// OpenHold submits the payer-signed hold intent (captor = this service) and
// returns the open hold.
func (s *Server) OpenHold(ctx context.Context, p Payment, ttlSeconds int64) (lxtypes.HoldView, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 120
	}
	return s.cli.SubmitHold(ctx, layerx.HoldIntent{
		FromDID:    p.FromDID,
		PublicKey:  p.PublicKey,
		Nonce:      p.Nonce,
		Signature:  p.Signature,
		ToDID:      p.ToDID,
		AmountUSDX: p.AmountUSDX,
		CaptorDID:  s.DID(),
		TTLSeconds: ttlSeconds,
		Ref:        p.Ref,
	})
}

// Capture consumes a hold for the exact charge; the remainder auto-returns to
// the payer inside the ledger transaction.
func (s *Server) Capture(ctx context.Context, holdID, amountUSDX string) (lxtypes.Receipt, error) {
	res, err := s.cli.Capture(ctx, holdID, amountUSDX)
	if err != nil {
		return lxtypes.Receipt{}, err
	}
	return res.Receipt, nil
}

// Release returns a hold's funds to the payer (execution failed; no charge).
func (s *Server) Release(ctx context.Context, holdID string) error {
	_, err := s.cli.Release(ctx, holdID)
	return err
}

// SettleReason maps a settlement error onto a fresh-402 reason ("" means the
// rail itself is down and the response must be 503 payment_unavailable).
func SettleReason(err error) (reason string, unavailable bool) {
	switch {
	case errors.Is(err, layerx.ErrUnavailable):
		return "", true
	case errors.Is(err, layerx.ErrInsufficientFunds):
		return ReasonInsufficientFunds, false
	case errors.Is(err, layerx.ErrUnauthorized):
		return ReasonPaymentRejected, false
	default:
		return ReasonInvalidPayment, false
	}
}

// challengeBody is the 402 JSON envelope.
type challengeBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
	LXP    *Terms `json:"lxp,omitempty"`
}

// WriteChallenge writes an HTTP 402 carrying fresh lxp/1 terms and a
// machine-readable reason.
func WriteChallenge(w http.ResponseWriter, terms Terms, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(challengeBody{Error: ReasonPaymentRequired, Reason: reason, LXP: &terms})
}

// WriteUnavailable writes the 503 payment_unavailable response (layerxd
// unreachable — a priced call is never served unpaid).
func WriteUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment_unavailable"})
}

// WriteIdentifyPayer writes a 402 with no terms: the service cannot prefetch a
// nonce without knowing the payer DID (send X-Caller-DID and retry).
func WriteIdentifyPayer(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(challengeBody{Error: ReasonPaymentRequired, Reason: ReasonIdentifyPayer})
}
