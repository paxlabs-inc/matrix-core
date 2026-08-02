package initiative

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/companystate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/workorder"
)

var ErrMutation = errors.New("initiative: unsafe plan mutation")

type Completion struct {
	NodeID                    string                `json:"node_id"`
	ReceiptReferences         []contracts.ReceiptID `json:"receipt_references"`
	CommittedEffectIdentities []string              `json:"committed_effect_identities"`
	CompletedAt               time.Time             `json:"completed_at"`
}

func (value Completion) Validate() error {
	if !validToken(value.NodeID) || !validUTC(value.CompletedAt) ||
		!validReceiptSet(value.ReceiptReferences) ||
		!validSet(value.CommittedEffectIdentities, 0, 128, 128) {
		return fmt.Errorf("initiative: completion is invalid")
	}
	return nil
}

type Invalidation struct {
	NodeID       string                         `json:"node_id"`
	Reason       string                         `json:"reason"`
	AuthorityRef string                         `json:"authority_ref"`
	CorrectionID contracts.CorrectionID         `json:"correction_id"`
	Evidence     []companystate.RecordReference `json:"evidence"`
}

func (value Invalidation) Validate() error {
	if !validToken(value.NodeID) || !validToken(value.AuthorityRef) ||
		!validToken(string(value.CorrectionID)) ||
		strings.TrimSpace(value.Reason) == "" || len(value.Reason) > 2048 ||
		!validRecordSet(value.Evidence) {
		return fmt.Errorf("initiative: correction-driven invalidation is invalid")
	}
	return nil
}

type MutationKind string

const (
	MutationReplan     MutationKind = "replan"
	MutationCancel     MutationKind = "cancel"
	MutationInvalidate MutationKind = "invalidate"
)

type PreservedReceipt struct {
	NodeID   string                `json:"node_id"`
	Receipts []contracts.ReceiptID `json:"receipts"`
}

type Mutation struct {
	SchemaVersion     string                         `json:"schema_version"`
	ID                string                         `json:"mutation_id"`
	Kind              MutationKind                   `json:"kind"`
	OrganizationID    contracts.OrganizationID       `json:"organization_id"`
	InitiativeID      string                         `json:"initiative_id"`
	FromPlanID        string                         `json:"from_plan_id"`
	FromPlanVersion   uint64                         `json:"from_plan_version"`
	ToPlanID          string                         `json:"to_plan_id"`
	ToPlanVersion     uint64                         `json:"to_plan_version"`
	Reason            string                         `json:"reason"`
	AuthorityRef      string                         `json:"authority_ref"`
	Evidence          []companystate.RecordReference `json:"evidence"`
	Invalidations     []Invalidation                 `json:"invalidations"`
	PreservedReceipts []PreservedReceipt             `json:"preserved_receipts"`
	MutatedAt         time.Time                      `json:"mutated_at"`
	Signature         contracts.Signature            `json:"signature"`
}

func (value Mutation) Validate() error {
	if value.SchemaVersion != MutationSchemaVersion || !validToken(value.ID) ||
		(value.Kind != MutationReplan && value.Kind != MutationCancel && value.Kind != MutationInvalidate) ||
		value.OrganizationID == "" || !validToken(value.InitiativeID) ||
		!validToken(value.FromPlanID) || value.FromPlanVersion == 0 ||
		!validToken(value.ToPlanID) || value.ToPlanVersion <= value.FromPlanVersion ||
		strings.TrimSpace(value.Reason) == "" || len(value.Reason) > 2048 ||
		!validToken(value.AuthorityRef) || !validUTC(value.MutatedAt) ||
		!validRecordSet(value.Evidence) {
		return fmt.Errorf("initiative: mutation identity or evidence is invalid")
	}
	previous := ""
	for index := range value.Invalidations {
		if value.Invalidations[index].Validate() != nil || value.Invalidations[index].NodeID <= previous {
			return fmt.Errorf("initiative: invalidations must be sorted and unique")
		}
		previous = value.Invalidations[index].NodeID
	}
	previous = ""
	for _, preserved := range value.PreservedReceipts {
		if !validToken(preserved.NodeID) || preserved.NodeID <= previous ||
			!validReceiptSet(preserved.Receipts) {
			return fmt.Errorf("initiative: preserved receipts must be sorted and unique")
		}
		previous = preserved.NodeID
	}
	return value.Signature.Validate()
}

type ReplanRequest struct {
	Previous      Plan
	Next          CompileInput
	Completions   []Completion
	Invalidations []Invalidation
	Reason        string
	AuthorityRef  string
	Evidence      []companystate.RecordReference
	RequestedAt   time.Time
}

