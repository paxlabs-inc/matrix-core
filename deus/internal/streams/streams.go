package streams

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/paxlabs-inc/deus/internal/auth"
	"github.com/paxlabs-inc/deus/internal/pricing"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

// Service opens, settles, and closes PaymentStreams sessions.
type Service struct {
	store    *store.Store
	pricing  *pricing.Service
	wallet   wallet.Client
	backend  AccrualBackend
	dev      *DevBackend
	chainSeq atomic.Uint64
}

// Config wires stream dependencies.
type Config struct {
	Store   *store.Store
	Pricing *pricing.Service
	Wallet  wallet.Client
	Backend AccrualBackend
	Dev     *DevBackend
}

// New constructs a streams service.
func New(cfg Config) *Service {
	return &Service{
		store:   cfg.Store,
		pricing: cfg.Pricing,
		wallet:  cfg.Wallet,
		backend: cfg.Backend,
		dev:     cfg.Dev,
	}
}

// OpenInput is POST /v1/streams.
type OpenInput struct {
	ServiceID string
	Operation string
	CapWei    string
	StopTime  uint64
}

// OpenResult is a newly opened stream.
type OpenResult struct {
	StreamID         string
	ChainStreamID    string
	ServiceID        string
	RatePerSecondWei string
	CapWei           string
	Status           string
	OpenTx           string
}

