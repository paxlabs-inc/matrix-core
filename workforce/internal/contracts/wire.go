package contracts

import (
	"fmt"
	"strings"
	"time"
)

// ModelBinding fixes the provider, model, and sampling lineage for one wake.
type ModelBinding struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             ModelBindingID `json:"model_binding_id"`
	Provider       string         `json:"provider"`
	ModelID        string         `json:"model_id"`
	ModelVersion   string         `json:"model_version"`
	WeightsDigest  *ContentHash   `json:"weights_digest"`
	SamplingDigest ContentHash    `json:"sampling_digest"`
}

// Validate enforces explicit provider, model, version, and sampling identity.
func (m ModelBinding) Validate() error {
	if err := validateSchema(m.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("model_binding_id", string(m.ID)); err != nil {
		return err
	}
	if err := validateID("model provider", m.Provider); err != nil {
		return err
	}
	if strings.TrimSpace(m.ModelID) == "" || len(m.ModelID) > 255 {
		return fmt.Errorf("model_id must contain 1 to 255 bytes")
	}
	if strings.TrimSpace(m.ModelVersion) == "" || len(m.ModelVersion) > 255 {
		return fmt.Errorf("model_version must contain 1 to 255 bytes")
	}
	if m.WeightsDigest != nil {
		if err := m.WeightsDigest.Validate(); err != nil {
			return fmt.Errorf("weights_digest: %w", err)
		}
	}
	if err := m.SamplingDigest.Validate(); err != nil {
		return fmt.Errorf("sampling_digest: %w", err)
	}
	return nil
}

// MGSGenomeRef identifies the exact genome supplied to one wake.
type MGSGenomeRef struct {
	Reference string      `json:"reference"`
	Digest    ContentHash `json:"digest"`
}

// Validate enforces an explicit, content-addressed genome.
func (r MGSGenomeRef) Validate() error {
	if err := validateID("MGS genome reference", r.Reference); err != nil {
		return err
	}
	return r.Digest.Validate()
}

// RuntimeBinding identifies the exact executable and registry lineage.
type RuntimeBinding struct {
	BuildDigest             ContentHash `json:"build_digest"`
	AuditorBuildDigest      ContentHash `json:"auditor_build_digest"`
	OperationRegistryDigest ContentHash `json:"operation_registry_digest"`
}

// Validate enforces content-addressed runtime lineage.
func (r RuntimeBinding) Validate() error {
	if err := r.BuildDigest.Validate(); err != nil {
		return fmt.Errorf("runtime build_digest: %w", err)
	}
	if err := r.AuditorBuildDigest.Validate(); err != nil {
		return fmt.Errorf("runtime auditor_build_digest: %w", err)
	}
	if err := r.OperationRegistryDigest.Validate(); err != nil {
		return fmt.Errorf("operation_registry_digest: %w", err)
	}
	return nil
}

// SourceState identifies the source snapshot on which work was based.
type SourceState struct {
	RootDigest      ContentHash `json:"root_digest"`
	GraphGeneration uint64      `json:"graph_generation"`
	LedgerCursor    uint64      `json:"ledger_cursor"`
}

// Validate enforces a content-addressed source state and non-zero generation.
func (s SourceState) Validate() error {
	if err := s.RootDigest.Validate(); err != nil {
		return fmt.Errorf("source root_digest: %w", err)
	}
	if s.GraphGeneration == 0 {
		return fmt.Errorf("source graph_generation must be positive")
	}
	return nil
}

// WakeBudget bounds every resource a fresh worker may consume.
type WakeBudget struct {
	MaxDurationMillis uint64 `json:"max_duration_millis"`
	MaxSteps          uint32 `json:"max_steps"`
	MaxModelCalls     uint32 `json:"max_model_calls"`
	MaxToolCalls      uint32 `json:"max_tool_calls"`
	MaxCostMinor      int64  `json:"max_cost_minor"`
	Currency          string `json:"currency"`
	MaxOutputBytes    uint64 `json:"max_output_bytes"`
}

// Validate enforces the Workforce hard ceilings and exact integer money.
func (b WakeBudget) Validate() error {
	if b.MaxDurationMillis == 0 || b.MaxDurationMillis > uint64((2*time.Hour)/time.Millisecond) {
		return fmt.Errorf("max_duration_millis must be between 1 and 7200000")
	}
	if b.MaxSteps == 0 || b.MaxSteps > 50 {
		return fmt.Errorf("max_steps must be between 1 and 50")
	}
	if b.MaxModelCalls > 25 || b.MaxToolCalls > 100 {
		return fmt.Errorf("wake call ceilings exceed engineering standards")
	}
	if b.MaxCostMinor < 0 {
		return fmt.Errorf("max_cost_minor cannot be negative")
	}
	if err := validateID("budget currency", b.Currency); err != nil {
		return err
	}
	if b.MaxOutputBytes == 0 || b.MaxOutputBytes > 2<<20 {
		return fmt.Errorf("max_output_bytes must be between 1 and 2097152")
	}
	return nil
}

// WakeLease is the immutable, signed authority for exactly one bounded wake.
type WakeLease struct {
	SchemaVersion      string         `json:"schema_version"`
	ID                 LeaseID        `json:"lease_id"`
	WakeID             WakeID         `json:"wake_id"`
	OrganizationID     OrganizationID `json:"organization_id"`
	SeatID             SeatID         `json:"seat_id"`
	SeatDID            SeatDID        `json:"seat_did"`
	Reason             string         `json:"reason"`
	MandateID          MandateID      `json:"mandate_id"`
	MandateVersion     uint64         `json:"mandate_version"`
	Policies           []PolicyRef    `json:"policies"`
	GraphScope         []IntentID     `json:"graph_scope"`
	Model              ModelBinding   `json:"model"`
	MGS                MGSGenomeRef   `json:"mgs"`
	Runtime            RuntimeBinding `json:"runtime"`
	SkillCatalogDigest ContentHash    `json:"skill_catalog_digest"`
	Budget             WakeBudget     `json:"budget"`
	IssuedAt           time.Time      `json:"issued_at"`
	ExpiresAt          time.Time      `json:"expires_at"`
	Fence              FenceToken     `json:"fence"`
	Signature          Signature      `json:"signature"`
}

// Validate enforces complete, bounded, signed authority with no zero-value fence.
func (l WakeLease) Validate() error {
	if err := validateSchema(l.SchemaVersion); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"lease_id", string(l.ID)},
		{"wake_id", string(l.WakeID)},
		{"organization_id", string(l.OrganizationID)},
		{"seat_id", string(l.SeatID)},
		{"seat_did", string(l.SeatDID)},
		{"mandate_id", string(l.MandateID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(l.Reason) == "" || len(l.Reason) > 255 {
		return fmt.Errorf("wake reason must contain 1 to 255 bytes")
	}
	if l.MandateVersion == 0 {
		return fmt.Errorf("mandate_version must be positive")
	}
	if len(l.Policies) == 0 || len(l.Policies) > 64 {
		return fmt.Errorf("lease must bind 1 to 64 policies")
	}
	for i := range l.Policies {
		if err := l.Policies[i].Validate(); err != nil {
			return fmt.Errorf("lease policy %d: %w", i, err)
		}
	}
	if len(l.GraphScope) == 0 || len(l.GraphScope) > 4096 {
		return fmt.Errorf("lease graph_scope must contain 1 to 4096 intents")
	}
	for _, intentID := range l.GraphScope {
		if err := validateID("graph scope intent_id", string(intentID)); err != nil {
			return err
		}
	}
	if err := l.Model.Validate(); err != nil {
		return fmt.Errorf("lease model: %w", err)
	}
	if err := l.MGS.Validate(); err != nil {
		return fmt.Errorf("lease MGS: %w", err)
	}
	if err := l.Runtime.Validate(); err != nil {
		return fmt.Errorf("lease runtime: %w", err)
	}
	if err := l.SkillCatalogDigest.Validate(); err != nil {
		return fmt.Errorf("skill_catalog_digest: %w", err)
	}
	if err := l.Budget.Validate(); err != nil {
		return fmt.Errorf("lease budget: %w", err)
	}
	if !isUTC(l.IssuedAt) || !isUTC(l.ExpiresAt) || !l.ExpiresAt.After(l.IssuedAt) {
		return fmt.Errorf("lease times must be UTC and expires_at must follow issued_at")
	}
	if time.Duration(l.Budget.MaxDurationMillis)*time.Millisecond > l.ExpiresAt.Sub(l.IssuedAt) {
		return fmt.Errorf("lease budget duration exceeds lease lifetime")
	}
	if err := l.Fence.Validate(); err != nil {
		return err
	}
	return l.Signature.Validate()
}

// ToolRef identifies one bounded tool schema permitted during a wake.
type ToolRef struct {
	Name         string      `json:"name"`
	SchemaDigest ContentHash `json:"schema_digest"`
}

// Validate enforces a named, content-addressed tool schema.
func (t ToolRef) Validate() error {
	if err := validateID("tool name", t.Name); err != nil {
		return err
	}
	return t.SchemaDigest.Validate()
}

// SkillRef identifies one versioned executable skill contract.
type SkillRef struct {
	ID      SkillID     `json:"skill_id"`
	Version uint64      `json:"version"`
	Digest  ContentHash `json:"digest"`
}

// Validate enforces a named, versioned, content-addressed skill.
func (s SkillRef) Validate() error {
	if err := validateID("skill_id", string(s.ID)); err != nil {
		return err
	}
	if s.Version == 0 {
		return fmt.Errorf("skill version must be positive")
	}
	return s.Digest.Validate()
}

// RequiredOutput defines one typed deliverable and its success predicate.
type RequiredOutput struct {
	Kind             string `json:"kind"`
	SuccessPredicate string `json:"success_predicate"`
}

// Validate enforces a named output with a bounded machine-facing predicate.
func (o RequiredOutput) Validate() error {
	if err := validateID("required output kind", o.Kind); err != nil {
		return err
	}
	if strings.TrimSpace(o.SuccessPredicate) == "" || len(o.SuccessPredicate) > 2048 {
		return fmt.Errorf("success_predicate must contain 1 to 2048 bytes")
	}
	return nil
}

// WorkPacket is the complete current-state projection supplied to one fresh worker.
// It contains no prior-session transcript, scratchpad, or private agent memory.
type WorkPacket struct {
	SchemaVersion   string            `json:"schema_version"`
	Lease           WakeLease         `json:"lease"`
	Seat            Seat              `json:"seat"`
	Mandate         Mandate           `json:"mandate"`
	Goal            Goal              `json:"goal"`
	Intent          Intent            `json:"intent"`
	VerifiedState   []RecordRef       `json:"verified_state"`
	Dependencies    []IntentID        `json:"dependencies"`
	Artifacts       []ArtifactRef     `json:"artifacts"`
	Evidence        []EvidenceRef     `json:"evidence"`
	Inbox           []MessageEnvelope `json:"inbox"`
	Tools           []ToolRef         `json:"tools"`
	Skills          []SkillRef        `json:"skills"`
	Policies        []PolicyRef       `json:"policies"`
	RequiredOutputs []RequiredOutput  `json:"required_outputs"`
	ProjectBrain    *ProjectBrainRef  `json:"project_brain"`
	AssembledAt     time.Time         `json:"assembled_at"`
}

// Validate enforces one internally consistent, bounded, stateless wake projection.
func (p WorkPacket) Validate() error {
	if err := validateSchema(p.SchemaVersion); err != nil {
		return err
	}
	if err := p.Lease.Validate(); err != nil {
		return fmt.Errorf("work packet lease: %w", err)
	}
	if err := p.Seat.Validate(); err != nil {
		return fmt.Errorf("work packet seat: %w", err)
	}
	if err := p.Mandate.Validate(); err != nil {
		return fmt.Errorf("work packet mandate: %w", err)
	}
	if err := p.Goal.Validate(); err != nil {
		return fmt.Errorf("work packet goal: %w", err)
	}
	if err := p.Intent.Validate(); err != nil {
		return fmt.Errorf("work packet intent: %w", err)
	}
	if p.Seat.Role == SeatAuditor {
		return fmt.Errorf("auditors receive VerdictPackets, not WorkPackets")
	}
	if p.Seat.ID != p.Lease.SeatID || p.Seat.DID != p.Lease.SeatDID ||
		p.Seat.OrganizationID != p.Lease.OrganizationID {
		return fmt.Errorf("work packet seat does not match lease authority")
	}
	if p.Goal.OrganizationID != p.Lease.OrganizationID ||
		p.Intent.OrganizationID != p.Lease.OrganizationID ||
		p.Intent.OwnerSeatID != p.Seat.ID {
		return fmt.Errorf("work packet goal or intent is outside seat authority")
	}
	if p.Mandate.ID != p.Lease.MandateID || p.Mandate.Version != p.Lease.MandateVersion {
		return fmt.Errorf("work packet mandate does not match lease authority")
	}
	if p.Goal.ID != p.Intent.GoalID || p.Intent.ID != p.Lease.GraphScope[0] {
		return fmt.Errorf("work packet goal or selected intent does not match graph scope")
	}
	if !isUTC(p.AssembledAt) || p.AssembledAt.Before(p.Lease.IssuedAt) ||
		!p.AssembledAt.Before(p.Lease.ExpiresAt) {
		return fmt.Errorf("assembled_at must fall within the lease lifetime")
	}
	if len(p.RequiredOutputs) == 0 {
		return fmt.Errorf("work packet must define at least one required output")
	}
	if !equalPolicyRefs(p.Policies, p.Lease.Policies) {
		return fmt.Errorf("work packet policies do not match lease authority")
	}
	allowedSkills := make(map[SkillID]struct{}, len(p.Mandate.AllowedSkills))
	for _, skillID := range p.Mandate.AllowedSkills {
		allowedSkills[skillID] = struct{}{}
	}
	for i := range p.VerifiedState {
		if err := p.VerifiedState[i].Validate(); err != nil {
			return fmt.Errorf("verified state %d: %w", i, err)
		}
	}
	for _, dependencyID := range p.Dependencies {
		if err := validateID("dependency intent_id", string(dependencyID)); err != nil {
			return err
		}
	}
	for i := range p.Artifacts {
		if err := p.Artifacts[i].Validate(); err != nil {
			return fmt.Errorf("artifact %d: %w", i, err)
		}
	}
	for i := range p.Evidence {
		if err := p.Evidence[i].Validate(); err != nil {
			return fmt.Errorf("evidence %d: %w", i, err)
		}
	}
	for i := range p.Inbox {
		if err := p.Inbox[i].Validate(); err != nil {
			return fmt.Errorf("inbox message %d: %w", i, err)
		}
		if p.Inbox[i].From.OrganizationID != p.Lease.OrganizationID ||
			!messageAddressesSeat(p.Inbox[i], p.Seat) {
			return fmt.Errorf("inbox message %d is outside seat authority", i)
		}
	}
	for i := range p.Tools {
		if err := p.Tools[i].Validate(); err != nil {
			return fmt.Errorf("tool %d: %w", i, err)
		}
	}
	for i := range p.Skills {
		if err := p.Skills[i].Validate(); err != nil {
			return fmt.Errorf("skill %d: %w", i, err)
		}
		if _, allowed := allowedSkills[p.Skills[i].ID]; !allowed {
			return fmt.Errorf("skill %q is outside the mandate", p.Skills[i].ID)
		}
	}
	for i := range p.Policies {
		if err := p.Policies[i].Validate(); err != nil {
			return fmt.Errorf("policy %d: %w", i, err)
		}
	}
	for i := range p.RequiredOutputs {
		if err := p.RequiredOutputs[i].Validate(); err != nil {
			return fmt.Errorf("required output %d: %w", i, err)
		}
	}
	if p.ProjectBrain != nil {
		if p.Seat.Role == SeatAuditor || p.Mandate.DepartmentKind != DepartmentDeveloper {
			return fmt.Errorf("project brain is restricted to Developer Lead and Executor seats")
		}
		if err := p.ProjectBrain.Validate(); err != nil {
			return fmt.Errorf("project brain: %w", err)
		}
		if !p.ProjectBrain.ExpiresAt.After(p.AssembledAt) {
			return fmt.Errorf("project brain view expires before the wake can use it")
		}
	}
	return nil
}

func messageAddressesSeat(message MessageEnvelope, seat Seat) bool {
	for _, address := range append(
		append([]SeatAddress(nil), message.To...), message.CC...,
	) {
		if address.OrganizationID == seat.OrganizationID &&
			address.DepartmentID == seat.DepartmentID &&
			address.SeatID == seat.ID {
			return true
		}
	}
	return false
}

func equalPolicyRefs(left, right []PolicyRef) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].ID != right[i].ID || left[i].Version != right[i].Version ||
			left[i].Hash != right[i].Hash {
			return false
		}
	}
	return true
}
