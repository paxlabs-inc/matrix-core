package initiative

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/portfolio"
	"centra/workforce/internal/workorder"
)

var (
	ErrAuthority       = errors.New("initiative: authority rejected")
	ErrCycle           = errors.New("initiative: graph cycle")
	ErrBudgetSplitting = errors.New("initiative: budget splitting rejected")
	ErrDuplicateEffect = errors.New("initiative: duplicate effect identity")
)

type Compiler struct {
	founderKeyID     string
	founderPublicKey ed25519.PublicKey
	issuerPrivateKey ed25519.PrivateKey
}

func NewCompiler(
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	issuerPrivateKey ed25519.PrivateKey,
) (*Compiler, error) {
	if !validToken(founderKeyID) || len(founderPublicKey) != ed25519.PublicKeySize ||
		len(issuerPrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("initiative: compiler keys are invalid")
	}
	return &Compiler{
		founderKeyID:     founderKeyID,
		founderPublicKey: append(ed25519.PublicKey(nil), founderPublicKey...),
		issuerPrivateKey: append(ed25519.PrivateKey(nil), issuerPrivateKey...),
	}, nil
}

type CompileInput struct {
	Authority         mission.ActivationAuthority
	Decision          portfolio.DecisionReceipt
	DecisionKeyID     string
	DecisionPublicKey ed25519.PublicKey
	Initiative        Initiative
	Blueprint         Blueprint
	CompiledAt        time.Time
}

func SignInitiative(
	value *Initiative,
	authority workorder.CompanyAuthority,
	privateKey ed25519.PrivateKey,
) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("initiative: signing authority is invalid")
	}
	if err := authority.Validate(value.OrganizationID); err != nil {
		return fmt.Errorf("%w: %v", ErrAuthority, err)
	}
	if value.MissionVersion != authority.CurrentMissionVersion ||
		value.ConstitutionVersion != authority.CurrentConstitutionVersion ||
		value.CapitalEnvelopeVersion != authority.CurrentCapitalEnvelopeVersion ||
		value.IssuerPolicyVersion != authority.Policy.Version ||
		value.CreatedAt.Before(authority.Policy.EffectiveAt) ||
		!value.CreatedAt.Before(authority.Policy.ExpiresAt) ||
		value.Deadline.After(authority.Policy.ExpiresAt) {
		return fmt.Errorf("%w: initiative authority binding", ErrAuthority)
	}
	issuerPublicKey, err := issuerKey(authority.Policy)
	if err != nil || !bytes.Equal(privateKey.Public().(ed25519.PublicKey), issuerPublicKey) {
		return fmt.Errorf("%w: initiative issuer key", ErrAuthority)
	}
	payload, err := initiativeSigningBytes(*value, authority.Policy.IssuerKeyID)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: authority.Policy.IssuerKeyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func VerifyInitiative(value Initiative, authority workorder.CompanyAuthority) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if err := authority.Validate(value.OrganizationID); err != nil {
		return fmt.Errorf("%w: %v", ErrAuthority, err)
	}
	if value.MissionVersion != authority.CurrentMissionVersion ||
		value.ConstitutionVersion != authority.CurrentConstitutionVersion ||
		value.CapitalEnvelopeVersion != authority.CurrentCapitalEnvelopeVersion ||
		value.IssuerPolicyVersion != authority.Policy.Version ||
		value.CreatedAt.Before(authority.Policy.EffectiveAt) ||
		!value.CreatedAt.Before(authority.Policy.ExpiresAt) ||
		value.Deadline.After(authority.Policy.ExpiresAt) ||
		value.Signature.KeyID != authority.Policy.IssuerKeyID {
		return fmt.Errorf("%w: initiative is not bound to current authority", ErrAuthority)
	}
	issuerPublicKey, err := issuerKey(authority.Policy)
	if err != nil {
		return err
	}
	payload, err := initiativeSigningBytes(value, authority.Policy.IssuerKeyID)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || decodeErr != nil || !ed25519.Verify(issuerPublicKey, payload, decoded) {
		return fmt.Errorf("%w: initiative signature", ErrAuthority)
	}
	return nil
}

