package workorder

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const CompanyCycleOrderSchemaVersion = "workforce.company-cycle-work-order.v1"

type CompanyCycleBinding struct {
	RuntimeConfigID          string                `json:"runtime_config_id"`
	RuntimeConfigVersion     uint64                `json:"runtime_config_version"`
	RuntimeConfigHash        contracts.ContentHash `json:"runtime_config_hash"`
	MissionVersion           uint64                `json:"mission_version"`
	MissionHash              contracts.ContentHash `json:"mission_hash"`
	ConstitutionVersion      uint64                `json:"constitution_version"`
	ConstitutionHash         contracts.ContentHash `json:"constitution_hash"`
	CapitalEnvelopeVersion   uint64                `json:"capital_envelope_version"`
	CapitalEnvelopeHash      contracts.ContentHash `json:"capital_envelope_hash"`
	CycleID                  string                `json:"cycle_id"`
	CadenceKind              string                `json:"cadence_kind"`
	RequiredCapabilities     []string              `json:"required_capabilities"`
	IndependentAudit         bool                  `json:"independent_audit"`
	MaximumCycleMicrounits   uint64                `json:"maximum_cycle_microunits"`
	AggregateLimitMicrounits uint64                `json:"aggregate_limit_microunits"`
}

func (value CompanyCycleBinding) Validate(organizationID contracts.OrganizationID) error {
	if value.RuntimeConfigID != "company-runtime:"+string(organizationID) ||
		value.RuntimeConfigVersion == 0 || value.MissionVersion == 0 ||
		value.ConstitutionVersion == 0 || value.CapitalEnvelopeVersion == 0 ||
		!validOrderID(value.CycleID) || !validOrderID(value.CadenceKind) ||
		!value.IndependentAudit || value.MaximumCycleMicrounits == 0 ||
		value.AggregateLimitMicrounits < value.MaximumCycleMicrounits ||
		!validCompanySet(value.RequiredCapabilities, 1, 64, 128) {
		return fmt.Errorf("workorder: company cycle binding is incomplete")
	}
	for _, hash := range []contracts.ContentHash{
		value.RuntimeConfigHash, value.MissionHash, value.ConstitutionHash,
		value.CapitalEnvelopeHash,
	} {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("workorder: company cycle authority hash: %w", err)
		}
	}
	return nil
}

type CompanyCycleOrder struct {
	SchemaVersion      string                     `json:"schema_version"`
	ID                 string                     `json:"work_order_id"`
	OrganizationID     contracts.OrganizationID   `json:"organization_id"`
	ControllerID       string                     `json:"controller_id"`
	Version            uint64                     `json:"version"`
	Objective          string                     `json:"objective"`
	Scope              string                     `json:"scope"`
	Departments        []contracts.DepartmentKind `json:"departments"`
	Priority           int32                      `json:"priority"`
	Budget             Budget                     `json:"budget"`
	Deadline           time.Time                  `json:"deadline"`
	Autonomy           string                     `json:"autonomy"`
	AcceptanceCriteria []string                   `json:"acceptance_criteria"`
	ModelProvider      string                     `json:"model_provider"`
	ModelID            string                     `json:"model_id"`
	MGSReference       string                     `json:"mgs_reference"`
	MGSDigest          string                     `json:"mgs_digest"`
	Binding            CompanyCycleBinding        `json:"company_cycle_binding"`
	CreatedAt          time.Time                  `json:"created_at"`
	IdempotencyKey     string                     `json:"idempotency_key"`
	Signature          contracts.Signature        `json:"signature"`
}

func (order CompanyCycleOrder) Validate() error {
	if order.SchemaVersion != CompanyCycleOrderSchemaVersion || order.ID == "" ||
		len(order.ID) > 96 || order.OrganizationID == "" ||
		order.ControllerID != "company-controller:"+string(order.OrganizationID) ||
		order.Version != 1 || strings.TrimSpace(order.Objective) == "" ||
		len(order.Objective) > 512 || strings.TrimSpace(order.Scope) == "" ||
		len(order.Scope) > 1024 || len(order.Departments) == 0 ||
		len(order.Departments) > 16 || order.Budget.MaxTasks == 0 ||
		order.Budget.MaxTasks > 50 || order.Budget.MaxSpendMicrounits == 0 ||
		order.Budget.MaxSpendMicrounits > order.Binding.MaximumCycleMicrounits ||
		order.Priority < -1000 || order.Priority > 1000 ||
		order.Autonomy != "bounded_auto" || !validOrderID(order.IdempotencyKey) ||
		order.ModelProvider == "" || len(order.ModelProvider) > 128 ||
		order.ModelID == "" || len(order.ModelID) > 128 ||
		order.MGSReference == "" || len(order.MGSReference) > 512 ||
		len(order.MGSDigest) != 64 || !validCompanySet(order.AcceptanceCriteria, 1, 64, 512) ||
		order.CreatedAt.IsZero() || order.CreatedAt.Location() != time.UTC ||
		order.Deadline.Location() != time.UTC || !order.Deadline.After(order.CreatedAt) {
		return fmt.Errorf("workorder: company cycle order is invalid")
	}
	if err := order.Binding.Validate(order.OrganizationID); err != nil {
		return err
	}
	if !slices.IsSorted(order.Departments) {
		return fmt.Errorf("workorder: company cycle departments must be canonical")
	}
	for index, department := range order.Departments {
		if !department.Valid() || index > 0 && department == order.Departments[index-1] {
			return fmt.Errorf("workorder: company cycle departments are invalid")
		}
	}
	for index := range order.AcceptanceCriteria {
		if _, err := ParseAcceptanceCriterion(index, order.AcceptanceCriteria[index]); err != nil {
			return err
		}
	}
	for _, character := range order.MGSDigest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return fmt.Errorf("workorder: company cycle MGS digest is invalid")
		}
	}
	return order.Signature.Validate()
}

