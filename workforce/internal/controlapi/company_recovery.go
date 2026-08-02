package controlapi

import (
	"context"
	"fmt"

	"matrix/workforce/internal/companyrecovery"
	"matrix/workforce/internal/contracts"
)

type UsageCommitRequest struct {
	ReservationID companyrecovery.ReservationID `json:"reservation_id"`
	ActualUnits   uint64                        `json:"actual_units"`
}

type UsageReleaseRequest struct {
	ReservationID companyrecovery.ReservationID `json:"reservation_id"`
	ReasonCode    string                        `json:"reason_code"`
}

type ShutdownRequest struct {
	ShutdownID companyrecovery.ShutdownID `json:"shutdown_id"`
	ReasonCode string                     `json:"reason_code"`
}

type ShutdownCompletionRequest struct {
	ShutdownID companyrecovery.ShutdownID `json:"shutdown_id"`
}

type RestoreAcknowledgementRequest struct {
	Authorization companyrecovery.RestoreAuthorization `json:"authorization"`
	EvidenceHash  contracts.ContentHash                `json:"evidence_hash"`
}

type RecoveryCircuitRequest struct {
	Scope      companyrecovery.ScopeRef     `json:"scope"`
	Resource   companyrecovery.ResourceKind `json:"resource"`
	ReasonCode string                       `json:"reason_code"`
}

func (service *Service) authorizedCompanyRecovery(
	ctx context.Context,
	principal Principal,
) (*companyrecovery.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	service.operatingStoresMu.RLock()
	store := service.companyRecovery
	service.operatingStoresMu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("controlapi: company recovery runtime is unavailable")
	}
	return store, nil
}

func (service *Service) RegisterCompanyLimitPolicy(
	ctx context.Context,
	principal Principal,
	value companyrecovery.LimitPolicy,
) (companyrecovery.LimitPolicy, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.LimitPolicy{}, false, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.LimitPolicy{}, false, ErrUnauthorized
	}
	replayed, err := store.RegisterLimitPolicy(ctx, value)
	if err != nil {
		return companyrecovery.LimitPolicy{}, false, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:                 fmt.Sprintf("event:company-limit-policy:%s:%d", value.Body.ID, value.Body.Version),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.limit-policy.registered",
		ResourceKind:       "company-limit-policy",
		ResourceID:         string(value.Body.ID),
		ResourceVersion:    value.Body.Version,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"scope_kind": value.Body.Scope.Kind,
			"scope_id":   value.Body.Scope.ID,
			"resource":   value.Body.Resource,
			"soft_limit": value.Body.SoftLimit,
			"hard_limit": value.Body.HardLimit,
			"replayed":   replayed,
		},
	})
	return value, replayed, err
}

func (service *Service) RegisterCompanyRecoveryPolicy(
	ctx context.Context,
	principal Principal,
	value companyrecovery.RecoveryPolicy,
) (companyrecovery.RecoveryPolicy, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.RecoveryPolicy{}, false, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.RecoveryPolicy{}, false, ErrUnauthorized
	}
	replayed, err := store.RegisterRecoveryPolicy(ctx, value)
	if err != nil {
		return companyrecovery.RecoveryPolicy{}, false, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:                 fmt.Sprintf("event:company-recovery-policy:%s:%d", value.Body.ID, value.Body.Version),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.recovery-policy.registered",
		ResourceKind:       "company-recovery-policy",
		ResourceID:         string(value.Body.ID),
		ResourceVersion:    value.Body.Version,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"backup_interval": value.Body.BackupInterval,
			"rpo":             value.Body.RPO,
			"rto":             value.Body.RTO,
			"pitr_required":   value.Body.PITRRequired,
			"replayed":        replayed,
		},
	})
	return value, replayed, err
}

func (service *Service) RegisterCompanyMachineKey(
	ctx context.Context,
	principal Principal,
	value companyrecovery.MachineKeyRegistration,
) (companyrecovery.MachineKeyRegistration, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.MachineKeyRegistration{}, false, err
	}
	if value.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.MachineKeyRegistration{}, false, ErrUnauthorized
	}
	replayed, err := store.RegisterMachineKey(ctx, value)
	if err != nil {
		return companyrecovery.MachineKeyRegistration{}, false, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:                 fmt.Sprintf("event:company-machine-key:%s:%d", value.Body.ID, value.Body.Version),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.machine-key.registered",
		ResourceKind:       "company-machine-key",
		ResourceID:         string(value.Body.ID),
		ResourceVersion:    value.Body.Version,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"machine_id":   value.Body.MachineID,
			"key_id":       value.Body.KeyID,
			"effective_at": value.Body.EffectiveAt,
			"expires_at":   value.Body.ExpiresAt,
			"replayed":     replayed,
		},
	})
	return value, replayed, err
}

