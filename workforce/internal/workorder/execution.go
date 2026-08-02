package workorder

import (
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/contracts"
)

// ExecutionOrder is the issuer-neutral projection consumed by the Workforce
// Execution Loop after the owning signature domain has been verified.
type ExecutionOrder struct {
	ID                 string
	Domain             string
	OrganizationID     contracts.OrganizationID
	Objective          string
	Scope              string
	ProjectID          contracts.ProjectID
	WorkspaceID        contracts.WorkspaceID
	ScopeFiles         []string
	ScopeSymbols       []string
	Departments        []contracts.DepartmentKind
	Priority           int32
	Budget             Budget
	Deadline           time.Time
	Autonomy           string
	AcceptanceCriteria []string
	ModelProvider      string
	ModelID            string
	MGSReference       string
	MGSDigest          string
	CreatedAt          time.Time
}

func ExecutionFromOwner(order Order) ExecutionOrder {
	return ExecutionOrder{
		ID: order.ID, Domain: "owner", OrganizationID: order.OrganizationID,
		Objective: order.Objective, Scope: order.Scope,
		ProjectID: order.ProjectID, WorkspaceID: order.WorkspaceID,
		ScopeFiles: slices.Clone(order.ScopeFiles), ScopeSymbols: slices.Clone(order.ScopeSymbols),
		Departments: slices.Clone(order.Departments), Priority: order.Priority,
		Budget: order.Budget, Deadline: order.Deadline, Autonomy: order.Autonomy,
		AcceptanceCriteria: slices.Clone(order.AcceptanceCriteria),
		ModelProvider:      order.ModelProvider, ModelID: order.ModelID,
		MGSReference: order.MGSReference, MGSDigest: order.MGSDigest,
		CreatedAt: order.CreatedAt,
	}
}

func ExecutionFromCompany(order CompanyOrder) ExecutionOrder {
	return ExecutionOrder{
		ID: order.ID, Domain: "company", OrganizationID: order.OrganizationID,
		Objective: order.Objective, Scope: order.Scope,
		ProjectID: order.ProjectID, WorkspaceID: order.WorkspaceID,
		ScopeFiles: slices.Clone(order.ScopeFiles), ScopeSymbols: slices.Clone(order.ScopeSymbols),
		Departments: slices.Clone(order.Departments), Priority: order.Priority,
		Budget: order.Budget, Deadline: order.Deadline, Autonomy: order.Autonomy,
		AcceptanceCriteria: slices.Clone(order.AcceptanceCriteria),
		ModelProvider:      order.ModelProvider, ModelID: order.ModelID,
		MGSReference: order.MGSReference, MGSDigest: order.MGSDigest,
		CreatedAt: order.CreatedAt,
	}
}

func ExecutionFromCompanyCycle(order CompanyCycleOrder) ExecutionOrder {
	return ExecutionOrder{
		ID: order.ID, Domain: "cycle", OrganizationID: order.OrganizationID,
		Objective: order.Objective, Scope: order.Scope,
		Departments: slices.Clone(order.Departments), Priority: order.Priority,
		Budget: order.Budget, Deadline: order.Deadline, Autonomy: order.Autonomy,
		AcceptanceCriteria: slices.Clone(order.AcceptanceCriteria),
		ModelProvider:      order.ModelProvider, ModelID: order.ModelID,
		MGSReference: order.MGSReference, MGSDigest: order.MGSDigest,
		CreatedAt: order.CreatedAt,
	}
}

func (value ExecutionOrder) Validate() error {
	if value.ID == "" || len(value.ID) > 96 ||
		(value.Domain != "owner" && value.Domain != "company" && value.Domain != "cycle") ||
		value.OrganizationID == "" ||
		value.Objective == "" || value.Scope == "" || len(value.Departments) == 0 ||
		value.Budget.MaxTasks == 0 || value.Budget.MaxSpendMicrounits == 0 ||
		value.Deadline.IsZero() || value.CreatedAt.IsZero() ||
		!value.Deadline.After(value.CreatedAt) || len(value.AcceptanceCriteria) == 0 ||
		value.ModelProvider == "" || value.ModelID == "" || value.MGSReference == "" ||
		len(value.MGSDigest) != 64 {
		return fmt.Errorf("workorder: execution projection is incomplete")
	}
	return nil
}
