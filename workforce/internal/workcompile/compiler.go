package workcompile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/skills"
)

var (
	ErrProposalInvalid = errors.New("workcompile: typed proposal is invalid")
	ErrPlanConflict    = errors.New("workcompile: immutable plan conflict")
	ErrPlanNotFound    = errors.New("workcompile: compiled plan not found")
)

type Proposal struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"proposal_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	WakeID         contracts.WakeID         `json:"wake_id"`
	IntentID       contracts.IntentID       `json:"intent_id"`
	SeatID         contracts.SeatID         `json:"seat_id"`
	Skill          contracts.SkillRef       `json:"skill"`
	Operation      string                   `json:"operation"`
	Provider       string                   `json:"provider"`
	IdempotencyKey string                   `json:"idempotency_key"`
	ApprovalID     contracts.ApprovalID     `json:"approval_id"`
	ApprovalCost   uint64                   `json:"approval_cost_microunits"`
	Input          json.RawMessage          `json:"input"`
	Deadline       time.Time                `json:"deadline"`
}

func (proposal Proposal) Validate() error {
	if proposal.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("%w: schema version", ErrProposalInvalid)
	}
	if proposal.ID == "" || proposal.OrganizationID == "" || proposal.WakeID == "" ||
		proposal.IntentID == "" || proposal.SeatID == "" {
		return fmt.Errorf("%w: identity", ErrProposalInvalid)
	}
	if proposal.Operation == "" || proposal.Provider == "" || proposal.IdempotencyKey == "" {
		return fmt.Errorf("%w: operation binding", ErrProposalInvalid)
	}
	if len(proposal.Input) == 0 || len(proposal.Input) > 256<<10 || !json.Valid(proposal.Input) {
		return fmt.Errorf("%w: input", ErrProposalInvalid)
	}
	if proposal.Deadline.IsZero() || proposal.Deadline.Location() != time.UTC {
		return fmt.Errorf("%w: deadline", ErrProposalInvalid)
	}
	if err := proposal.Skill.Validate(); err != nil {
		return fmt.Errorf("%w: skill: %v", ErrProposalInvalid, err)
	}
	return nil
}

