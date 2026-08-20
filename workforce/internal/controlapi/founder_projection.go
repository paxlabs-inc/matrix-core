package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/founderprojection"
)

func (service *Service) CaptureFounderProjection(
	ctx context.Context,
	principal Principal,
	draft founderprojection.CaptureDraft,
) (founderprojection.CurrentReceipt, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return founderprojection.CurrentReceipt{}, err
	}
	service.operatingStoresMu.RLock()
	store := service.founderProjection
	service.operatingStoresMu.RUnlock()
	if store == nil {
		return founderprojection.CurrentReceipt{}, fmt.Errorf("controlapi: founder projection capture is unavailable")
	}
	receipt, err := store.Capture(ctx, draft)
	if err != nil {
		return founderprojection.CurrentReceipt{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:founder-projection:" + receipt.ReceiptID + ":" + fmt.Sprint(receipt.Version),
		OrganizationID: principal.OrganizationID,
		Type:           "founder.projection.captured", ResourceKind: "founder-projection",
		ResourceID: receipt.ReceiptID, ResourceVersion: receipt.Version,
		VerifiedCompletion: true,
		Fields: map[string]any{
			"initiative_id": receipt.InitiativeID, "snapshot_hash": receipt.SnapshotHash,
			"snapshot_cursor": receipt.SnapshotCursor, "fresh_until": receipt.FreshUntil,
			"process_id": receipt.Process.ProcessID, "wake_id": receipt.Process.WakeID,
		},
	})
	return receipt, err
}
