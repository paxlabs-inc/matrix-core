package wakeruntime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/lease"
)

func TestAcceptancePredicatesFailClosedUnlessEvidenceHashIsExplicit(t *testing.T) {
	observation := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "effect:wake:test",
		Hash: contracts.ContentHash{
			Algorithm: "sha256", Digest: strings.Repeat("a", 64),
		},
		Kind:       "provider_observation",
		ObservedAt: time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC),
	}
	predicates, err := acceptancePredicates(
		[]contracts.RequiredOutput{
			{
				Kind:             "criterion_01",
				SuccessPredicate: "The result satisfies the business objective",
			},
			{
				Kind:             "criterion_02",
				SuccessPredicate: "evidence_hash: provider evidence is content-addressed",
			},
		},
		observation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(predicates) != 2 ||
		predicates[0].Kind != contracts.PredicateSemantic ||
		predicates[0].SubjectID != "" ||
		predicates[0].ExpectedHash != nil {
		t.Fatalf("semantic predicate = %+v", predicates[0])
	}
	if predicates[1].Kind != contracts.PredicateEvidenceHash ||
		predicates[1].SubjectID != string(observation.ID) ||
		predicates[1].ExpectedHash == nil ||
		*predicates[1].ExpectedHash != observation.Hash {
		t.Fatalf("evidence predicate = %+v", predicates[1])
	}
}

func TestBindGrantPreservesLargeAuthorityIntegers(t *testing.T) {
	const fence contracts.FenceToken = 9007199254740993
	encoded, err := bindGrant(
		json.RawMessage(`{"query":"current"}`),
		lease.Grant{Fence: fence},
	)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion string      `json:"schema_version"`
		Grant         lease.Grant `json:"grant"`
		Query         string      `json:"query"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != contracts.SchemaVersionV1 ||
		envelope.Grant.Fence != fence || envelope.Query != "current" {
		t.Fatalf("bound grant = %#v", envelope)
	}
}