type CompanyCycleAuthority struct {
	RuntimeConfigID          string
	RuntimeConfigVersion     uint64
	RuntimeConfigHash        contracts.ContentHash
	MissionVersion           uint64
	MissionHash              contracts.ContentHash
	ConstitutionVersion      uint64
	ConstitutionHash         contracts.ContentHash
	CapitalEnvelopeVersion   uint64
	CapitalEnvelopeHash      contracts.ContentHash
	AggregateLimitMicrounits uint64
	ControllerKeyID          string
	ControllerPublicKey      ed25519.PublicKey
	EffectiveAt              time.Time
	At                       time.Time
	ExpiresAt                time.Time
}

func (authority CompanyCycleAuthority) Validate(organizationID contracts.OrganizationID) error {
	if authority.RuntimeConfigID != "company-runtime:"+string(organizationID) ||
		authority.RuntimeConfigVersion == 0 || authority.MissionVersion == 0 ||
		authority.ConstitutionVersion == 0 || authority.CapitalEnvelopeVersion == 0 ||
		authority.AggregateLimitMicrounits == 0 || authority.ControllerKeyID == "" ||
		len(authority.ControllerPublicKey) != ed25519.PublicKeySize ||
		authority.EffectiveAt.IsZero() || authority.EffectiveAt.Location() != time.UTC ||
		authority.At.IsZero() || authority.At.Location() != time.UTC ||
		authority.ExpiresAt.Location() != time.UTC || !authority.At.Before(authority.ExpiresAt) {
		return fmt.Errorf("workorder: company cycle authority is invalid")
	}
	for _, hash := range []contracts.ContentHash{
		authority.RuntimeConfigHash, authority.MissionHash, authority.ConstitutionHash,
		authority.CapitalEnvelopeHash,
	} {
		if err := hash.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func SignCompanyCycle(
	order *CompanyCycleOrder,
	authority CompanyCycleAuthority,
	privateKey ed25519.PrivateKey,
) error {
	if order == nil || len(privateKey) != ed25519.PrivateKeySize ||
		!bytes.Equal(privateKey.Public().(ed25519.PublicKey), authority.ControllerPublicKey) {
		return fmt.Errorf("workorder: company cycle signing key is invalid")
	}
	if err := validateCompanyCycleAuthority(*order, authority); err != nil {
		return err
	}
	payload, err := companyCycleSigningBytes(*order, authority.ControllerKeyID)
	if err != nil {
		return err
	}
	order.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: authority.ControllerKeyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return order.Validate()
}

func VerifyCompanyCycle(order CompanyCycleOrder, authority CompanyCycleAuthority) error {
	if err := order.Validate(); err != nil {
		return err
	}
	if err := validateCompanyCycleAuthority(order, authority); err != nil {
		return err
	}
	payload, err := companyCycleSigningBytes(order, authority.ControllerKeyID)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(order.Signature.Value)
	if err != nil || decodeErr != nil || order.Signature.KeyID != authority.ControllerKeyID ||
		!ed25519.Verify(authority.ControllerPublicKey, payload, decoded) {
		return fmt.Errorf("workorder: company cycle signature verification failed")
	}
	return nil
}

func validateCompanyCycleAuthority(order CompanyCycleOrder, authority CompanyCycleAuthority) error {
	if err := authority.Validate(order.OrganizationID); err != nil {
		return err
	}
	binding := order.Binding
	if binding.RuntimeConfigID != authority.RuntimeConfigID ||
		binding.RuntimeConfigVersion != authority.RuntimeConfigVersion ||
		binding.RuntimeConfigHash != authority.RuntimeConfigHash ||
		binding.MissionVersion != authority.MissionVersion || binding.MissionHash != authority.MissionHash ||
		binding.ConstitutionVersion != authority.ConstitutionVersion ||
		binding.ConstitutionHash != authority.ConstitutionHash ||
		binding.CapitalEnvelopeVersion != authority.CapitalEnvelopeVersion ||
		binding.CapitalEnvelopeHash != authority.CapitalEnvelopeHash ||
		binding.AggregateLimitMicrounits != authority.AggregateLimitMicrounits ||
		order.CreatedAt.Before(authority.EffectiveAt) || order.CreatedAt.After(authority.At.Add(5*time.Minute)) ||
		!order.Deadline.After(authority.At) || order.Deadline.After(authority.ExpiresAt) {
		return fmt.Errorf("workorder: company cycle order does not match current authority")
	}
	return nil
}

func companyCycleSigningBytes(order CompanyCycleOrder, keyID string) ([]byte, error) {
	order.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	return contracts.EncodeCanonical(&order)
}