func (compiler *Compiler) Replan(request ReplanRequest) (Plan, Mutation, error) {
	if compiler == nil || !validUTC(request.RequestedAt) ||
		strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 2048 ||
		!validToken(request.AuthorityRef) || !validRecordSet(request.Evidence) {
		return Plan{}, Mutation{}, ErrMutation
	}
	if request.Next.CompiledAt != request.RequestedAt ||
		request.Next.Blueprint.Version != request.Previous.Version+1 ||
		request.Next.Initiative.ID != request.Previous.InitiativeID ||
		request.Next.Initiative.OrganizationID != request.Previous.OrganizationID {
		return Plan{}, Mutation{}, fmt.Errorf("%w: successor plan binding", ErrMutation)
	}
	companyAuthority := workorder.CompanyAuthority{
		Policy:       request.Next.Authority.IssuerPolicy,
		FounderKeyID: compiler.founderKeyID, FounderPublicKey: compiler.founderPublicKey,
		CurrentMissionVersion:         request.Next.Authority.Mission.Version,
		CurrentConstitutionVersion:    request.Next.Authority.Constitution.Version,
		CurrentCapitalEnvelopeVersion: request.Next.Authority.Capital.Version,
		At:                            request.RequestedAt,
	}
	if err := VerifyPlan(request.Previous, companyAuthority); err != nil {
		return Plan{}, Mutation{}, err
	}
	if !sortedCompletions(request.Completions) || !sortedInvalidations(request.Invalidations) {
		return Plan{}, Mutation{}, fmt.Errorf("%w: mutation inputs must be sorted and unique", ErrMutation)
	}
	next, err := compiler.Compile(request.Next)
	if err != nil {
		return Plan{}, Mutation{}, err
	}
	invalidated := make(map[string]Invalidation, len(request.Invalidations))
	for _, value := range request.Invalidations {
		if value.Validate() != nil || findCompiledNode(request.Previous.Nodes, value.NodeID) == nil {
			return Plan{}, Mutation{}, fmt.Errorf("%w: invalidation target", ErrMutation)
		}
		invalidated[value.NodeID] = value
	}
	committedEffects := make(map[string]string)
	preserved := make([]PreservedReceipt, 0, len(request.Completions))
	for _, completion := range request.Completions {
		if completion.Validate() != nil {
			return Plan{}, Mutation{}, fmt.Errorf("%w: completion", ErrMutation)
		}
		oldNode := findCompiledNode(request.Previous.Nodes, completion.NodeID)
		newNode := findCompiledNode(next.Nodes, completion.NodeID)
		if oldNode == nil {
			return Plan{}, Mutation{}, fmt.Errorf("%w: completion target", ErrMutation)
		}
		for _, identity := range completion.CommittedEffectIdentities {
			if owner, exists := committedEffects[identity]; exists {
				return Plan{}, Mutation{}, fmt.Errorf("%w: %s and %s", ErrDuplicateEffect, owner, completion.NodeID)
			}
			committedEffects[identity] = completion.NodeID
		}
		preserved = append(preserved, PreservedReceipt{
			NodeID:   completion.NodeID,
			Receipts: append([]contracts.ReceiptID(nil), completion.ReceiptReferences...),
		})
		_, mustInvalidate := invalidated[completion.NodeID]
		if newNode == nil || newNode.Digest != oldNode.Digest {
			if !mustInvalidate {
				return Plan{}, Mutation{}, fmt.Errorf("%w: materially changed completed node %s", ErrMutation, completion.NodeID)
			}
			continue
		}
		if mustInvalidate {
			newNode.State = StateInvalidated
			newNode.ReceiptReferences = append([]contracts.ReceiptID(nil), completion.ReceiptReferences...)
			continue
		}
		newNode.State = StatePreserved
		newNode.ReceiptReferences = append([]contracts.ReceiptID(nil), completion.ReceiptReferences...)
	}
	for nodeID := range invalidated {
		if node := findCompiledNode(next.Nodes, nodeID); node != nil {
			node.State = StateInvalidated
		}
	}
	for index := range next.Nodes {
		node := &next.Nodes[index]
		if node.State == StatePreserved || node.State == StateInvalidated || node.Order == nil {
			continue
		}
		for _, identity := range node.Order.Binding.EffectIdentities {
			if priorNode, exists := committedEffects[identity]; exists {
				return Plan{}, Mutation{}, fmt.Errorf("%w: committed by %s and rescheduled by %s", ErrDuplicateEffect, priorNode, node.Template.ID)
			}
		}
	}
	if err := compiler.signPlan(&next, request.Next.Authority.IssuerPolicy.IssuerKeyID); err != nil {
		return Plan{}, Mutation{}, err
	}
	mutation := Mutation{
		SchemaVersion: MutationSchemaVersion,
		ID:            "plan-mutation:" + string(next.InitiativeID) + ":" + fmt.Sprint(next.Version),
		Kind:          MutationReplan, OrganizationID: next.OrganizationID,
		InitiativeID: string(next.InitiativeID),
		FromPlanID:   request.Previous.ID, FromPlanVersion: request.Previous.Version,
		ToPlanID: next.ID, ToPlanVersion: next.Version,
		Reason: request.Reason, AuthorityRef: request.AuthorityRef,
		Evidence:          append([]companystate.RecordReference(nil), request.Evidence...),
		Invalidations:     append([]Invalidation(nil), request.Invalidations...),
		PreservedReceipts: preserved, MutatedAt: request.RequestedAt,
	}
	if len(request.Invalidations) > 0 {
		mutation.Kind = MutationInvalidate
	}
	if err := compiler.signMutation(&mutation, request.Next.Authority.IssuerPolicy.IssuerKeyID); err != nil {
		return Plan{}, Mutation{}, err
	}
	return next, mutation, nil
}

