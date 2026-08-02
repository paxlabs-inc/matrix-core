package productcapability

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

// MetricDefinition is the exact Business Analytics identity used for a KPI.
// Values with incompatible identities are never treated as one time series.
type MetricDefinition struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"metric_id"`
	Version                   uint64                   `json:"version"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	InitiativeID              InitiativeID             `json:"initiative_id"`
	Name                      string                   `json:"name"`
	Unit                      string                   `json:"unit"`
	Numerator                 string                   `json:"numerator"`
	Denominator               string                   `json:"denominator"`
	Attribution               string                   `json:"attribution"`
	Source                    string                   `json:"source"`
	SourceIdentity            string                   `json:"source_identity"`
	MaximumAgeSeconds         uint64                   `json:"maximum_age_seconds"`
	UncertaintyBasisPoints    uint32                   `json:"uncertainty_basis_points"`
	GuardrailMetricIDs        []string                 `json:"guardrail_metric_ids"`
	ReconciliationProcedureID string                   `json:"reconciliation_procedure_id"`
	EffectiveAt               time.Time                `json:"effective_at"`
	ExpiresAt                 time.Time                `json:"expires_at"`
}

// Validate enforces an exact numerator, denominator, attribution, source,
// freshness, uncertainty, guardrail, and reconciliation definition.
func (value MetricDefinition) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.Version == 0 {
		return fmt.Errorf("product capability: metric schema or version is invalid")
	}
	for name, tokenValue := range map[string]string{
		"metric_id":                   value.ID,
		"organization_id":             string(value.OrganizationID),
		"initiative_id":               string(value.InitiativeID),
		"unit":                        value.Unit,
		"attribution":                 value.Attribution,
		"source":                      value.Source,
		"source_identity":             value.SourceIdentity,
		"reconciliation_procedure_id": value.ReconciliationProcedureID,
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	for name, text := range map[string]string{
		"name": value.Name, "numerator": value.Numerator,
		"denominator": value.Denominator,
	} {
		if strings.TrimSpace(text) == "" || len(text) > 2048 {
			return fmt.Errorf("product capability: metric %s is empty or oversized", name)
		}
	}
	if !slices.Contains([]string{
		"direct", "first_touch", "last_touch", "multi_touch", "experiment", "none",
	}, value.Attribution) {
		return fmt.Errorf("product capability: metric attribution is unsupported")
	}
	if !slices.Contains([]string{
		"product_telemetry", "customer_observation", "deployment_observation",
		"support_observation", "analytical_derivation",
	}, value.Source) {
		return fmt.Errorf("product capability: metric source is unsupported")
	}
	if value.MaximumAgeSeconds == 0 || value.MaximumAgeSeconds > uint64((365*24*time.Hour)/time.Second) ||
		value.UncertaintyBasisPoints > 10000 {
		return fmt.Errorf("product capability: metric freshness or uncertainty is outside bounds")
	}
	if err := validateTokenSet("guardrail metric id", value.GuardrailMetricIDs, 0, 32); err != nil {
		return err
	}
	if slices.Contains(value.GuardrailMetricIDs, value.ID) {
		return fmt.Errorf("product capability: metric cannot be its own guardrail")
	}
	if !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("product capability: metric effective times are invalid")
	}
	return nil
}

// ComparableWith reports exact KPI compatibility; a false result is an
// incompatible-comparison warning, not a zero or missing observation.
func (value MetricDefinition) ComparableWith(other MetricDefinition) bool {
	return value.Validate() == nil && other.Validate() == nil &&
		value.ID == other.ID && value.Unit == other.Unit &&
		value.Numerator == other.Numerator && value.Denominator == other.Denominator &&
		value.Attribution == other.Attribution && value.Source == other.Source &&
		value.SourceIdentity == other.SourceIdentity &&
		value.MaximumAgeSeconds == other.MaximumAgeSeconds &&
		value.UncertaintyBasisPoints == other.UncertaintyBasisPoints &&
		slices.Equal(value.GuardrailMetricIDs, other.GuardrailMetricIDs) &&
		value.ReconciliationProcedureID == other.ReconciliationProcedureID
}

// IncidentState is the closed Reliability incident lifecycle.
type IncidentState string

