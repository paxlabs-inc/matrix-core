package workorder

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/mission"
)

const CompanyOrderSchemaVersion = "workforce.company-work-order.v1"

type IssuerKind string

const (
	IssuerOwner             IssuerKind = "owner"
	IssuerCompanyController IssuerKind = "company_controller"
)

func (value IssuerKind) Valid() bool {
	return value == IssuerOwner || value == IssuerCompanyController
}

// CompanyBinding is the exact authority and initiative lineage for one
// controller-issued Work Order. It is part of the controller signature.
type CompanyBinding struct {
	MissionID                   string                `json:"mission_id"`
	MissionVersion              uint64                `json:"mission_version"`
	ConstitutionID              string                `json:"constitution_id"`
	ConstitutionVersion         uint64                `json:"constitution_version"`
	InitiativeID                string                `json:"initiative_id"`
	PortfolioDecisionID         string                `json:"portfolio_decision_id"`
	CapitalAllocationID         string                `json:"capital_allocation_id"`
	CapitalEnvelopeVersion      uint64                `json:"capital_envelope_version"`
	CapitalMicrounits           uint64                `json:"capital_microunits"`
	RiskMicrounits              uint64                `json:"risk_microunits"`
	IssuerPolicyVersion         uint64                `json:"issuer_policy_version"`
	WorkOrderClass              string                `json:"work_order_class"`
	CapabilityPlanID            string                `json:"capability_plan_id"`
	CapabilityPlanHash          contracts.ContentHash `json:"capability_plan_hash"`
	InitiativePlanID            string                `json:"initiative_plan_id"`
	InitiativePlanVersion       uint64                `json:"initiative_plan_version"`
	PlanNodeID                  string                `json:"plan_node_id"`
	InitiativeExecutionCriteria []string              `json:"initiative_execution_criteria"`
	BusinessSuccessCriteria     []string              `json:"business_success_criteria"`
	BusinessOutcomeGateIDs      []string              `json:"business_outcome_gate_ids"`
	EffectIdentities            []string              `json:"effect_identities"`
	IssueIdentity               string                `json:"issue_identity"`
}

func (value CompanyBinding) Validate(organizationID contracts.OrganizationID) error {
	if value.MissionID != "mission:"+string(organizationID) || value.MissionVersion == 0 ||
		value.ConstitutionID != "constitution:"+string(organizationID) ||
		value.ConstitutionVersion == 0 || value.InitiativeID == "" ||
		value.PortfolioDecisionID == "" || value.CapitalAllocationID == "" ||
		value.CapitalEnvelopeVersion == 0 || value.CapitalMicrounits == 0 ||
		value.IssuerPolicyVersion == 0 || value.WorkOrderClass == "" ||
		value.CapabilityPlanID == "" || value.InitiativePlanID == "" ||
		value.InitiativePlanVersion == 0 || value.PlanNodeID == "" ||
		value.IssueIdentity == "" {
		return fmt.Errorf("workorder: company authority binding is incomplete")
	}
	for name, value := range map[string]string{
		"initiative": value.InitiativeID, "portfolio decision": value.PortfolioDecisionID,
		"capital allocation": value.CapitalAllocationID, "Work Order class": value.WorkOrderClass,
		"capability plan": value.CapabilityPlanID, "initiative plan": value.InitiativePlanID,
		"plan node": value.PlanNodeID, "issue identity": value.IssueIdentity,
	} {
		if !validOrderID(value) {
			return fmt.Errorf("workorder: company %s identity is invalid", name)
		}
	}
	if err := value.CapabilityPlanHash.Validate(); err != nil {
		return fmt.Errorf("workorder: capability plan hash: %w", err)
	}
	for name, entries := range map[string][]string{
		"initiative execution criteria": value.InitiativeExecutionCriteria,
		"business criteria":             value.BusinessSuccessCriteria,
		"business gates":                value.BusinessOutcomeGateIDs,
		"effect identities":             value.EffectIdentities,
	} {
		if !validCompanySet(entries, 1, 128, 1024) {
			return fmt.Errorf("workorder: company %s must be sorted, unique, and bounded", name)
		}
	}
	return nil
}

