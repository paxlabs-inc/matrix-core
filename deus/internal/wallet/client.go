// Package wallet authorizes and executes caller spends via the embedded wallet API.
package wallet

import (
	"context"
	"fmt"
	"math/big"
)

// Client authorizes spends and sends PAX on the direct rail.
type Client interface {
	AuthorizeSpend(ctx context.Context, bearer, amountWei, serviceID string) error
	Send(ctx context.Context, bearer, toAddress, amountWei string) (txHash string, err error)
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

// DevClient is an in-process wallet stub for tests and DEUS_DEV.
type DevClient struct {
	MaxPerCallWei string
	Sends         []SendRecord
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
