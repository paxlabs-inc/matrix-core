// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"fmt"
)

// FailureLayer identifies where in the operation lifecycle a failure occurred.
// Per O1 req.6 ac.2: transport, process, protocol, HTTP, application,
// validation, policy, authorization, conflict, and cancellation remain distinct.
type FailureLayer string

const (
	LayerNone          FailureLayer = ""
	LayerTransport     FailureLayer = "transport"
	LayerProcess       FailureLayer = "process"
	LayerProtocol      FailureLayer = "protocol"
	LayerHTTP          FailureLayer = "http"
	LayerApplication   FailureLayer = "application"
	LayerValidation    FailureLayer = "validation"
	LayerPolicy        FailureLayer = "policy"
	LayerAuthorization FailureLayer = "authorization"
	LayerConflict      FailureLayer = "conflict"
	LayerCancellation  FailureLayer = "cancellation"
	LayerInvocation    FailureLayer = "invocation"
)

// TransitionKind is a bounded recovery transition the owning capability allows.
type TransitionKind string

const (
	TransitionRetry         TransitionKind = "retry"
	TransitionRepair        TransitionKind = "repair"
	TransitionReconcile     TransitionKind = "reconcile"
	TransitionDecompose     TransitionKind = "decompose"
	TransitionStop          TransitionKind = "stop"
	TransitionAskUser       TransitionKind = "ask_user"
	TransitionAuthorizedFix TransitionKind = "authorized_fix"
	TransitionProbe         TransitionKind = "probe"
)

