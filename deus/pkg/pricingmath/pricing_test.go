package pricingmath_test

import (
	"math/big"
	"testing"

	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

func TestChargePerCallFloor(t *testing.T) {
	units := big.NewInt(1)
	got, err := pricingmath.Charge("200000000000000", "200000000000000", units)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "200000000000000" {
		t.Fatalf("got %s", got)
	}
}

func TestChargePerUnitMinFloor(t *testing.T) {
	units := big.NewInt(0)
	got, err := pricingmath.Charge("100", "500", units)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "500" {
		t.Fatalf("got %s", got)
	}
}

func TestChargeMultiply(t *testing.T) {
	units := big.NewInt(3)
	got, err := pricingmath.Charge("100", "0", units)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "300" {
		t.Fatalf("got %s", got)
	}
}