// CompanyOrder is a distinct controller-signed envelope. It deliberately
// does not reuse Order's owner-signature wire format.
type CompanyOrder struct {
	SchemaVersion      string                     `json:"schema_version"`
	ID                 string                     `json:"work_order_id"`
	OrganizationID     contracts.OrganizationID   `json:"organization_id"`
	IssuerKind         IssuerKind                 `json:"issuer_kind"`
	ControllerID       string                     `json:"controller_id"`
	Version            uint64                     `json:"version"`
	Objective          string                     `json:"objective"`
	Scope              string                     `json:"scope"`
	ProjectID          contracts.ProjectID        `json:"project_id"`
	WorkspaceID        contracts.WorkspaceID      `json:"workspace_id"`
	ScopeFiles         []string                   `json:"scope_files"`
	ScopeSymbols       []string                   `json:"scope_symbols"`
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
	Binding            CompanyBinding             `json:"company_binding"`
	CreatedAt          time.Time                  `json:"created_at"`
	IdempotencyKey     string                     `json:"idempotency_key"`
	Signature          contracts.Signature        `json:"signature"`
}

func (order CompanyOrder) Validate() error {
	if order.SchemaVersion != CompanyOrderSchemaVersion || order.ID == "" ||
		len(order.ID) > 96 || order.OrganizationID == "" ||
		order.IssuerKind != IssuerCompanyController ||
		order.ControllerID != "company-controller:"+string(order.OrganizationID) ||
		order.Version == 0 {
		return fmt.Errorf("workorder: company order identity is invalid")
	}
	if strings.TrimSpace(order.Objective) == "" || len(order.Objective) > 512 ||
		strings.TrimSpace(order.Scope) == "" || len(order.Scope) > 1024 {
		return fmt.Errorf("workorder: company objective and scope are required")
	}
	if len(order.Departments) == 0 || len(order.Departments) > 7 {
		return fmt.Errorf("workorder: company order requires one to seven departments")
	}
	seen := make(map[contracts.DepartmentKind]bool, len(order.Departments))
	developer := false
	for _, department := range order.Departments {
		if !department.Valid() || seen[department] {
			return fmt.Errorf("workorder: company departments are invalid")
		}
		seen[department] = true
		developer = developer || department == contracts.DepartmentDeveloper
	}
	if developer {
		if !filepath.IsAbs(order.Scope) || !validOrderID(string(order.ProjectID)) ||
			!validOrderID(string(order.WorkspaceID)) || !validScopeValues(order.ScopeFiles, true) ||
			!validScopeValues(order.ScopeSymbols, false) {
			return fmt.Errorf("workorder: company Developer order requires exact project scope")
		}
	} else if order.ProjectID != "" || order.WorkspaceID != "" ||
		len(order.ScopeFiles) != 0 || len(order.ScopeSymbols) != 0 {
		return fmt.Errorf("workorder: company Developer scope requires Developer department")
	}
	if order.Priority < -1000 || order.Priority > 1000 || order.Budget.MaxTasks == 0 ||
		order.Budget.MaxTasks > 50 || order.Budget.MaxSpendMicrounits == 0 {
		return fmt.Errorf("workorder: company priority or budget is invalid")
	}
	if order.CreatedAt.IsZero() || order.CreatedAt.Location() != time.UTC ||
		order.Deadline.Location() != time.UTC || !order.Deadline.After(order.CreatedAt) {
		return fmt.Errorf("workorder: company order times are invalid")
	}
	if order.Autonomy != "supervised" && order.Autonomy != "review_required" &&
		order.Autonomy != "bounded_auto" {
		return fmt.Errorf("workorder: company autonomy is invalid")
	}
	if len(order.AcceptanceCriteria) == 0 || len(order.AcceptanceCriteria) > 20 {
		return fmt.Errorf("workorder: company execution criteria are required")
	}
	for index, criterion := range order.AcceptanceCriteria {
		if _, err := ParseAcceptanceCriterion(index, criterion); err != nil {
			return err
		}
	}
	if strings.TrimSpace(order.ModelProvider) == "" || len(order.ModelProvider) > 128 ||
		strings.TrimSpace(order.ModelID) == "" || len(order.ModelID) > 128 ||
		strings.TrimSpace(order.MGSReference) == "" || len(order.MGSReference) > 512 ||
		!validOrderID(order.IdempotencyKey) || len(order.MGSDigest) != 64 {
		return fmt.Errorf("workorder: company model, MGS, or idempotency binding is invalid")
	}
	for _, character := range order.MGSDigest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return fmt.Errorf("workorder: company MGS digest is invalid")
		}
	}
	if err := order.Binding.Validate(order.OrganizationID); err != nil {
		return err
	}
	if order.Binding.CapitalMicrounits < order.Budget.MaxSpendMicrounits ||
		order.Binding.IssueIdentity != ExpectedCompanyIssueIdentity(order) {
		return fmt.Errorf("workorder: company budget or issue identity is invalid")
	}
	return order.Signature.Validate()
}

