package hosting

import (
	"context"
	"fmt"
	"math/big"

	"github.com/paxlabs-inc/deus/internal/store"
)

// Budget enforces aggregate free-hosting ceilings.
type Budget struct {
	limits Limits
	store  *store.Store
}

// NewBudget wires budget checks.
func NewBudget(st *store.Store, limits Limits) *Budget {
	return &Budget{limits: limits, store: st}
}

// AllowAlwaysWarm returns an error when always-warm capacity is exhausted or kill-switch tripped.
func (b *Budget) AllowAlwaysWarm(ctx context.Context) error {
	if b.limits.KillSwitch {
		return fmt.Errorf("hosting: kill-switch active; always-warm allocations refused")
	}
	n, err := b.store.CountAlwaysWarmDeployments(ctx)
	if err != nil {
		return err
	}
	if n >= b.limits.MaxAlwaysWarm {
		return fmt.Errorf("hosting: always-warm budget exhausted (%d/%d)", n, b.limits.MaxAlwaysWarm)
	}
	return nil
}

// AllowNewDeployment checks kill-switch for scale-to-zero deploys.
func (b *Budget) AllowNewDeployment(ctx context.Context, alwaysWarm bool) error {
	if alwaysWarm {
		return b.AllowAlwaysWarm(ctx)
	}
	if b.limits.KillSwitch {
		return fmt.Errorf("hosting: kill-switch active")
	}
	return nil
}

// BudgetWei returns configured aggregate budget as big.Int.
func (b *Budget) BudgetWei() *big.Int {
	v, ok := new(big.Int).SetString(b.limits.BudgetPAXWei, 10)
	if !ok {
		return big.NewInt(0)
	}
	return v
}
