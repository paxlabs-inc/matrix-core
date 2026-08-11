package external

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/skills"
)

var (
	ErrDenied         = errors.New("external adapter: operation denied")
	ErrConflict       = errors.New("external adapter: durable identity conflict")
	ErrAmbiguous      = errors.New("external adapter: external outcome is ambiguous")
	ErrUnavailable    = errors.New("external adapter: provider unavailable")
	ErrIntegrity      = errors.New("external adapter: sealed authority integrity failure")
	ErrRetryExhausted = errors.New("external adapter: retry budget exhausted")
	ErrCapacity       = errors.New("external adapter: path-local capacity exhausted")
	ErrCircuitOpen    = errors.New("external adapter: path-local circuit is open")
)

const SchemaVersion = contracts.SchemaVersionV1

type Family string

const (
	FamilyBrowserResearch      Family = "browser_research"
	FamilyBrowserAuthenticated Family = "browser_authenticated"
	FamilyWebsite              Family = "website"
	FamilyPublication          Family = "publication"
	FamilyProductAnalytics     Family = "product_analytics"
	FamilyDeployment           Family = "deployment"
	FamilyInfrastructure       Family = "infrastructure"
	FamilyAuthoritativeObserve Family = "authoritative_observation"
	FamilyFinancialTransport   Family = "financial_transport"
)

func (value Family) Valid() bool {
	switch value {
	case FamilyBrowserResearch, FamilyBrowserAuthenticated, FamilyWebsite,
		FamilyPublication, FamilyProductAnalytics, FamilyDeployment,
		FamilyInfrastructure, FamilyAuthoritativeObserve, FamilyFinancialTransport:
		return true
	default:
		return false
	}
}

func containsClassification(values []DataClassification, target DataClassification) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type Protocol string

const (
	ProtocolWorkforceJSON Protocol = "workforce_json_v1"
	ProtocolMatrixMCP     Protocol = "matrix_mcp_2024_11_05"
)

func (value Protocol) Valid() bool {
	return value == ProtocolWorkforceJSON || value == ProtocolMatrixMCP
}

type ActionClass string

const (
	ActionNavigate    ActionClass = "navigate"
	ActionRead        ActionClass = "read"
	ActionExtract     ActionClass = "extract"
	ActionInteract    ActionClass = "interact"
	ActionSubmit      ActionClass = "submit"
	ActionUpload      ActionClass = "upload"
	ActionDownload    ActionClass = "download"
	ActionPublish     ActionClass = "publish"
	ActionUnpublish   ActionClass = "unpublish"
	ActionRecordEvent ActionClass = "record_event"
	ActionQueryMetric ActionClass = "query_metric"
	ActionDeploy      ActionClass = "deploy"
	ActionRollback    ActionClass = "rollback"
	ActionConfigure   ActionClass = "configure"
	ActionObserve     ActionClass = "observe"
)

func (value ActionClass) Valid() bool {
	switch value {
	case ActionNavigate, ActionRead, ActionExtract, ActionInteract, ActionSubmit,
		ActionUpload, ActionDownload, ActionPublish, ActionUnpublish,
		ActionRecordEvent, ActionQueryMetric, ActionDeploy, ActionRollback,
		ActionConfigure, ActionObserve:
		return true
	default:
		return false
	}
}

func (value ActionClass) Mutates() bool {
	switch value {
	case ActionInteract, ActionSubmit, ActionUpload, ActionPublish,
		ActionUnpublish, ActionRecordEvent, ActionDeploy, ActionRollback,
		ActionConfigure:
		return true
	default:
		return false
	}
}

type NavigationClass string

const (
	NavigationDirect      NavigationClass = "direct"
	NavigationSameOrigin  NavigationClass = "same_origin"
	NavigationAllowlisted NavigationClass = "allowlisted_origins"
)

func (value NavigationClass) Valid() bool {
	return value == NavigationDirect || value == NavigationSameOrigin ||
		value == NavigationAllowlisted
}

type DataClassification string

const (
	DataPublic           DataClassification = "public"
	DataInternal         DataClassification = "internal"
	DataConfidential     DataClassification = "confidential"
	DataCustomerPersonal DataClassification = "customer_personal"
	DataFinancial        DataClassification = "financial"
	DataRestricted       DataClassification = "restricted"
)

func (value DataClassification) Valid() bool {
	switch value {
	case DataPublic, DataInternal, DataConfidential, DataCustomerPersonal,
		DataFinancial, DataRestricted:
		return true
	default:
		return false
	}
}

type TransferMode string

const (
	TransferDeny     TransferMode = "deny"
	TransferMetadata TransferMode = "metadata_only"
	TransferInline   TransferMode = "inline_exact"
)

func (value TransferMode) Valid() bool {
	return value == TransferDeny || value == TransferMetadata || value == TransferInline
}

type IdempotencyMode string

const (
	IdempotencyProviderKey     IdempotencyMode = "provider_key"
	IdempotencyResourceVersion IdempotencyMode = "resource_version"
	IdempotencySingleAttempt   IdempotencyMode = "single_attempt"
)

func (value IdempotencyMode) Valid() bool {
	return value == IdempotencyProviderKey || value == IdempotencyResourceVersion ||
		value == IdempotencySingleAttempt
}

type ExternalState string

