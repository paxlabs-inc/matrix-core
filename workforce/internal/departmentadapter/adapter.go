// Package departmentadapter binds the typed departmental work services to the
// credential-bearing effect gateway.
package departmentadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/domainwork"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/knowledgework"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/operationswork"
	"matrix/workforce/internal/skills"
)

const (
	KnowledgeProvider      = "knowledge"
	MarketingLegalProvider = "marketing_legal"
	OperationsProvider     = "operations"
)

type Adapter struct {
	name       string
	knowledge  *knowledgework.Service
	domain     *domainwork.Service
	operations *operationswork.Service
	now        func() time.Time
}

func New(
	now func() time.Time,
	approvals *approval.Store,
) ([]effect.Adapter, error) {
	knowledge, err := knowledgework.New(now)
	if err != nil {
		return nil, err
	}
	domain, err := domainwork.New(now, approvals)
	if err != nil {
		return nil, err
	}
	operations, err := operationswork.New(now, approvals)
	if err != nil {
		return nil, err
	}
	return []effect.Adapter{
		&Adapter{name: KnowledgeProvider, knowledge: knowledge, now: now},
		&Adapter{name: MarketingLegalProvider, domain: domain, now: now},
		&Adapter{name: OperationsProvider, operations: operations, now: now},
	}, nil
}

func (adapter *Adapter) Name() string {
	if adapter == nil {
		return ""
	}
	return adapter.name
}

func (adapter *Adapter) Dispatch(
	ctx context.Context,
	operation effect.Operation,
) (effect.DispatchResult, error) {
	if adapter == nil || adapter.now == nil {
		return effect.DispatchResult{}, fmt.Errorf("department adapter is unavailable")
	}
	payload, err := openPayload(operation)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	var result any
	switch adapter.name {
	case KnowledgeProvider:
		var input knowledgework.Input
		if err := decode(payload, &input); err != nil {
			return effect.DispatchResult{}, err
		}
		result, err = adapter.knowledge.Execute(ctx, input)
	case MarketingLegalProvider:
		var input domainwork.Input
		if err := decode(payload, &input); err != nil {
			return effect.DispatchResult{}, err
		}
		result, err = adapter.domain.Execute(ctx, input)
	case OperationsProvider:
		var input operationswork.Input
		if err := decode(payload, &input); err != nil {
			return effect.DispatchResult{}, err
		}
		result, err = adapter.operations.Execute(ctx, input)
	default:
		return effect.DispatchResult{}, fmt.Errorf("department provider is not registered")
	}
	if err != nil {
		return effect.DispatchResult{Started: true, ObservedAt: adapter.now()}, err
	}
	observation, err := json.Marshal(result)
	if err != nil {
		return effect.DispatchResult{}, err
	}
	return effect.DispatchResult{
		Started: true, ExternalID: externalID(adapter.name, operation.IdempotencyKey),
		Observation: observation, ObservedAt: adapter.now(),
	}, nil
}

func (adapter *Adapter) Probe(
	ctx context.Context,
	operation effect.Operation,
) (effect.ProbeResult, error) {
	result, err := adapter.Dispatch(ctx, operation)
	if err != nil {
		return effect.ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: result,
			Reason: "typed_department_service_unavailable",
		}, err
	}
	return effect.ProbeResult{
		Outcome: skills.ProbeCompletedOutOfBand, Dispatch: result,
		Reason: "content_addressed_department_result",
	}, nil
}

func openPayload(operation effect.Operation) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := decode(operation.Input, &envelope); err != nil || envelope == nil {
		return nil, fmt.Errorf("department adapter input is invalid")
	}
	rawGrant, ok := envelope["grant"]
	if !ok {
		return nil, fmt.Errorf("department adapter grant is missing")
	}
	var grant lease.Grant
	if err := json.Unmarshal(rawGrant, &grant); err != nil ||
		grant.Request.Validate() != nil || grant.State != lease.StateActive ||
		grant.Fence.Validate() != nil ||
		grant.OrganizationID != operation.OrganizationID ||
		grant.SeatID != operation.SeatID || grant.ID != operation.LeaseID ||
		grant.Fence != operation.Fence {
		return nil, fmt.Errorf("department adapter grant does not match effect authority")
	}
	delete(envelope, "grant")
	return json.Marshal(envelope)
}

func decode(value []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("department adapter input has trailing data")
	}
	return nil
}

func externalID(provider, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + idempotencyKey))
	return provider + ":" + hex.EncodeToString(digest[:16])
}

var _ effect.Adapter = (*Adapter)(nil)
