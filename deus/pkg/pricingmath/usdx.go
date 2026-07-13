package pricingmath

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// MicroPerUSDX is the fixed-point scale for USDX amounts: prices and charges
// are integer counts of micro-USDX (1 USDX = 1_000_000 micro-USDX, 6 dp),
// matching how LayerX settles. Integer math avoids float drift on money.
const MicroPerUSDX int64 = 1_000_000

// ParseUSDX parses a non-negative decimal USDX string (up to 6 fractional
// digits) into micro-USDX. Strict: rejects signs, trailing garbage, excess
// precision, and int64 overflow — this parses money.
func ParseUSDX(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("pricingmath: empty usdx amount")
	}
	wholeStr, fracStr, hasFrac := strings.Cut(s, ".")
	if wholeStr == "" && (!hasFrac || fracStr == "") {
		return 0, fmt.Errorf("pricingmath: invalid usdx amount %q", s)
	}
	var whole int64
	if wholeStr != "" {
		if !allDigits(wholeStr) {
			return 0, fmt.Errorf("pricingmath: invalid usdx amount %q", s)
		}
		w, err := strconv.ParseInt(wholeStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("pricingmath: usdx amount %q out of range", s)
		}
		whole = w
	}
	var frac int64
	if hasFrac && fracStr != "" {
		if len(fracStr) > 6 {
			return 0, fmt.Errorf("pricingmath: usdx amount %q exceeds 6 dp", s)
		}
		if !allDigits(fracStr) {
			return 0, fmt.Errorf("pricingmath: invalid usdx amount %q", s)
		}
		f, err := strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("pricingmath: invalid usdx amount %q", s)
		}
		for i := len(fracStr); i < 6; i++ {
			f *= 10
		}
		frac = f
	}
	if whole > (math.MaxInt64-frac)/MicroPerUSDX {
		return 0, fmt.Errorf("pricingmath: usdx amount %q out of range", s)
	}
	return whole*MicroPerUSDX + frac, nil
}

// FormatUSDX renders micro-USDX as a normalized 6dp decimal USDX string.
func FormatUSDX(micro int64) string {
	neg := micro < 0
	if neg {
		micro = -micro
	}
	s := fmt.Sprintf("%d.%06d", micro/MicroPerUSDX, micro%MicroPerUSDX)
	if neg {
		s = "-" + s
	}
	return s
}

// ChargeUSDX computes charge_micro = unit_price * units with a min_charge
// floor, all in micro-USDX. Inputs are decimal USDX strings (manifest
// unit_price_usdx / min_charge_usdx); the multiply runs in big.Int and any
// result that does not fit int64 micro-USDX is rejected.
func ChargeUSDX(unitPriceUSDX, minChargeUSDX string, units *big.Int) (int64, error) {
	if units == nil || units.Sign() < 0 {
		return 0, fmt.Errorf("pricingmath: units must be non-negative")
	}
	unitPrice, err := ParseUSDX(unitPriceUSDX)
	if err != nil {
		return 0, err
	}
	minCharge, err := ParseUSDX(minChargeUSDX)
	if err != nil {
		return 0, err
	}
	raw := new(big.Int).Mul(big.NewInt(unitPrice), units)
	if raw.Cmp(big.NewInt(minCharge)) < 0 {
		return minCharge, nil
	}
	if !raw.IsInt64() {
		return 0, fmt.Errorf("pricingmath: usdx charge out of range")
	}
	return raw.Int64(), nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