const (
	ExternalCompleted  ExternalState = "completed"
	ExternalPending    ExternalState = "pending"
	ExternalRejected   ExternalState = "rejected"
	ExternalReversed   ExternalState = "reversed"
	ExternalDrifted    ExternalState = "drifted"
	ExternalConflicted ExternalState = "conflicted"
	ExternalUnknown    ExternalState = "unknown"
)

func (value ExternalState) Valid() bool {
	switch value {
	case ExternalCompleted, ExternalPending, ExternalRejected, ExternalReversed,
		ExternalDrifted, ExternalConflicted, ExternalUnknown:
		return true
	default:
		return false
	}
}

type ObservationAuthority string

const (
	AuthorityUntrustedExternal ObservationAuthority = "untrusted_external_data"
	AuthorityProvider          ObservationAuthority = "provider_authoritative"
	AuthorityControlPlane      ObservationAuthority = "control_plane_authoritative"
)

func (value ObservationAuthority) Valid() bool {
	return value == AuthorityUntrustedExternal || value == AuthorityProvider ||
		value == AuthorityControlPlane
}

type CredentialKind string

const (
	CredentialNone   CredentialKind = "none"
	CredentialBearer CredentialKind = "bearer"
	CredentialAPIKey CredentialKind = "api_key"
	CredentialBasic  CredentialKind = "basic"
)

func (value CredentialKind) Valid() bool {
	return value == CredentialNone || value == CredentialBearer ||
		value == CredentialAPIKey || value == CredentialBasic
}

type CredentialMaterial struct {
	ID         string         `json:"id"`
	Kind       CredentialKind `json:"kind"`
	HeaderName string         `json:"header_name"`
	Scheme     string         `json:"scheme"`
	Username   string         `json:"username"`
	Secret     []byte         `json:"secret"`
}

type credentialBinding struct {
	SchemaVersion string             `json:"schema_version"`
	ConnectionID  string             `json:"connection_id"`
	Version       uint64             `json:"version"`
	Credential    CredentialMaterial `json:"credential"`
}

func (value credentialBinding) Validate() error {
	if value.SchemaVersion != SchemaVersion ||
		token("connection id", value.ConnectionID) != nil || value.Version == 0 ||
		value.Credential.Validate() != nil {
		return fmt.Errorf("external adapter: credential binding is invalid")
	}
	return nil
}

func (value CredentialMaterial) Validate() error {
	if err := token("credential id", value.ID); err != nil || !value.Kind.Valid() {
		return fmt.Errorf("external adapter: invalid credential identity")
	}
	if containsLineBreak(value.HeaderName) || containsLineBreak(value.Scheme) ||
		containsLineBreak(value.Username) || len(value.HeaderName) > 128 ||
		len(value.Scheme) > 64 || len(value.Username) > 512 || len(value.Secret) > 16<<10 {
		return fmt.Errorf("external adapter: invalid credential material")
	}
	switch value.Kind {
	case CredentialNone:
		if value.HeaderName != "" || value.Scheme != "" || value.Username != "" || len(value.Secret) != 0 {
			return fmt.Errorf("external adapter: unauthenticated credential must be empty")
		}
	case CredentialBearer:
		if value.HeaderName != "Authorization" || value.Scheme != "Bearer" ||
			value.Username != "" || len(value.Secret) == 0 {
			return fmt.Errorf("external adapter: bearer credential profile is invalid")
		}
	case CredentialAPIKey:
		if !validHeaderName(value.HeaderName) || value.Scheme != "" ||
			value.Username != "" || len(value.Secret) == 0 {
			return fmt.Errorf("external adapter: API key credential profile is invalid")
		}
	case CredentialBasic:
		if value.HeaderName != "Authorization" || value.Scheme != "Basic" ||
			strings.TrimSpace(value.Username) == "" || len(value.Secret) == 0 {
			return fmt.Errorf("external adapter: basic credential profile is invalid")
		}
	}
	return nil
}

func (value CredentialMaterial) Clone() CredentialMaterial {
	value.Secret = append([]byte(nil), value.Secret...)
	return value
}

type AuthorityBinding struct {
	MissionVersion      uint64                `json:"mission_version"`
	ConstitutionVersion uint64                `json:"constitution_version"`
	OrganizationVersion uint64                `json:"organization_version"`
	OperatingScopeID    string                `json:"operating_scope_id"`
	OperatingScopeHash  contracts.ContentHash `json:"operating_scope_hash"`
	PolicyRefs          []contracts.PolicyRef `json:"policy_refs"`
}

func (value AuthorityBinding) Validate() error {
	if value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.OrganizationVersion == 0 || token("operating scope id", value.OperatingScopeID) != nil ||
		value.OperatingScopeHash.Validate() != nil || len(value.PolicyRefs) == 0 ||
		len(value.PolicyRefs) > 64 {
		return fmt.Errorf("external adapter: authority binding is incomplete")
	}
	seen := make(map[contracts.PolicyID]bool, len(value.PolicyRefs))
	for _, reference := range value.PolicyRefs {
		if reference.Validate() != nil || seen[reference.ID] {
			return fmt.Errorf("external adapter: authority policy binding is invalid")
		}
		seen[reference.ID] = true
	}
	return nil
}