func (service *Service) AdmitCompanyUsage(
	ctx context.Context,
	principal Principal,
	value companyrecovery.UsageRequest,
) (companyrecovery.UsageReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return companyrecovery.UsageReceipt{}, ErrUnauthorized
	}
	receipt, err := store.Admit(ctx, value)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	return receipt, service.publishUsageReceipt(ctx, principal, receipt)
}

func (service *Service) CommitCompanyUsage(
	ctx context.Context,
	principal Principal,
	request UsageCommitRequest,
) (companyrecovery.UsageReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	receipt, err := store.CommitUsage(ctx, request.ReservationID, request.ActualUnits)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	if receipt.Request.OrganizationID != principal.OrganizationID {
		return companyrecovery.UsageReceipt{}, ErrUnauthorized
	}
	return receipt, service.publishUsageReceipt(ctx, principal, receipt)
}

func (service *Service) ReleaseCompanyUsage(
	ctx context.Context,
	principal Principal,
	request UsageReleaseRequest,
) (companyrecovery.UsageReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	receipt, err := store.ReleaseUsage(ctx, request.ReservationID, request.ReasonCode)
	if err != nil {
		return companyrecovery.UsageReceipt{}, err
	}
	if receipt.Request.OrganizationID != principal.OrganizationID {
		return companyrecovery.UsageReceipt{}, ErrUnauthorized
	}
	return receipt, service.publishUsageReceipt(ctx, principal, receipt)
}

func (service *Service) RecordCompanyMetric(
	ctx context.Context,
	principal Principal,
	value companyrecovery.MetricSample,
) (companyrecovery.MetricSample, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.MetricSample{}, false, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return companyrecovery.MetricSample{}, false, ErrUnauthorized
	}
	replayed, err := store.RecordMetric(ctx, value)
	return value, replayed, err
}

func (service *Service) RecordCompanyTrace(
	ctx context.Context,
	principal Principal,
	value companyrecovery.TraceSpan,
) (companyrecovery.TraceSpan, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.TraceSpan{}, false, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return companyrecovery.TraceSpan{}, false, ErrUnauthorized
	}
	replayed, err := store.RecordTrace(ctx, value)
	return value, replayed, err
}

func (service *Service) RecordCompanyIncident(
	ctx context.Context,
	principal Principal,
	value companyrecovery.Incident,
) (companyrecovery.Incident, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.Incident{}, false, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return companyrecovery.Incident{}, false, ErrUnauthorized
	}
	replayed, err := store.RecordIncident(ctx, value)
	return value, replayed, err
}

func (service *Service) BeginCompanyShutdown(
	ctx context.Context,
	principal Principal,
	request ShutdownRequest,
) (companyrecovery.ShutdownReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.ShutdownReceipt{}, err
	}
	receipt, err := store.BeginShutdown(ctx, request.ShutdownID, request.ReasonCode)
	if err != nil {
		return companyrecovery.ShutdownReceipt{}, err
	}
	return receipt, service.publishShutdownReceipt(ctx, principal, receipt)
}

func (service *Service) CompleteCompanyShutdown(
	ctx context.Context,
	principal Principal,
	request ShutdownCompletionRequest,
) (companyrecovery.ShutdownReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.ShutdownReceipt{}, err
	}
	receipt, err := store.CompleteShutdown(ctx, request.ShutdownID)
	if err != nil {
		return companyrecovery.ShutdownReceipt{}, err
	}
	return receipt, service.publishShutdownReceipt(ctx, principal, receipt)
}

func (service *Service) CreateCompanyBackup(
	ctx context.Context,
	principal Principal,
	authorization companyrecovery.BackupAuthorization,
) (companyrecovery.RecoveryBundle, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.RecoveryBundle{}, false, err
	}
	if authorization.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.RecoveryBundle{}, false, ErrUnauthorized
	}
	bundle, replayed, err := store.CreateBackup(ctx, authorization)
	if err != nil {
		return companyrecovery.RecoveryBundle{}, false, err
	}
	return bundle, replayed, service.publishBackupManifest(ctx, principal, bundle.Manifest, replayed)
}

