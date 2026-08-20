// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package records

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const SchemaVersion uint32 = 1

type Kind string

const (
	KindTurn        Kind = "turn"
	KindCycle       Kind = "cycle"
	KindEffect      Kind = "effect"
	KindConvergence Kind = "convergence"
	KindAnswer      Kind = "answer"
	KindDelivery    Kind = "delivery"
)

type envelope struct {
	RecordType    Kind            `json:"record_type"`
	SchemaVersion uint32          `json:"schema_version"`
	Record        json.RawMessage `json:"record"`
}

var ErrInvalidRecord = errors.New("neo runtime records: invalid canonical record")

func EncodeTurn(record TurnRecord) ([]byte, error) {
	return encode(KindTurn, record)
}

func DecodeTurn(data []byte) (TurnRecord, error) {
	return decode[TurnRecord](data, KindTurn)
}

func EncodeCycle(record CycleRecord) ([]byte, error) {
	return encode(KindCycle, record)
}

func DecodeCycle(data []byte) (CycleRecord, error) {
	return decode[CycleRecord](data, KindCycle)
}

func EncodeEffect(record EffectRecord) ([]byte, error) {
	return encode(KindEffect, record)
}

func DecodeEffect(data []byte) (EffectRecord, error) {
	return decode[EffectRecord](data, KindEffect)
}

func EncodeConvergence(record ConvergenceRecord) ([]byte, error) {
	return encode(KindConvergence, record)
}

func DecodeConvergence(data []byte) (ConvergenceRecord, error) {
	return decode[ConvergenceRecord](data, KindConvergence)
}

func EncodeAnswer(record AnswerRecord) ([]byte, error) {
	return encode(KindAnswer, record)
}

func DecodeAnswer(data []byte) (AnswerRecord, error) {
	return decode[AnswerRecord](data, KindAnswer)
}

func EncodeDelivery(record DeliveryRecord) ([]byte, error) {
	return encode(KindDelivery, record)
}

func DecodeDelivery(data []byte) (DeliveryRecord, error) {
	return decode[DeliveryRecord](data, KindDelivery)
}

func encode(kind Kind, record any) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("%w: encode %s: %v", ErrInvalidRecord, kind, err)
	}
	return json.Marshal(envelope{
		RecordType:    kind,
		SchemaVersion: SchemaVersion,
		Record:        payload,
	})
}

func decode[T any](data []byte, expected Kind) (T, error) {
	var zero T
	decoder := json.NewDecoder(bytes.NewReader(data))
	var wrapped envelope
	if err := decoder.Decode(&wrapped); err != nil {
		return zero, fmt.Errorf("%w: decode envelope: %v", ErrInvalidRecord, err)
	}
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if !errors.Is(trailingErr, io.EOF) || wrapped.RecordType != expected ||
		wrapped.SchemaVersion == 0 || wrapped.SchemaVersion > SchemaVersion ||
		len(wrapped.Record) == 0 {
		return zero, fmt.Errorf("%w: unexpected %s record envelope", ErrInvalidRecord, expected)
	}
	var record T
	if err := json.Unmarshal(wrapped.Record, &record); err != nil {
		return zero, fmt.Errorf("%w: decode %s: %v", ErrInvalidRecord, expected, err)
	}
	return record, nil
}
