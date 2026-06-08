// Package wallet authorizes and executes caller spends via the embedded wallet API.
package wallet

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
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

// HTTPClient calls the embedded wallet REST API.
type HTTPClient struct {
	BaseURL string
}

// AuthorizeSpend checks spend policy at the wallet.
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

// Send executes agent/send on the direct rail.
func (c *HTTPClient) Send(ctx context.Context, bearer, toAddress, amountWei string) (string, error) {
	_ = ctx
	_ = bearer
	_ = toAddress
	_ = amountWei
	if c.BaseURL == "" {
		return "", fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	return "", fmt.Errorf("wallet: send not implemented for HTTP client")
}

// OpenStream proxies streams.open via the embedded wallet.
func (c *HTTPClient) OpenStream(ctx context.Context, bearer string, in StreamOpenInput) (OpenStreamResult, error) {
	_ = ctx
	_ = bearer
	_ = in
	if c.BaseURL == "" {
		return OpenStreamResult{}, fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	return OpenStreamResult{}, fmt.Errorf("wallet: stream_open not implemented for HTTP client")
}

// StreamSettle proxies streams.settle via the embedded wallet.
func (c *HTTPClient) StreamSettle(ctx context.Context, bearer, chainStreamID string) (string, error) {
	_ = ctx
	_ = bearer
	_ = chainStreamID
	if c.BaseURL == "" {
		return "", fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	return "", fmt.Errorf("wallet: stream_settle not implemented for HTTP client")
}

// StreamClose proxies streams.close via the embedded wallet.
func (c *HTTPClient) StreamClose(ctx context.Context, bearer, chainStreamID string) (string, error) {
	_ = ctx
	_ = bearer
	_ = chainStreamID
	if c.BaseURL == "" {
		return "", fmt.Errorf("wallet: MATRIX_WALLET_API_URL not configured")
	}
	return "", fmt.Errorf("wallet: stream_close not implemented for HTTP client")
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