type CancelRequest struct {
	Previous     Plan
	Authority    workorder.CompanyAuthority
	Reason       string
	AuthorityRef string
	Evidence     []companystate.RecordReference
	CancelledAt  time.Time
}

func (compiler *Compiler) Cancel(request CancelRequest) (Plan, Mutation, error) {
	if compiler == nil || VerifyPlan(request.Previous, request.Authority) != nil ||
		!validUTC(request.CancelledAt) || strings.TrimSpace(request.Reason) == "" ||
		len(request.Reason) > 2048 || !validToken(request.AuthorityRef) ||
		!validRecordSet(request.Evidence) {
		return Plan{}, Mutation{}, ErrMutation
	}
	next := request.Previous
	next.Version++
	next.ID = "initiative-plan:" + string(next.InitiativeID) + ":" + fmt.Sprint(next.Version)
	next.CompiledAt = request.CancelledAt
	for index := range next.Nodes {
		if next.Nodes[index].State == StatePending || next.Nodes[index].State == StateInvalidated {
			next.Nodes[index].State = StateCancelled
		}
	}
	if err := compiler.signPlan(&next, request.Authority.Policy.IssuerKeyID); err != nil {
		return Plan{}, Mutation{}, err
	}
	mutation := Mutation{
		SchemaVersion: MutationSchemaVersion,
		ID:            "plan-mutation:" + string(next.InitiativeID) + ":" + fmt.Sprint(next.Version),
		Kind:          MutationCancel, OrganizationID: next.OrganizationID,
		InitiativeID: string(next.InitiativeID),
		FromPlanID:   request.Previous.ID, FromPlanVersion: request.Previous.Version,
		ToPlanID: next.ID, ToPlanVersion: next.Version,
		Reason: request.Reason, AuthorityRef: request.AuthorityRef,
		Evidence:  append([]companystate.RecordReference(nil), request.Evidence...),
		MutatedAt: request.CancelledAt,
	}
	for _, node := range next.Nodes {
		if len(node.ReceiptReferences) > 0 {
			mutation.PreservedReceipts = append(mutation.PreservedReceipts, PreservedReceipt{
				NodeID:   node.Template.ID,
				Receipts: append([]contracts.ReceiptID(nil), node.ReceiptReferences...),
			})
		}
	}
	if err := compiler.signMutation(&mutation, request.Authority.Policy.IssuerKeyID); err != nil {
		return Plan{}, Mutation{}, err
	}
	return next, mutation, nil
}

type GateResult struct {
	GateNodeID  string                         `json:"gate_node_id"`
	Outcome     GateOutcome                    `json:"outcome"`
	Evidence    []companystate.RecordReference `json:"evidence"`
	CommittedAt time.Time                      `json:"committed_at"`
}

type ScheduledSuccessor struct {
	NodeID   string            `json:"node_id"`
	Schedule SuccessorSchedule `json:"schedule"`
}

func Successors(plan Plan, completedNodeID string, result *GateResult, at time.Time) ([]ScheduledSuccessor, error) {
	completed := findCompiledNode(plan.Nodes, completedNodeID)
	if !validToken(completedNodeID) || !validUTC(at) || completed == nil {
		return nil, fmt.Errorf("initiative: successor input is invalid")
	}
	if result != nil {
		expectedGateID := completedNodeID
		if completed.Template.Kind == NodeBranch && completed.Template.Branch != nil {
			expectedGateID = completed.Template.Branch.GateNodeID
		}
		if result.GateNodeID != expectedGateID || !result.Outcome.Valid() ||
			!validUTC(result.CommittedAt) || result.CommittedAt.After(at) ||
			!validRecordSet(result.Evidence) {
			return nil, fmt.Errorf("initiative: branch result lacks committed evidence")
		}
	}
	if completed.Template.Kind == NodeBranch && result == nil {
		return nil, fmt.Errorf("initiative: branch successor requires a committed gate result")
	}
	values := make([]ScheduledSuccessor, 0)
	for _, edge := range plan.Edges {
		if edge.Prerequisite != completedNodeID || edge.Schedule.Deadline.Before(at) {
			continue
		}
		if edge.When != nil && (result == nil || *edge.When != result.Outcome) {
			continue
		}
		if edge.When == nil && result != nil {
			return nil, fmt.Errorf("initiative: gate result cannot select an unconditional successor")
		}
		values = append(values, ScheduledSuccessor{NodeID: edge.Successor, Schedule: edge.Schedule})
	}
	slices.SortFunc(values, func(left, right ScheduledSuccessor) int {
		return strings.Compare(left.NodeID, right.NodeID)
	})
	return values, nil
}