func (compiler *Compiler) Compile(input CompileInput) (Plan, error) {
	if compiler == nil {
		return Plan{}, fmt.Errorf("initiative: compiler is nil")
	}
	if !validUTC(input.CompiledAt) {
		return Plan{}, fmt.Errorf("initiative: compile time must be UTC")
	}
	if err := mission.VerifyActivationAuthority(
		input.Authority, compiler.founderKeyID, compiler.founderPublicKey,
	); err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrAuthority, err)
	}
	companyAuthority := workorder.CompanyAuthority{
		Policy:                        input.Authority.IssuerPolicy,
		FounderKeyID:                  compiler.founderKeyID,
		FounderPublicKey:              compiler.founderPublicKey,
		CurrentMissionVersion:         input.Authority.Mission.Version,
		CurrentConstitutionVersion:    input.Authority.Constitution.Version,
		CurrentCapitalEnvelopeVersion: input.Authority.Capital.Version,
		At:                            input.CompiledAt,
	}
	if err := VerifyInitiative(input.Initiative, companyAuthority); err != nil {
		return Plan{}, err
	}
	if err := portfolio.VerifyDecision(
		input.Decision, input.DecisionKeyID, input.DecisionPublicKey,
	); err != nil {
		return Plan{}, fmt.Errorf("%w: %v", ErrAuthority, err)
	}
	if err := input.Blueprint.Validate(); err != nil {
		return Plan{}, err
	}
	if err := validateCompileBindings(input); err != nil {
		return Plan{}, err
	}
	if expected, err := capabilityPlanHash(input.Initiative.CapabilityPlan); err != nil ||
		expected != input.Initiative.CapabilityPlan.Hash {
		return Plan{}, fmt.Errorf("initiative: capability plan hash mismatch")
	}
	topological, err := validateGraph(input.Blueprint, input.Initiative)
	if err != nil {
		return Plan{}, err
	}
	planID := "initiative-plan:" + string(input.Initiative.ID) + ":" + fmt.Sprint(input.Blueprint.Version)
	if !validToken(planID) {
		return Plan{}, fmt.Errorf("initiative: derived plan identity is invalid")
	}
	plan := Plan{
		SchemaVersion: PlanSchemaVersion, ID: planID, Version: input.Blueprint.Version,
		OrganizationID: input.Initiative.OrganizationID,
		InitiativeID:   input.Initiative.ID, InitiativeVersion: input.Initiative.Version,
		BlueprintID: input.Blueprint.ID, BlueprintVersion: input.Blueprint.Version,
		Authority: AuthorityBinding{
			MissionVersion:         input.Initiative.MissionVersion,
			ConstitutionVersion:    input.Initiative.ConstitutionVersion,
			CapitalEnvelopeVersion: input.Initiative.CapitalEnvelopeVersion,
			IssuerPolicyVersion:    input.Initiative.IssuerPolicyVersion,
			PortfolioDecisionID:    string(input.Initiative.PortfolioDecisionID),
			CapitalAllocationID:    input.Initiative.Allocation.ID,
			CapabilityPlanID:       input.Initiative.CapabilityPlan.ID,
		},
		Edges:            append([]Edge(nil), input.Blueprint.Edges...),
		TopologicalOrder: topological, CompiledAt: input.CompiledAt,
	}
	slices.SortFunc(plan.Edges, func(left, right Edge) int {
		leftKey := left.Prerequisite + "\x00" + left.Successor
		rightKey := right.Prerequisite + "\x00" + right.Successor
		if left.When != nil {
			leftKey += "\x00" + string(*left.When)
		}
		if right.When != nil {
			rightKey += "\x00" + string(*right.When)
		}
		return strings.Compare(leftKey, rightKey)
	})
	effects := make(map[string]string)
	for index := range input.Blueprint.Nodes {
		template := input.Blueprint.Nodes[index]
		if template.WorkOrder != nil {
			workSpec := *template.WorkOrder
			workSpec.EffectIdentities = append([]string(nil), template.WorkOrder.EffectIdentities...)
			template.WorkOrder = &workSpec
		}
		compiled := CompiledNode{Template: template, State: StatePending}
		if template.WorkOrder != nil {
			order, capital, risk, compileErr := compiler.compileOrder(
				input, companyAuthority, planID, template,
			)
			if compileErr != nil {
				return Plan{}, compileErr
			}
			if plan.CapitalMicrounits > math.MaxUint64-capital ||
				plan.RiskMicrounits > math.MaxUint64-risk {
				return Plan{}, ErrBudgetSplitting
			}
			plan.CapitalMicrounits += capital
			plan.RiskMicrounits += risk
			for _, identity := range template.WorkOrder.EffectIdentities {
				if owner, duplicate := effects[identity]; duplicate {
					return Plan{}, fmt.Errorf("%w: %s and %s", ErrDuplicateEffect, owner, template.ID)
				}
				effects[identity] = template.ID
			}
			compiled.Template.WorkOrder.Order = order
			compiled.Order = &order
		}
		digest, digestErr := compiledNodeDigest(compiled)
		if digestErr != nil {
			return Plan{}, digestErr
		}
		compiled.Digest = digest
		plan.Nodes = append(plan.Nodes, compiled)
	}
	if plan.CapitalMicrounits > input.Initiative.Allocation.CapitalMicrounits ||
		plan.RiskMicrounits > input.Initiative.Allocation.RiskMicrounits ||
		plan.CapitalMicrounits > input.Decision.CapitalImpactMicrounits ||
		plan.RiskMicrounits > input.Decision.RiskImpactMicrounits {
		return Plan{}, ErrBudgetSplitting
	}
	if err := ensureOutcomeGateClosure(plan, input.Initiative); err != nil {
		return Plan{}, err
	}
	if err := compiler.signPlan(&plan, input.Authority.IssuerPolicy.IssuerKeyID); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func validateCompileBindings(input CompileInput) error {
	initiativeID := input.Decision.InitiativeID
	initiative := input.Initiative
	if input.Decision.Decision != portfolio.DecisionGO || initiativeID == nil ||
		*initiativeID != initiative.ID || input.Decision.OrganizationID != initiative.OrganizationID ||
		input.Decision.ID != initiative.PortfolioDecisionID ||
		input.Blueprint.OrganizationID != initiative.OrganizationID ||
		input.Blueprint.InitiativeID != initiative.ID ||
		initiative.MissionVersion != input.Authority.Mission.Version ||
		initiative.ConstitutionVersion != input.Authority.Constitution.Version ||
		initiative.CapitalEnvelopeVersion != input.Authority.Capital.Version ||
		initiative.IssuerPolicyVersion != input.Authority.IssuerPolicy.Version ||
		initiative.Allocation.CapitalMicrounits > input.Decision.CapitalImpactMicrounits ||
		initiative.Allocation.RiskMicrounits > input.Decision.RiskImpactMicrounits ||
		initiative.Allocation.CapitalMicrounits > input.Authority.Capital.SpendCeilingMicrounits ||
		initiative.Allocation.RiskMicrounits > input.Authority.Capital.ExposureCeilingMicrounits ||
		!validToken(input.DecisionKeyID) || input.Decision.Signature.KeyID != input.DecisionKeyID ||
		input.Decision.CreatedAt.After(initiative.CreatedAt) ||
		input.CompiledAt.Before(input.Decision.CreatedAt) ||
		!input.CompiledAt.Before(input.Decision.NextReviewAt) ||
		input.CompiledAt.Before(initiative.CreatedAt) || !input.CompiledAt.Before(initiative.Deadline) {
		return fmt.Errorf("%w: compile input bindings are inconsistent", ErrAuthority)
	}
	return nil
}

