package organization

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

type registryEntry struct {
	ID      CapabilityID          `json:"capability_id"`
	Version uint64                `json:"version"`
	Digest  contracts.ContentHash `json:"digest"`
}

type registryEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Entries       []registryEntry `json:"entries"`
}

func (value registryEnvelope) Validate() error {
	if value.SchemaVersion != RegistrySchemaVersion || len(value.Entries) == 0 ||
		len(value.Entries) > 64 {
		return fmt.Errorf("organization: capability registry must contain 1 to 64 current definitions")
	}
	previous := ""
	for _, entry := range value.Entries {
		if err := validateID("capability_id", string(entry.ID)); err != nil {
			return err
		}
		if entry.Version == 0 {
			return fmt.Errorf("organization: registry capability version must be positive")
		}
		if err := entry.Digest.Validate(); err != nil {
			return err
		}
		if string(entry.ID) <= previous {
			return fmt.Errorf("organization: registry entries must be sorted and unique")
		}
		previous = string(entry.ID)
	}
	return nil
}

type Registry struct {
	organizationID contracts.OrganizationID
	definitions    map[CapabilityID]map[uint64]CapabilityDefinition
	current        map[CapabilityID]CapabilityRef
	digest         contracts.ContentHash
}

func NewRegistry(
	values []CapabilityDefinition,
	ownerKeyID string,
	ownerPublicKey ed25519.PublicKey,
	at time.Time,
) (*Registry, error) {
	if !validUTC(at) || ownerKeyID == "" || len(ownerPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("organization: registry authority and evaluation time are required")
	}
	return newRegistry(values, at, func(value CapabilityDefinition) error {
		return VerifyCapabilityDefinition(value, ownerKeyID, ownerPublicKey)
	})
}

func newVerifiedRegistry(values []CapabilityDefinition, at time.Time) (*Registry, error) {
	return newRegistry(values, at, nil)
}

func newRegistry(
	values []CapabilityDefinition,
	at time.Time,
	verify func(CapabilityDefinition) error,
) (*Registry, error) {
	if !validUTC(at) {
		return nil, fmt.Errorf("organization: registry evaluation time must be UTC")
	}
	if len(values) == 0 || len(values) > 512 {
		return nil, fmt.Errorf("organization: registry requires 1 to 512 capability versions")
	}
	definitions := make(map[CapabilityID]map[uint64]CapabilityDefinition)
	var organizationID contracts.OrganizationID
	for _, original := range values {
		value := copyCapabilityDefinition(original)
		if verify != nil {
			if err := verify(value); err != nil {
				return nil, err
			}
		} else if err := value.Validate(); err != nil {
			return nil, err
		}
		if organizationID == "" {
			organizationID = value.OrganizationID
		}
		if value.OrganizationID != organizationID {
			return nil, fmt.Errorf("organization: registry definitions cross organization boundaries")
		}
		versions := definitions[value.ID]
		if versions == nil {
			versions = make(map[uint64]CapabilityDefinition)
			definitions[value.ID] = versions
		}
		if _, duplicate := versions[value.Version]; duplicate {
			return nil, fmt.Errorf("organization: duplicate capability %s version %d", value.ID, value.Version)
		}
		versions[value.Version] = value
	}
	current := make(map[CapabilityID]CapabilityRef, len(definitions))
	entries := make([]registryEntry, 0, len(definitions))
	for id, versions := range definitions {
		for version, value := range versions {
			if version > 1 {
				previous, exists := versions[version-1]
				if !exists {
					return nil, fmt.Errorf("organization: capability %s version chain has a gap", id)
				}
				digest, err := capabilityDigest(previous)
				if err != nil {
					return nil, err
				}
				if value.Previous == nil || value.Previous.Digest != digest ||
					!value.EffectiveAt.After(previous.EffectiveAt) {
					return nil, fmt.Errorf("organization: capability %s previous digest is invalid", id)
				}
			}
			if value.EffectiveAt.After(at) || value.ExpiresAt != nil && !value.ExpiresAt.After(at) {
				continue
			}
			if existing, exists := current[id]; exists && existing.Version > version {
				continue
			}
			digest, err := capabilityDigest(value)
			if err != nil {
				return nil, err
			}
			current[id] = CapabilityRef{ID: id, Version: version, Digest: digest}
		}
	}
	for id, reference := range current {
		entries = append(entries, registryEntry{ID: id, Version: reference.Version, Digest: reference.Digest})
	}
	if len(entries) == 0 || len(entries) > 64 {
		return nil, fmt.Errorf("organization: registry has no bounded current capability set")
	}
	slices.SortFunc(entries, func(left, right registryEntry) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	envelope := registryEnvelope{SchemaVersion: RegistrySchemaVersion, Entries: entries}
	canonical, err := contracts.EncodeCanonical(&envelope)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return &Registry{
		organizationID: organizationID,
		definitions:    definitions,
		current:        current,
		digest: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
	}, nil
}

func (registry *Registry) OrganizationID() contracts.OrganizationID {
	if registry == nil {
		return ""
	}
	return registry.organizationID
}

func (registry *Registry) Digest() contracts.ContentHash {
	if registry == nil {
		return contracts.ContentHash{}
	}
	return registry.digest
}

func (registry *Registry) Current(id CapabilityID) (CapabilityDefinition, CapabilityRef, error) {
	if registry == nil {
		return CapabilityDefinition{}, CapabilityRef{}, fmt.Errorf("organization: registry is required")
	}
	reference, exists := registry.current[id]
	if !exists {
		return CapabilityDefinition{}, CapabilityRef{}, fmt.Errorf("organization: capability %q has no current version", id)
	}
	return copyCapabilityDefinition(registry.definitions[id][reference.Version]), reference, nil
}

func (registry *Registry) Resolve(reference CapabilityRef) (CapabilityDefinition, error) {
	if registry == nil {
		return CapabilityDefinition{}, fmt.Errorf("organization: registry is required")
	}
	if err := reference.Validate(); err != nil {
		return CapabilityDefinition{}, err
	}
	versions := registry.definitions[reference.ID]
	value, exists := versions[reference.Version]
	if !exists {
		return CapabilityDefinition{}, fmt.Errorf("organization: capability reference is not registered")
	}
	digest, err := capabilityDigest(value)
	if err != nil {
		return CapabilityDefinition{}, err
	}
	if digest != reference.Digest {
		return CapabilityDefinition{}, fmt.Errorf("organization: capability digest does not match the registry")
	}
	return copyCapabilityDefinition(value), nil
}

func (registry *Registry) CurrentReferences() []CapabilityRef {
	if registry == nil {
		return nil
	}
	result := make([]CapabilityRef, 0, len(registry.current))
	for _, reference := range registry.current {
		result = append(result, reference)
	}
	slices.SortFunc(result, func(left, right CapabilityRef) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	return result
}

func copyCapabilityDefinition(value CapabilityDefinition) CapabilityDefinition {
	value.LifecycleStages = append([]LifecycleStage(nil), value.LifecycleStages...)
	value.AllowedRoles = append([]contracts.SeatRole(nil), value.AllowedRoles...)
	value.RequiredSkills = append([]contracts.SkillID(nil), value.RequiredSkills...)
	value.RequiredDataScopes = append([]contracts.DataScope(nil), value.RequiredDataScopes...)
	value.ReceiptSchemaVersions = append([]string(nil), value.ReceiptSchemaVersions...)
	if value.Previous != nil {
		previous := *value.Previous
		value.Previous = &previous
	}
	if value.ExpiresAt != nil {
		expiresAt := *value.ExpiresAt
		value.ExpiresAt = &expiresAt
	}
	return value
}