const (
	IncidentOpen       IncidentState = "open"
	IncidentMitigating IncidentState = "mitigating"
	IncidentObserved   IncidentState = "recovery_observed"
	IncidentResolved   IncidentState = "resolved"
	IncidentEscalated  IncidentState = "escalated"
)

// Valid reports whether the incident state is closed and known.
func (value IncidentState) Valid() bool {
	switch value {
	case IncidentOpen, IncidentMitigating, IncidentObserved,
		IncidentResolved, IncidentEscalated:
		return true
	default:
		return false
	}
}

// ReliabilityIncident records detection, impact, response, recovery, and
// independent resolution evidence without inferring recovery from an action.
type ReliabilityIncident struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"incident_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   InitiativeID             `json:"initiative_id"`
	Service        string                   `json:"service"`
	Environment    string                   `json:"environment"`
	Severity       uint8                    `json:"severity"`
	State          IncidentState            `json:"state"`
	Detection      Artifact                 `json:"detection"`
	Impact         Artifact                 `json:"impact"`
	Response       Artifact                 `json:"response"`
	Recovery       *Artifact                `json:"recovery"`
	Independent    *Artifact                `json:"independent_verification"`
	StartedAt      time.Time                `json:"started_at"`
	ResolvedAt     *time.Time               `json:"resolved_at"`
}