func (service *Service) CompanyBackup(
	ctx context.Context,
	principal Principal,
	backupID companyrecovery.BackupID,
) (companyrecovery.RecoveryBundle, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.RecoveryBundle{}, err
	}
	bundle, err := store.LoadBackup(ctx, backupID)
	if err != nil {
		return companyrecovery.RecoveryBundle{}, err
	}
	if bundle.Manifest.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.RecoveryBundle{}, ErrUnauthorized
	}
	return bundle, nil
}

type BackupImportRequest struct {
	Authorization companyrecovery.BackupAuthorization `json:"authorization"`
	Bundle        companyrecovery.RecoveryBundle      `json:"bundle"`
}

func (service *Service) ImportCompanyBackup(
	ctx context.Context,
	principal Principal,
	request BackupImportRequest,
) (bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return false, err
	}
	if request.Authorization.Body.OrganizationID != principal.OrganizationID ||
		request.Bundle.Manifest.Body.OrganizationID != principal.OrganizationID ||
		request.Bundle.Manifest.Body.TenantID != principal.TenantID {
		return false, ErrUnauthorized
	}
	replayed, err := store.ImportBackup(ctx, request.Authorization, request.Bundle)
	if err != nil {
		return false, err
	}
	return replayed, service.publishBackupManifest(
		ctx, principal, request.Bundle.Manifest, replayed,
	)
}

func (service *Service) RestoreCompanyBackup(
	ctx context.Context,
	principal Principal,
	authorization companyrecovery.RestoreAuthorization,
) (companyrecovery.RestoreReceipt, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.RestoreReceipt{}, false, err
	}
	if authorization.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.RestoreReceipt{}, false, ErrUnauthorized
	}
	receipt, replayed, err := store.Restore(ctx, authorization)
	if err != nil {
		return companyrecovery.RestoreReceipt{}, false, err
	}
	return receipt, replayed, service.publishRestoreReceipt(ctx, principal, receipt, replayed)
}

func (service *Service) AcknowledgeCompanyRestore(
	ctx context.Context,
	principal Principal,
	request RestoreAcknowledgementRequest,
) (companyrecovery.RestoreReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.RestoreReceipt{}, err
	}
	if request.Authorization.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.RestoreReceipt{}, ErrUnauthorized
	}
	receipt, err := store.AcknowledgeRestore(ctx, request.Authorization, request.EvidenceHash)
	if err != nil {
		return companyrecovery.RestoreReceipt{}, err
	}
	return receipt, service.publishRestoreReceipt(ctx, principal, receipt, true)
}

func (service *Service) ExecuteCompanyErasure(
	ctx context.Context,
	principal Principal,
	directive companyrecovery.ErasureDirective,
) (companyrecovery.ErasureReceipt, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.ErasureReceipt{}, false, err
	}
	if directive.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.ErasureReceipt{}, false, ErrUnauthorized
	}
	receipt, replayed, err := store.ExecuteErasure(ctx, directive)
	if err != nil {
		return companyrecovery.ErasureReceipt{}, false, err
	}
	return receipt, replayed, service.publishErasureReceipt(ctx, principal, receipt, replayed)
}

func (service *Service) ApplyCompanyRetention(
	ctx context.Context,
	principal Principal,
) ([]companyrecovery.ErasureReceipt, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return nil, err
	}
	receipts, err := store.ApplyRetention(ctx)
	if err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		if receipt.Body.OrganizationID != principal.OrganizationID {
			return nil, ErrUnauthorized
		}
		if err := service.publishErasureReceipt(ctx, principal, receipt, true); err != nil {
			return nil, err
		}
	}
	return receipts, nil
}

func (service *Service) CoalesceCompanyOfflineBatch(
	ctx context.Context,
	principal Principal,
	batch companyrecovery.OfflineBatch,
) (companyrecovery.OfflineReceipt, bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return companyrecovery.OfflineReceipt{}, false, err
	}
	if batch.Body.TenantID != principal.TenantID || batch.Body.OrganizationID != principal.OrganizationID {
		return companyrecovery.OfflineReceipt{}, false, ErrUnauthorized
	}
	receipt, replayed, err := store.CoalesceOfflineBatch(ctx, batch)
	if err != nil {
		return companyrecovery.OfflineReceipt{}, false, err
	}
	return receipt, replayed, service.publishOfflineReceipt(ctx, principal, receipt, replayed)
}