func (compiler *Compiler) compileOrder(
	input CompileInput,
	authority workorder.CompanyAuthority,
	planID string,
	template NodeTemplate,
) (workorder.CompanyOrder, uint64, uint64, error) {
	spec := template.WorkOrder
	order := spec.Order
	order.SchemaVersion = workorder.CompanyOrderSchemaVersion
	order.OrganizationID = input.Initiative.OrganizationID
	order.IssuerKind = workorder.IssuerCompanyController
	order.ControllerID = "company-controller:" + string(input.Initiative.OrganizationID)
	if order.Version == 0 {
		order.Version = 1
	}
	order.CreatedAt = input.CompiledAt
	order.Signature = contracts.Signature{}
	if order.Deadline.After(input.Initiative.Deadline) || !order.Deadline.After(input.CompiledAt) ||
		order.Budget.MaxSpendMicrounits != spec.CapitalMicrounits ||
		spec.CapitalMicrounits > input.Authority.IssuerPolicy.MaxWorkOrderMicrounits ||
		reservedClass(input.Authority.Constitution, spec.Class) ||
		!capabilitiesCover(input.Initiative.CapabilityPlan, order.Departments) {
		return workorder.CompanyOrder{}, 0, 0, fmt.Errorf("%w: Work Order %s", ErrAuthority, template.ID)
	}
	gateIDs := make([]string, len(input.Initiative.BusinessGates))
	for index := range input.Initiative.BusinessGates {
		gateIDs[index] = input.Initiative.BusinessGates[index].ID
	}
	order.Binding = workorder.CompanyBinding{
		MissionID:              input.Initiative.MissionID,
		MissionVersion:         input.Initiative.MissionVersion,
		ConstitutionID:         input.Initiative.ConstitutionID,
		ConstitutionVersion:    input.Initiative.ConstitutionVersion,
		InitiativeID:           string(input.Initiative.ID),
		PortfolioDecisionID:    string(input.Initiative.PortfolioDecisionID),
		CapitalAllocationID:    input.Initiative.Allocation.ID,
		CapitalEnvelopeVersion: input.Initiative.CapitalEnvelopeVersion,
		CapitalMicrounits:      spec.CapitalMicrounits, RiskMicrounits: spec.RiskMicrounits,
		IssuerPolicyVersion: input.Initiative.IssuerPolicyVersion,
		WorkOrderClass:      spec.Class,
		CapabilityPlanID:    input.Initiative.CapabilityPlan.ID,
		CapabilityPlanHash:  input.Initiative.CapabilityPlan.Hash,
		InitiativePlanID:    planID, InitiativePlanVersion: input.Blueprint.Version,
		PlanNodeID:                  template.ID,
		InitiativeExecutionCriteria: append([]string(nil), input.Initiative.ExecutionCriteria...),
		BusinessSuccessCriteria:     append([]string(nil), input.Initiative.BusinessCriteria...),
		BusinessOutcomeGateIDs:      gateIDs,
		EffectIdentities:            append([]string(nil), spec.EffectIdentities...),
	}
	order.Binding.IssueIdentity = workorder.ExpectedCompanyIssueIdentity(order)
	if err := workorder.SignCompany(&order, authority, compiler.issuerPrivateKey); err != nil {
		return workorder.CompanyOrder{}, 0, 0, err
	}
	return order, spec.CapitalMicrounits, spec.RiskMicrounits, nil
}