type CompanyAuthority struct {
	Policy                        mission.CompanyIssuerPolicy
	FounderKeyID                  string
	FounderPublicKey              ed25519.PublicKey
	CurrentMissionVersion         uint64
	CurrentConstitutionVersion    uint64
	CurrentCapitalEnvelopeVersion uint64
	At                            time.Time
}

func (authority CompanyAuthority) Validate(organizationID contracts.OrganizationID) error {
	if authority.Policy.OrganizationID != organizationID ||
		authority.CurrentMissionVersion == 0 ||
		authority.CurrentConstitutionVersion == 0 ||
		authority.CurrentCapitalEnvelopeVersion == 0 ||
		authority.Policy.MissionVersion != authority.CurrentMissionVersion ||
		authority.Policy.ConstitutionVersion != authority.CurrentConstitutionVersion ||
		authority.Policy.CapitalEnvelopeVersion != authority.CurrentCapitalEnvelopeVersion {
		return fmt.Errorf("workorder: company issuer policy is not the exact current authority")
	}
	return mission.VerifyCompanyIssuerPolicy(
		authority.Policy, authority.FounderKeyID, authority.FounderPublicKey, authority.At,
	)
}

func SignCompany(order *CompanyOrder, authority CompanyAuthority, privateKey ed25519.PrivateKey) error {
	if order == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("workorder: company signing authority is invalid")
	}
	if err := validateCompanyAuthority(*order, authority); err != nil {
		return err
	}
	issuerPublicKey, err := decodeIssuerPublicKey(authority.Policy)
	if err != nil || !bytes.Equal(privateKey.Public().(ed25519.PublicKey), issuerPublicKey) {
		return fmt.Errorf("workorder: company private key does not match issuer policy")
	}
	payload, err := companySigningBytes(*order, authority.Policy.IssuerKeyID)
	if err != nil {
		return err
	}
	order.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: authority.Policy.IssuerKeyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return order.Validate()
}

func VerifyCompany(order CompanyOrder, authority CompanyAuthority) error {
	if err := order.Validate(); err != nil {
		return err
	}
	if err := validateCompanyAuthority(order, authority); err != nil {
		return err
	}
	issuerPublicKey, err := decodeIssuerPublicKey(authority.Policy)
	if err != nil || order.Signature.KeyID != authority.Policy.IssuerKeyID {
		return fmt.Errorf("workorder: company signature authority mismatch")
	}
	payload, err := companySigningBytes(order, authority.Policy.IssuerKeyID)
	if err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(order.Signature.Value)
	if err != nil || !ed25519.Verify(issuerPublicKey, payload, decoded) {
		return fmt.Errorf("workorder: company signature verification failed")
	}
	return nil
}