func (service *Service) ResolveCompanyOfflineReconciliation(
	ctx context.Context,
	principal Principal,
	resolution companyrecovery.OfflineReconciliationResolution,
) (bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return false, err
	}
	if resolution.OrganizationID != principal.OrganizationID {
		return false, ErrUnauthorized
	}
	replayed, err := store.ResolveOfflineReconciliation(ctx, resolution)
	if err != nil {
		return false, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:offline-reconciliation:" + resolution.ReconciliationID,
		OrganizationID: principal.OrganizationID,
		Type:           "company_recovery.offline_reconciliation.resolved",
		ResourceKind:   "offline-reconciliation", ResourceID: resolution.ReconciliationID,
		ResourceVersion: resolution.Version, VerifiedCompletion: true,
		Fields: map[string]any{
			"batch_id": resolution.BatchID, "machine_id": resolution.MachineID,
			"decision": resolution.Decision, "replayed": replayed,
		},
	})
	return replayed, err
}

func (service *Service) CommitCompanyRecoveryQualification(
	ctx context.Context,
	principal Principal,
	value companyrecovery.RecoveryQualification,
) (bool, error) {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return false, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return false, ErrUnauthorized
	}
	replayed, err := store.CommitRecoveryQualification(ctx, value)
	if err != nil {
		return false, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:recovery-qualification:" + string(value.ID),
		OrganizationID: principal.OrganizationID,
		Type:           "company_recovery.qualified", ResourceKind: "recovery-qualification",
		ResourceID: string(value.ID), ResourceVersion: 1, VerifiedCompletion: true,
		Fields: map[string]any{
			"recovery_policy_id": value.RecoveryPolicyID, "expires_at": value.ExpiresAt,
			"clean_restore_ready": value.CleanRestoreReady, "replayed": replayed,
		},
	})
	return replayed, err
}

func (service *Service) OpenCompanyCircuit(
	ctx context.Context,
	principal Principal,
	request RecoveryCircuitRequest,
) error {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return err
	}
	if err := store.OpenCircuit(ctx, request.Scope, request.Resource, request.ReasonCode); err != nil {
		return err
	}
	return service.publishRecoveryCircuit(ctx, principal, request, "open")
}

func (service *Service) CloseCompanyCircuit(
	ctx context.Context,
	principal Principal,
	request RecoveryCircuitRequest,
) error {
	store, err := service.authorizedCompanyRecovery(ctx, principal)
	if err != nil {
		return err
	}
	if err := store.CloseCircuit(ctx, request.Scope, request.Resource, request.ReasonCode); err != nil {
		return err
	}
	return service.publishRecoveryCircuit(ctx, principal, request, "closed")
}

func (service *Service) publishUsageReceipt(
	ctx context.Context,
	principal Principal,
	receipt companyrecovery.UsageReceipt,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:              "event:company-usage:" + string(receipt.Request.ID) + ":" + string(receipt.State),
		OrganizationID:  principal.OrganizationID,
		Type:            "company.usage." + string(receipt.State),
		ResourceKind:    "company-usage",
		ResourceID:      string(receipt.Request.ID),
		ResourceVersion: 1,
		VerifiedCompletion: receipt.State == companyrecovery.ReservationCommitted ||
			receipt.State == companyrecovery.ReservationReleased,
		Fields: map[string]any{
			"state":       receipt.State,
			"resource":    receipt.Request.Resource,
			"units":       receipt.Request.Units,
			"reason_code": receipt.ReasonCode,
		},
	})
	return err
}

func (service *Service) publishShutdownReceipt(
	ctx context.Context,
	principal Principal,
	receipt companyrecovery.ShutdownReceipt,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-shutdown:" + string(receipt.Body.ID) + ":" + string(receipt.Body.State),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.shutdown." + string(receipt.Body.State),
		ResourceKind:       "company-shutdown",
		ResourceID:         string(receipt.Body.ID),
		ResourceVersion:    1,
		VerifiedCompletion: receipt.Body.State == companyrecovery.ShutdownStopped,
		Fields: map[string]any{
			"state":                 receipt.Body.State,
			"reason_code":           receipt.Body.ReasonCode,
			"released_reservations": receipt.Body.ReleasedReservations,
			"cancelled_leases":      receipt.Body.CancelledLeases,
			"coalesced_wakes":       receipt.Body.CoalescedWakes,
			"quarantined_effects":   receipt.Body.QuarantinedEffects,
		},
	})
	return err
}

