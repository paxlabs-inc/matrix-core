// Package pricing resolves manifest pricing plans for quotes and charges.
package pricing

import (
	"context"
	"fmt"
	"math/big"

	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/manifest"
	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

// Plan is a resolved pricing plan for one operation. A plan carries a USDX
// denomination (unit_price_usdx/min_charge_usdx decimal strings), a legacy wei
// denomination, or both during the rail migration.
type Plan struct {
	Model         string
	Unit          string
	UnitPriceWei  string
	MinChargeWei  string
	UnitPriceUSDX string
	MinChargeUSDX string
	Version       int
}

// HasUSDX reports whether the plan is USDX-denominated.
func (p Plan) HasUSDX() bool { return p.UnitPriceUSDX != "" && p.MinChargeUSDX != "" }

// Service loads pricing plans for a service from the store.
type Service struct {
	store *store.Store
}

// New returns a pricing service.
func New(st *store.Store) *Service {
	return &Service{store: st}
}

// PlanForOperation returns the active plan for an operation name.
func (s *Service) PlanForOperation(ctx context.Context, serviceID, operation string) (Plan, error) {
	row, err := s.store.GetServiceByID(ctx, serviceID)
	if err != nil {
		return Plan{}, err
	}
	m, err := manifest.Parse(row.Manifest)
	if err != nil {
		return Plan{}, err
	}
	var found *manifest.Pricing
	for i := range m.Pricing {
		if m.Pricing[i].Operation == operation {
			found = &m.Pricing[i]
			break
		}
	}
	if found == nil {
		return Plan{}, fmt.Errorf("pricing: unknown operation %q", operation)
	}
	version := 1
	plans, err := s.store.PricingByService(ctx, serviceID)
	if err == nil && len(plans) > 0 {
		version = plans[0].Version
	}
	return Plan{
		Model:         found.Model,
		Unit:          found.Unit,
		UnitPriceWei:  found.PriceWei,
		MinChargeWei:  found.MinChargeWei,
		UnitPriceUSDX: found.UnitPriceUSDX,
		MinChargeUSDX: found.MinChargeUSDX,
		Version:       version,
	}, nil
}

// UnitsFor resolves the billable unit count for a plan and an estimate.
func UnitsFor(plan Plan, estimatedUnits string) (*big.Int, error) {
	units, err := pricingmath.ParseUnits(estimatedUnits)
	if err != nil {
		return nil, err
	}
	switch plan.Model {
	case "per_call":
		return big.NewInt(1), nil
	case "per_unit", "per_second":
		return units, nil
	default:
		return nil, fmt.Errorf("pricing: unsupported model %q", plan.Model)
	}
}

// QuoteUSDX computes the max charge in micro-USDX for estimated units. It
// errors when the plan carries no USDX denomination.
func (s *Service) QuoteUSDX(ctx context.Context, serviceID, operation, estimatedUnits string) (Plan, int64, error) {
	plan, err := s.PlanForOperation(ctx, serviceID, operation)
	if err != nil {
		return Plan{}, 0, err
	}
	if !plan.HasUSDX() {
		return Plan{}, 0, fmt.Errorf("pricing: operation %q has no USDX pricing", operation)
	}
	units, err := UnitsFor(plan, estimatedUnits)
	if err != nil {
		return Plan{}, 0, err
	}
	charge, err := pricingmath.ChargeUSDX(plan.UnitPriceUSDX, plan.MinChargeUSDX, units)
	if err != nil {
		return Plan{}, 0, err
	}
	return plan, charge, nil
}

// Quote computes max charge for estimated units.
func (s *Service) Quote(ctx context.Context, serviceID, operation, estimatedUnits string) (Plan, *big.Int, error) {
	plan, err := s.PlanForOperation(ctx, serviceID, operation)
	if err != nil {
		return Plan{}, nil, err
	}
	units, err := pricingmath.ParseUnits(estimatedUnits)
	if err != nil {
		return Plan{}, nil, err
	}
	switch plan.Model {
	case "per_call":
		units = big.NewInt(1)
	case "per_unit", "per_second":
		// units from request
	default:
		return Plan{}, nil, fmt.Errorf("pricing: unsupported model %q", plan.Model)
	}
	charge, err := pricingmath.Charge(plan.UnitPriceWei, plan.MinChargeWei, units)
	if err != nil {
		return Plan{}, nil, err
	}
	return plan, charge, nil
}