// ValidateAt enforces evidence-backed incident progression and independent
// proof before the resolved state can be committed.
func (value ReliabilityIncident) ValidateAt(now time.Time) error {
	if value.SchemaVersion != SchemaVersion || !value.State.Valid() ||
		value.Severity == 0 || value.Severity > 5 {
		return fmt.Errorf("product capability: reliability incident identity is invalid")
	}
	for name, tokenValue := range map[string]string{
		"incident_id": value.ID, "organization_id": string(value.OrganizationID),
		"initiative_id": string(value.InitiativeID), "service": value.Service,
		"environment": value.Environment,
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	for _, artifact := range []Artifact{value.Detection, value.Impact, value.Response} {
		if err := artifact.ValidateAt(now); err != nil {
			return err
		}
		if artifact.OrganizationID != value.OrganizationID ||
			artifact.InitiativeID != value.InitiativeID {
			return fmt.Errorf("product capability: incident artifact crosses scope")
		}
	}
	if value.Detection.Kind != ArtifactIncidentEvidence ||
		value.Impact.Kind != ArtifactReliabilityEvidence ||
		value.Response.Kind != ArtifactOperationsReadiness {
		return fmt.Errorf("product capability: incident artifact kinds are invalid")
	}
	if !validUTC(value.StartedAt) || !validUTC(now) || value.StartedAt.After(now) {
		return fmt.Errorf("product capability: incident start time is invalid")
	}
	resolved := value.State == IncidentResolved
	if resolved != (value.ResolvedAt != nil && value.Recovery != nil && value.Independent != nil) {
		return fmt.Errorf("product capability: incident resolution evidence is incomplete")
	}
	if value.ResolvedAt != nil {
		if !validUTC(*value.ResolvedAt) || value.ResolvedAt.Before(value.StartedAt) ||
			value.ResolvedAt.After(now) {
			return fmt.Errorf("product capability: incident resolved_at is invalid")
		}
	}
	if value.Recovery != nil {
		if err := value.Recovery.ValidateAt(now); err != nil || value.Recovery.Kind != ArtifactHealthEvidence {
			return fmt.Errorf("product capability: incident recovery evidence is invalid")
		}
	}
	if value.Independent != nil {
		if err := value.Independent.ValidateAt(now); err != nil ||
			value.Independent.Kind != ArtifactIndependentReview ||
			value.Independent.AuthorSeatID == value.Response.AuthorSeatID {
			return fmt.Errorf("product capability: incident independent verification is invalid")
		}
	}
	return nil
}

// Validate performs structural validation at the latest timestamp in the record.
func (value ReliabilityIncident) Validate() error {
	now := value.Response.EffectiveAt
	if now.Before(value.Detection.EffectiveAt) {
		now = value.Detection.EffectiveAt
	}
	if now.Before(value.Impact.EffectiveAt) {
		now = value.Impact.EffectiveAt
	}
	if value.ResolvedAt != nil {
		now = *value.ResolvedAt
	}
	return value.ValidateAt(now)
}

// RecordBody is one immutable typed payload and version-chain identity.
// Exactly one payload matching Kind must be present.
type RecordBody struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             RecordID                 `json:"record_id"`
	ChainID        ChainID                  `json:"chain_id"`
	Version        uint64                   `json:"version"`
	Kind           RecordKind               `json:"kind"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   InitiativeID             `json:"initiative_id"`
	ProjectID      contracts.ProjectID      `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID    `json:"workspace_id"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	Supersedes     *RecordID                `json:"supersedes"`
	Handoff        *ProductDesignHandoff    `json:"product_design_handoff"`
	Engineering    *EngineeringResult       `json:"engineering_result"`
	Metric         *MetricDefinition        `json:"metric_definition"`
	Incident       *ReliabilityIncident     `json:"reliability_incident"`
	CreatedAt      time.Time                `json:"created_at"`
	EffectiveAt    time.Time                `json:"effective_at"`
	FreshUntil     time.Time                `json:"fresh_until"`
}

// Validate enforces exact payload union, scope, version, and chronology.
func (value RecordBody) Validate() error {
	if value.SchemaVersion != SchemaVersion || !value.Kind.Valid() || value.Version == 0 {
		return fmt.Errorf("product capability: record body schema, kind, or version is invalid")
	}
	for name, tokenValue := range map[string]string{
		"record_id": string(value.ID), "chain_id": string(value.ChainID),
		"organization_id": string(value.OrganizationID),
		"initiative_id":   string(value.InitiativeID),
		"project_id":      string(value.ProjectID),
		"workspace_id":    string(value.WorkspaceID),
		"author_seat_id":  string(value.AuthorSeatID),
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("product capability: record supersession chain is invalid")
	}
	if value.Supersedes != nil {
		if err := validateToken("supersedes", string(*value.Supersedes)); err != nil {
			return err
		}
	}
	if !validUTC(value.CreatedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.FreshUntil) || value.EffectiveAt.Before(value.CreatedAt) ||
		!value.FreshUntil.After(value.EffectiveAt) {
		return fmt.Errorf("product capability: record chronology is invalid")
	}
	present := 0
	if value.Handoff != nil {
		present++
	}
	if value.Engineering != nil {
		present++
	}
	if value.Metric != nil {
		present++
	}
	if value.Incident != nil {
		present++
	}
	if present != 1 {
		return fmt.Errorf("product capability: record must contain exactly one payload")
	}
	switch value.Kind {
	case RecordProductDesignHandoff:
		if value.Handoff == nil || value.Handoff.OrganizationID != value.OrganizationID ||
			value.Handoff.InitiativeID != value.InitiativeID ||
			value.Handoff.ProjectID != value.ProjectID || value.Handoff.WorkspaceID != value.WorkspaceID {
			return fmt.Errorf("product capability: handoff payload is absent or unrelated")
		}
		if err := value.Handoff.ValidateAt(value.CreatedAt); err != nil {
			return err
		}
		if value.FreshUntil.After(value.Handoff.ExpiresAt) {
			return fmt.Errorf("product capability: handoff record outlives its payload")
		}
	case RecordEngineeringResult:
		if value.Engineering == nil || value.Engineering.OrganizationID != value.OrganizationID ||
			value.Engineering.InitiativeID != value.InitiativeID ||
			value.Engineering.ProjectID != value.ProjectID || value.Engineering.WorkspaceID != value.WorkspaceID {
			return fmt.Errorf("product capability: engineering payload is absent or unrelated")
		}
		if err := value.Engineering.ValidateAt(value.CreatedAt); err != nil {
			return err
		}
		for _, artifact := range value.Engineering.Artifacts {
			if value.FreshUntil.After(artifact.FreshUntil) {
				return fmt.Errorf("product capability: engineering record outlives an artifact")
			}
		}
	case RecordMetricDefinition:
		if value.Metric == nil || value.Metric.OrganizationID != value.OrganizationID ||
			value.Metric.InitiativeID != value.InitiativeID {
			return fmt.Errorf("product capability: metric payload is absent or unrelated")
		}
		if err := value.Metric.Validate(); err != nil {
			return err
		}
		if value.FreshUntil.After(value.Metric.ExpiresAt) {
			return fmt.Errorf("product capability: metric record outlives its definition")
		}
	case RecordReliabilityIncident:
		if value.Incident == nil || value.Incident.OrganizationID != value.OrganizationID ||
			value.Incident.InitiativeID != value.InitiativeID {
			return fmt.Errorf("product capability: incident payload is absent or unrelated")
		}
		if err := value.Incident.ValidateAt(value.CreatedAt); err != nil {
			return err
		}
		for _, artifact := range value.artifacts() {
			if value.FreshUntil.After(artifact.FreshUntil) {
				return fmt.Errorf("product capability: incident record outlives its evidence")
			}
		}
	}
	return nil
}

// Record is an author-signed immutable product-capability body.
type Record struct {
	Body      RecordBody          `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

// Validate checks the canonical body and signature profile.
func (value Record) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

// Verification is a fresh independent verifier's signed decision over one record.
type Verification struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               contracts.VerdictID      `json:"verdict_id"`
	RecordID         RecordID                 `json:"record_id"`
	RecordHash       contracts.ContentHash    `json:"record_hash"`
	AuthorSeatID     contracts.SeatID         `json:"author_seat_id"`
	VerifierSeatID   contracts.SeatID         `json:"verifier_seat_id"`
	ProcedureID      string                   `json:"procedure_id"`
	ProcedureVersion uint64                   `json:"procedure_version"`
	ProcedureDigest  contracts.ContentHash    `json:"procedure_digest"`
	Outcome          contracts.VerdictOutcome `json:"outcome"`
	Evidence         []contracts.EvidenceRef  `json:"evidence"`
	VerifiedAt       time.Time                `json:"verified_at"`
	ExpiresAt        time.Time                `json:"expires_at"`
	Signature        contracts.Signature      `json:"signature"`
}