func (service *Service) publishBackupManifest(
	ctx context.Context,
	principal Principal,
	manifest companyrecovery.BackupManifest,
	replayed bool,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-backup:" + string(manifest.Body.BackupID),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.backup.completed",
		ResourceKind:       "company-backup",
		ResourceID:         string(manifest.Body.BackupID),
		ResourceVersion:    1,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"scope_kind":   manifest.Body.Scope.Kind,
			"scope_id":     manifest.Body.Scope.ID,
			"snapshot_at":  manifest.Body.SnapshotAt,
			"completed_at": manifest.Body.CompletedAt,
			"rpo":          manifest.Body.RPO,
			"rto":          manifest.Body.RTO,
			"table_count":  len(manifest.Body.Tables),
			"replayed":     replayed,
		},
	})
	return err
}

func (service *Service) publishRestoreReceipt(
	ctx context.Context,
	principal Principal,
	receipt companyrecovery.RestoreReceipt,
	replayed bool,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-restore:" + string(receipt.Body.ID) + ":" + string(receipt.Body.State),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.restore." + string(receipt.Body.State),
		ResourceKind:       "company-restore",
		ResourceID:         string(receipt.Body.ID),
		ResourceVersion:    1,
		VerifiedCompletion: receipt.Body.State == companyrecovery.RestoreReady,
		Fields: map[string]any{
			"state":                        receipt.Body.State,
			"backup_id":                    receipt.Body.BackupID,
			"restored_tables":              receipt.Body.RestoredTables,
			"restored_rows":                receipt.Body.RestoredRows,
			"cancelled_runtime_leases":     receipt.Body.CancelledRuntimeLeases,
			"invalidated_authority_leases": receipt.Body.InvalidatedAuthorityLeases,
			"coalesced_wakes":              receipt.Body.CoalescedWakes,
			"quarantined_effects":          receipt.Body.QuarantinedEffects,
			"quarantined_external_state":   receipt.Body.QuarantinedExternalState,
			"rto_status":                   receipt.Body.RTOStatus,
			"replayed":                     replayed,
		},
	})
	return err
}

func (service *Service) publishErasureReceipt(
	ctx context.Context,
	principal Principal,
	receipt companyrecovery.ErasureReceipt,
	replayed bool,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-erasure:" + string(receipt.Body.ID),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.erasure.completed",
		ResourceKind:       "company-erasure",
		ResourceID:         string(receipt.Body.ID),
		ResourceVersion:    1,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"target_kind":     receipt.Body.TargetKind,
			"target_id":       receipt.Body.TargetID,
			"class":           receipt.Body.Class,
			"action":          receipt.Body.Action,
			"destroyed_keys":  receipt.Body.DestroyedKeys,
			"deleted_objects": receipt.Body.DeletedObjects,
			"replayed":        replayed,
		},
	})
	return err
}

func (service *Service) publishOfflineReceipt(
	ctx context.Context,
	principal Principal,
	receipt companyrecovery.OfflineReceipt,
	replayed bool,
) error {
	complete := true
	for _, result := range receipt.Body.Results {
		if result.Disposition == companyrecovery.OfflineConflict ||
			result.Disposition == companyrecovery.OfflineNeedsReconciliation {
			complete = false
			break
		}
	}
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-offline:" + string(receipt.Body.BatchID),
		OrganizationID:     principal.OrganizationID,
		Type:               "company.offline.coalesced",
		ResourceKind:       "company-offline-batch",
		ResourceID:         string(receipt.Body.BatchID),
		ResourceVersion:    receipt.Body.Sequence,
		VerifiedCompletion: complete,
		Fields: map[string]any{
			"machine_id":   receipt.Body.MachineID,
			"sequence":     receipt.Body.Sequence,
			"result_count": len(receipt.Body.Results),
			"replayed":     replayed,
		},
	})
	return err
}

func (service *Service) publishRecoveryCircuit(
	ctx context.Context,
	principal Principal,
	request RecoveryCircuitRequest,
	state string,
) error {
	_, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:company-recovery-circuit:" + string(request.Scope.Kind) + ":" + request.Scope.ID + ":" + string(request.Resource) + ":" + state,
		OrganizationID:     principal.OrganizationID,
		Type:               "company.recovery-circuit." + state,
		ResourceKind:       "company-recovery-circuit",
		ResourceID:         string(request.Scope.Kind) + ":" + request.Scope.ID + ":" + string(request.Resource),
		ResourceVersion:    1,
		VerifiedCompletion: state == "closed",
		Fields: map[string]any{
			"scope_kind":  request.Scope.Kind,
			"scope_id":    request.Scope.ID,
			"resource":    request.Resource,
			"state":       state,
			"reason_code": request.ReasonCode,
		},
	})
	return err
}
