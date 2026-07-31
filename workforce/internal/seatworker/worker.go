// Package seatworker contains only the deterministic, one-shot seat process
// projection. It intentionally has no durable-store or effect dependencies.
package seatworker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"matrix/workforce/internal/contracts"
)

const (
	MaxPacketBytes = 4 << 20
	MaxOutputBytes = 1 << 20
)

type SeatOutput struct {
	SchemaVersion  string                     `json:"schema_version"`
	WakeID         contracts.WakeID           `json:"wake_id"`
	LeaseID        contracts.LeaseID          `json:"lease_id"`
	SeatID         contracts.SeatID           `json:"seat_id"`
	IntentID       contracts.IntentID         `json:"intent_id"`
	Disposition    contracts.WakeDisposition  `json:"disposition"`
	PacketDigest   contracts.ContentHash      `json:"packet_digest"`
	RequiredOutput []contracts.RequiredOutput `json:"required_outputs"`
	InputCounts    InputCounts                `json:"input_counts"`
	Proposal       *Proposal                  `json:"proposal"`
	ReasonCode     string                     `json:"reason_code"`
}

type Proposal struct {
	SkillID   contracts.SkillID `json:"skill_id"`
	Operation string            `json:"operation"`
	Provider  string            `json:"provider"`
	Input     json.RawMessage   `json:"input"`
}

func (proposal Proposal) Validate() error {
	if strings.TrimSpace(string(proposal.SkillID)) == "" ||
		strings.TrimSpace(proposal.Operation) == "" ||
		strings.TrimSpace(proposal.Provider) == "" ||
		len(proposal.Input) == 0 || len(proposal.Input) > 256<<10 ||
		!json.Valid(proposal.Input) {
		return fmt.Errorf("seatworker: proposal is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(proposal.Input, &object); err != nil || object == nil {
		return fmt.Errorf("seatworker: proposal input must be a JSON object")
	}
	return nil
}

type ModelDecision struct {
	SchemaVersion string                    `json:"schema_version"`
	Disposition   contracts.WakeDisposition `json:"disposition"`
	Proposal      *Proposal                 `json:"proposal"`
	ReasonCode    string                    `json:"reason_code"`
}

type SessionInput struct {
	SchemaVersion string               `json:"schema_version"`
	Packet        contracts.WorkPacket `json:"packet"`
	ModelResponse json.RawMessage      `json:"model_response"`
}

func (input SessionInput) Validate() error {
	if input.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("seatworker: session input schema is invalid")
	}
	if err := input.Packet.Validate(); err != nil {
		return err
	}
	if len(input.ModelResponse) > MaxOutputBytes ||
		(len(input.ModelResponse) > 0 && !json.Valid(input.ModelResponse)) {
		return fmt.Errorf("seatworker: model response is invalid")
	}
	return nil
}