func validateGraph(blueprint Blueprint, initiative Initiative) ([]string, error) {
	nodes := make(map[string]NodeTemplate, len(blueprint.Nodes))
	indegree := make(map[string]int, len(blueprint.Nodes))
	adjacent := make(map[string][]string, len(blueprint.Nodes))
	for _, node := range blueprint.Nodes {
		nodes[node.ID] = node
		indegree[node.ID] = 0
	}
	seenEdges := make(map[string]bool, len(blueprint.Edges))
	seenPairs := make(map[string]bool, len(blueprint.Edges))
	for _, edge := range blueprint.Edges {
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		from, fromExists := nodes[edge.Prerequisite]
		_, toExists := nodes[edge.Successor]
		if !fromExists || !toExists || edge.Schedule.NotBefore.Before(initiative.CreatedAt) ||
			edge.Schedule.Deadline.After(initiative.Deadline) {
			return nil, fmt.Errorf("initiative: edge references or schedule are outside initiative")
		}
		pair := edge.Prerequisite + "\x00" + edge.Successor
		key := pair
		if edge.When != nil {
			key += "\x00" + string(*edge.When)
		}
		if seenEdges[key] || seenPairs[pair] || (from.Kind == NodeBranch) != (edge.When != nil) {
			return nil, fmt.Errorf("initiative: duplicate or incorrectly conditioned edge")
		}
		seenEdges[key] = true
		seenPairs[pair] = true
		adjacent[edge.Prerequisite] = append(adjacent[edge.Prerequisite], edge.Successor)
		indegree[edge.Successor]++
	}
	for _, node := range blueprint.Nodes {
		if node.Kind != NodeBranch {
			continue
		}
		gate, ok := nodes[node.Branch.GateNodeID]
		if !ok || gate.Kind != NodeDecisionGate && gate.Kind != NodeEvidenceGate &&
			gate.Kind != NodeApprovalGate && gate.Kind != NodeEffectGate && gate.Kind != NodeOutcomeGate {
			return nil, fmt.Errorf("initiative: branch gate is not a typed gate")
		}
		if !seenEdges[node.Branch.GateNodeID+"\x00"+node.ID] {
			return nil, fmt.Errorf("initiative: branch does not depend on its exact gate")
		}
		for _, branch := range node.Branch.Cases {
			key := node.ID + "\x00" + branch.Successor + "\x00" + string(branch.Outcome)
			if !seenEdges[key] {
				return nil, fmt.Errorf("initiative: branch case has no exact conditioned edge")
			}
		}
	}
	ready := make([]string, 0, len(nodes))
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	slices.Sort(ready)
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		slices.Sort(adjacent[id])
		for _, successor := range adjacent[id] {
			indegree[successor]--
			if indegree[successor] == 0 {
				ready = append(ready, successor)
				slices.Sort(ready)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, ErrCycle
	}
	return order, nil
}

func ensureOutcomeGateClosure(plan Plan, initiative Initiative) error {
	outcomeNodes := make(map[string]string)
	successID := ""
	for _, node := range plan.Nodes {
		if node.Template.Kind == NodeOutcomeGate {
			predicateID := node.Template.Gate.PredicateID
			if _, duplicate := outcomeNodes[predicateID]; duplicate {
				return fmt.Errorf("initiative: duplicate business outcome predicate %s", predicateID)
			}
			outcomeNodes[predicateID] = node.Template.ID
		}
		if node.Template.Kind == NodeTerminalSuccess {
			successID = node.Template.ID
		}
	}
	if successID == "" {
		return fmt.Errorf("initiative: success terminal is absent")
	}
	for _, gate := range initiative.BusinessGates {
		nodeID, exists := outcomeNodes[gate.ID]
		if !exists {
			return fmt.Errorf("initiative: business outcome gate %s is absent", gate.ID)
		}
		if reachableWithout(plan, successID, nodeID) {
			return fmt.Errorf("initiative: business outcome gate %s can be bypassed", gate.ID)
		}
	}
	for _, edge := range plan.Edges {
		if edge.Successor == successID {
			predecessor := findCompiledNode(plan.Nodes, edge.Prerequisite)
			if predecessor == nil || predecessor.Template.Kind != NodeOutcomeGate &&
				predecessor.Template.Kind != NodeBranch {
				return fmt.Errorf("initiative: success terminal bypasses business outcome gates")
			}
		}
	}
	return nil
}

func reachableWithout(plan Plan, targetID, omittedID string) bool {
	originalIndegree := make(map[string]int, len(plan.Nodes))
	adjacent := make(map[string][]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		originalIndegree[node.Template.ID] = 0
	}
	for _, edge := range plan.Edges {
		originalIndegree[edge.Successor]++
	}
	for _, edge := range plan.Edges {
		if edge.Prerequisite == omittedID || edge.Successor == omittedID {
			continue
		}
		if _, exists := originalIndegree[edge.Prerequisite]; !exists {
			continue
		}
		if _, exists := originalIndegree[edge.Successor]; !exists {
			continue
		}
		adjacent[edge.Prerequisite] = append(adjacent[edge.Prerequisite], edge.Successor)
	}
	queue := make([]string, 0)
	for nodeID, degree := range originalIndegree {
		if degree == 0 && nodeID != omittedID {
			queue = append(queue, nodeID)
		}
	}
	seen := make(map[string]bool, len(originalIndegree))
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		if nodeID == targetID {
			return true
		}
		queue = append(queue, adjacent[nodeID]...)
	}
	return false
}

