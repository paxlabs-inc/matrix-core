package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type preparedRecord struct {
	record        contracts.Record
	canonicalHash contracts.ContentHash
	sealed        []byte
}

func (s *Store) prepareRecord(record contracts.Record) (preparedRecord, error) {
	canonical, err := contracts.EncodeCanonical(&record)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("ledger prepare record: %w", err)
	}
	sum := sha256.Sum256(canonical)
	canonicalHash := contracts.ContentHash{
		Algorithm: "sha256",
		Digest:    hex.EncodeToString(sum[:]),
	}
	sealed, err := s.vault.SealRecord(s.recordAD(record), canonical)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("ledger prepare record: seal: %w", err)
	}
	return preparedRecord{record: record, canonicalHash: canonicalHash, sealed: sealed}, nil
}

func (s *Store) openRecord(
	recordAD vault.AD,
	sealed []byte,
	expectedHash string,
) (contracts.Record, error) {
	canonical, err := s.vault.OpenRecord(recordAD, sealed)
	if err != nil {
		return contracts.Record{}, fmt.Errorf("%w: Vault authentication", ErrIntegrity)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return contracts.Record{}, fmt.Errorf("%w: canonical hash mismatch", ErrIntegrity)
	}
	record, err := contracts.DecodeCanonical[contracts.Record, *contracts.Record](canonical)
	if err != nil {
		return contracts.Record{}, fmt.Errorf("%w: canonical decode", ErrIntegrity)
	}
	return record, nil
}

func (s *Store) recordAD(record contracts.Record) vault.AD {
	return s.recordADFor(
		record.OrganizationID,
		record.ID,
		record.Kind,
		record.ProjectID,
		record.SchemaVersion,
	)
}

func (s *Store) recordADFor(
	organizationID contracts.OrganizationID,
	recordID contracts.RecordID,
	kind contracts.RecordKind,
	projectID *contracts.ProjectID,
	schemaVersion string,
) vault.AD {
	project := "-"
	if projectID != nil {
		project = string(*projectID)
	}
	return vault.AD{
		User:   s.tenantID,
		Store:  "workforce.ledger." + string(kind),
		Stream: strings.Join([]string{string(organizationID), project, string(recordID)}, "/"),
		Schema: schemaVersion,
	}
}