type TransferPolicy struct {
	Mode              TransferMode `json:"mode"`
	MaxBytes          uint64       `json:"max_bytes"`
	AllowedMediaTypes []string     `json:"allowed_media_types"`
}

func (value TransferPolicy) Validate() error {
	if !value.Mode.Valid() || value.MaxBytes > 192<<10 || len(value.AllowedMediaTypes) > 64 {
		return fmt.Errorf("external adapter: transfer policy is invalid")
	}
	if value.Mode == TransferDeny {
		if value.MaxBytes != 0 || len(value.AllowedMediaTypes) != 0 {
			return fmt.Errorf("external adapter: denied transfer cannot carry allowances")
		}
		return nil
	}
	if value.MaxBytes == 0 || len(value.AllowedMediaTypes) == 0 {
		return fmt.Errorf("external adapter: allowed transfer requires bounded media types")
	}
	for _, mediaType := range value.AllowedMediaTypes {
		if !validMediaType(mediaType) {
			return fmt.Errorf("external adapter: invalid transfer media type")
		}
	}
	return nil
}

type GovernancePolicy struct {
	Channels           []string `json:"channels"`
	Audiences          []string `json:"audiences"`
	Claims             []string `json:"claims"`
	Environments       []string `json:"environments"`
	Jurisdictions      []string `json:"jurisdictions"`
	ConsentRefs        []string `json:"consent_refs"`
	Recipients         []string `json:"recipients"`
	Counterparties     []string `json:"counterparties"`
	BrandPolicyRefs    []string `json:"brand_policy_refs"`
	LegalPolicyRefs    []string `json:"legal_policy_refs"`
	SecurityPolicyRefs []string `json:"security_policy_refs"`
	RollbackPolicyRefs []string `json:"rollback_policy_refs"`
}

func (value GovernancePolicy) Validate() error {
	sets := [][]string{
		value.Channels, value.Audiences, value.Claims, value.Environments,
		value.Jurisdictions, value.ConsentRefs, value.Recipients,
		value.Counterparties, value.BrandPolicyRefs, value.LegalPolicyRefs,
		value.SecurityPolicyRefs, value.RollbackPolicyRefs,
	}
	for _, set := range sets {
		if len(set) > 128 || !validDistinctValues(set, 1024) {
			return fmt.Errorf("external adapter: governance policy is invalid")
		}
	}
	return nil
}

type ResourceLimits struct {
	ConnectTimeout        time.Duration `json:"connect_timeout"`
	ResponseHeaderTimeout time.Duration `json:"response_header_timeout"`
	StreamIdleTimeout     time.Duration `json:"stream_idle_timeout"`
	TotalTimeout          time.Duration `json:"total_timeout"`
	RetryWindow           time.Duration `json:"retry_window"`
	MaxObservationAge     time.Duration `json:"max_observation_age"`
	OutputBytes           uint64        `json:"output_bytes"`
	MaxConcurrent         uint16        `json:"max_concurrent"`
	MaxAttempts           uint16        `json:"max_attempts"`
	MaxRedirects          uint16        `json:"max_redirects"`
	DriftBlindMutations   uint16        `json:"drift_blind_mutations"`
	FailureThreshold      uint16        `json:"failure_threshold"`
	SuccessThreshold      uint16        `json:"success_threshold"`
	HalfOpenLimit         uint16        `json:"half_open_limit"`
	CircuitWindow         time.Duration `json:"circuit_window"`
	CircuitOpenDuration   time.Duration `json:"circuit_open_duration"`
}

func (value ResourceLimits) Validate() error {
	if value.ConnectTimeout <= 0 || value.ConnectTimeout > 30*time.Second ||
		value.ResponseHeaderTimeout <= 0 || value.ResponseHeaderTimeout > 2*time.Minute ||
		value.StreamIdleTimeout <= 0 || value.StreamIdleTimeout > 5*time.Minute ||
		value.TotalTimeout <= 0 || value.TotalTimeout > 10*time.Minute ||
		value.ConnectTimeout > value.TotalTimeout ||
		value.ResponseHeaderTimeout > value.TotalTimeout ||
		value.RetryWindow <= 0 || value.RetryWindow > 24*time.Hour ||
		value.MaxObservationAge <= 0 || value.MaxObservationAge > 30*24*time.Hour ||
		value.OutputBytes == 0 || value.OutputBytes > 1<<20 ||
		value.MaxConcurrent == 0 || value.MaxConcurrent > 4 ||
		value.MaxAttempts == 0 || value.MaxAttempts > 16 ||
		value.MaxRedirects > 10 || value.DriftBlindMutations > 8 {
		return fmt.Errorf("external adapter: resource limits are outside hard ceilings")
	}
	if value.FailureThreshold == 0 || value.FailureThreshold > 100 ||
		value.SuccessThreshold == 0 || value.SuccessThreshold > 100 ||
		value.HalfOpenLimit == 0 || value.HalfOpenLimit > 4 ||
		value.CircuitWindow <= 0 || value.CircuitWindow > 24*time.Hour ||
		value.CircuitOpenDuration <= 0 || value.CircuitOpenDuration > 24*time.Hour {
		return fmt.Errorf("external adapter: circuit limits are outside hard ceilings")
	}
	return nil
}

