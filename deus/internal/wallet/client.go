// Package wallet authorizes and executes caller spends via the embedded wallet API.
package wallet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// StreamOpenInput opens a PaymentStreams 0x0906 session.
type StreamOpenInput struct {
	Payee            string
	RatePerSecondWei string
	CapWei           string
	StopTime         uint64
	Token            string // empty = native PAX
}

// OpenStreamResult is a wallet stream open response.
type OpenStreamResult struct {
	ChainStreamID string
	TxHash        string
}

// Client authorizes spends and sends PAX on the direct and stream rails.
type Client interface {
	AuthorizeSpend(ctx context.Context, bearer, amountWei, serviceID string) error
	Send(ctx context.Context, bearer, toAddress, amountWei string) (txHash string, err error)
	OpenStream(ctx context.Context, bearer string, in StreamOpenInput) (OpenStreamResult, error)
	StreamSettle(ctx context.Context, bearer, chainStreamID string) (txHash string, err error)
	StreamClose(ctx context.Context, bearer, chainStreamID string) (txHash string, err error)
}

// PolicyDenied indicates wallet policy rejected a spend.
type PolicyDenied struct {
	Message string
	CapWei  string
}

func (e *PolicyDenied) Error() string { return e.Message }

// HTTPClient calls the Paxeer embedded wallet REST API (the caller's spend
// authority and signer). The per-call bearer is the caller's agent-wallet token,
// forwarded by the gateway; the wallet keeps the EVM key, enforces owner policy,
// signs, and broadcasts. Mirrors tachyon/internal/wallet/embedded.go and
// docs/10-integration.md §10.3.
type HTTPClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// AuthorizeSpend is a pre-flight hook. Custody policy (per-call cap, total cap,
// allowlist, frozen/read-only) is enforced atomically by the wallet at Send time
// — the wallet returns 403 which Send maps to *PolicyDenied — so there is no
// separate authorize call to make and no silent bypass: enforcement lives on the
// money-moving path itself (docs/08 §8.4, docs/10 §10.3; audit F8).
func (c *HTTPClient) AuthorizeSpend(ctx context.Context, bearer, amountWei, serviceID string) error {
	_ = ctx
	_ = bearer
	_ = amountWei
	_ = serviceID
	if c.BaseURL == "" {
		return fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	return nil
}

// Send executes a native PAX transfer to the developer payout via agent/send.
func (c *HTTPClient) Send(ctx context.Context, bearer, toAddress, amountWei string) (string, error) {
	var out sendResponse
	err := c.do(ctx, bearer, "/v1/agent/send", map[string]any{
		"tx": map[string]any{"to": toAddress, "value": amountWei},
	}, &out)
	if err != nil {
		return "", err
	}
	if out.TxHash == "" {
		return "", fmt.Errorf("wallet: send returned no tx_hash")
	}
	return out.TxHash, nil
}

// OpenStream funds a PaymentStreams 0x0906 session via the wallet precompile route.
func (c *HTTPClient) OpenStream(ctx context.Context, bearer string, in StreamOpenInput) (OpenStreamResult, error) {
	body := map[string]any{
		"payee":           in.Payee,
		"token":           streamToken(in.Token),
		"rate_per_second": in.RatePerSecondWei,
		"cap":             in.CapWei,
	}
	if in.StopTime > 0 {
		body["stop_time"] = in.StopTime
	}
	var out sendResponse
	if err := c.do(ctx, bearer, "/v1/agent/precompiles/streams/open", body, &out); err != nil {
		return OpenStreamResult{}, err
	}
	if out.TxHash == "" {
		return OpenStreamResult{}, fmt.Errorf("wallet: stream open returned no tx_hash")
	}
	return OpenStreamResult{ChainStreamID: out.StreamID, TxHash: out.TxHash}, nil
}

// StreamSettle settles accrued stream amount via the wallet precompile route.
func (c *HTTPClient) StreamSettle(ctx context.Context, bearer, chainStreamID string) (string, error) {
	return c.streamOp(ctx, bearer, "settle", chainStreamID)
}

// StreamClose closes a stream and refunds unspent cap via the wallet precompile route.
func (c *HTTPClient) StreamClose(ctx context.Context, bearer, chainStreamID string) (string, error) {
	return c.streamOp(ctx, bearer, "close", chainStreamID)
}

func (c *HTTPClient) streamOp(ctx context.Context, bearer, op, chainStreamID string) (string, error) {
	var out sendResponse
	err := c.do(ctx, bearer, "/v1/agent/precompiles/streams/"+op, map[string]any{
		"stream_id": chainStreamID,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.TxHash, nil
}

func streamToken(tok string) string {
	if strings.TrimSpace(tok) == "" {
		return "0x0000000000000000000000000000000000000000"
	}
	return tok
}

type sendResponse struct {
	TxHash   string `json:"tx_hash"`
	StreamID string `json:"stream_id"`
}

// do performs an authed JSON POST and maps wallet policy denials (HTTP 403) to
// *PolicyDenied so the gateway can surface policy_denied.
func (c *HTTPClient) do(ctx context.Context, bearer, path string, body, out any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	if strings.TrimSpace(bearer) == "" {
		return fmt.Errorf("wallet: missing caller bearer")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("wallet: request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var e walletError
		_ = json.Unmarshal(respBody, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusForbidden {
			return &PolicyDenied{Message: orDefault(msg, "spend denied by wallet policy"), CapWei: e.CapWei}
		}
		return fmt.Errorf("wallet: http %d: %s", resp.StatusCode, orDefault(msg, strings.TrimSpace(string(respBody))))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

type walletError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	CapWei  string `json:"cap_wei"`
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// DevClient is an in-process wallet stub for tests and DEUS_DEV.
type DevClient struct {
	MaxPerCallWei string
	Sends         []SendRecord
	StreamOpens   []StreamOpenRecord
	StreamRefunds []StreamRefundRecord
	streamSeq     atomic.Uint64
	streamCaps    map[string]string // chain_stream_id -> cap funded
}

// StreamOpenRecord captures a stream open funding event.
type StreamOpenRecord struct {
	Payee            string
	RatePerSecondWei string
	CapWei           string
	ChainStreamID    string
	TxHash           string
}

// StreamRefundRecord captures cap returned on stream close.
type StreamRefundRecord struct {
	ChainStreamID string
	RefundWei     string
	TxHash        string
}

// SendRecord captures a direct-rail transfer.
type SendRecord struct {
	To        string
	AmountWei string
}

// AuthorizeSpend allows spends up to MaxPerCallWei in dev.
func (d *DevClient) AuthorizeSpend(ctx context.Context, bearer, amountWei, serviceID string) error {
	_ = ctx
	_ = bearer
	_ = serviceID
	if d.MaxPerCallWei == "" {
		return nil
	}
	capWei, ok := new(big.Int).SetString(d.MaxPerCallWei, 10)
	if !ok {
		return nil
	}
	amt, ok := new(big.Int).SetString(amountWei, 10)
	if !ok {
		return fmt.Errorf("wallet: invalid amount")
	}
	if amt.Cmp(capWei) > 0 {
		return &PolicyDenied{Message: "spend exceeds per-call cap", CapWei: d.MaxPerCallWei}
	}
	return nil
}

// Send records a dev transfer and returns a synthetic tx hash.
func (d *DevClient) Send(ctx context.Context, bearer, toAddress, amountWei string) (string, error) {
	if err := d.AuthorizeSpend(ctx, bearer, amountWei, ""); err != nil {
		return "", err
	}
	d.Sends = append(d.Sends, SendRecord{To: toAddress, AmountWei: amountWei})
	return fmt.Sprintf("0xdev%08x", len(d.Sends)), nil
}

// OpenStream records stream cap funding in dev mode.
func (d *DevClient) OpenStream(ctx context.Context, bearer string, in StreamOpenInput) (OpenStreamResult, error) {
	if err := d.AuthorizeSpend(ctx, bearer, in.CapWei, ""); err != nil {
		return OpenStreamResult{}, err
	}
	if d.streamCaps == nil {
		d.streamCaps = make(map[string]string)
	}
	id := d.streamSeq.Add(1)
	chainID := fmt.Sprintf("%d", id)
	tx := fmt.Sprintf("0xstreamopen%08x", id)
	d.StreamOpens = append(d.StreamOpens, StreamOpenRecord{
		Payee:            in.Payee,
		RatePerSecondWei: in.RatePerSecondWei,
		CapWei:           in.CapWei,
		ChainStreamID:    chainID,
		TxHash:           tx,
	})
	d.streamCaps[chainID] = in.CapWei
	return OpenStreamResult{ChainStreamID: chainID, TxHash: tx}, nil
}

// StreamSettle records a dev settle tx.
func (d *DevClient) StreamSettle(ctx context.Context, bearer, chainStreamID string) (string, error) {
	_ = ctx
	_ = bearer
	return fmt.Sprintf("0xstreamsettle%s", chainStreamID), nil
}

// StreamClose records refund of unspent cap in dev mode.
func (d *DevClient) StreamClose(ctx context.Context, bearer, chainStreamID string) (string, error) {
	_ = ctx
	_ = bearer
	capWei := d.streamCaps[chainStreamID]
	delete(d.streamCaps, chainStreamID)
	d.StreamRefunds = append(d.StreamRefunds, StreamRefundRecord{
		ChainStreamID: chainStreamID,
		RefundWei:     capWei,
		TxHash:        fmt.Sprintf("0xstreamclose%s", chainStreamID),
	})
	return fmt.Sprintf("0xstreamclose%s", chainStreamID), nil
}
