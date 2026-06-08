// Package streams manages PaymentStreams sessions (docs/05-api.md §5.5).
package streams

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// AccrualBackend reads and mutates on-chain (or dev) stream accrual.
type AccrualBackend interface {
	Accrued(ctx context.Context, chainStreamID string) (string, error)
	Settle(ctx context.Context, chainStreamID string) (settledWei string, err error)
	Close(ctx context.Context, chainStreamID string) (refundWei string, err error)
}

// DevBackend simulates 0x0906 accrual for tests and DEUS_DEV.
type DevBackend struct {
	mu      sync.Mutex
	streams map[string]*devStream
}

type devStream struct {
	ratePerSecond *big.Int
	capWei        *big.Int
	settledWei    *big.Int
	openedAt      time.Time
	closed        bool
}

// NewDevBackend returns an in-memory streams precompile stub.
func NewDevBackend() *DevBackend {
	return &DevBackend{streams: make(map[string]*devStream)}
}

// Register adds a stream after open (called by Service.Open).
func (d *DevBackend) Register(chainStreamID, ratePerSecondWei, capWei string, openedAt time.Time) error {
	rate, ok := new(big.Int).SetString(ratePerSecondWei, 10)
	if !ok {
		return fmt.Errorf("streams: invalid rate")
	}
	capAmount, ok := new(big.Int).SetString(capWei, 10)
	if !ok {
		return fmt.Errorf("streams: invalid cap")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.streams[chainStreamID] = &devStream{
		ratePerSecond: rate,
		capWei:        capAmount,
		settledWei:    big.NewInt(0),
		openedAt:      openedAt,
	}
	return nil
}

func (d *DevBackend) get(chainStreamID string) (*devStream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.streams[chainStreamID]
	if !ok {
		return nil, fmt.Errorf("streams: unknown chain stream %s", chainStreamID)
	}
	return st, nil
}

func (d *DevBackend) accruedLocked(st *devStream, now time.Time) *big.Int {
	if st.closed {
		return new(big.Int).Set(st.settledWei)
	}
	elapsed := int64(now.Sub(st.openedAt).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	raw := new(big.Int).Mul(st.ratePerSecond, big.NewInt(elapsed))
	if raw.Cmp(st.capWei) > 0 {
		raw.Set(st.capWei)
	}
	return raw
}

// Accrued returns total accrued wei for the stream.
func (d *DevBackend) Accrued(ctx context.Context, chainStreamID string) (string, error) {
	_ = ctx
	st, err := d.get(chainStreamID)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.accruedLocked(st, time.Now().UTC()).String(), nil
}

// Settle moves accrued into settled (pays payee in dev ledger).
func (d *DevBackend) Settle(ctx context.Context, chainStreamID string) (string, error) {
	_ = ctx
	st, err := d.get(chainStreamID)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.closed {
		return st.settledWei.String(), nil
	}
	acc := d.accruedLocked(st, time.Now().UTC())
	delta := new(big.Int).Sub(acc, st.settledWei)
	if delta.Sign() < 0 {
		delta = big.NewInt(0)
	}
	st.settledWei.Add(st.settledWei, delta)
	return delta.String(), nil
}

// Close refunds unspent cap and marks the stream closed.
func (d *DevBackend) Close(ctx context.Context, chainStreamID string) (string, error) {
	_ = ctx
	st, err := d.get(chainStreamID)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.closed {
		refund := new(big.Int).Sub(st.capWei, st.settledWei)
		if refund.Sign() < 0 {
			refund = big.NewInt(0)
		}
		return refund.String(), nil
	}
	acc := d.accruedLocked(st, time.Now().UTC())
	if acc.Cmp(st.settledWei) > 0 {
		st.settledWei.Set(acc)
	}
	st.closed = true
	refund := new(big.Int).Sub(st.capWei, st.settledWei)
	if refund.Sign() < 0 {
		refund = big.NewInt(0)
	}
	return refund.String(), nil
}
