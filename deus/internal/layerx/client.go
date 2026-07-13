// Package layerx is deus's typed client for layerxd — the ONE seam through
// which the gateway prefetches challenge nonces, submits payer-signed pay/hold
// intents, runs captor operations under its own DID identity, and reads
// receipts/accounts (DEUS-LAYERX req.5). Deus never holds funds: this client
// transports signatures, it never signs on a payer's behalf.
package layerx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	lxtypes "github.com/paxlabs-inc/layerx/pkg/types"
)

// Typed failure classes callers branch on (mapped from layerxd's stable error
// codes; any transport failure is ErrUnavailable so a priced call can 503
// rather than execute unpaid).
var (
	ErrInsufficientFunds = errors.New("layerx: insufficient funds")
	ErrUnauthorized      = errors.New("layerx: unauthorized")
	ErrNotFound          = errors.New("layerx: not found")
	ErrConflict          = errors.New("layerx: conflict")
	ErrInvalid           = errors.New("layerx: invalid request")
	ErrUnavailable       = errors.New("layerx: unavailable")
)

// Config wires a Client.
type Config struct {
	// BaseURL is layerxd's base URL (DEUS_LAYERX_URL).
	BaseURL string
	// Bearer is the optional shared transport token (DEUS_LAYERX_BEARER).
	Bearer string
	// KeyHex is the gateway's ed25519 identity (DEUS_LXP_KEY): a 32-byte seed
	// or 64-byte private key, hex. Optional — required only for captor ops.
	KeyHex string
	// DIDLabel names the gateway identity (did:matrix:<label>:<keyfp>).
	// Defaults to "deus-gateway".
	DIDLabel string
	// HTTPClient overrides the default 15s-timeout client (tests).
	HTTPClient *http.Client
}

// Client talks to one layerxd.
type Client struct {
	base   string
	bearer string
	http   *http.Client

	did  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time
}

// New builds a Client. The gateway DID is derived from the key fingerprint,
// mirroring the daemon executor-key convention (label + hex(pub)[:16]).
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("layerx: base url required")
	}
	c := &Client{
		base:   base,
		bearer: cfg.Bearer,
		http:   cfg.HTTPClient,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.KeyHex != "" {
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(cfg.KeyHex), "0x"))
		if err != nil {
			return nil, fmt.Errorf("layerx: decode key: %w", err)
		}
		switch len(raw) {
		case ed25519.SeedSize:
			c.priv = ed25519.NewKeyFromSeed(raw)
		case ed25519.PrivateKeySize:
			c.priv = ed25519.PrivateKey(raw)
		default:
			return nil, fmt.Errorf("layerx: key must be a 32-byte seed or 64-byte private key")
		}
		c.pub = c.priv.Public().(ed25519.PublicKey)
		label := cfg.DIDLabel
		if label == "" {
			label = "deus-gateway"
		}
		c.did = fmt.Sprintf("did:matrix:%s:%s", label, hex.EncodeToString(c.pub)[:16])
	}
	return c, nil
}

// DID returns the gateway's LayerX identity ("" when no key is configured).
func (c *Client) DID() string { return c.did }

// ─── challenge prefetch ──────────────────────────────────────────────────────

// Challenge mints a single-use nonce for did via the public challenge lane —
// the nonce a 402 terms body carries for the payer to sign against.
func (c *Client) Challenge(ctx context.Context, did string) (lxtypes.ChallengeResponse, error) {
	var out lxtypes.ChallengeResponse
	err := c.do(ctx, http.MethodPost, "/v1/agent/auth/challenge", lxtypes.ChallengeRequest{DID: did}, "", &out)
	return out, err
}

// ─── payer-signed intent submission (deus = transporter, invariant i6) ──────

// PayIntent is a payer-signed pay the gateway submits verbatim.
type PayIntent struct {
	FromDID    string
	PublicKey  string
	Nonce      string
	Signature  string
	ToDID      string
	AmountUSDX string
	Ref        string
}

// SubmitPay submits a payer-signed pay intent and returns the signed receipt.
func (c *Client) SubmitPay(ctx context.Context, in PayIntent) (lxtypes.Receipt, error) {
	var out lxtypes.Receipt
	err := c.do(ctx, http.MethodPost, "/v1/pay", lxtypes.PayRequest{
		ToDID:      in.ToDID,
		AmountUSDX: in.AmountUSDX,
		Ref:        in.Ref,
		FromDID:    in.FromDID,
		PublicKey:  in.PublicKey,
		Nonce:      in.Nonce,
		Signature:  in.Signature,
	}, "", &out)
	return out, err
}

// HoldIntent is a payer-signed hold the gateway submits verbatim.
type HoldIntent struct {
	FromDID    string
	PublicKey  string
	Nonce      string
	Signature  string
	ToDID      string
	AmountUSDX string
	CaptorDID  string
	TTLSeconds int64
	Ref        string
}

// SubmitHold submits a payer-signed hold intent and returns the open hold.
func (c *Client) SubmitHold(ctx context.Context, in HoldIntent) (lxtypes.HoldView, error) {
	var out lxtypes.HoldView
	err := c.do(ctx, http.MethodPost, "/v1/hold", lxtypes.HoldRequest{
		ToDID:      in.ToDID,
		AmountUSDX: in.AmountUSDX,
		CaptorDID:  in.CaptorDID,
		TTLSeconds: in.TTLSeconds,
		Ref:        in.Ref,
		FromDID:    in.FromDID,
		PublicKey:  in.PublicKey,
		Nonce:      in.Nonce,
		Signature:  in.Signature,
	}, "", &out)
	return out, err
}

