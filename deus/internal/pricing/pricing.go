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

// Plan is a resolved pricing plan for one operation.
type Plan struct {
	Model        string
	Unit         string
	UnitPriceWei string
	MinChargeWei string
	Version      int
}

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
		Model:        found.Model,
		Unit:         found.Unit,
		UnitPriceWei: found.PriceWei,
		MinChargeWei: found.MinChargeWei,
		Version:      version,
	}, nil
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