func reservedClass(constitution mission.CompanyConstitution, class string) bool {
	for _, reserved := range constitution.ReservedDecisions {
		if class == string(reserved.Kind) || class == reserved.ClauseID {
			return true
		}
	}
	return false
}

func capabilitiesCover(plan CapabilityPlan, departments []contracts.DepartmentKind) bool {
	for _, department := range departments {
		covered := false
		for _, requirement := range plan.Requirements {
			if requirement.Department == department {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func CapabilityPlanHash(value CapabilityPlan) (contracts.ContentHash, error) {
	payload := struct {
		ID           string                  `json:"capability_plan_id"`
		Version      uint64                  `json:"version"`
		Requirements []CapabilityRequirement `json:"requirements"`
	}{value.ID, value.Version, value.Requirements}
	canonical, err := contracts.EncodeCanonical(&canonicalValue[struct {
		ID           string                  `json:"capability_plan_id"`
		Version      uint64                  `json:"version"`
		Requirements []CapabilityRequirement `json:"requirements"`
	}]{Value: payload})
	if err != nil {
		return contracts.ContentHash{}, err
	}
	return sha256Hash(canonical), nil
}

func capabilityPlanHash(value CapabilityPlan) (contracts.ContentHash, error) {
	return CapabilityPlanHash(value)
}

type canonicalValue[T any] struct {
	Value T `json:"value"`
}

func (*canonicalValue[T]) Validate() error { return nil }

func compiledNodeDigest(value CompiledNode) (contracts.ContentHash, error) {
	payload := struct {
		Template NodeTemplate            `json:"template"`
		Order    *workorder.CompanyOrder `json:"company_order"`
	}{value.Template, value.Order}
	canonical, err := contracts.EncodeCanonical(&canonicalValue[struct {
		Template NodeTemplate            `json:"template"`
		Order    *workorder.CompanyOrder `json:"company_order"`
	}]{Value: payload})
	if err != nil {
		return contracts.ContentHash{}, err
	}
	return sha256Hash(canonical), nil
}

func (compiler *Compiler) signPlan(plan *Plan, keyID string) error {
	if err := validatePlanCore(*plan); err != nil {
		return err
	}
	hashPayload := *plan
	hashPayload.Hash = contracts.ContentHash{}
	hashPayload.Signature = contracts.Signature{}
	canonical, err := contracts.EncodeCanonical(&canonicalValue[Plan]{Value: hashPayload})
	if err != nil {
		return err
	}
	plan.Hash = sha256Hash(canonical)
	payload, err := planSigningBytes(*plan, keyID)
	if err != nil {
		return err
	}
	plan.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(compiler.issuerPrivateKey, payload)),
	}
	return nil
}

func validatePlanCore(plan Plan) error {
	if plan.SchemaVersion != PlanSchemaVersion || !validToken(plan.ID) || plan.Version == 0 ||
		plan.OrganizationID == "" || !validToken(string(plan.InitiativeID)) ||
		plan.InitiativeVersion == 0 || !validToken(plan.BlueprintID) ||
		plan.BlueprintVersion != plan.Version || plan.Authority.MissionVersion == 0 ||
		plan.Authority.ConstitutionVersion == 0 || plan.Authority.CapitalEnvelopeVersion == 0 ||
		plan.Authority.IssuerPolicyVersion == 0 || !validToken(plan.Authority.PortfolioDecisionID) ||
		!validToken(plan.Authority.CapitalAllocationID) || !validToken(plan.Authority.CapabilityPlanID) ||
		len(plan.Nodes) == 0 || !validUTC(plan.CompiledAt) {
		return fmt.Errorf("initiative: compiled plan identity is invalid")
	}
	indegree := make(map[string]int, len(plan.Nodes))
	adjacent := make(map[string][]string, len(plan.Nodes))
	effects := make(map[string]string)
	var capital uint64
	var risk uint64
	previousNode := ""
	for index := range plan.Nodes {
		node := plan.Nodes[index]
		if node.Template.Validate() != nil || node.Template.ID <= previousNode ||
			(node.State != StatePending && node.State != StatePreserved &&
				node.State != StateInvalidated && node.State != StateCancelled) {
			return fmt.Errorf("initiative: compiled nodes are invalid or non-canonical")
		}
		if node.State == StatePreserved {
			if !validReceiptSet(node.ReceiptReferences) {
				return fmt.Errorf("initiative: preserved node lacks receipts")
			}
		} else if node.State == StateInvalidated && len(node.ReceiptReferences) > 0 {
			if !validReceiptSet(node.ReceiptReferences) {
				return fmt.Errorf("initiative: invalidated node receipt references are invalid")
			}
		} else if len(node.ReceiptReferences) != 0 {
			return fmt.Errorf("initiative: pending or cancelled node cannot claim receipts")
		}
		expectedDigest, err := compiledNodeDigest(node)
		if err != nil || expectedDigest != node.Digest {
			return fmt.Errorf("initiative: compiled node digest mismatch")
		}
		if node.Template.Kind == NodeWorkOrder {
			if node.Order == nil || node.Order.Validate() != nil ||
				!sameCompanyOrder(node.Template.WorkOrder.Order, *node.Order) ||
				node.Order.OrganizationID != plan.OrganizationID ||
				node.Order.Binding.InitiativeID != string(plan.InitiativeID) ||
				node.Order.Binding.InitiativePlanID != plan.ID ||
				node.Order.Binding.InitiativePlanVersion != plan.Version ||
				node.Order.Binding.PlanNodeID != node.Template.ID ||
				node.Order.Binding.MissionVersion != plan.Authority.MissionVersion ||
				node.Order.Binding.ConstitutionVersion != plan.Authority.ConstitutionVersion ||
				node.Order.Binding.CapitalEnvelopeVersion != plan.Authority.CapitalEnvelopeVersion ||
				node.Order.Binding.IssuerPolicyVersion != plan.Authority.IssuerPolicyVersion {
				return fmt.Errorf("initiative: compiled Work Order binding mismatch")
			}
			if capital > math.MaxUint64-node.Order.Binding.CapitalMicrounits ||
				risk > math.MaxUint64-node.Order.Binding.RiskMicrounits {
				return ErrBudgetSplitting
			}
			capital += node.Order.Binding.CapitalMicrounits
			risk += node.Order.Binding.RiskMicrounits
			for _, identity := range node.Order.Binding.EffectIdentities {
				if owner, duplicate := effects[identity]; duplicate {
					return fmt.Errorf("%w: %s and %s", ErrDuplicateEffect, owner, node.Template.ID)
				}
				effects[identity] = node.Template.ID
			}
		} else if node.Order != nil {
			return fmt.Errorf("initiative: non-Work Order node carries an order")
		}
		indegree[node.Template.ID] = 0
		previousNode = node.Template.ID
	}
	if capital != plan.CapitalMicrounits || risk != plan.RiskMicrounits {
		return fmt.Errorf("initiative: compiled plan budget totals mismatch")
	}
	previousEdge := ""
	for _, edge := range plan.Edges {
		if edge.Validate() != nil {
			return fmt.Errorf("initiative: compiled edge is invalid")
		}
		if _, exists := indegree[edge.Prerequisite]; !exists {
			return fmt.Errorf("initiative: compiled edge prerequisite is absent")
		}
		if _, exists := indegree[edge.Successor]; !exists {
			return fmt.Errorf("initiative: compiled edge successor is absent")
		}
		key := edge.Prerequisite + "\x00" + edge.Successor
		if edge.When != nil {
			key += "\x00" + string(*edge.When)
		}
		if key <= previousEdge {
			return fmt.Errorf("initiative: compiled edges must be sorted and unique")
		}
		previousEdge = key
		indegree[edge.Successor]++
		adjacent[edge.Prerequisite] = append(adjacent[edge.Prerequisite], edge.Successor)
	}
	ready := make([]string, 0)
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	slices.Sort(ready)
	order := make([]string, 0, len(plan.Nodes))
	for len(ready) > 0 {
		nodeID := ready[0]
		ready = ready[1:]
		order = append(order, nodeID)
		slices.Sort(adjacent[nodeID])
		for _, successor := range adjacent[nodeID] {
			indegree[successor]--
			if indegree[successor] == 0 {
				ready = append(ready, successor)
				slices.Sort(ready)
			}
		}
	}
	if len(order) != len(plan.Nodes) || !slices.Equal(order, plan.TopologicalOrder) {
		return ErrCycle
	}
	return nil
}

func sameCompanyOrder(left, right workorder.CompanyOrder) bool {
	leftCanonical, leftErr := contracts.EncodeCanonical(&left)
	rightCanonical, rightErr := contracts.EncodeCanonical(&right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func planSigningBytes(plan Plan, keyID string) ([]byte, error) {
	plan.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&canonicalValue[Plan]{Value: plan})
}

func initiativeSigningBytes(value Initiative, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func issuerKey(policy mission.CompanyIssuerPolicy) (ed25519.PublicKey, error) {
	value, err := base64.RawURLEncoding.DecodeString(policy.IssuerPublicKey)
	if err != nil || len(value) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: issuer public key", ErrAuthority)
	}
	return ed25519.PublicKey(value), nil
}

func sha256Hash(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func findCompiledNode(nodes []CompiledNode, id string) *CompiledNode {
	for index := range nodes {
		if nodes[index].Template.ID == id {
			return &nodes[index]
		}
	}
	return nil
}

func SortedEffectIdentities(order workorder.CompanyOrder) []string {
	values := append([]string(nil), order.Binding.EffectIdentities...)
	slices.Sort(values)
	return values
}

func ExactAuthoritySummary(plan Plan) string {
	return strings.Join([]string{
		fmt.Sprint(plan.Authority.MissionVersion), fmt.Sprint(plan.Authority.ConstitutionVersion),
		fmt.Sprint(plan.Authority.CapitalEnvelopeVersion), fmt.Sprint(plan.Authority.IssuerPolicyVersion),
		plan.Authority.PortfolioDecisionID, plan.Authority.CapitalAllocationID,
		plan.Authority.CapabilityPlanID,
	}, ":")
}