// Open opens a stream for a per_second operation on a service.
func (s *Service) Open(ctx context.Context, caller auth.Caller, in OpenInput) (OpenResult, error) {
	if in.ServiceID == "" {
		return OpenResult{}, &Error{Code: "invalid_request", Message: "service_id required", HTTPStatus: 400}
	}
	if in.CapWei == "" {
		return OpenResult{}, &Error{Code: "invalid_request", Message: "cap_wei required", HTTPStatus: 400}
	}
	op := in.Operation
	if op == "" {
		op = "run"
	}

	svc, err := s.store.GetServiceByID(ctx, in.ServiceID)
	if err != nil {
		return OpenResult{}, &Error{Code: "not_found", Message: "service not found", HTTPStatus: 404}
	}
	if svc.Status != "active" {
		return OpenResult{}, &Error{Code: "service_unavailable", Message: "service not active", HTTPStatus: 503}
	}
	plan, err := s.pricing.PlanForOperation(ctx, in.ServiceID, op)
	if err != nil {
		return OpenResult{}, &Error{Code: "invalid_request", Message: err.Error(), HTTPStatus: 400}
	}
	if plan.Model != "per_second" {
		return OpenResult{}, &Error{Code: "invalid_request", Message: "stream requires per_second pricing", HTTPStatus: 400}
	}

	payout, err := s.store.DeveloperPayoutByService(ctx, in.ServiceID)
	if err != nil {
		return OpenResult{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	if err := s.wallet.AuthorizeSpend(ctx, caller.Bearer, in.CapWei, in.ServiceID); err != nil {
		var pd *wallet.PolicyDenied
		if errors.As(err, &pd) {
			return OpenResult{}, &Error{Code: "policy_denied", Message: pd.Message, HTTPStatus: 403}
		}
		return OpenResult{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
	}

	openRes, err := s.wallet.OpenStream(ctx, caller.Bearer, wallet.StreamOpenInput{
		Payee:            payout,
		RatePerSecondWei: plan.UnitPriceWei,
		CapWei:           in.CapWei,
		StopTime:         in.StopTime,
	})
	if err != nil {
		return OpenResult{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
	}
	chainID := openRes.ChainStreamID
	if chainID == "" {
		seq := s.chainSeq.Add(1)
		chainID = fmt.Sprintf("%d", seq)
	}
	now := time.Now().UTC()
	if s.dev != nil {
		if err := s.dev.Register(chainID, plan.UnitPriceWei, in.CapWei, now); err != nil {
			return OpenResult{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
		}
	}
	openTx := openRes.TxHash
	var openTxPtr *string
	if openTx != "" {
		openTxPtr = &openTx
	}
	id, err := s.store.InsertStream(ctx, store.StreamRow{
		ChainStreamID:    chainID,
		ServiceID:        in.ServiceID,
		CallerDID:        caller.DID,
		CallerWallet:     caller.Wallet,
		PayeeAddress:     payout,
		RatePerSecondWei: plan.UnitPriceWei,
		CapWei:           in.CapWei,
		SettledWei:       "0",
		MeteredWei:       "0",
		Status:           "open",
		OpenTx:           openTxPtr,
		OpenedAt:         now,
		LastMeteredAt:    now,
	})
	if err != nil {
		return OpenResult{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	return OpenResult{
		StreamID:         id,
		ChainStreamID:    chainID,
		ServiceID:        in.ServiceID,
		RatePerSecondWei: plan.UnitPriceWei,
		CapWei:           in.CapWei,
		Status:           "open",
		OpenTx:           openTx,
	}, nil
}

// State is GET /v1/streams/{id}.
type State struct {
	StreamID         string
	ChainStreamID    string
	ServiceID        string
	RatePerSecondWei string
	CapWei           string
	AccruedWei       string
	SettledWei       string
	MeteredWei       string
	Status           string
	OpenTx           string
	LastSettleTx     string
	CloseTx          string
}

// Get returns stream state including live accrued().
func (s *Service) Get(ctx context.Context, caller auth.Caller, streamID string) (State, error) {
	row, err := s.loadOwned(ctx, caller, streamID)
	if err != nil {
		return State{}, err
	}
	accrued, err := s.backend.Accrued(ctx, row.ChainStreamID)
	if err != nil {
		return State{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	return stateFromRow(row, accrued), nil
}

// Settle settles accrued on-chain via 0x0906 settle().
func (s *Service) Settle(ctx context.Context, caller auth.Caller, streamID string) (State, error) {
	row, err := s.loadOwned(ctx, caller, streamID)
	if err != nil {
		return State{}, err
	}
	if row.Status != "open" {
		return State{}, &Error{Code: "conflict", Message: "stream closed", HTTPStatus: 409}
	}
	delta, err := s.backend.Settle(ctx, row.ChainStreamID)
	if err != nil {
		return State{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	txHash, err := s.wallet.StreamSettle(ctx, caller.Bearer, row.ChainStreamID)
	if err != nil {
		return State{}, &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
	}
	settled := addWei(row.SettledWei, delta)
	if err := s.store.UpdateStreamSettled(ctx, row.ID, settled, txHash); err != nil {
		return State{}, &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	row.SettledWei = settled
	if txHash != "" {
		row.LastSettleTx = &txHash
	}
	accrued, _ := s.backend.Accrued(ctx, row.ChainStreamID)
	return stateFromRow(row, accrued), nil
}

// Close closes the stream and refunds unspent cap.
func (s *Service) Close(ctx context.Context, caller auth.Caller, streamID string) (State, string, error) {
	row, err := s.loadOwned(ctx, caller, streamID)
	if err != nil {
		return State{}, "", err
	}
	if row.Status == "closed" {
		accrued, _ := s.backend.Accrued(ctx, row.ChainStreamID)
		return stateFromRow(row, accrued), "0", nil
	}
	refund, err := s.backend.Close(ctx, row.ChainStreamID)
	if err != nil {
		return State{}, "", &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	txHash, err := s.wallet.StreamClose(ctx, caller.Bearer, row.ChainStreamID)
	if err != nil {
		return State{}, "", &Error{Code: "payment_required", Message: err.Error(), HTTPStatus: 402}
	}
	now := time.Now().UTC()
	if err := s.store.CloseStream(ctx, row.ID, txHash, now); err != nil {
		return State{}, "", &Error{Code: "internal_error", Message: err.Error(), HTTPStatus: 500}
	}
	row.Status = "closed"
	row.CloseTx = &txHash
	row.ClosedAt = &now
	accrued, _ := s.backend.Accrued(ctx, row.ChainStreamID)
	return stateFromRow(row, accrued), refund, nil
}

// MeterDelta returns newly accrued wei since the last invocation meter checkpoint.
func (s *Service) MeterDelta(ctx context.Context, row store.StreamRow) (delta, accrued string, err error) {
	accrued, err = s.backend.Accrued(ctx, row.ChainStreamID)
	if err != nil {
		return "", "", err
	}
	acc, ok := new(big.Int).SetString(accrued, 10)
	if !ok {
		return "", "", fmt.Errorf("streams: invalid accrued")
	}
	metered, ok := new(big.Int).SetString(row.MeteredWei, 10)
	if !ok {
		metered = big.NewInt(0)
	}
	deltaInt := new(big.Int).Sub(acc, metered)
	if deltaInt.Sign() < 0 {
		deltaInt = big.NewInt(0)
	}
	return deltaInt.String(), accrued, nil
}

// RecordMeter advances the metered checkpoint after a successful stream invoke.
func (s *Service) RecordMeter(ctx context.Context, streamID, accruedWei string) error {
	return s.store.UpdateStreamMetered(ctx, streamID, accruedWei, time.Now().UTC())
}

// GetOwned loads a stream owned by the caller.
func (s *Service) GetOwned(ctx context.Context, caller auth.Caller, streamID string) (store.StreamRow, error) {
	return s.loadOwned(ctx, caller, streamID)
}

func (s *Service) loadOwned(ctx context.Context, caller auth.Caller, streamID string) (store.StreamRow, error) {
	row, err := s.store.GetStreamByID(ctx, streamID)
	if err != nil {
		return store.StreamRow{}, &Error{Code: "not_found", Message: "stream not found", HTTPStatus: 404}
	}
	if row.CallerDID != caller.DID {
		return store.StreamRow{}, &Error{Code: "forbidden", Message: "stream caller mismatch", HTTPStatus: 403}
	}
	return row, nil
}

func stateFromRow(row store.StreamRow, accrued string) State {
	st := State{
		StreamID:         row.ID,
		ChainStreamID:    row.ChainStreamID,
		ServiceID:        row.ServiceID,
		RatePerSecondWei: row.RatePerSecondWei,
		CapWei:           row.CapWei,
		AccruedWei:       accrued,
		SettledWei:       row.SettledWei,
		MeteredWei:       row.MeteredWei,
		Status:           row.Status,
	}
	if row.OpenTx != nil {
		st.OpenTx = *row.OpenTx
	}
	if row.LastSettleTx != nil {
		st.LastSettleTx = *row.LastSettleTx
	}
	if row.CloseTx != nil {
		st.CloseTx = *row.CloseTx
	}
	return st
}

func addWei(a, b string) string {
	ai, ok := new(big.Int).SetString(a, 10)
	if !ok {
		ai = big.NewInt(0)
	}
	bi, ok := new(big.Int).SetString(b, 10)
	if !ok {
		return ai.String()
	}
	return new(big.Int).Add(ai, bi).String()
}

// ApplyMinCharge floors a delta charge.
func ApplyMinCharge(delta, minCharge string) string {
	d, ok := new(big.Int).SetString(delta, 10)
	if !ok {
		d = big.NewInt(0)
	}
	minWei, ok := new(big.Int).SetString(minCharge, 10)
	if !ok || minWei.Sign() <= 0 {
		return d.String()
	}
	if d.Cmp(minWei) < 0 && d.Sign() > 0 {
		return minWei.String()
	}
	if d.Sign() == 0 {
		return minWei.String()
	}
	return d.String()
}

// Error is a typed streams failure.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}
