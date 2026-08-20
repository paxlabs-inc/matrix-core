package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/companyruntime"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/productexecution"
)

func (service *Service) PreviewCompanyStart(
	ctx context.Context,
	principal Principal,
	draft companyruntime.StartDraft,
) (companyruntime.StartConfiguration, error) {
	if service.companyRuntime == nil {
		return companyruntime.StartConfiguration{}, fmt.Errorf("controlapi: company runtime is unavailable")
	}
	_, keyID, err := service.currentCompanyAuthority(ctx, principal)
	if err != nil {
		return companyruntime.StartConfiguration{}, err
	}
	if _, err := service.commandKey(ctx, principal, keyID); err != nil {
		return companyruntime.StartConfiguration{}, ErrUnauthorized
	}
	return service.companyRuntime.Store().PreviewStart(ctx, draft)
}

func (service *Service) StartCompany(
	ctx context.Context,
	principal Principal,
	configuration companyruntime.StartConfiguration,
) (companyruntime.StartResult, error) {
	if service.companyRuntime == nil {
		return companyruntime.StartResult{}, fmt.Errorf("controlapi: company runtime is unavailable")
	}
	if _, err := service.commandKey(ctx, principal, configuration.Signature.KeyID); err != nil {
		return companyruntime.StartResult{}, ErrUnauthorized
	}
	result, err := service.companyRuntime.Store().Activate(ctx, configuration)
	if err != nil {
		return companyruntime.StartResult{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:company-runtime:" + string(principal.OrganizationID) + ":" + fmt.Sprint(configuration.Version),
		OrganizationID: principal.OrganizationID, Type: "company.runtime.started",
		ResourceKind: "company-runtime", ResourceID: configuration.ID,
		ResourceVersion: configuration.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"state": "active", "procedure_id": configuration.Procedure.ID,
			"cadence_count": len(configuration.Cadences), "expires_at": configuration.ExpiresAt,
		},
	})
	if err != nil {
		return companyruntime.StartResult{}, err
	}
	return result, nil
}

func (service *Service) CurrentCompanyRuntime(
	ctx context.Context,
	principal Principal,
) (companyruntime.StartConfiguration, error) {
	if service.companyRuntime == nil {
		return companyruntime.StartConfiguration{}, fmt.Errorf("controlapi: company runtime is unavailable")
	}
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return companyruntime.StartConfiguration{}, err
	}
	return service.companyRuntime.Store().LoadCurrent(ctx)
}

func (service *Service) FundCompanyInitiative(
	ctx context.Context,
	principal Principal,
	request companyruntime.FundingRequest,
) (companyruntime.FundingResult, error) {
	if service.companyRuntime == nil {
		return companyruntime.FundingResult{}, fmt.Errorf("controlapi: company runtime is unavailable")
	}
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return companyruntime.FundingResult{}, err
	}
	var productStore *productexecution.Store
	if request.ProductExecution != nil {
		var err error
		productStore, err = service.productExecutionStore()
		if err != nil {
			return companyruntime.FundingResult{}, err
		}
	}
	result, err := service.companyRuntime.FundInitiative(ctx, request)
	if err != nil {
		return companyruntime.FundingResult{}, err
	}
	var productView *productexecution.View
	if result.State == "plan_committed" && request.ProductExecution != nil {
		authority, authorityErr := service.companyRuntime.CompanyAuthority(ctx)
		if authorityErr != nil {
			return companyruntime.FundingResult{}, authorityErr
		}
		stages := make([]productexecution.StagePlan, len(request.ProductExecution.Stages))
		for index, stage := range request.ProductExecution.Stages {
			stages[index] = productexecution.StagePlan{
				Stage: productexecution.Stage(stage.Stage), PlanNodeID: stage.PlanNodeID,
				NeedID: stage.NeedID,
			}
		}
		started, startErr := productStore.Start(ctx, productexecution.StartRequest{
			SchemaVersion:    productexecution.SchemaVersion,
			ID:               productexecution.ExecutionID(request.ProductExecution.ExecutionID),
			OrganizationID:   principal.OrganizationID,
			InitiativeID:     result.Checkpoint.InitiativeID,
			ProjectID:        request.ProductExecution.ProjectID,
			WorkspaceID:      request.ProductExecution.WorkspaceID,
			CompanyAuthority: authority, PortfolioDecision: result.Decision,
			SquadRequirement: request.ProductExecution.SquadRequirement,
			Stages:           stages, HandoffID: request.ProductExecution.HandoffID,
			BaselineSource:       request.ProductExecution.BaselineSource,
			BrainViewDigest:      request.ProductExecution.BrainViewDigest,
			CompanyStateRecordID: request.ProductExecution.CompanyStateRecordID,
			IdempotencyKey:       request.ProductExecution.IdempotencyKey,
			CreatedAt:            result.Checkpoint.UpdatedAt,
		})
		if startErr != nil {
			return companyruntime.FundingResult{}, startErr
		}
		productView = &started
	}
	resourceID := string(request.OpportunityID)
	resourceKind := "portfolio-decision"
	resourceVersion := uint64(1)
	fields := map[string]any{
		"decision": result.Decision.Decision, "state": result.State,
		"score_bps":          result.Decision.ScoreBPS,
		"capital_microunits": result.Decision.CapitalImpactMicrounits,
	}
	if result.State == "plan_committed" {
		resourceID = result.Plan.ID
		resourceKind = "initiative-plan"
		resourceVersion = result.Plan.Version
		fields["initiative_id"] = result.Plan.InitiativeID
		fields["lifecycle_state"] = result.Checkpoint.State
		fields["work_order_count"] = len(result.Plan.Nodes)
		if productView != nil {
			fields["product_execution_id"] = productView.ID
			fields["product_execution_phase"] = productView.Phase
			fields["squad_assignment_id"] = productView.SquadAssignmentID
		}
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:company-funding:" + request.FundingID,
		OrganizationID: principal.OrganizationID, Type: "company.portfolio.decided",
		ResourceKind: resourceKind, ResourceID: resourceID, ResourceVersion: resourceVersion,
		VerifiedCompletion: false, ReceiptID: contracts.ReceiptID(result.Checkpoint.LastReceiptID), Fields: fields,
	})
	if err != nil {
		return companyruntime.FundingResult{}, err
	}
	return result, nil
}
