package productexecution

import (
	"context"
	"fmt"

	"centra/workforce/internal/contracts"
)

// AdvanceReceipt routes a committed, independently verified Workforce receipt
// to the durable product saga when that stage has no additional typed gate
// payload. Stages that require Product records or lifecycle evidence remain
// closed until those exact records are supplied through their typed methods.
func (store *Store) AdvanceReceipt(
	ctx context.Context,
	intentID contracts.IntentID,
	receiptID contracts.ReceiptID,
) (View, bool, error) {
	view, binding, err := store.LoadByIntent(ctx, intentID)
	if err != nil {
		return View{}, false, err
	}
	request := ReceiptRequest{
		ExecutionID:    view.ID,
		ReceiptID:      receiptID,
		IdempotencyKey: fmt.Sprintf("product-execution:%s:%s:receipt", view.ID, binding.Stage),
	}
	switch binding.Stage {
	case StageProduct:
		advanced, err := store.CompleteProduct(ctx, request)
		return advanced, true, err
	case StageDeployment:
		advanced, err := store.CompleteDeploymentPreparation(ctx, request)
		return advanced, true, err
	default:
		return view, false, nil
	}
}
