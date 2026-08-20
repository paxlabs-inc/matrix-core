package squad

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/organization"
)

var (
	ErrNoQualifiedSquad = errors.New("squad: no qualified squad")
	ErrPlanningLimit    = errors.New("squad: deterministic planning limit exhausted")
	ErrCapturedAuditor  = errors.New("squad: independent Auditor is captured")
	ErrResourceLimit    = errors.New("squad: resource limit exceeded")
)

type Selection struct {
	Members          []AssignmentMember
	SatisfiedRuleIDs []string
	SearchNodes      uint64
}

type planItem struct {
	id        string
	need      *CapabilityNeed
	role      *contracts.SeatRole
	eligible  []int
	resources organization.ResourceVector
}

type searchPlan struct {
	selected []bool
	needIDs  [][]string
	usage    []organization.ResourceVector
	count    int
}

type planner struct {
	requirement Requirement
	template    organization.OrganizationTemplate
	candidates  []Candidate
	items       []planItem
	nodes       uint64
	best        *searchPlan
}

func SelectSmallest(
	requirement Requirement,
	template organization.OrganizationTemplate,
	registry *organization.Registry,
	candidates []Candidate,
) (Selection, error) {
	if err := requirement.Validate(); err != nil {
		return Selection{}, err
	}
	if err := template.Validate(); err != nil {
		return Selection{}, err
	}
	if err := validateRequirementRegistry(requirement, template, registry); err != nil {
		return Selection{}, err
	}
	templateDigest, err := organization.TemplateDigest(template)
	if err != nil {
		return Selection{}, err
	}
	if template.OrganizationID != requirement.OrganizationID || template.ID != requirement.TemplateID ||
		template.Version != requirement.TemplateVersion || templateDigest != requirement.TemplateDigest ||
		template.EffectiveAt.After(requirement.IssuedAt) ||
		template.ExpiresAt != nil && template.ExpiresAt.Before(requirement.ExpiresAt) {
		return Selection{}, fmt.Errorf("squad: requirement does not bind the current template")
	}
	if len(candidates) == 0 || len(candidates) > MaximumCandidates {
		return Selection{}, ErrNoQualifiedSquad
	}
	ordered := append([]Candidate(nil), candidates...)
	slices.SortFunc(ordered, func(left, right Candidate) int {
		return strings.Compare(string(left.Mandate.SeatID), string(right.Mandate.SeatID))
	})
	seenSeats := make(map[contracts.SeatID]bool, len(ordered))
	for _, candidate := range ordered {
		if err := candidate.Validate(); err != nil {
			return Selection{}, err
		}
		if seenSeats[candidate.Mandate.SeatID] {
			return Selection{}, fmt.Errorf("squad: duplicate candidate seat %q", candidate.Mandate.SeatID)
		}
		seenSeats[candidate.Mandate.SeatID] = true
		if !candidateMatchesTemplate(candidate, template) {
			return Selection{}, fmt.Errorf("squad: candidate is not in the bound template")
		}
	}
	planner := &planner{requirement: requirement, template: template, candidates: ordered}
	items, err := planner.buildItems()
	if err != nil {
		return Selection{}, err
	}
	planner.items = items
	initial := searchPlan{
		selected: make([]bool, len(ordered)), needIDs: make([][]string, len(ordered)),
		usage: make([]organization.ResourceVector, len(ordered)),
	}
	for index, candidate := range ordered {
		initial.usage[index].Currency = candidate.Runtime.ResourceAvailable.Currency
	}
	if err := planner.search(0, &initial); err != nil {
		return Selection{}, err
	}
	if planner.best == nil {
		return Selection{}, ErrNoQualifiedSquad
	}
	members := planner.members(planner.best)
	ruleIDs := make([]string, len(requirement.SegregationRules))
	for index, rule := range requirement.SegregationRules {
		ruleIDs[index] = rule.ID
	}
	slices.Sort(ruleIDs)
	return Selection{Members: members, SatisfiedRuleIDs: ruleIDs, SearchNodes: planner.nodes}, nil
}