// Validate enforces independent, evidence-backed, versioned verification.
func (value Verification) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.ProcedureVersion == 0 ||
		!value.Outcome.Valid() {
		return fmt.Errorf("product capability: verification schema or outcome is invalid")
	}
	for name, tokenValue := range map[string]string{
		"verdict_id": string(value.ID), "record_id": string(value.RecordID),
		"author_seat_id":   string(value.AuthorSeatID),
		"verifier_seat_id": string(value.VerifierSeatID),
		"procedure_id":     value.ProcedureID,
	} {
		if err := validateToken(name, tokenValue); err != nil {
			return err
		}
	}
	if value.AuthorSeatID == value.VerifierSeatID {
		return fmt.Errorf("product capability: record author cannot verify its own claim")
	}
	if err := value.RecordHash.Validate(); err != nil {
		return err
	}
	if err := value.ProcedureDigest.Validate(); err != nil {
		return err
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 128 {
		return fmt.Errorf("product capability: verification evidence is outside bounds")
	}
	for _, evidence := range value.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	if !validUTC(value.VerifiedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.VerifiedAt) {
		return fmt.Errorf("product capability: verification times are invalid")
	}
	return value.Signature.Validate()
}

// VerifiedRecord is durable truth only after an independent passing verdict.
type VerifiedRecord struct {
	Record       Record       `json:"record"`
	Verification Verification `json:"verification"`
}

// Validate verifies exact record binding and a passing independent outcome.
func (value VerifiedRecord) Validate() error {
	return value.ValidateAt(value.Verification.VerifiedAt)
}

// ValidateAt additionally rejects expired records and verdicts for live use.
func (value VerifiedRecord) ValidateAt(now time.Time) error {
	if err := value.Record.Validate(); err != nil {
		return err
	}
	if err := value.Verification.Validate(); err != nil {
		return err
	}
	if !validUTC(now) || !value.Record.Body.FreshUntil.After(now) ||
		!value.Verification.ExpiresAt.After(now) {
		return fmt.Errorf("product capability: verified record is expired")
	}
	if value.Verification.Outcome != contracts.VerdictPass ||
		value.Verification.RecordID != value.Record.Body.ID ||
		value.Verification.AuthorSeatID != value.Record.Body.AuthorSeatID {
		return fmt.Errorf("product capability: verification does not authorize this record")
	}
	procedure, err := RecordVerifierProcedure(value.Record.Body)
	if err != nil {
		return err
	}
	if value.Verification.ProcedureID != procedure.ID ||
		value.Verification.ProcedureVersion != procedure.Version ||
		value.Verification.ProcedureDigest != procedure.Digest {
		return fmt.Errorf("product capability: verification procedure is not current")
	}
	hash, err := RecordHash(value.Record)
	if err != nil {
		return err
	}
	if hash != value.Verification.RecordHash ||
		value.Verification.VerifiedAt.Before(value.Record.Body.EffectiveAt) {
		return fmt.Errorf("product capability: verification hash or chronology is invalid")
	}
	return nil
}