type OperationPolicy struct {
	Name                  string             `json:"name"`
	Action                ActionClass        `json:"action"`
	EffectClass           skills.EffectClass `json:"effect_class"`
	Method                string             `json:"method"`
	ProbeMethod           string             `json:"probe_method"`
	DispatchPath          string             `json:"dispatch_path"`
	ProbePath             string             `json:"probe_path"`
	MCPTool               string             `json:"mcp_tool"`
	MCPProbeTool          string             `json:"mcp_probe_tool"`
	Idempotency           IdempotencyMode    `json:"idempotency"`
	IdempotencyHeader     string             `json:"idempotency_header"`
	DispatchAuthoritative bool               `json:"dispatch_authoritative"`
	ProbeAuthoritative    bool               `json:"probe_authoritative"`
	CompensationOperation string             `json:"compensation_operation"`
	CompensatesOperation  string             `json:"compensates_operation"`
}

func (value OperationPolicy) Validate(protocol Protocol) error {
	if token("operation", value.Name) != nil || !value.Action.Valid() ||
		!value.EffectClass.Valid() || !value.Idempotency.Valid() {
		return fmt.Errorf("external adapter: operation policy identity is invalid")
	}
	if value.Action.Mutates() == (value.EffectClass == skills.EffectRead) {
		return fmt.Errorf("external adapter: action and effect class disagree")
	}
	if protocol == ProtocolMatrixMCP {
		if !allowedMCPTool(value.MCPTool, value.Action) || value.Method != "" ||
			value.ProbeMethod != "" || value.DispatchPath != "" || value.ProbePath != "" ||
			value.DispatchAuthoritative || value.ProbeAuthoritative {
			return fmt.Errorf("external adapter: MCP operation policy is invalid")
		}
		if value.MCPProbeTool != "" && !allowedMCPTool(value.MCPProbeTool, ActionRead) {
			return fmt.Errorf("external adapter: MCP probe tool is unsafe")
		}
	} else {
		if value.MCPTool != "" || value.MCPProbeTool != "" ||
			!validMethod(value.Method) || !validEndpointPath(value.DispatchPath) {
			return fmt.Errorf("external adapter: HTTP operation policy is invalid")
		}
		if value.ProbeAuthoritative &&
			(!validMethod(value.ProbeMethod) || !validEndpointPath(value.ProbePath)) {
			return fmt.Errorf("external adapter: authoritative probe path is required")
		}
		if !value.ProbeAuthoritative && (value.ProbeMethod != "" || value.ProbePath != "") {
			return fmt.Errorf("external adapter: drift-blind operation cannot declare a probe route")
		}
	}
	if value.Action.Mutates() {
		switch value.Idempotency {
		case IdempotencyProviderKey:
			if !validHeaderName(value.IdempotencyHeader) {
				return fmt.Errorf("external adapter: provider idempotency header is invalid")
			}
		case IdempotencyResourceVersion:
			if value.IdempotencyHeader != "If-Match" {
				return fmt.Errorf("external adapter: resource-version operation must use If-Match")
			}
		case IdempotencySingleAttempt:
			if value.IdempotencyHeader != "" || value.ProbeAuthoritative {
				return fmt.Errorf("external adapter: single-attempt policy is inconsistent")
			}
		}
	}
	if value.Idempotency == IdempotencyProviderKey &&
		!validHeaderName(value.IdempotencyHeader) {
		return fmt.Errorf("external adapter: provider idempotency header is invalid")
	}
	if value.Idempotency == IdempotencyResourceVersion &&
		value.IdempotencyHeader != "If-Match" {
		return fmt.Errorf("external adapter: resource-version operation must use If-Match")
	}
	if protocol == ProtocolMatrixMCP && value.Action.Mutates() &&
		value.Idempotency != IdempotencySingleAttempt {
		return fmt.Errorf("external adapter: browser mutations without provider keys are single-attempt")
	}
	if value.EffectClass == skills.EffectReversible &&
		token("compensation operation", value.CompensationOperation) != nil {
		return fmt.Errorf("external adapter: reversible operation requires compensation")
	}
	if value.EffectClass != skills.EffectReversible && value.CompensationOperation != "" {
		return fmt.Errorf("external adapter: only reversible operations declare compensation")
	}
	if value.CompensatesOperation != "" && value.Action != ActionRollback &&
		value.Action != ActionUnpublish {
		return fmt.Errorf("external adapter: compensation must be rollback or unpublish")
	}
	return nil
}

type Connection struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"id"`
	Version             uint64                   `json:"version"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	AdapterName         string                   `json:"adapter_name"`
	Family              Family                   `json:"family"`
	Protocol            Protocol                 `json:"protocol"`
	Provider            string                   `json:"provider"`
	EndpointURL         string                   `json:"endpoint_url"`
	PrivateNetworkHTTP  bool                     `json:"private_network_http"`
	AccountID           string                   `json:"account_id"`
	IdentityID          string                   `json:"identity_id"`
	CredentialID        string                   `json:"credential_id"`
	CredentialBinding   contracts.ContentHash    `json:"credential_binding"`
	Authority           AuthorityBinding         `json:"authority"`
	TargetOrigins       []string                 `json:"target_origins"`
	NavigationPrefixes  []string                 `json:"navigation_prefixes"`
	NavigationClasses   []NavigationClass        `json:"navigation_classes"`
	DataClassifications []DataClassification     `json:"data_classifications"`
	Upload              TransferPolicy           `json:"upload"`
	Download            TransferPolicy           `json:"download"`
	Governance          GovernancePolicy         `json:"governance"`
	Limits              ResourceLimits           `json:"limits"`
	Operations          []OperationPolicy        `json:"operations"`
	EffectiveAt         time.Time                `json:"effective_at"`
	ExpiresAt           time.Time                `json:"expires_at"`
	Signature           contracts.Signature      `json:"signature"`
}