type Plan struct {
	SchemaVersion  string                   `json:"schema_version"`
	ProposalID     string                   `json:"proposal_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	WakeID         contracts.WakeID         `json:"wake_id"`
	IntentID       contracts.IntentID       `json:"intent_id"`
	SeatID         contracts.SeatID         `json:"seat_id"`
	SeatDID        contracts.SeatDID        `json:"seat_did"`
	MandateID      contracts.MandateID      `json:"mandate_id"`
	MandateVersion uint64                   `json:"mandate_version"`
	Policies       []contracts.PolicyRef    `json:"policies"`
	GraphScope     []contracts.IntentID     `json:"graph_scope"`
	Model          contracts.ModelBinding   `json:"model"`
	MGS            contracts.MGSGenomeRef   `json:"mgs"`
	Runtime        contracts.RuntimeBinding `json:"runtime"`
	Source         contracts.SourceState    `json:"source"`
	Skill          contracts.SkillRef       `json:"skill"`
	VerifierDigest contracts.ContentHash    `json:"verifier_digest"`
	Operation      effect.Proposal          `json:"operation"`
	Resources      skills.ResourceEstimate  `json:"resources"`
	CreatedAt      time.Time                `json:"created_at"`
	PlanHash       contracts.ContentHash    `json:"plan_hash"`
}

func (plan Plan) Validate() error {
	if plan.SchemaVersion != contracts.SchemaVersionV1 ||
		plan.ProposalID == "" || plan.OrganizationID == "" || plan.WakeID == "" ||
		plan.IntentID == "" || plan.SeatID == "" || plan.SeatDID == "" ||
		plan.MandateID == "" || plan.MandateVersion == 0 ||
		len(plan.Policies) == 0 || len(plan.GraphScope) == 0 ||
		plan.CreatedAt.IsZero() || plan.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("workcompile: plan identity is incomplete")
	}
	if err := plan.Model.Validate(); err != nil {
		return err
	}
	if err := plan.MGS.Validate(); err != nil {
		return err
	}
	if err := plan.Runtime.Validate(); err != nil {
		return err
	}
	if err := plan.Source.Validate(); err != nil {
		return err
	}
	if err := plan.Skill.Validate(); err != nil {
		return err
	}
	if err := plan.VerifierDigest.Validate(); err != nil {
		return err
	}
	if err := plan.Operation.Validate(); err != nil {
		return err
	}
	if err := plan.PlanHash.Validate(); err != nil {
		return err
	}
	return nil
}

type Compiler struct {
	pool      *pgxpool.Pool
	vault     *vault.UserVault
	tenantID  string
	skills    *skills.Store
	authority *policy.Store
	leases    *lease.Store
	now       func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	skillStore *skills.Store,
	authority *policy.Store,
	leases *lease.Store,
	now func() time.Time,
) (*Compiler, error) {
	if pool == nil || userVault == nil || tenantID == "" || skillStore == nil ||
		authority == nil || leases == nil || now == nil {
		return nil, fmt.Errorf("workcompile: durable compiler sources are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("workcompile: Vault user does not match tenant")
	}
	return &Compiler{
		pool: pool, vault: userVault, tenantID: tenantID, skills: skillStore,
		authority: authority, leases: leases, now: now,
	}, nil
}

// authorizePacket establishes that the caller's packet carries real authority
// before anything it claims is compiled into an executable effect. The packet
// is transported, so its own contents prove nothing: the signed lease, seat,
// and mandate are verified against the owner root, and the live runtime fence
// must still match. Skipping this would let a caller holding one valid lease
// attach a mandate of its own making and compile operations it was never
// granted.
func (compiler *Compiler) authorizePacket(
	ctx context.Context,
	packet contracts.WorkPacket,
) error {
	if err := compiler.authority.AuthorizeWorkPacket(ctx, packet); err != nil {
		return err
	}
	if err := compiler.authority.AuthorizeLease(ctx, packet.Lease.ID); err != nil {
		return err
	}
	registeredLease, err := compiler.authority.LoadLease(ctx, packet.Lease.ID)
	if err != nil || !sameCanonical(registeredLease, packet.Lease) {
		return fmt.Errorf("workcompile: packet lease is not the current registered authority")
	}
	currentSeat, err := compiler.authority.LoadCurrentSeat(ctx, packet.Seat.ID)
	if err != nil || !sameCanonical(currentSeat, packet.Seat) {
		return fmt.Errorf("workcompile: packet seat is not current")
	}
	currentMandate, err := compiler.authority.LoadMandate(
		ctx, packet.Mandate.ID, packet.Mandate.Version,
	)
	if err != nil || !sameCanonical(currentMandate, packet.Mandate) {
		return fmt.Errorf("workcompile: packet mandate is not current")
	}
	runtimeLease, err := compiler.leases.Authorize(
		ctx, packet.Lease.OrganizationID, packet.Lease.ID, packet.Lease.Fence,
	)
	if err != nil {
		return err
	}
	if runtimeLease.WakeID != packet.Lease.WakeID ||
		runtimeLease.SeatID != packet.Lease.SeatID ||
		runtimeLease.MandateID != packet.Lease.MandateID ||
		runtimeLease.MandateVersion != packet.Lease.MandateVersion {
		return fmt.Errorf("workcompile: authority and runtime leases disagree")
	}
	return nil
}

func sameCanonical(left, right contracts.Validatable) bool {
	leftCanonical, leftErr := contracts.EncodeCanonical(left)
	rightCanonical, rightErr := contracts.EncodeCanonical(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func (compiler *Compiler) Compile(
	ctx context.Context,
	packet contracts.WorkPacket,
	proposal Proposal,
	source contracts.SourceState,
) (Plan, error) {
	if err := packet.Validate(); err != nil {
		return Plan{}, err
	}
	if err := proposal.Validate(); err != nil {
		return Plan{}, err
	}
	if err := source.Validate(); err != nil {
		return Plan{}, err
	}
	if proposal.OrganizationID != packet.Lease.OrganizationID ||
		proposal.WakeID != packet.Lease.WakeID ||
		proposal.IntentID != packet.Intent.ID || proposal.SeatID != packet.Seat.ID ||
		!proposal.Deadline.After(packet.AssembledAt) ||
		proposal.Deadline.After(packet.Lease.ExpiresAt) {
		return Plan{}, invalidProposal("packet binding")
	}
	if !containsSkill(packet.Skills, proposal.Skill) {
		return Plan{}, invalidProposal("packet skill")
	}
	if err := compiler.authorizePacket(ctx, packet); err != nil {
		return Plan{}, err
	}
	published, err := compiler.skills.LoadAccepted(ctx, proposal.Skill)
	if err != nil {
		return Plan{}, err
	}
	operation, ok := findOperation(published.Contract, proposal.Operation)
	if !ok || !validateJSONSchema(operation.InputSchema, proposal.Input) {
		return Plan{}, invalidProposal("operation schema")
	}
	// An operation name is not authority to reach a credentialed adapter, and a
	// caller-declared price is not authority to spend against an owner ceiling.
	// Both come from the owner-signed contract.
	if !operation.AuthorizesProvider(proposal.Provider) {
		return Plan{}, invalidProposal("provider authority")
	}
	irreversible := operation.EffectClass == skills.EffectIrreversible
	if irreversible != (proposal.ApprovalID != "" && proposal.ApprovalCost > 0) {
		return Plan{}, invalidProposal("approval presence")
	}
	if irreversible && proposal.ApprovalCost != operation.ApprovalCost() {
		return Plan{}, invalidProposal("approval cost")
	}
	if published.Contract.Resources.ModelCalls > uint16(packet.Lease.Budget.MaxModelCalls) ||
		published.Contract.Resources.EffectCalls > uint16(packet.Lease.Budget.MaxToolCalls) ||
		published.Contract.Resources.MaxDuration >
			time.Duration(packet.Lease.Budget.MaxDurationMillis)*time.Millisecond {
		return Plan{}, invalidProposal("resource budget")
	}
	operationDigest, err := digestOperation(operation, proposal.Input)
	if err != nil {
		return Plan{}, err
	}
	now := compiler.now()
	if now.IsZero() || now.Location() != time.UTC || !proposal.Deadline.After(now) {
		return Plan{}, invalidProposal("current deadline")
	}
	plan := Plan{
		SchemaVersion: contracts.SchemaVersionV1,
		ProposalID:    proposal.ID, OrganizationID: proposal.OrganizationID,
		WakeID: proposal.WakeID, IntentID: proposal.IntentID,
		SeatID: packet.Seat.ID, SeatDID: packet.Seat.DID,
		MandateID: packet.Mandate.ID, MandateVersion: packet.Mandate.Version,
		Policies:   append([]contracts.PolicyRef(nil), packet.Policies...),
		GraphScope: append([]contracts.IntentID(nil), packet.Lease.GraphScope...),
		Model:      packet.Lease.Model, MGS: packet.Lease.MGS,
		Runtime: packet.Lease.Runtime, Source: source,
		Skill: proposal.Skill, VerifierDigest: published.Contract.VerifierDigest,
		Operation: effect.Proposal{
			ID: proposal.ID, OrganizationID: proposal.OrganizationID,
			IntentID: proposal.IntentID, NodeID: dependency.NodeID(proposal.IntentID),
			SeatID: proposal.SeatID, LeaseID: packet.Lease.ID,
			Fence: packet.Lease.Fence, Provider: proposal.Provider,
			SkillID: proposal.Skill.ID, EffectClass: operation.EffectClass,
			Irreversible: irreversible,
			Operation:    operation.Name, IdempotencyKey: proposal.IdempotencyKey,
			SkillDigest: proposal.Skill.Digest, OperationDigest: operationDigest,
			ApprovalID: proposal.ApprovalID, ApprovalCost: proposal.ApprovalCost,
			Input: append([]byte(nil), proposal.Input...), Deadline: proposal.Deadline,
		},
		Resources: published.Contract.Resources, CreatedAt: now,
	}
	hash, _, err := hashPlan(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.PlanHash = hash
	encoded, err := json.Marshal(plan)
	if err != nil {
		return Plan{}, err
	}
	sealed, err := compiler.vault.SealRecord(compiler.ad(plan), encoded)
	if err != nil {
		return Plan{}, err
	}
	command, err := compiler.pool.Exec(ctx, `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT DO NOTHING
	`, compiler.tenantID, plan.OrganizationID, plan.ProposalID, plan.IntentID,
		plan.Skill.ID, plan.Skill.Version, plan.Skill.Digest.Digest,
		plan.Operation.OperationDigest.Digest, plan.VerifierDigest.Digest,
		plan.PlanHash.Digest, effect.ProposalHash(plan.Operation), sealed, now)
	if err != nil {
		return Plan{}, err
	}
	if command.RowsAffected() == 0 {
		existing, loadErr := compiler.Load(ctx, plan.OrganizationID, plan.ProposalID)
		if loadErr != nil {
			return Plan{}, loadErr
		}
		if existing.PlanHash != plan.PlanHash {
			return Plan{}, ErrPlanConflict
		}
		return existing, nil
	}
	return plan, plan.Validate()
}

func invalidProposal(reason string) error {
	return fmt.Errorf("%w: %s", ErrProposalInvalid, reason)
}

func (compiler *Compiler) Load(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	proposalID string,
) (Plan, error) {
	var sealed []byte
	var expectedHash string
	err := compiler.pool.QueryRow(ctx, `
		SELECT sealed_plan,plan_hash FROM workforce_compiled_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, compiler.tenantID, organizationID, proposalID).Scan(&sealed, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrPlanNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	ad := vault.AD{
		User: compiler.tenantID, Store: "workforce.compiled.plan",
		Stream: string(organizationID) + "/" + proposalID,
		Schema: contracts.SchemaVersionV1,
	}
	opened, err := compiler.vault.OpenRecord(ad, sealed)
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := json.Unmarshal(opened, &plan); err != nil ||
		plan.Validate() != nil || plan.PlanHash.Digest != expectedHash {
		return Plan{}, ErrPlanConflict
	}
	hash, _, err := hashPlan(plan)
	if err != nil || hash != plan.PlanHash {
		return Plan{}, ErrPlanConflict
	}
	return plan, nil
}

