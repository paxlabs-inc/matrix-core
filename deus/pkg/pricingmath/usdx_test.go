package pricingmath_test

import (
	"math/big"
	"testing"

	"github.com/paxlabs-inc/deus/pkg/pricingmath"
)

func TestParseUSDX(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0.000001", 1, true},
		{"1", 1_000_000, true},
		{"1.5", 1_500_000, true},
		{".5", 500_000, true},
		{"0.000200", 200, true},
		{"9223372036854.775807", 9223372036854775807, true}, // int64 max exactly
		{"9223372036854.775808", 0, false},                  // int64 max + 1 micro
		{"0.0000001", 0, false},                             // 7 dp
		{"-1", 0, false},                                    // signs rejected for prices
		{"+1", 0, false},
		{"1e6", 0, false},
		{"", 0, false},
		{".", 0, false},
		{"1.2.3", 0, false},
	}
	for _, c := range cases {
		got, err := pricingmath.ParseUSDX(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Fatalf("ParseUSDX(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Fatalf("ParseUSDX(%q) accepted, want error", c.in)
		}
	}
}

func TestFormatUSDXRoundTrip(t *testing.T) {
	for _, micro := range []int64{0, 1, 200, 999_999, 1_000_000, 31_500, 9223372036854775807} {
		s := pricingmath.FormatUSDX(micro)
		back, err := pricingmath.ParseUSDX(s)
		if err != nil || back != micro {
			t.Fatalf("round trip %d -> %q -> %d, %v", micro, s, back, err)
		}
	}
	if got := pricingmath.FormatUSDX(31_500); got != "0.031500" {
		t.Fatalf("FormatUSDX(31500) = %q, want 0.031500", got)
	}
}

func TestChargeUSDX(t *testing.T) {
	// Min-charge floor.
	got, err := pricingmath.ChargeUSDX("0.000100", "0.000500", big.NewInt(0))
	if err != nil || got != 500 {
		t.Fatalf("floor charge = %d, %v; want 500", got, err)
	}
	// Multiply.
	got, err = pricingmath.ChargeUSDX("0.000100", "0", big.NewInt(3))
	if err != nil || got != 300 {
		t.Fatalf("multiply charge = %d, %v; want 300", got, err)
	}
	// Per-call price exactly at 6dp resolution.
	got, err = pricingmath.ChargeUSDX("0.000001", "0.000001", big.NewInt(1))
	if err != nil || got != 1 {
		t.Fatalf("1-micro charge = %d, %v; want 1", got, err)
	}
	// Overflow rejected.
	if _, err := pricingmath.ChargeUSDX("9223372036854.775807", "0", big.NewInt(2)); err == nil {
		t.Fatal("overflowing charge accepted")
	}
	// Negative units rejected.
	if _, err := pricingmath.ChargeUSDX("1", "0", big.NewInt(-1)); err == nil {
		t.Fatal("negative units accepted")
	}
}