func VerifyPlan(plan Plan, authority workorder.CompanyAuthority) error {
	if err := validatePlanCore(plan); err != nil {
		return err
	}
	if plan.SchemaVersion != PlanSchemaVersion || !validToken(plan.ID) || plan.Version == 0 ||
		plan.OrganizationID == "" || plan.InitiativeID == "" || len(plan.Nodes) == 0 ||
		len(plan.TopologicalOrder) != len(plan.Nodes) || !validUTC(plan.CompiledAt) ||
		plan.Hash.Validate() != nil || plan.Signature.Validate() != nil ||
		plan.Authority.MissionVersion != authority.CurrentMissionVersion ||
		plan.Authority.ConstitutionVersion != authority.CurrentConstitutionVersion ||
		plan.Authority.CapitalEnvelopeVersion != authority.CurrentCapitalEnvelopeVersion ||
		plan.Authority.IssuerPolicyVersion != authority.Policy.Version ||
		plan.Signature.KeyID != authority.Policy.IssuerKeyID {
		return fmt.Errorf("%w: plan identity or authority", ErrAuthority)
	}
	if err := authority.Validate(plan.OrganizationID); err != nil {
		return err
	}
	hashPayload := plan
	hashPayload.Hash = contracts.ContentHash{}
	hashPayload.Signature = contracts.Signature{}
	canonical, err := contracts.EncodeCanonical(&canonicalValue[Plan]{Value: hashPayload})
	if err != nil || sha256Hash(canonical) != plan.Hash {
		return fmt.Errorf("initiative: plan hash mismatch")
	}
	issuerPublicKey, err := issuerKey(authority.Policy)
	payload, payloadErr := planSigningBytes(plan, authority.Policy.IssuerKeyID)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(plan.Signature.Value)
	if err != nil || payloadErr != nil || decodeErr != nil ||
		!ed25519.Verify(issuerPublicKey, payload, decoded) {
		return fmt.Errorf("initiative: plan signature verification failed")
	}
	return nil
}

// VerifyMutation authenticates a controller-signed plan mutation against the
// exact current founder-signed issuer policy.
func VerifyMutation(value Mutation, authority workorder.CompanyAuthority) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if value.OrganizationID != authority.Policy.OrganizationID ||
		value.Signature.KeyID != authority.Policy.IssuerKeyID {
		return fmt.Errorf("%w: mutation authority", ErrAuthority)
	}
	if err := authority.Validate(value.OrganizationID); err != nil {
		return err
	}
	issuerPublicKey, err := issuerKey(authority.Policy)
	if err != nil {
		return err
	}
	payload, err := mutationSigningBytes(value, authority.Policy.IssuerKeyID)
	if err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize ||
		!ed25519.Verify(issuerPublicKey, payload, decoded) {
		return fmt.Errorf("initiative: mutation signature verification failed")
	}
	return nil
}

func (compiler *Compiler) signMutation(value *Mutation, keyID string) error {
	payload, err := mutationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(compiler.issuerPrivateKey, payload)),
	}
	return value.Validate()
}

func mutationSigningBytes(value Mutation, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func validRecordSet(values []companystate.RecordReference) bool {
	if len(values) == 0 || len(values) > 256 {
		return false
	}
	previous := ""
	for index := range values {
		if values[index].Validate() != nil || values[index].ID <= previous {
			return false
		}
		previous = values[index].ID
	}
	return true
}

func validReceiptSet(values []contracts.ReceiptID) bool {
	if len(values) == 0 || len(values) > 256 {
		return false
	}
	previous := contracts.ReceiptID("")
	for _, value := range values {
		if !validToken(string(value)) || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func sortedCompletions(values []Completion) bool {
	previous := ""
	for _, value := range values {
		if value.NodeID <= previous {
			return false
		}
		previous = value.NodeID
	}
	return true
}

func sortedInvalidations(values []Invalidation) bool {
	previous := ""
	for _, value := range values {
		if value.NodeID <= previous {
			return false
		}
		previous = value.NodeID
	}
	return true
}