func hashPlan(plan Plan) (contracts.ContentHash, []byte, error) {
	plan.PlanHash = contracts.ContentHash{}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return contracts.ContentHash{}, nil, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}, encoded, nil
}

func digestOperation(operation skills.Operation, input json.RawMessage) (contracts.ContentHash, error) {
	value := struct {
		Operation skills.Operation `json:"operation"`
		Input     json.RawMessage  `json:"input"`
	}{operation, input}
	encoded, err := json.Marshal(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

type jsonSchemaRule struct {
	Type                 json.RawMessage            `json:"type"`
	Required             []string                   `json:"required"`
	Properties           map[string]*jsonSchemaRule `json:"properties"`
	Items                *jsonSchemaRule            `json:"items"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	Enum                 []json.RawMessage          `json:"enum"`
	Pattern              string                     `json:"pattern"`
	MinItems             *int                       `json:"minItems"`
	MaxItems             *int                       `json:"maxItems"`
	MinLength            *int                       `json:"minLength"`
}

func validateJSONSchema(schema, input json.RawMessage) bool {
	var rule jsonSchemaRule
	if len(schema) == 0 || len(input) == 0 ||
		json.Unmarshal(schema, &rule) != nil || !json.Valid(input) {
		return false
	}
	types, valid := schemaTypes(rule.Type)
	if !valid || len(types) != 1 || types[0] != "object" {
		return false
	}
	return validateJSONValue(&rule, input)
}

// InputConforms reports whether canonical operation input satisfies the
// bounded JSON Schema subset enforced by the compiler.
func InputConforms(schema, input json.RawMessage) bool {
	return validateJSONSchema(schema, input)
}

func validateJSONValue(rule *jsonSchemaRule, raw json.RawMessage) bool {
	if rule == nil || !json.Valid(raw) {
		return false
	}
	if len(rule.Enum) > 0 {
		matched := false
		for _, candidate := range rule.Enum {
			if json.Valid(candidate) && bytes.Equal(compactJSON(candidate), compactJSON(raw)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	types, valid := schemaTypes(rule.Type)
	if !valid {
		return false
	}
	if len(types) == 0 {
		return true
	}
	for _, kind := range types {
		if validateJSONType(rule, raw, kind) {
			return true
		}
	}
	return false
}

func validateJSONType(rule *jsonSchemaRule, raw json.RawMessage, kind string) bool {
	switch kind {
	case "object":
		var value map[string]json.RawMessage
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return false
		}
		for _, name := range rule.Required {
			if _, exists := value[name]; !exists {
				return false
			}
		}
		if rule.AdditionalProperties != nil && !*rule.AdditionalProperties {
			for name := range value {
				if _, declared := rule.Properties[name]; !declared {
					return false
				}
			}
		}
		for name, property := range rule.Properties {
			if nested, exists := value[name]; exists &&
				!validateJSONValue(property, nested) {
				return false
			}
		}
		return true
	case "array":
		var value []json.RawMessage
		if json.Unmarshal(raw, &value) != nil || value == nil ||
			rule.MinItems != nil && len(value) < *rule.MinItems ||
			rule.MaxItems != nil && len(value) > *rule.MaxItems {
			return false
		}
		if rule.Items != nil {
			for _, item := range value {
				if !validateJSONValue(rule.Items, item) {
					return false
				}
			}
		}
		return true
	case "string":
		var value string
		if json.Unmarshal(raw, &value) != nil ||
			rule.MinLength != nil && len(value) < *rule.MinLength {
			return false
		}
		if rule.Pattern == "" {
			return true
		}
		pattern, err := regexp.Compile(rule.Pattern)
		return err == nil && pattern.MatchString(value)
	case "integer", "number":
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value json.Number
		if decoder.Decode(&value) != nil {
			return false
		}
		if kind == "integer" &&
			strings.ContainsAny(value.String(), ".eE") {
			return false
		}
		_, err := value.Float64()
		return err == nil
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "null":
		return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	default:
		return false
	}
}

func schemaTypes(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return nil, false
	}
	return many, true
}

func compactJSON(raw json.RawMessage) []byte {
	var result bytes.Buffer
	if json.Compact(&result, raw) != nil {
		return nil
	}
	return result.Bytes()
}

func containsSkill(values []contracts.SkillRef, target contracts.SkillRef) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func findOperation(contract skills.Contract, name string) (skills.Operation, bool) {
	for _, operation := range contract.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return skills.Operation{}, false
}

func (compiler *Compiler) ad(plan Plan) vault.AD {
	return vault.AD{
		User: compiler.tenantID, Store: "workforce.compiled.plan",
		Stream: string(plan.OrganizationID) + "/" + plan.ProposalID,
		Schema: contracts.SchemaVersionV1,
	}
}
