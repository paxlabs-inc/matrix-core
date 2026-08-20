package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/productexecution"
)

func (service *Service) authorizeProductExecution(
	ctx context.Context,
	principal Principal,
) (*productexecution.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	return service.productExecutionStore()
}

func (service *Service) StartProductExecution(
	ctx context.Context,
	principal Principal,
	request productexecution.StartRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.Start(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.started")
}

func (service *Service) publishProductExecution(
	ctx context.Context,
	principal Principal,
	view productexecution.View,
	kind string,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:" + kind + ":" + string(view.ID) + ":" + fmt.Sprint(view.Version),
		OrganizationID: principal.OrganizationID, Type: kind,
		ResourceKind: "product-execution", ResourceID: string(view.ID),
		ResourceVersion: view.Version, VerifiedCompletion: view.Phase == productexecution.PhaseLaunched,
		Fields: map[string]any{
			"phase": view.Phase, "initiative_id": view.InitiativeID,
			"project_id": view.ProjectID, "workspace_id": view.WorkspaceID,
			"checkpoint_version": view.CheckpointVersion,
		},
	})
	return err
}

func (service *Service) CompleteProductExecutionProduct(
	ctx context.Context, principal Principal, request productexecution.ReceiptRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteProduct(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.product.completed")
}

func (service *Service) CompleteProductExecutionDesign(
	ctx context.Context, principal Principal, request productexecution.CompleteDesignRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteDesign(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.design.completed")
}

func (service *Service) CompleteProductExecutionBuild(
	ctx context.Context, principal Principal, request productexecution.CompleteStageRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteBuild(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.build.completed")
}

func (service *Service) CompleteProductExecutionVerification(
	ctx context.Context, principal Principal, request productexecution.CompleteStageRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteVerification(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.verification.completed")
}

func (service *Service) CompleteProductExecutionDeploymentPreparation(
	ctx context.Context, principal Principal, request productexecution.ReceiptRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteDeploymentPreparation(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.deployment.prepared")
}

func (service *Service) DeployProductExecution(
	ctx context.Context, principal Principal, request productexecution.DeploymentRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.ExecuteDeployment(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.deployment.dispatched")
}

func (service *Service) ReconcileProductExecutionDeployment(
	ctx context.Context, principal Principal, request productexecution.DeploymentRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.ReconcileDeployment(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.deployment.reconciled")
}

func (service *Service) CompleteProductExecutionLaunch(
	ctx context.Context, principal Principal, request productexecution.CompleteLaunchRequest,
) (productexecution.View, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.View{}, err
	}
	view, err := store.CompleteLaunch(ctx, request)
	if err != nil {
		return productexecution.View{}, err
	}
	return view, service.publishProductExecution(ctx, principal, view, "product.execution.launched")
}

func (service *Service) RecoverProductExecution(
	ctx context.Context, principal Principal, request productexecution.ResumeRequest,
) (productexecution.Recovery, error) {
	store, err := service.authorizeProductExecution(ctx, principal)
	if err != nil {
		return productexecution.Recovery{}, err
	}
	recovery, err := store.Recover(ctx, request)
	if err != nil {
		return productexecution.Recovery{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID: "event:product.execution.recovered:" + string(recovery.Execution.ID) + ":" +
			fmt.Sprint(recovery.Execution.Version),
		OrganizationID:     principal.OrganizationID,
		Type:               "product.execution.recovered",
		ResourceKind:       "product-execution",
		ResourceID:         string(recovery.Execution.ID),
		ResourceVersion:    recovery.Execution.Version,
		VerifiedCompletion: false,
		Fields: map[string]any{
			"phase":                   recovery.Execution.Phase,
			"initiative_id":           recovery.Execution.InitiativeID,
			"project_id":              recovery.Execution.ProjectID,
			"workspace_id":            recovery.Execution.WorkspaceID,
			"checkpoint_version":      recovery.Execution.CheckpointVersion,
			"requires_reconciliation": recovery.RequiresReconcile,
		},
	})
	return recovery, err
}