// ─── captor operations (the gateway's own DID via principal token) ──────────

// Capture consumes a hold as the gateway captor and returns the closed hold +
// the emitted transfer receipt.
func (c *Client) Capture(ctx context.Context, holdID, amountUSDX string) (lxtypes.CaptureResponse, error) {
	var out lxtypes.CaptureResponse
	tok, err := c.principalToken(ctx)
	if err != nil {
		return out, err
	}
	err = c.do(ctx, http.MethodPost, "/v1/hold/"+holdID+"/capture", lxtypes.CaptureRequest{AmountUSDX: amountUSDX}, tok, &out)
	return out, err
}

// Release returns a hold's funds to the payer as the gateway captor.
func (c *Client) Release(ctx context.Context, holdID string) (lxtypes.HoldView, error) {
	var out lxtypes.HoldView
	tok, err := c.principalToken(ctx)
	if err != nil {
		return out, err
	}
	err = c.do(ctx, http.MethodPost, "/v1/hold/"+holdID+"/release", lxtypes.ReleaseRequest{}, tok, &out)
	return out, err
}

// ─── public reads ────────────────────────────────────────────────────────────

// GetHold reads a hold (public).
func (c *Client) GetHold(ctx context.Context, holdID string) (lxtypes.HoldView, error) {
	var out lxtypes.HoldView
	err := c.do(ctx, http.MethodGet, "/v1/hold/"+holdID, nil, "", &out)
	return out, err
}

// Receipt reads a transfer receipt by seq (public).
func (c *Client) Receipt(ctx context.Context, seq int64) (lxtypes.Receipt, error) {
	var out lxtypes.Receipt
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/receipt/%d", seq), nil, "", &out)
	return out, err
}

// Account reads a public account view (balance + history).
func (c *Client) Account(ctx context.Context, did string) (lxtypes.AccountResponse, error) {
	var out lxtypes.AccountResponse
	err := c.do(ctx, http.MethodGet, "/v1/account/"+did, nil, "", &out)
	return out, err
}

// ─── principal-token lane (challenge -> sign -> verify, cached) ─────────────

// principalToken returns a live X-LayerX-Agent token for the gateway DID,
// minting one through the challenge/verify lane when the cache is cold or
// near expiry.
func (c *Client) principalToken(ctx context.Context) (string, error) {
	if c.priv == nil {
		return "", fmt.Errorf("%w: gateway key not configured (DEUS_LXP_KEY)", ErrUnauthorized)
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	ch, err := c.Challenge(ctx, c.did)
	if err != nil {
		return "", err
	}
	sig := hex.EncodeToString(ed25519.Sign(c.priv, []byte(ch.Message)))
	var out lxtypes.VerifyResponse
	if err := c.do(ctx, http.MethodPost, "/v1/agent/auth/verify", lxtypes.VerifyRequest{
		DID:       c.did,
		PublicKey: hex.EncodeToString(c.pub),
		Nonce:     ch.Nonce,
		Signature: sig,
	}, "", &out); err != nil {
		return "", err
	}
	c.token = out.Token
	// Renew ahead of expiry so an in-flight captor op never races the TTL.
	c.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - 30*time.Second)
	return c.token, nil
}

// ─── transport ───────────────────────────────────────────────────────────────

// do runs one JSON round trip and maps the {ok,data,error} envelope onto typed
// errors. Transport-level failures are ErrUnavailable — the caller must treat
// them as "payment rail down", never as "free call".
func (c *Client) do(ctx context.Context, method, path string, body any, agentToken string, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("layerx: encode request: %w", err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return fmt.Errorf("layerx: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if agentToken != "" {
		req.Header.Set("X-LayerX-Agent", agentToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	var env struct {
		Ok    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *lxtypes.Error  `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: http %d", ErrUnavailable, resp.StatusCode)
		}
		return fmt.Errorf("layerx: malformed response (http %d)", resp.StatusCode)
	}
	if !env.Ok {
		return typedError(resp.StatusCode, env.Error)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("layerx: decode response: %w", err)
		}
	}
	return nil
}

// typedError maps a layerxd error envelope to the client's typed errors,
// keeping the server's message for the log line.
func typedError(status int, e *lxtypes.Error) error {
	code, msg := "", ""
	if e != nil {
		code, msg = e.Code, e.Message
	}
	var base error
	switch code {
	case lxtypes.CodeInsufficientFunds:
		base = ErrInsufficientFunds
	case lxtypes.CodeUnauthorized:
		base = ErrUnauthorized
	case lxtypes.CodeNotFound:
		base = ErrNotFound
	case lxtypes.CodeConflict:
		base = ErrConflict
	case lxtypes.CodeInvalidRequest:
		base = ErrInvalid
	default:
		if status >= 500 || status == http.StatusTooManyRequests {
			base = ErrUnavailable
		} else {
			base = ErrInvalid
		}
	}
	if msg == "" {
		return fmt.Errorf("%w (http %d)", base, status)
	}
	return fmt.Errorf("%w: %s", base, msg)
}