func (value Connection) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("connection id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("adapter name", value.AdapterName) != nil || !value.Family.Valid() ||
		!value.Protocol.Valid() || token("provider", value.Provider) != nil ||
		boundedIdentifier("account id", value.AccountID) != nil ||
		boundedIdentifier("identity id", value.IdentityID) != nil ||
		token("credential id", value.CredentialID) != nil || value.CredentialBinding.Validate() != nil {
		return fmt.Errorf("external adapter: connection identity is invalid")
	}
	if value.Protocol == ProtocolMatrixMCP && value.Family != FamilyBrowserResearch {
		return fmt.Errorf("external adapter: Centra AI MCP is restricted to unauthenticated research")
	}
	if _, err := validatedEndpoint(value.EndpointURL, value.PrivateNetworkHTTP); err != nil {
		return err
	}
	if err := value.Authority.Validate(); err != nil || value.Upload.Validate() != nil ||
		value.Download.Validate() != nil || value.Governance.Validate() != nil ||
		value.Limits.Validate() != nil {
		return fmt.Errorf("external adapter: connection policy is invalid")
	}
	if len(value.TargetOrigins) == 0 || len(value.TargetOrigins) > 64 ||
		len(value.NavigationPrefixes) == 0 || len(value.NavigationPrefixes) > 128 ||
		len(value.NavigationClasses) == 0 || len(value.NavigationClasses) > 3 ||
		len(value.DataClassifications) == 0 || len(value.DataClassifications) > 6 ||
		len(value.Operations) == 0 || len(value.Operations) > 64 {
		return fmt.Errorf("external adapter: connection allowlists are outside bounds")
	}
	seenOrigins := make(map[string]bool, len(value.TargetOrigins))
	for _, origin := range value.TargetOrigins {
		normal, err := normalizedOrigin(origin)
		if err != nil || normal != origin || seenOrigins[origin] {
			return fmt.Errorf("external adapter: target origin is not normalized and unique")
		}
		seenOrigins[origin] = true
	}
	if !validNavigationPrefixes(value.NavigationPrefixes) ||
		!validDistinctNavigation(value.NavigationClasses) ||
		!validDistinctClassifications(value.DataClassifications) {
		return fmt.Errorf("external adapter: connection navigation or data scope is invalid")
	}
	seenOperations := make(map[string]OperationPolicy, len(value.Operations))
	for _, operation := range value.Operations {
		if err := operation.Validate(value.Protocol); err != nil {
			return err
		}
		if _, exists := seenOperations[operation.Name]; exists {
			return fmt.Errorf("external adapter: operation is duplicated")
		}
		seenOperations[operation.Name] = operation
	}
	if value.Family == FamilyBrowserResearch || value.Family == FamilyAuthoritativeObserve {
		for _, operation := range value.Operations {
			if operation.Action.Mutates() {
				return fmt.Errorf("external adapter: read-only family cannot declare mutations")
			}
		}
	}
	for _, operation := range value.Operations {
		if operation.CompensationOperation != "" {
			compensation, exists := seenOperations[operation.CompensationOperation]
			if !exists || compensation.CompensatesOperation != operation.Name {
				return fmt.Errorf("external adapter: compensation operation is not reciprocal")
			}
		}
		if operation.Action.Mutates() && !operation.ProbeAuthoritative &&
			value.Limits.DriftBlindMutations == 0 {
			return fmt.Errorf("external adapter: drift-blind mutation has no autonomy ceiling")
		}
		if operation.Action.Mutates() && operation.ProbeAuthoritative &&
			value.Limits.MaxAttempts < 2 {
			return fmt.Errorf("external adapter: mutation retry budget must reserve one probe")
		}
		if operation.Idempotency == IdempotencySingleAttempt && value.Limits.DriftBlindMutations > 1 {
			return fmt.Errorf("external adapter: single-attempt mutation ceiling must be one")
		}
		if operation.Action.Mutates() && len(value.Governance.SecurityPolicyRefs) == 0 {
			return fmt.Errorf("external adapter: mutation requires a security policy")
		}
		if operation.EffectClass == skills.EffectReversible &&
			len(value.Governance.RollbackPolicyRefs) == 0 {
			return fmt.Errorf("external adapter: reversible mutation requires rollback policy")
		}
	}
	if value.Family == FamilyPublication || value.Family == FamilyWebsite {
		if len(value.Governance.Channels) == 0 || len(value.Governance.Audiences) == 0 ||
			len(value.Governance.Claims) == 0 || len(value.Governance.BrandPolicyRefs) == 0 ||
			len(value.Governance.LegalPolicyRefs) == 0 ||
			len(value.Governance.SecurityPolicyRefs) == 0 ||
			len(value.Governance.RollbackPolicyRefs) == 0 {
			return fmt.Errorf("external adapter: publication governance is incomplete")
		}
	}
	if value.Family == FamilyDeployment || value.Family == FamilyInfrastructure {
		if len(value.Governance.Environments) == 0 ||
			len(value.Governance.SecurityPolicyRefs) == 0 ||
			len(value.Governance.RollbackPolicyRefs) == 0 {
			return fmt.Errorf("external adapter: deployment governance is incomplete")
		}
	}
	if value.Family == FamilyFinancialTransport {
		if !containsClassification(value.DataClassifications, DataFinancial) ||
			len(value.Governance.Counterparties) == 0 ||
			len(value.Governance.Jurisdictions) == 0 ||
			len(value.Governance.SecurityPolicyRefs) == 0 ||
			value.Upload.Mode != TransferDeny || value.Download.Mode != TransferDeny {
			return fmt.Errorf("external adapter: financial transport governance is incomplete")
		}
	}
	if !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) ||
		value.ExpiresAt.Sub(value.EffectiveAt) > 366*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("external adapter: connection validity or signature is invalid")
	}
	return nil
}