// OperationOutcome is the discriminated result of every operation.
// Per O1 req.6 ac.1: operation id, success state, failure layer, stable class,
// retryability, evidence, state effects, satisfied postconditions, unsatisfied
// postconditions, and allowed next transitions.
type OperationOutcome struct {
	Operation       string            `json:"operation"`
	Success         bool              `json:"success"`
	FailureLayer    FailureLayer      `json:"failure_layer,omitempty"`
	FailureClass    string            `json:"failure_class,omitempty"`
	Retryable       bool              `json:"retryable"`
	Evidence        string            `json:"evidence"`
	EffectsObserved []string          `json:"effects_observed,omitempty"`
	PostSatisfied   []string          `json:"postconditions_satisfied,omitempty"`
	PostUnsatisfied []string          `json:"postconditions_unsatisfied,omitempty"`
	Transitions     []TransitionKind  `json:"allowed_transitions,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// IsTerminal returns true when no recovery transitions are available.
func (o OperationOutcome) IsTerminal() bool {
	return len(o.Transitions) == 0
}

// AllowsTransition checks whether a specific recovery transition is permitted.
func (o OperationOutcome) AllowsTransition(t TransitionKind) bool {
	for _, tr := range o.Transitions {
		if tr == t {
			return true
		}
	}
	return false
}

// Verify checks the outcome's internal consistency.
// An outcome must have an operation, a valid failure layer when failed,
// and at least one transition when retryable.
func (o OperationOutcome) Verify() error {
	if o.Operation == "" {
		return fmt.Errorf("outcome missing operation")
	}
	if !o.Success && o.FailureLayer == "" {
		return fmt.Errorf("failed outcome %q missing failure layer", o.Operation)
	}
	if o.Success && (o.FailureLayer != LayerNone || o.FailureClass != "" || o.Retryable) {
		return fmt.Errorf("successful outcome %q carries failure state", o.Operation)
	}
	if !o.Success && o.FailureClass == "" {
		return fmt.Errorf("failed outcome %q missing stable failure class", o.Operation)
	}
	if o.Evidence == "" {
		return fmt.Errorf("outcome %q missing evidence", o.Operation)
	}
	if !o.Success {
		switch o.FailureLayer {
		case LayerTransport, LayerProcess, LayerProtocol, LayerHTTP, LayerApplication,
			LayerValidation, LayerPolicy, LayerAuthorization, LayerConflict,
			LayerCancellation, LayerInvocation:
		default:
			return fmt.Errorf("outcome %q has invalid failure layer: %s", o.Operation, o.FailureLayer)
		}
	}
	if o.Retryable && len(o.Transitions) == 0 {
		return fmt.Errorf("retryable outcome %q has no allowed transitions", o.Operation)
	}
	if !o.Retryable {
		for _, transition := range o.Transitions {
			if transition == TransitionRetry {
				return fmt.Errorf("non-retryable outcome %q allows retry", o.Operation)
			}
		}
	}
	seen := make(map[TransitionKind]bool, len(o.Transitions))
	for _, transition := range o.Transitions {
		switch transition {
		case TransitionRetry, TransitionRepair, TransitionReconcile, TransitionDecompose,
			TransitionStop, TransitionAskUser, TransitionAuthorizedFix, TransitionProbe:
		default:
			return fmt.Errorf("outcome %q has invalid transition %q", o.Operation, transition)
		}
		if seen[transition] {
			return fmt.Errorf("outcome %q repeats transition %q", o.Operation, transition)
		}
		seen[transition] = true
	}
	return nil
}

// OutcomeBuilder constructs OperationOutcome incrementally, enforcing that
// every failed outcome carries a layer and every retryable outcome carries
// at least one transition.
type OutcomeBuilder struct {
	op     string
	result OperationOutcome
}

func NewOutcome(operation string) *OutcomeBuilder {
	return &OutcomeBuilder{
		op: operation,
		result: OperationOutcome{
			Operation: operation,
			Metadata:  make(map[string]string),
		},
	}
}

func (b *OutcomeBuilder) Success() *OutcomeBuilder {
	b.result.Success = true
	b.result.FailureLayer = LayerNone
	b.result.FailureClass = ""
	b.result.Retryable = false
	b.result.Transitions = nil
	return b
}

func (b *OutcomeBuilder) Fail(layer FailureLayer, class string) *OutcomeBuilder {
	b.result.Success = false
	b.result.FailureLayer = layer
	b.result.FailureClass = class
	return b
}

func (b *OutcomeBuilder) Retryable(yes bool) *OutcomeBuilder {
	b.result.Retryable = yes
	return b
}

func (b *OutcomeBuilder) Evidence(e string) *OutcomeBuilder {
	b.result.Evidence = e
	return b
}

func (b *OutcomeBuilder) Effect(e string) *OutcomeBuilder {
	b.result.EffectsObserved = append(b.result.EffectsObserved, e)
	return b
}

func (b *OutcomeBuilder) PostSatisfied(p string) *OutcomeBuilder {
	b.result.PostSatisfied = append(b.result.PostSatisfied, p)
	return b
}

func (b *OutcomeBuilder) PostUnsatisfied(p string) *OutcomeBuilder {
	b.result.PostUnsatisfied = append(b.result.PostUnsatisfied, p)
	return b
}

func (b *OutcomeBuilder) AllowTransition(t TransitionKind) *OutcomeBuilder {
	b.result.Transitions = append(b.result.Transitions, t)
	return b
}

func (b *OutcomeBuilder) SetMeta(key, value string) *OutcomeBuilder {
	b.result.Metadata[key] = value
	return b
}

func (b *OutcomeBuilder) Build() (OperationOutcome, error) {
	if err := b.result.Verify(); err != nil {
		return OperationOutcome{}, err
	}
	return b.result, nil
}

func (b *OutcomeBuilder) MustBuild() OperationOutcome {
	o, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("outcome build: %v", err))
	}
	return o
}

// OutcomeRenderer converts an OperationOutcome to the model-facing text
// representation. The model never parses raw error prose — it reads structured
// fields. Per O1 req.6 ac.4.
func RenderOutcome(o OperationOutcome) string {
	b, _ := json.Marshal(struct {
		Operation   string           `json:"operation"`
		Success     bool             `json:"success"`
		Layer       string           `json:"failure_layer,omitempty"`
		Class       string           `json:"failure_class,omitempty"`
		Retryable   bool             `json:"retryable"`
		Evidence    string           `json:"evidence"`
		Transitions []TransitionKind `json:"allowed_transitions,omitempty"`
		Unsatisfied []string         `json:"unsatisfied_postconditions,omitempty"`
	}{
		Operation:   o.Operation,
		Success:     o.Success,
		Layer:       string(o.FailureLayer),
		Class:       o.FailureClass,
		Retryable:   o.Retryable,
		Evidence:    o.Evidence,
		Transitions: o.Transitions,
		Unsatisfied: o.PostUnsatisfied,
	})
	return string(b)
}
