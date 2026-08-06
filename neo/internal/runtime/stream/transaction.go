// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package stream

import (
	"context"
	"fmt"
	"sync"

	"matrix/neo/internal/runtime/records"
)

type Recorder interface {
	AppendStreamOutput(context.Context, string, uint64, records.StreamedOutput) error
}

type Sink interface {
	Provisional(context.Context, records.StreamedOutput) error
	Commit(context.Context, string) error
	Retract(context.Context, string) error
}

// Transaction owns the one monotonic event sequence for a logical turn. Every
// provisional delta is persisted before it reaches the frontend; acceptance or
// repair appends explicit state transitions for that exact attempt.
type Transaction struct {
	mu         sync.Mutex
	turnID     string
	generation uint64
	attempt    uint64
	sequence   uint64
	current    []records.StreamedOutput
	recorder   Recorder
	sink       Sink
}

func New(turnID string, recorder Recorder, sink Sink) *Transaction {
	return &Transaction{turnID: turnID, recorder: recorder, sink: sink}
}

func (transaction *Transaction) Durable() bool {
	return transaction != nil && transaction.recorder != nil
}

func (transaction *Transaction) Begin(generation uint64) {
	transaction.mu.Lock()
	transaction.generation = generation
	transaction.mu.Unlock()
}

func (transaction *Transaction) Delta(ctx context.Context, channel records.StreamChannel, content string) error {
	if content == "" {
		return nil
	}
	transaction.mu.Lock()
	transaction.sequence++
	output := records.StreamedOutput{
		AttemptID: fmt.Sprintf("%s:%d", transaction.turnID, transaction.attempt),
		Sequence:  transaction.sequence, Channel: channel,
		Status: records.StreamProvisional, Content: content,
	}
	transaction.current = append(transaction.current, output)
	generation := transaction.generation
	transaction.mu.Unlock()
	if transaction.recorder != nil {
		if err := transaction.recorder.AppendStreamOutput(ctx, transaction.turnID, generation, output); err != nil {
			return err
		}
	}
	if transaction.sink != nil {
		return transaction.sink.Provisional(ctx, output)
	}
	return nil
}

func (transaction *Transaction) Commit(ctx context.Context) error {
	return transaction.finish(ctx, records.StreamCommitted)
}

func (transaction *Transaction) Retract(ctx context.Context) error {
	return transaction.finish(ctx, records.StreamRetracted)
}

func (transaction *Transaction) finish(ctx context.Context, status records.StreamStatus) error {
	transaction.mu.Lock()
	attemptID := fmt.Sprintf("%s:%d", transaction.turnID, transaction.attempt)
	generation := transaction.generation
	transitions := make([]records.StreamedOutput, 0, len(transaction.current))
	for _, current := range transaction.current {
		transaction.sequence++
		transitions = append(transitions, records.StreamedOutput{
			AttemptID: attemptID, Sequence: transaction.sequence,
			Channel: current.Channel, Status: status, Content: current.Content,
		})
	}
	transaction.current = nil
	transaction.attempt++
	transaction.mu.Unlock()
	for _, transition := range transitions {
		if transaction.recorder != nil {
			if err := transaction.recorder.AppendStreamOutput(ctx, transaction.turnID, generation, transition); err != nil {
				return err
			}
		}
	}
	if transaction.sink == nil {
		return nil
	}
	if status == records.StreamCommitted {
		return transaction.sink.Commit(ctx, attemptID)
	}
	return transaction.sink.Retract(ctx, attemptID)
}
