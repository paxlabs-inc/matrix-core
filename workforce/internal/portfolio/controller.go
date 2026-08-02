package portfolio

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"
	"time"

	"matrix/workforce/internal/contracts"
)

// CyclePlan is deterministic controller work to be compiled into bounded Work Orders.
type CyclePlan struct {
	SchemaVersion        string                     `json:"schema_version"`
	ID                   string                     `json:"cycle_id"`
	OrganizationID       contracts.OrganizationID   `json:"organization_id"`
	Kind                 CadenceKind                `json:"kind"`
	Departments          []contracts.DepartmentKind `json:"departments"`
	RequiredCapabilities []string                   `json:"required_capabilities"`
	IndependentAudit     bool                       `json:"independent_audit"`
	DueAt                time.Time                  `json:"due_at"`
	NextAt               time.Time                  `json:"next_at"`
}

// Validate enforces canonical coverage and an independent review boundary.
func (value CyclePlan) Validate() error {
	if value.SchemaVersion != "workforce.company-cycle.v1" || value.ID == "" ||
		value.OrganizationID == "" || !value.Kind.Valid() || len(value.Departments) == 0 ||
		len(value.RequiredCapabilities) == 0 || !value.IndependentAudit ||
		!validUTC(value.DueAt) || !validUTC(value.NextAt) || !value.NextAt.After(value.DueAt) {
		return fmt.Errorf("portfolio: company cycle plan is invalid")
	}
	previousDepartment := ""
	for _, department := range value.Departments {
		if !department.Valid() || string(department) <= previousDepartment {
			return fmt.Errorf("portfolio: cycle departments must be sorted and unique")
		}
		previousDepartment = string(department)
	}
	if !sortedUniqueText(value.RequiredCapabilities) {
		return fmt.Errorf("portfolio: cycle capabilities must be sorted and unique")
	}
	return nil
}

// Controller owns deterministic cadence planning and portfolio selection. It
// never receives provider credentials or performs an external effect.
type Controller struct {
	store              *Store
	procedure          DecisionProcedure
	procedurePublicKey ed25519.PublicKey
}

// NewController binds the controller to one current founder-signed procedure.
func NewController(
	store *Store,
	procedure DecisionProcedure,
	procedurePublicKey ed25519.PublicKey,
) (*Controller, error) {
	if store == nil || procedure.OrganizationID != store.organizationID {
		return nil, fmt.Errorf("portfolio: controller store and procedure are required")
	}
	if err := VerifyProcedure(procedure, procedure.Signature.KeyID, procedurePublicKey); err != nil {
		return nil, err
	}
	return &Controller{
		store: store, procedure: procedure,
		procedurePublicKey: append(ed25519.PublicKey(nil), procedurePublicKey...),
	}, nil
}

// ClaimCyclePlans atomically coalesces due cadences into typed company work.
func (controller *Controller) ClaimCyclePlans(ctx context.Context, limit uint16) ([]CyclePlan, error) {
	due, err := controller.store.ClaimDueCadences(ctx, limit)
	if err != nil {
		return nil, err
	}
	plans := make([]CyclePlan, 0, len(due))
	for _, cadence := range due {
		departments, capabilities := cycleCoverage(cadence.Kind)
		plan := CyclePlan{
			SchemaVersion:  "workforce.company-cycle.v1",
			ID:             fmt.Sprintf("company-cycle:%s:%d", cadence.ID, cadence.DueAt.UnixNano()),
			OrganizationID: controller.store.organizationID,
			Kind:           cadence.Kind, Departments: departments,
			RequiredCapabilities: capabilities, IndependentAudit: true,
			DueAt: cadence.DueAt, NextAt: cadence.NextAt,
		}
		if err := plan.Validate(); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// Decide applies the current founder-signed procedure and commits its receipt.
func (controller *Controller) Decide(
	ctx context.Context,
	id DecisionID,
	opportunityID OpportunityID,
	assessment Assessment,
	alternatives []Alternative,
	idempotencyKey string,
) (DecisionReceipt, bool, error) {
	return controller.store.Decide(ctx, DecideRequest{
		ID: id, OpportunityID: opportunityID, Assessment: assessment,
		Procedure:          controller.procedure,
		ProcedurePublicKey: controller.procedurePublicKey,
		Alternatives:       alternatives, IdempotencyKey: idempotencyKey,
	})
}

func cycleCoverage(kind CadenceKind) ([]contracts.DepartmentKind, []string) {
	departments := []contracts.DepartmentKind{contracts.DepartmentExecutive}
	capabilities := []string{"decision.portfolio"}
	switch kind {
	case CadenceDiscovery:
		departments = append(departments, contracts.DepartmentResearch)
		capabilities = append(capabilities, "market.research", "opportunity.intake")
	case CadencePortfolio:
		departments = append(departments, contracts.DepartmentAccounting,
			contracts.DepartmentLegal, contracts.DepartmentResearch)
		capabilities = append(capabilities, "capital.analysis", "evidence.review", "legal.review")
	case CadenceCapital:
		departments = append(departments, contracts.DepartmentAccounting, contracts.DepartmentLegal)
		capabilities = append(capabilities, "capital.analysis", "risk.review")
	case CadenceProduct:
		departments = append(departments, contracts.DepartmentDeveloper, contracts.DepartmentResearch)
		capabilities = append(capabilities, "product.review", "source.review")
	case CadenceCommercial:
		departments = append(departments, contracts.DepartmentAccounting, contracts.DepartmentMarketing)
		capabilities = append(capabilities, "commercial.review", "customer.outcome.read")
	case CadenceOperations:
		departments = append(departments, contracts.DepartmentBackOffice, contracts.DepartmentDeveloper)
		capabilities = append(capabilities, "operations.review", "reliability.review")
	case CadenceLearning:
		departments = append(departments, contracts.DepartmentAccounting,
			contracts.DepartmentResearch)
		capabilities = append(capabilities, "learning.review", "measurement.review")
	}
	slices.Sort(departments)
	slices.Sort(capabilities)
	return departments, capabilities
}