func validateCompanyAuthority(order CompanyOrder, authority CompanyAuthority) error {
	if err := authority.Validate(order.OrganizationID); err != nil {
		return err
	}
	binding := order.Binding
	if binding.MissionVersion != authority.CurrentMissionVersion ||
		binding.ConstitutionVersion != authority.CurrentConstitutionVersion ||
		binding.CapitalEnvelopeVersion != authority.CurrentCapitalEnvelopeVersion ||
		binding.IssuerPolicyVersion != authority.Policy.Version ||
		order.CreatedAt.Before(authority.Policy.EffectiveAt) ||
		!order.CreatedAt.Before(authority.Policy.ExpiresAt) ||
		order.Deadline.After(authority.Policy.ExpiresAt) ||
		order.Budget.MaxSpendMicrounits > authority.Policy.MaxWorkOrderMicrounits ||
		!slices.Contains(authority.Policy.AllowedWorkOrderClasses, binding.WorkOrderClass) {
		return fmt.Errorf("workorder: company order exceeds or mismatches issuer policy")
	}
	return nil
}

func ExpectedCompanyIssueIdentity(order CompanyOrder) string {
	value := strings.Join([]string{
		string(order.OrganizationID), order.Binding.InitiativeID,
		order.Binding.InitiativePlanID, fmt.Sprint(order.Binding.InitiativePlanVersion),
		order.Binding.PlanNodeID, order.Binding.WorkOrderClass,
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return "company-issue:" + hex.EncodeToString(sum[:])
}

func companySigningBytes(order CompanyOrder, keyID string) ([]byte, error) {
	order.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	return contracts.EncodeCanonical(&order)
}

func decodeIssuerPublicKey(policy mission.CompanyIssuerPolicy) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(policy.IssuerPublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("workorder: company issuer public key is invalid")
	}
	return ed25519.PublicKey(decoded), nil
}

// IssuedOrder is an explicit owner-or-controller union. The two signature
// domains cannot be decoded or verified as one another.
type IssuedOrder struct {
	Issuer  IssuerKind    `json:"issuer"`
	Owner   *Order        `json:"owner_order"`
	Company *CompanyOrder `json:"company_order"`
}

func (value IssuedOrder) Validate() error {
	switch value.Issuer {
	case IssuerOwner:
		if value.Owner == nil || value.Company != nil {
			return fmt.Errorf("workorder: owner issuer union is invalid")
		}
		return value.Owner.Validate()
	case IssuerCompanyController:
		if value.Company == nil || value.Owner != nil {
			return fmt.Errorf("workorder: company issuer union is invalid")
		}
		return value.Company.Validate()
	default:
		return fmt.Errorf("workorder: issuer kind is invalid")
	}
}

type VerificationAuthority struct {
	OwnerKeyID     string
	OwnerPublicKey ed25519.PublicKey
	Company        *CompanyAuthority
}

func VerifyIssued(value IssuedOrder, authority VerificationAuthority) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if value.Issuer == IssuerOwner {
		if authority.Company != nil {
			return fmt.Errorf("workorder: company authority cannot verify an owner order")
		}
		return Verify(*value.Owner, authority.OwnerKeyID, authority.OwnerPublicKey)
	}
	if authority.Company == nil || authority.OwnerKeyID != "" || len(authority.OwnerPublicKey) != 0 {
		return fmt.Errorf("workorder: owner authority cannot verify a company order")
	}
	return VerifyCompany(*value.Company, *authority.Company)
}

func validCompanySet(values []string, minimum, maximum, maxBytes int) bool {
	if len(values) < minimum || len(values) > maximum || !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > maxBytes ||
			index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}