func (value Connection) Operation(name string) (OperationPolicy, bool) {
	for _, operation := range value.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return OperationPolicy{}, false
}

type ConnectionRevocation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ConnectionID   string                   `json:"connection_id"`
	Version        uint64                   `json:"version"`
	ReasonCode     string                   `json:"reason_code"`
	RevokedAt      time.Time                `json:"revoked_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value ConnectionRevocation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("revocation id", value.ID) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.Version == 0 ||
		token("reason code", value.ReasonCode) != nil || !validUTC(value.RevokedAt) ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("external adapter: connection revocation is invalid")
	}
	return nil
}

type Transfer struct {
	MediaType string                `json:"media_type"`
	SizeBytes uint64                `json:"size_bytes"`
	Hash      contracts.ContentHash `json:"hash"`
	Content   []byte                `json:"content"`
}

func (value Transfer) Validate(policy TransferPolicy) error {
	if policy.Mode != TransferInline || !validMediaType(value.MediaType) ||
		!contains(policy.AllowedMediaTypes, value.MediaType) || value.SizeBytes == 0 ||
		value.SizeBytes > policy.MaxBytes || value.SizeBytes != uint64(len(value.Content)) ||
		value.Hash.Validate() != nil {
		return fmt.Errorf("external adapter: transfer is outside policy")
	}
	sum := sha256.Sum256(value.Content)
	if value.Hash.Digest != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("external adapter: transfer hash mismatch")
	}
	return nil
}

type BoundRequest struct {
	Action                  ActionClass        `json:"action"`
	Navigation              NavigationClass    `json:"navigation"`
	TargetURL               string             `json:"target_url"`
	AccountID               string             `json:"account_id"`
	IdentityID              string             `json:"identity_id"`
	DataClassification      DataClassification `json:"data_classification"`
	Channel                 string             `json:"channel"`
	Audience                string             `json:"audience"`
	Claims                  []string           `json:"claims"`
	Environment             string             `json:"environment"`
	Jurisdiction            string             `json:"jurisdiction"`
	ConsentRef              string             `json:"consent_ref"`
	Recipient               string             `json:"recipient"`
	Counterparty            string             `json:"counterparty"`
	BrandPolicyRef          string             `json:"brand_policy_ref"`
	LegalPolicyRef          string             `json:"legal_policy_ref"`
	SecurityPolicyRef       string             `json:"security_policy_ref"`
	RollbackPolicyRef       string             `json:"rollback_policy_ref"`
	ExpectedResourceVersion string             `json:"expected_resource_version"`
	CompensatesKey          string             `json:"compensates_key"`
	OutputBytes             uint64             `json:"output_bytes"`
	Body                    json.RawMessage    `json:"body"`
	Upload                  *Transfer          `json:"upload"`
	Download                bool               `json:"download"`
	IssuedAt                time.Time          `json:"issued_at"`
	ExpiresAt               time.Time          `json:"expires_at"`
}

func (value BoundRequest) Validate() error {
	if !value.Action.Valid() || !value.Navigation.Valid() ||
		!value.DataClassification.Valid() ||
		len(value.TargetURL) == 0 || len(value.TargetURL) > 4096 ||
		boundedIdentifier("account id", value.AccountID) != nil ||
		boundedIdentifier("identity id", value.IdentityID) != nil ||
		value.OutputBytes == 0 || value.OutputBytes > 1<<20 ||
		!validUTC(value.IssuedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.IssuedAt) ||
		value.ExpiresAt.Sub(value.IssuedAt) > 10*time.Minute {
		return fmt.Errorf("external adapter: bound request identity, resource, or time is invalid")
	}
	if _, err := targetOrigin(value.TargetURL); err != nil {
		return err
	}
	if _, err := targetPath(value.TargetURL); err != nil {
		return err
	}
	fields := []string{
		value.Channel, value.Audience, value.Environment, value.Jurisdiction,
		value.ConsentRef, value.Recipient, value.Counterparty,
		value.BrandPolicyRef, value.LegalPolicyRef, value.SecurityPolicyRef,
		value.RollbackPolicyRef, value.ExpectedResourceVersion, value.CompensatesKey,
	}
	for _, field := range fields {
		if field != "" && (strings.TrimSpace(field) != field || len(field) > 1024 ||
			containsLineBreak(field)) {
			return fmt.Errorf("external adapter: bound request contains an invalid policy value")
		}
	}
	if len(value.Claims) > 128 || !validDistinctValues(value.Claims, 1024) {
		return fmt.Errorf("external adapter: bound request claims are invalid")
	}
	if len(value.Body) == 0 || len(value.Body) > 192<<10 {
		return fmt.Errorf("external adapter: bound request body is outside limits")
	}
	var body any
	if err := decodeStrict(value.Body, &body); err != nil {
		return fmt.Errorf("external adapter: bound request body is invalid: %w", err)
	}
	canonical, err := json.Marshal(body)
	if err != nil || !bytes.Equal(canonical, value.Body) {
		return fmt.Errorf("external adapter: bound request body is not canonical")
	}
	if value.Upload != nil {
		if !validMediaType(value.Upload.MediaType) || value.Upload.SizeBytes == 0 ||
			value.Upload.SizeBytes > 192<<10 ||
			value.Upload.SizeBytes != uint64(len(value.Upload.Content)) ||
			value.Upload.Hash.Validate() != nil {
			return fmt.Errorf("external adapter: bound request upload is invalid")
		}
		sum := sha256.Sum256(value.Upload.Content)
		if value.Upload.Hash.Digest != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("external adapter: bound request upload hash mismatch")
		}
	}
	return nil
}

func EncodeBody(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > 192<<10 {
		return nil, fmt.Errorf("external adapter: operation body cannot be encoded within limits")
	}
	var decoded any
	if err := decodeStrict(encoded, &decoded); err != nil {
		return nil, fmt.Errorf("external adapter: operation body is invalid: %w", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || len(canonical) == 0 || len(canonical) > 192<<10 {
		return nil, fmt.Errorf("external adapter: operation body cannot be canonicalized within limits")
	}
	return append(json.RawMessage(nil), canonical...), nil
}

type Envelope struct {
	SchemaVersion     string                `json:"schema_version"`
	Grant             lease.Grant           `json:"grant"`
	ConnectionID      string                `json:"connection_id"`
	ConnectionVersion uint64                `json:"connection_version"`
	ConnectionHash    contracts.ContentHash `json:"connection_hash"`
	Request           BoundRequest          `json:"request"`
}

func (value Envelope) Validate() error {
	grant := value.Grant
	if value.SchemaVersion != SchemaVersion ||
		token("connection id", value.ConnectionID) != nil ||
		value.ConnectionVersion == 0 || value.ConnectionHash.Validate() != nil ||
		grant.Request.Validate() != nil || grant.Fence.Validate() != nil ||
		grant.State != lease.StateActive || value.Request.Validate() != nil ||
		value.Request.ExpiresAt.After(grant.ExpiresAt) {
		return fmt.Errorf("external adapter: envelope authority or request is invalid")
	}
	if grant.RenewedAt != nil && (!validUTC(*grant.RenewedAt) ||
		grant.RenewedAt.Before(grant.IssuedAt) || grant.RenewedAt.After(grant.ExpiresAt)) {
		return fmt.Errorf("external adapter: envelope renewal time is invalid")
	}
	return nil
}

type ProviderResponse struct {
	SchemaVersion   string                `json:"schema_version"`
	ExternalID      string                `json:"external_id"`
	IdempotencyKey  string                `json:"idempotency_key"`
	RequestHash     contracts.ContentHash `json:"request_hash"`
	AccountID       string                `json:"account_id"`
	IdentityID      string                `json:"identity_id"`
	State           ExternalState         `json:"state"`
	Authoritative   bool                  `json:"authoritative"`
	ObservedAt      time.Time             `json:"observed_at"`
	FinalURL        string                `json:"final_url"`
	ResourceVersion string                `json:"resource_version"`
	Output          json.RawMessage       `json:"output"`
}

type Observation struct {
	SchemaVersion      string                   `json:"schema_version"`
	OrganizationID     contracts.OrganizationID `json:"organization_id"`
	ConnectionID       string                   `json:"connection_id"`
	ConnectionVersion  uint64                   `json:"connection_version"`
	Family             Family                   `json:"family"`
	Provider           string                   `json:"provider"`
	Operation          string                   `json:"operation"`
	Action             ActionClass              `json:"action"`
	TargetOrigin       string                   `json:"target_origin"`
	AccountID          string                   `json:"account_id"`
	IdentityID         string                   `json:"identity_id"`
	DataClassification DataClassification       `json:"data_classification"`
	ExternalID         string                   `json:"external_id"`
	IdempotencyKey     string                   `json:"idempotency_key"`
	State              ExternalState            `json:"state"`
	Authority          ObservationAuthority     `json:"authority"`
	UntrustedData      bool                     `json:"untrusted_data"`
	FinalURL           string                   `json:"final_url"`
	ResourceVersion    string                   `json:"resource_version"`
	ProviderObservedAt time.Time                `json:"provider_observed_at"`
	CapturedAt         time.Time                `json:"captured_at"`
	OutputHash         contracts.ContentHash    `json:"output_hash"`
	Output             json.RawMessage          `json:"output"`
}

func (value Observation) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.OrganizationID == "" ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		!value.Family.Valid() || token("provider", value.Provider) != nil ||
		token("operation", value.Operation) != nil || !value.Action.Valid() ||
		boundedIdentifier("account id", value.AccountID) != nil ||
		boundedIdentifier("identity id", value.IdentityID) != nil ||
		!value.DataClassification.Valid() || strings.TrimSpace(value.ExternalID) == "" ||
		len(value.ExternalID) > 512 || containsLineBreak(value.ExternalID) ||
		token("idempotency key", value.IdempotencyKey) != nil || !value.State.Valid() ||
		!value.Authority.Valid() || !validUTC(value.ProviderObservedAt) ||
		!validUTC(value.CapturedAt) || value.OutputHash.Validate() != nil ||
		len(value.Output) == 0 {
		return fmt.Errorf("external adapter: observation is invalid")
	}
	sum := sha256.Sum256(value.Output)
	if value.OutputHash.Digest != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("external adapter: observation output hash mismatch")
	}
	if !value.UntrustedData {
		return fmt.Errorf("external adapter: external output must remain untrusted data")
	}
	if origin, err := normalizedOrigin(value.TargetOrigin); err != nil || origin != value.TargetOrigin {
		return fmt.Errorf("external adapter: observation target origin is invalid")
	}
	if value.ProviderObservedAt.After(value.CapturedAt.Add(5 * time.Minute)) {
		return fmt.Errorf("external adapter: observation time is inconsistent")
	}
	return nil
}

func CanonicalHash[T contracts.Validatable](value T) (contracts.ContentHash, error) {
	encoded, err := contracts.EncodeCanonical(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func CredentialBindingHash(
	connectionID string,
	version uint64,
	credential CredentialMaterial,
) (contracts.ContentHash, error) {
	if token("connection id", connectionID) != nil || version == 0 ||
		credential.Validate() != nil {
		return contracts.ContentHash{}, fmt.Errorf("external adapter: credential binding input is invalid")
	}
	encoded, err := contracts.EncodeCanonical(&credentialBinding{
		SchemaVersion: SchemaVersion,
		ConnectionID:  connectionID,
		Version:       version,
		Credential:    credential,
	})
	if err != nil {
		return contracts.ContentHash{}, err
	}
	defer zeroBytes(encoded)
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}, nil
}

func normalizedOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("external adapter: invalid origin")
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("external adapter: target origins require HTTPS")
	}
	parsed.Path = ""
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), nil
}

func validatedEndpoint(raw string, privateHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Path != "" && path.Clean(parsed.Path) != parsed.Path) {
		return nil, fmt.Errorf("external adapter: provider endpoint is invalid")
	}
	if parsed.Scheme == "https" {
		return parsed, nil
	}
	if parsed.Scheme != "http" || !privateHTTP || !privateHost(parsed.Hostname()) {
		return nil, fmt.Errorf("external adapter: provider endpoint requires TLS or explicit private HTTP")
	}
	return parsed, nil
}

func targetOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("external adapter: target URL is invalid")
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host), nil
}

func targetPath(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Path == "" {
		return "", fmt.Errorf("external adapter: target URL has no path")
	}
	return path.Clean(parsed.Path), nil
}

func privateHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".flycast") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func validEndpointPath(value string) bool {
	if value == "" || len(value) > 1024 || !strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "?#\\") || path.Clean(value) != value {
		return false
	}
	return true
}

func validNavigationPrefixes(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || len(value) > 1024 || !strings.HasPrefix(value, "/") ||
			strings.ContainsAny(value, "?#\\") || path.Clean(value) != value || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func allowedMCPTool(tool string, action ActionClass) bool {
	allowed := map[ActionClass]map[string]bool{
		ActionNavigate: {"browser_navigate": true, "browser_navigate_back": true},
		ActionRead: {"browser_snapshot": true, "browser_console_messages": true,
			"browser_network_requests": true, "browser_network_request": true,
			"browser_take_screenshot": true, "browser_tabs": true,
			"browser_wait_for": true},
		ActionExtract: {"browser_snapshot": true, "browser_take_screenshot": true},
		ActionInteract: {"browser_click": true, "browser_drag": true,
			"browser_hover": true, "browser_press_key": true,
			"browser_select_option": true, "browser_type": true,
			"browser_fill_form": true, "browser_handle_dialog": true},
		ActionSubmit: {"browser_click": true, "browser_press_key": true,
			"browser_fill_form": true},
	}
	return allowed[action] != nil && allowed[action][tool]
}

func validMethod(value string) bool {
	switch value {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func validHeaderName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}

func validMediaType(value string) bool {
	if value == "" || len(value) > 256 || containsLineBreak(value) || strings.Contains(value, " ") {
		return false
	}
	parts := strings.Split(value, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func validDistinctValues(values []string, limit int) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > limit || containsLineBreak(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validDistinctNavigation(values []NavigationClass) bool {
	seen := make(map[NavigationClass]bool, len(values))
	for _, value := range values {
		if !value.Valid() || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validDistinctClassifications(values []DataClassification) bool {
	seen := make(map[DataClassification]bool, len(values))
	for _, value := range values {
		if !value.Valid() || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func contains[T comparable](values []T, target T) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func token(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("external adapter: %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("external adapter: %s contains an invalid character", name)
	}
	return nil
}

func boundedIdentifier(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || containsLineBreak(value) {
		return fmt.Errorf("external adapter: %s must contain 1 to 512 safe bytes", name)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("external adapter: %s contains control data", name)
		}
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