func validateRequirementRegistry(
	requirement Requirement,
	template organization.OrganizationTemplate,
	registry *organization.Registry,
) error {
	if registry == nil || registry.OrganizationID() != requirement.OrganizationID ||
		registry.Digest() != template.CapabilityRegistryDigest {
		return fmt.Errorf("squad: requirement capability registry is not the template registry")
	}
	for _, need := range requirement.Needs {
		definition, err := registry.Resolve(need.Capability)
		if err != nil {
			return err
		}
		_, currentReference, err := registry.Current(need.Capability.ID)
		if err != nil || currentReference != need.Capability {
			return fmt.Errorf("squad: capability %q is not the current registry version", need.Capability.ID)
		}
		if definition.EffectiveAt.After(requirement.IssuedAt) ||
			definition.ExpiresAt != nil && definition.ExpiresAt.Before(requirement.ExpiresAt) ||
			!slices.Contains(definition.LifecycleStages, requirement.LifecycleStage) {
			return fmt.Errorf("squad: capability %q is not current for lifecycle stage %s", need.Capability.ID, requirement.LifecycleStage)
		}
		if need.Kind == NeedVerification && definition.Kind != organization.CapabilityVerification ||
			need.Kind == NeedWork && definition.Kind == organization.CapabilityVerification {
			return fmt.Errorf("squad: capability %q does not match the requested duty", need.Capability.ID)
		}
		for _, role := range need.AllowedRoles {
			if !slices.Contains(definition.AllowedRoles, role) {
				return fmt.Errorf("squad: capability need widens its allowed roles")
			}
		}
		if !allSkillIDsContained(need.Skills, definition.RequiredSkills) ||
			!allScopesContained(need.DataScopes, definition.RequiredDataScopes) ||
			!definition.ResourceEstimate.Fits(need.Resources) ||
			!allStringsContained(definition.ReceiptSchemaVersions, need.ReceiptSchemaVersions) {
			return fmt.Errorf("squad: capability need omits a registry requirement")
		}
	}
	return nil
}

