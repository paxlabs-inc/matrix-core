// Package pricingmath provides pure wei pricing calculations (docs/08-payments-billing.md §8.4).
package pricingmath

import (
	"fmt"
	"math/big"
)

// Charge computes price_wei = unit_price * units with min_charge floor.
func Charge(unitPriceWei, minChargeWei string, units *big.Int) (*big.Int, error) {
	if units == nil || units.Sign() < 0 {
		return nil, fmt.Errorf("pricingmath: units must be non-negative")
	}
	unitPrice, ok := new(big.Int).SetString(unitPriceWei, 10)
	if !ok {
		return nil, fmt.Errorf("pricingmath: invalid unit_price_wei")
	}
	minCharge, ok := new(big.Int).SetString(minChargeWei, 10)
	if !ok {
		return nil, fmt.Errorf("pricingmath: invalid min_charge_wei")
	}
	raw := new(big.Int).Mul(unitPrice, units)
	if raw.Cmp(minCharge) < 0 {
		return new(big.Int).Set(minCharge), nil
	}
	return raw, nil
}

// ParseUnits parses a decimal string into big.Int wei/units.
func ParseUnits(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("pricingmath: invalid units %q", s)
	}
	if n.Sign() < 0 {
		return nil, fmt.Errorf("pricingmath: units must be non-negative")
	}
	return n, nil
}

// FormatWei returns a decimal string for a wei amount.
func FormatWei(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.Text(10)
}
