package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"matrix/workforce/internal/contracts"
)

type Catalog struct {
	contracts map[contracts.SkillID]Contract
	digest    contracts.ContentHash
}

type catalogEntry struct {
	ID      contracts.SkillID `json:"skill_id"`
	Version uint64            `json:"version"`
	Digest  contracts.ContentHash
}

type catalogEnvelope struct {
	SchemaVersion string         `json:"schema_version"`
	Entries       []catalogEntry `json:"entries"`
}

func (envelope catalogEnvelope) Validate() error {
	if envelope.SchemaVersion != contracts.SchemaVersionV1 || len(envelope.Entries) == 0 {
		return fmt.Errorf("skills: catalog must contain versioned entries")
	}
	for _, entry := range envelope.Entries {
		if entry.ID == "" || entry.Version == 0 {
			return fmt.Errorf("skills: catalog entry identity is invalid")
		}
		if err := entry.Digest.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func NewCatalog(values []Contract) (*Catalog, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("skills: catalog is empty")
	}
	byID := make(map[contracts.SkillID]Contract, len(values))
	entries := make([]catalogEntry, 0, len(values))
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if _, exists := byID[value.ID]; exists {
			return nil, fmt.Errorf("skills: duplicate skill %q", value.ID)
		}
		byID[value.ID] = copyContract(value)
		entries = append(entries, catalogEntry{ID: value.ID, Version: value.Version, Digest: value.Digest})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	canonical, err := contracts.EncodeCanonical(&catalogEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Entries:       entries,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return &Catalog{
		contracts: byID,
		digest: contracts.ContentHash{
			Algorithm: "sha256",
			Digest:    hex.EncodeToString(sum[:]),
		},
	}, nil
}

func copyContract(value Contract) Contract {
	value.InputSchema = append([]byte(nil), value.InputSchema...)
	value.OutputSchema = append([]byte(nil), value.OutputSchema...)
	value.Capabilities = append([]string(nil), value.Capabilities...)
	value.DataScopes = append([]string(nil), value.DataScopes...)
	value.Preconditions = append([]string(nil), value.Preconditions...)
	value.Operations = append([]Operation(nil), value.Operations...)
	for index := range value.Operations {
		value.Operations[index].InputSchema = append([]byte(nil), value.Operations[index].InputSchema...)
		value.Operations[index].OutputSchema = append([]byte(nil), value.Operations[index].OutputSchema...)
		value.Operations[index].DataScopes = append([]string(nil), value.Operations[index].DataScopes...)
	}
	value.Postconditions = append([]string(nil), value.Postconditions...)
	value.Retry.RetryOn = append([]string(nil), value.Retry.RetryOn...)
	value.Idempotency.KeyFields = append([]string(nil), value.Idempotency.KeyFields...)
	value.Approvals = append([]string(nil), value.Approvals...)
	value.ScheduleEligibility.WakeReasons = append([]string(nil), value.ScheduleEligibility.WakeReasons...)
	if value.Probe != nil {
		probe := *value.Probe
		probe.OutputSchema = append([]byte(nil), probe.OutputSchema...)
		value.Probe = &probe
	}
	return value
}

func (catalog *Catalog) Digest() contracts.ContentHash {
	if catalog == nil {
		return contracts.ContentHash{}
	}
	return catalog.digest
}

func (catalog *Catalog) Resolve(ids []contracts.SkillID) ([]contracts.SkillRef, error) {
	if catalog == nil {
		return nil, fmt.Errorf("skills: catalog is required")
	}
	result := make([]contracts.SkillRef, 0, len(ids))
	seen := make(map[contracts.SkillID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("skills: duplicate requested skill %q", id)
		}
		value, ok := catalog.contracts[id]
		if !ok {
			return nil, fmt.Errorf("skills: skill %q is not in the current catalog", id)
		}
		seen[id] = true
		result = append(result, contracts.SkillRef{
			ID: id, Version: value.Version, Digest: value.Digest,
		})
	}
	return result, nil
}