func (planner *planner) buildItems() ([]planItem, error) {
	items := make([]planItem, 0, len(planner.requirement.Needs)+len(planner.requirement.RequiredRoles))
	for index := range planner.requirement.Needs {
		need := &planner.requirement.Needs[index]
		item := planItem{id: need.ID, need: need, resources: need.Resources}
		for candidateIndex, candidate := range planner.candidates {
			if planner.candidateAvailable(candidate) && candidateCoversNeed(candidate, *need) {
				item.eligible = append(item.eligible, candidateIndex)
			}
		}
		if len(item.eligible) == 0 {
			return nil, fmt.Errorf("%w: capability need %s", ErrNoQualifiedSquad, need.ID)
		}
		items = append(items, item)
	}
	for index := range planner.requirement.RequiredRoles {
		role := planner.requirement.RequiredRoles[index]
		item := planItem{
			id: "role:" + string(role), role: &role,
			resources: organization.ResourceVector{Currency: planner.candidates[0].Runtime.ResourceAvailable.Currency},
		}
		for candidateIndex, candidate := range planner.candidates {
			if planner.candidateAvailable(candidate) && candidate.Mandate.Role == role {
				item.eligible = append(item.eligible, candidateIndex)
			}
		}
		if len(item.eligible) == 0 {
			return nil, fmt.Errorf("%w: required role %s", ErrNoQualifiedSquad, role)
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(left, right planItem) int {
		if len(left.eligible) < len(right.eligible) {
			return -1
		}
		if len(left.eligible) > len(right.eligible) {
			return 1
		}
		return strings.Compare(left.id, right.id)
	})
	return items, nil
}

func (planner *planner) search(position int, current *searchPlan) error {
	planner.nodes++
	if planner.nodes > MaximumSearchNodes {
		return ErrPlanningLimit
	}
	if planner.best != nil && current.count > planner.best.count ||
		current.count > int(planner.requirement.MaximumMembers) {
		return nil
	}
	if position == len(planner.items) {
		if current.count < 2 || !planner.segregationSatisfied(current) {
			return nil
		}
		if planner.best == nil || comparePlans(current, planner.best, planner.candidates) < 0 {
			copy := clonePlan(current)
			planner.best = &copy
		}
		return nil
	}
	item := planner.items[position]
	for _, candidateIndex := range item.eligible {
		if !planner.canAdd(current, candidateIndex, item) {
			continue
		}
		wasSelected := current.selected[candidateIndex]
		previousUsage := current.usage[candidateIndex]
		combined := previousUsage
		var err error
		if item.need != nil {
			combined, err = previousUsage.Add(item.resources)
		}
		remaining, remainingErr := planner.candidates[candidateIndex].RemainingResources()
		if err != nil || remainingErr != nil || !combined.Fits(remaining) ||
			!combined.Fits(planner.candidates[candidateIndex].Mandate.ResourceLimit) {
			continue
		}
		current.usage[candidateIndex] = combined
		current.needIDs[candidateIndex] = append(current.needIDs[candidateIndex], item.id)
		if !wasSelected {
			current.selected[candidateIndex] = true
			current.count++
		}
		if err := planner.search(position+1, current); err != nil {
			return err
		}
		if !wasSelected {
			current.selected[candidateIndex] = false
			current.count--
		}
		current.needIDs[candidateIndex] = current.needIDs[candidateIndex][:len(current.needIDs[candidateIndex])-1]
		current.usage[candidateIndex] = previousUsage
	}
	return nil
}

func (planner *planner) canAdd(current *searchPlan, candidateIndex int, item planItem) bool {
	candidate := planner.candidates[candidateIndex]
	for selectedIndex, selected := range current.selected {
		if !selected || selectedIndex == candidateIndex {
			continue
		}
		other := planner.candidates[selectedIndex]
		if planner.prohibitedPair(candidate.Mandate.SeatID, other.Mandate.SeatID) {
			return false
		}
		candidateAudit := candidate.Mandate.Role == contracts.SeatAuditor
		otherAudit := other.Mandate.Role == contracts.SeatAuditor
		if candidateAudit != otherAudit {
			if candidate.Mandate.IndependenceDomain == other.Mandate.IndependenceDomain {
				return false
			}
			if planner.distinctAuditDepartment() && candidate.DepartmentID == other.DepartmentID {
				return false
			}
		}
	}
	if item.need != nil && item.need.Kind == NeedVerification &&
		candidate.Mandate.Role != contracts.SeatAuditor {
		return false
	}
	if item.need != nil && item.need.Kind == NeedWork &&
		candidate.Mandate.Role == contracts.SeatAuditor {
		return false
	}
	return true
}

func (planner *planner) candidateAvailable(candidate Candidate) bool {
	if candidate.Runtime.Availability != RuntimeAvailable ||
		candidate.Runtime.TemplateID != planner.requirement.TemplateID ||
		candidate.Runtime.TemplateVersion != planner.requirement.TemplateVersion ||
		candidate.Runtime.AvailableFrom.After(planner.requirement.IssuedAt) ||
		candidate.Runtime.AvailableUntil.Before(planner.requirement.ExpiresAt) ||
		candidate.Runtime.ObservedAt.After(planner.requirement.IssuedAt) ||
		candidate.Runtime.ExpiresAt.Before(planner.requirement.ExpiresAt) ||
		candidate.Mandate.EffectiveAt.After(planner.requirement.IssuedAt) ||
		candidate.Mandate.ExpiresAt != nil && candidate.Mandate.ExpiresAt.Before(planner.requirement.ExpiresAt) {
		return false
	}
	if intersects(candidate.Runtime.HeldConflictScopes, planner.requirement.GraphScopes) ||
		intersects(candidate.Runtime.HeldConflictScopes, planner.requirement.ConflictDomains) ||
		intersects(candidate.ActiveConflictScopes, planner.requirement.GraphScopes) ||
		intersects(candidate.ActiveConflictScopes, planner.requirement.ConflictDomains) ||
		intersects(candidate.Mandate.ConflictDomains, planner.requirement.GraphScopes) ||
		intersects(candidate.Mandate.ConflictDomains, planner.requirement.ConflictDomains) {
		return false
	}
	return allStringsContained(candidate.Mandate.ReceiptSchemaVersions, planner.requirement.ReceiptSchemaVersions)
}

func candidateCoversNeed(candidate Candidate, need CapabilityNeed) bool {
	remaining, err := candidate.RemainingResources()
	if err != nil {
		return false
	}
	if !slices.Contains(need.AllowedRoles, candidate.Mandate.Role) ||
		!slices.Contains(candidate.Mandate.AllowedCapabilities, need.Capability) ||
		!allScopesContained(candidate.Mandate.DataScopes, need.DataScopes) ||
		!allSkillsContained(candidate.Mandate.AllowedSkills, need.Skills) ||
		!slices.Contains(need.ModelBindings, candidate.Mandate.ModelBinding) ||
		!allStringsContained(candidate.Mandate.ReceiptSchemaVersions, need.ReceiptSchemaVersions) ||
		!need.Resources.Fits(candidate.Mandate.ResourceLimit) ||
		!need.Resources.Fits(remaining) {
		return false
	}
	return true
}

func candidateMatchesTemplate(candidate Candidate, template organization.OrganizationTemplate) bool {
	if candidate.Runtime.TemplateID != template.ID || candidate.Runtime.TemplateVersion != template.Version {
		return false
	}
	for _, department := range template.Departments {
		if department.ID != candidate.DepartmentID {
			continue
		}
		for _, mandate := range department.Mandates {
			if mandate.SeatID != candidate.Mandate.SeatID {
				continue
			}
			left, leftErr := organization.SeatMandateDigest(mandate)
			right, rightErr := organization.SeatMandateDigest(candidate.Mandate)
			return leftErr == nil && rightErr == nil && left == right
		}
	}
	return false
}

func (planner *planner) segregationSatisfied(plan *searchPlan) bool {
	hasAuditor := false
	hasProducer := false
	for index, selected := range plan.selected {
		if !selected {
			continue
		}
		if planner.candidates[index].Mandate.Role == contracts.SeatAuditor {
			hasAuditor = true
		} else {
			hasProducer = true
		}
	}
	return hasAuditor && hasProducer
}

func (planner *planner) prohibitedPair(left, right contracts.SeatID) bool {
	if left > right {
		left, right = right, left
	}
	for _, rule := range planner.requirement.SegregationRules {
		if rule.Kind == SegregationProhibitedPair && rule.Pair != nil &&
			rule.Pair.First == left && rule.Pair.Second == right {
			return true
		}
	}
	return false
}

func (planner *planner) distinctAuditDepartment() bool {
	for _, rule := range planner.requirement.SegregationRules {
		if rule.Kind == SegregationDistinctDepartment {
			return true
		}
	}
	return false
}

func (planner *planner) members(plan *searchPlan) []AssignmentMember {
	result := make([]AssignmentMember, 0, plan.count)
	for index, selected := range plan.selected {
		if !selected {
			continue
		}
		candidate := planner.candidates[index]
		digest, _ := organization.SeatMandateDigest(candidate.Mandate)
		needIDs := append([]string(nil), plan.needIDs[index]...)
		slices.Sort(needIDs)
		result = append(result, AssignmentMember{
			SeatID: candidate.Mandate.SeatID, DepartmentID: candidate.DepartmentID,
			Role: candidate.Mandate.Role, MandateID: candidate.Mandate.ID,
			MandateVersion: candidate.Mandate.Version, MandateDigest: digest,
			ModelBinding:       candidate.Mandate.ModelBinding,
			IndependenceDomain: candidate.Mandate.IndependenceDomain,
			NeedIDs:            needIDs, AllocatedResources: plan.usage[index],
		})
	}
	return result
}

func clonePlan(value *searchPlan) searchPlan {
	result := searchPlan{
		selected: append([]bool(nil), value.selected...),
		needIDs:  make([][]string, len(value.needIDs)),
		usage:    append([]organization.ResourceVector(nil), value.usage...),
		count:    value.count,
	}
	for index := range value.needIDs {
		result.needIDs[index] = append([]string(nil), value.needIDs[index]...)
	}
	return result
}

func comparePlans(left, right *searchPlan, candidates []Candidate) int {
	if left.count < right.count {
		return -1
	}
	if left.count > right.count {
		return 1
	}
	leftIdentity := planIdentity(left, candidates)
	rightIdentity := planIdentity(right, candidates)
	return strings.Compare(leftIdentity, rightIdentity)
}

func planIdentity(value *searchPlan, candidates []Candidate) string {
	parts := make([]string, 0, value.count)
	for index, selected := range value.selected {
		if !selected {
			continue
		}
		needs := append([]string(nil), value.needIDs[index]...)
		slices.Sort(needs)
		parts = append(parts, string(candidates[index].Mandate.SeatID)+"="+strings.Join(needs, ","))
	}
	return strings.Join(parts, ";")
}

func intersects(left, right []string) bool {
	for _, value := range left {
		if slices.Contains(right, value) {
			return true
		}
	}
	return false
}

func allStringsContained(available, required []string) bool {
	for _, value := range required {
		if !slices.Contains(available, value) {
			return false
		}
	}
	return true
}

func allScopesContained(available, required []contracts.DataScope) bool {
	for _, value := range required {
		if !slices.Contains(available, value) {
			return false
		}
	}
	return true
}

func allSkillsContained(available, required []contracts.SkillRef) bool {
	for _, value := range required {
		if !slices.Contains(available, value) {
			return false
		}
	}
	return true
}

func allSkillIDsContained(available []contracts.SkillRef, required []contracts.SkillID) bool {
	for _, skillID := range required {
		found := false
		for _, skill := range available {
			if skill.ID == skillID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