// RecordHash returns the canonical body hash signed by the author and verifier.
func RecordHash(value Record) (contracts.ContentHash, error) {
	if err := value.Body.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	return canonicalHash(value.Body)
}

// SignRecord signs the canonical record body with a current seat key.
func SignRecord(value *Record, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("product capability: record and Ed25519 author key are required")
	}
	if err := validateToken("author key id", keyID); err != nil {
		return err
	}
	payload, err := canonicalBytes(value.Body)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

// SignVerification signs an exact independent verification decision.
func SignVerification(value *Verification, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("product capability: verification and Ed25519 verifier key are required")
	}
	if err := validateToken("verifier key id", keyID); err != nil {
		return err
	}
	value.Signature = signingShapeSignature(keyID)
	payload, err := verificationSigningBytes(*value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return value.Validate()
}

func verifyRecord(value Record, publicKey ed25519.PublicKey) error {
	payload, err := canonicalBytes(value.Body)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("product capability: record signature is invalid")
	}
	return nil
}

func verifyVerification(value Verification, publicKey ed25519.PublicKey) error {
	payload, err := verificationSigningBytes(value)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("product capability: verification signature is invalid")
	}
	return nil
}

func verificationSigningBytes(value Verification) ([]byte, error) {
	value.Signature = signingShapeSignature(value.Signature.KeyID)
	return canonicalBytes(value)
}

func signingShapeSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func canonicalBytes[T contracts.Validatable](value T) ([]byte, error) {
	return contracts.EncodeCanonical(value)
}

func canonicalHash[T contracts.Validatable](value T) (contracts.ContentHash, error) {
	payload, err := canonicalBytes(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(payload)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func (value RecordBody) artifacts() []Artifact {
	switch {
	case value.Handoff != nil:
		return append([]Artifact(nil), value.Handoff.Artifacts...)
	case value.Engineering != nil:
		return append([]Artifact(nil), value.Engineering.Artifacts...)
	case value.Incident != nil:
		result := []Artifact{value.Incident.Detection, value.Incident.Impact, value.Incident.Response}
		if value.Incident.Recovery != nil {
			result = append(result, *value.Incident.Recovery)
		}
		if value.Incident.Independent != nil {
			result = append(result, *value.Incident.Independent)
		}
		return result
	default:
		return nil
	}
}

// Validate makes Artifact usable at canonical serialization boundaries.
func (value Artifact) Validate() error {
	return value.ValidateAt(value.EffectiveAt)
}

// Validate makes ProductDesignHandoff usable at canonical serialization boundaries.
func (value ProductDesignHandoff) Validate() error {
	return value.ValidateAt(value.CreatedAt)
}

// Validate makes EngineeringResult usable at canonical serialization boundaries.
func (value EngineeringResult) Validate() error {
	return value.ValidateAt(value.CompletedAt)
}

// Validate enforces a closed machine-generated launch assessment.
func (value LaunchAssessment) Validate() error {
	if value.SchemaVersion != SchemaVersion ||
		(value.State != LaunchBlocked && value.State != LaunchReady && value.State != LaunchRequiresHuman) {
		return fmt.Errorf("product capability: launch assessment is invalid")
	}
	if value.State == LaunchReady && len(value.Missing) != 0 ||
		value.State == LaunchBlocked && len(value.Missing) == 0 {
		return fmt.Errorf("product capability: launch state and missing evidence disagree")
	}
	for _, kind := range value.Missing {
		if !kind.Valid() {
			return fmt.Errorf("product capability: launch assessment contains an invalid kind")
		}
	}
	return value.EvidenceHash.Validate()
}