func (input SessionInput) HasModelResponse() bool {
	value := bytes.TrimSpace(input.ModelResponse)
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

type InputCounts struct {
	VerifiedRecords uint32 `json:"verified_records"`
	Dependencies    uint32 `json:"dependencies"`
	Artifacts       uint32 `json:"artifacts"`
	Evidence        uint32 `json:"evidence"`
	Inbox           uint32 `json:"inbox"`
	Tools           uint32 `json:"tools"`
	Skills          uint32 `json:"skills"`
}

func (output SeatOutput) Validate() error {
	if output.SchemaVersion != contracts.SchemaVersionV1 ||
		output.WakeID == "" || output.LeaseID == "" ||
		output.SeatID == "" || output.IntentID == "" {
		return fmt.Errorf("seatworker: seat output identity is incomplete")
	}
	if !output.Disposition.Valid() {
		return fmt.Errorf("seatworker: invalid disposition %q", output.Disposition)
	}
	if err := output.PacketDigest.Validate(); err != nil {
		return err
	}
	if len(output.RequiredOutput) == 0 {
		return fmt.Errorf("seatworker: required outputs are missing")
	}
	for _, required := range output.RequiredOutput {
		if err := required.Validate(); err != nil {
			return err
		}
	}
	switch output.Disposition {
	case contracts.DispositionProgressed:
		if output.Proposal != nil {
			if err := output.Proposal.Validate(); err != nil {
				return err
			}
		}
		if output.ReasonCode != "" {
			return fmt.Errorf("seatworker: progressed output cannot carry a reason")
		}
	case contracts.DispositionBlocked:
		if output.Proposal != nil ||
			strings.TrimSpace(output.ReasonCode) == "" ||
			len(output.ReasonCode) > 128 {
			return fmt.Errorf("seatworker: blocked output is incomplete")
		}
	default:
		if output.Proposal != nil || output.ReasonCode != "" {
			return fmt.Errorf("seatworker: output fields do not match disposition")
		}
	}
	return nil
}

func Orient(packet contracts.WorkPacket) (SeatOutput, error) {
	if err := packet.Validate(); err != nil {
		return SeatOutput{}, err
	}
	canonical, err := contracts.EncodeCanonical(&packet)
	if err != nil {
		return SeatOutput{}, err
	}
	sum := sha256.Sum256(canonical)
	output := SeatOutput{
		SchemaVersion: contracts.SchemaVersionV1,
		WakeID:        packet.Lease.WakeID,
		LeaseID:       packet.Lease.ID,
		SeatID:        packet.Seat.ID,
		IntentID:      packet.Intent.ID,
		Disposition:   contracts.DispositionProgressed,
		PacketDigest: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
		RequiredOutput: append([]contracts.RequiredOutput(nil), packet.RequiredOutputs...),
		InputCounts: InputCounts{
			VerifiedRecords: uint32(len(packet.VerifiedState)),
			Dependencies:    uint32(len(packet.Dependencies)),
			Artifacts:       uint32(len(packet.Artifacts)),
			Evidence:        uint32(len(packet.Evidence)),
			Inbox:           uint32(len(packet.Inbox)),
			Tools:           uint32(len(packet.Tools)),
			Skills:          uint32(len(packet.Skills)),
		},
	}
	return output, output.Validate()
}

func ApplyModel(packet contracts.WorkPacket, response []byte) (SeatOutput, error) {
	output, err := Orient(packet)
	if err != nil {
		return SeatOutput{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.DisallowUnknownFields()
	var decision ModelDecision
	if err := decoder.Decode(&decision); err != nil {
		return SeatOutput{}, fmt.Errorf("seatworker: decode model decision: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SeatOutput{}, fmt.Errorf("seatworker: model decision has trailing data")
	}
	if decision.SchemaVersion != contracts.SchemaVersionV1 {
		return SeatOutput{}, fmt.Errorf("seatworker: model decision schema is invalid")
	}
	switch decision.Disposition {
	case contracts.DispositionProgressed:
		if decision.Proposal == nil {
			return SeatOutput{}, fmt.Errorf("seatworker: progressed decision requires a proposal")
		}
		allowed := false
		for _, skill := range packet.Skills {
			if skill.ID == decision.Proposal.SkillID {
				allowed = true
				break
			}
		}
		if !allowed {
			return SeatOutput{}, fmt.Errorf("seatworker: proposed skill is outside the WorkPacket")
		}
	case contracts.DispositionBlocked:
		if decision.Proposal != nil ||
			decision.ReasonCode != "insufficient_current_authority" {
			return SeatOutput{}, fmt.Errorf("seatworker: blocked decision is invalid")
		}
	default:
		return SeatOutput{}, fmt.Errorf("seatworker: model disposition is invalid")
	}
	output.Disposition = decision.Disposition
	output.Proposal = decision.Proposal
	output.ReasonCode = decision.ReasonCode
	if err := output.Validate(); err != nil {
		return SeatOutput{}, err
	}
	return output, nil
}
