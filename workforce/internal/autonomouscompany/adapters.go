package autonomouscompany

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type EvidenceVerificationFunc func(
	context.Context,
	pgx.Tx,
	EvidenceBinding,
	time.Time,
) error

type ProcessVerificationFunc func(
	context.Context,
	pgx.Tx,
	ProcessIdentity,
	string,
	time.Time,
) error

type EvidenceVerifierAdapter struct {
	verify        EvidenceVerificationFunc
	verifyProcess ProcessVerificationFunc
}

func NewEvidenceVerifierAdapter(
	verify EvidenceVerificationFunc,
	verifyProcess ...ProcessVerificationFunc,
) (*EvidenceVerifierAdapter, error) {
	if verify == nil || len(verifyProcess) > 1 ||
		(len(verifyProcess) == 1 && verifyProcess[0] == nil) {
		return nil, fmt.Errorf("autonomous company: evidence verification function is required")
	}
	adapter := &EvidenceVerifierAdapter{verify: verify}
	if len(verifyProcess) == 1 {
		adapter.verifyProcess = verifyProcess[0]
	}
	return adapter, nil
}

func (adapter *EvidenceVerifierAdapter) VerifyAutonomousCompanyEvidence(
	ctx context.Context,
	tx pgx.Tx,
	binding EvidenceBinding,
	at time.Time,
) error {
	if adapter == nil || adapter.verify == nil {
		return ErrEvidence
	}
	return adapter.verify(ctx, tx, binding, at)
}

func (adapter *EvidenceVerifierAdapter) VerifyAutonomousCompanyProcess(
	ctx context.Context,
	tx pgx.Tx,
	process ProcessIdentity,
	initiativeID string,
	at time.Time,
) error {
	if adapter == nil || adapter.verifyProcess == nil {
		return fmt.Errorf("%w: process receipt", ErrUnsupportedEvidence)
	}
	return adapter.verifyProcess(ctx, tx, process, initiativeID, at)
}

type RecoveryEvidenceFunc func(
	context.Context,
	string,
	time.Time,
) (RecoveryEvidence, error)

type RecoveryEvidenceSourceAdapter struct {
	load RecoveryEvidenceFunc
}

func NewRecoveryEvidenceSourceAdapter(
	load RecoveryEvidenceFunc,
) (*RecoveryEvidenceSourceAdapter, error) {
	if load == nil {
		return nil, fmt.Errorf("autonomous company: recovery evidence function is required")
	}
	return &RecoveryEvidenceSourceAdapter{load: load}, nil
}

func (adapter *RecoveryEvidenceSourceAdapter) CurrentRecoveryEvidence(
	ctx context.Context,
	initiativeID string,
	at time.Time,
) (RecoveryEvidence, error) {
	if adapter == nil || adapter.load == nil || token(initiativeID) != nil || !utc(at) {
		return RecoveryEvidence{}, ErrEvidence
	}
	value, err := adapter.load(ctx, initiativeID, at)
	if err != nil || value.ValidateAt(at) != nil ||
		value.Qualification.InitiativeID != initiativeID {
		return RecoveryEvidence{}, ErrEvidence
	}
	return value, nil
}

type FounderProjectionFunc func(
	context.Context,
	string,
	time.Time,
) (EvidenceBinding, error)

type FounderProjectionSourceAdapter struct {
	load FounderProjectionFunc
}

func NewFounderProjectionSourceAdapter(
	load FounderProjectionFunc,
) (*FounderProjectionSourceAdapter, error) {
	if load == nil {
		return nil, fmt.Errorf("autonomous company: founder projection function is required")
	}
	return &FounderProjectionSourceAdapter{load: load}, nil
}

func (adapter *FounderProjectionSourceAdapter) CurrentFounderProjection(
	ctx context.Context,
	initiativeID string,
	at time.Time,
) (EvidenceBinding, error) {
	if adapter == nil || adapter.load == nil || token(initiativeID) != nil || !utc(at) {
		return EvidenceBinding{}, ErrEvidence
	}
	value, err := adapter.load(ctx, initiativeID, at)
	if err != nil || value.Validate() != nil ||
		value.Kind != EvidenceFounderProjectionReceipt ||
		value.SourceState != "current" || value.InitiativeID != initiativeID ||
		!value.currentAt(at) {
		return EvidenceBinding{}, ErrEvidence
	}
	return value, nil
}

type NextCycleDispatchFunc func(
	context.Context,
	NextCycleSnapshot,
) (NextCycleUpdate, error)

type NextCycleReconcileFunc func(
	context.Context,
	NextCycleSnapshot,
) (NextCycleUpdate, error)

type NextCycleExecutorAdapter struct {
	dispatch  NextCycleDispatchFunc
	reconcile NextCycleReconcileFunc
}

func NewNextCycleExecutorAdapter(
	dispatch NextCycleDispatchFunc,
	reconcile NextCycleReconcileFunc,
) (*NextCycleExecutorAdapter, error) {
	if dispatch == nil || reconcile == nil {
		return nil, fmt.Errorf("autonomous company: next-cycle execution functions are required")
	}
	return &NextCycleExecutorAdapter{dispatch: dispatch, reconcile: reconcile}, nil
}

func (adapter *NextCycleExecutorAdapter) DispatchNextCycle(
	ctx context.Context,
	snapshot NextCycleSnapshot,
) (NextCycleUpdate, error) {
	if adapter == nil || adapter.dispatch == nil {
		return NextCycleUpdate{}, ErrUnauthorized
	}
	return adapter.dispatch(ctx, snapshot)
}

func (adapter *NextCycleExecutorAdapter) ReconcileNextCycle(
	ctx context.Context,
	snapshot NextCycleSnapshot,
) (NextCycleUpdate, error) {
	if adapter == nil || adapter.reconcile == nil {
		return NextCycleUpdate{}, ErrUnauthorized
	}
	return adapter.reconcile(ctx, snapshot)
}

var (
	_ EvidenceVerifier        = (*EvidenceVerifierAdapter)(nil)
	_ RecoveryEvidenceSource  = (*RecoveryEvidenceSourceAdapter)(nil)
	_ FounderProjectionSource = (*FounderProjectionSourceAdapter)(nil)
	_ NextCycleExecutor       = (*NextCycleExecutorAdapter)(nil)
)
