// Package auditorworker contains only deterministic, one-shot Auditor
// evaluation. It intentionally has no durable-store or effect dependencies.
package auditorworker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"centra/workforce/internal/contracts"
)

const (
	MaxPacketBytes = 4 << 20
	MaxOutputBytes = 1 << 20
)

type Decision struct {
	SchemaVersion string                             `json:"schema_version"`
	IntentID      contracts.IntentID                 `json:"intent_id"`
	AuditorSeatID contracts.SeatID                   `json:"auditor_seat_id"`
	Procedure     contracts.VerificationProcedureRef `json:"procedure"`
	PacketDigest  contracts.ContentHash              `json:"packet_digest"`
	Outcome       contracts.VerdictOutcome           `json:"outcome"`
	ReasonCodes   []string                           `json:"reason_codes"`
	Evidence      []contracts.EvidenceRef            `json:"evidence"`
}

func (value Decision) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		value.IntentID == "" || value.AuditorSeatID == "" ||
		!value.Outcome.Valid() || len(value.ReasonCodes) == 0 {
		return fmt.Errorf("auditorworker: decision is incomplete")
	}
	if err := value.Procedure.Validate(); err != nil {
		return err
	}
	if err := value.PacketDigest.Validate(); err != nil {
		return err
	}
	for i := range value.Evidence {
		if err := value.Evidence[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}

func Evaluate(packet contracts.VerdictPacket) (Decision, error) {
	if err := packet.Validate(); err != nil {
		return Decision{}, err
	}
	canonical, err := contracts.EncodeCanonical(&packet)
	if err != nil {
		return Decision{}, err
	}
	sum := sha256.Sum256(canonical)
	outcome := contracts.VerdictPass
	reasons := make([]string, 0, len(packet.Predicates))
	evidence := make([]contracts.EvidenceRef, 0)
	requiresHuman := false
	failed := false
	for _, predicate := range packet.Predicates {
		passed := false
		switch predicate.Kind {
		case contracts.PredicateArtifactHash:
			for _, artifact := range packet.Artifacts {
				if string(artifact.ID) == predicate.SubjectID &&
					predicate.ExpectedHash != nil && artifact.Hash == *predicate.ExpectedHash {
					passed = true
					break
				}
			}
		case contracts.PredicateEvidenceHash:
			for _, observation := range packet.Observations {
				if string(observation.ID) == predicate.SubjectID &&
					predicate.ExpectedHash != nil && observation.Hash == *predicate.ExpectedHash {
					passed = true
					evidence = append(evidence, observation)
					break
				}
			}
		case contracts.PredicateApproval:
			for _, approval := range packet.Approvals {
				if string(approval) == predicate.SubjectID {
					passed = true
					break
				}
			}
		case contracts.PredicateSemantic:
			requiresHuman = true
			reasons = append(reasons, predicate.ID+":requires_human")
			continue
		}
		if passed {
			reasons = append(reasons, predicate.ID+":pass")
		} else {
			failed = true
			reasons = append(reasons, predicate.ID+":fail")
		}
	}
	if failed {
		outcome = contracts.VerdictFail
	} else if requiresHuman {
		outcome = contracts.VerdictRequiresHuman
	}
	decision := Decision{
		SchemaVersion: contracts.SchemaVersionV1,
		IntentID:      packet.Intent.ID, AuditorSeatID: packet.AuditorSeatID,
		Procedure: packet.Procedure,
		PacketDigest: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
		Outcome: outcome, ReasonCodes: reasons,
		Evidence: uniqueEvidence(evidence),
	}
	return decision, decision.Validate()
}

func uniqueEvidence(values []contracts.EvidenceRef) []contracts.EvidenceRef {
	seen := make(map[contracts.EvidenceID]bool, len(values))
	result := make([]contracts.EvidenceRef, 0, len(values))
	for _, value := range values {
		if !seen[value.ID] {
			seen[value.ID] = true
			result = append(result, value)
		}
	}
	return result
}
