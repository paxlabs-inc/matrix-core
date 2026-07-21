package crossverse

import (
	"fmt"
	"math/big"
	"strings"
)

type Rounding int

const (
	RoundExact Rounding = iota
	RoundTrunc
	RoundHalfAwayFromZero
)

const (
	maxDecimalInputLen = 64
	maxDecimalExponent = 30
)

var maxInt64 = big.NewInt(1<<63 - 1)
var minInt64 = big.NewInt(-(1 << 63))

func ParseScaled(input string, scale int64, rounding Rounding) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("crossverse decimal is empty")
	}
	if len(s) > maxDecimalInputLen {
		return 0, fmt.Errorf("crossverse decimal %q exceeds %d characters", s, maxDecimalInputLen)
	}
	if scale <= 0 {
		return 0, fmt.Errorf("crossverse scale %d is invalid", scale)
	}
	neg := false
	rest := s
	switch {
	case strings.HasPrefix(rest, "-"):
		neg = true
		rest = rest[1:]
	case strings.HasPrefix(rest, "+"):
		rest = rest[1:]
	}
	mantissaPart := rest
	expPart := ""
	hasExp := false
	if i := strings.IndexAny(rest, "eE"); i >= 0 {
		mantissaPart = rest[:i]
		expPart = rest[i+1:]
		hasExp = true
	}
	intPart := mantissaPart
	fracPart := ""
	if i := strings.IndexByte(mantissaPart, '.'); i >= 0 {
		intPart = mantissaPart[:i]
		fracPart = mantissaPart[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("crossverse decimal %q has multiple points", s)
		}
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("crossverse decimal %q has no digits", s)
	}
	mantissa := new(big.Int)
	for _, part := range []string{intPart, fracPart} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("crossverse decimal %q has invalid digit %q", s, c)
			}
			mantissa.Mul(mantissa, big.NewInt(10))
			mantissa.Add(mantissa, big.NewInt(int64(c-'0')))
		}
	}
	exponent := -len(fracPart)
	if hasExp {
		expNeg := false
		digits := expPart
		switch {
		case strings.HasPrefix(digits, "-"):
			expNeg = true
			digits = digits[1:]
		case strings.HasPrefix(digits, "+"):
			digits = digits[1:]
		}
		if digits == "" {
			return 0, fmt.Errorf("crossverse decimal %q has an empty exponent", s)
		}
		expVal := 0
		for _, c := range digits {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("crossverse decimal %q has invalid exponent digit %q", s, c)
			}
			expVal = expVal*10 + int(c-'0')
			if expVal > 10*maxDecimalExponent {
				return 0, fmt.Errorf("crossverse decimal %q exponent is out of range", s)
			}
		}
		if expNeg {
			expVal = -expVal
		}
		exponent += expVal
	}
	if exponent > maxDecimalExponent || exponent < -maxDecimalExponent {
		return 0, fmt.Errorf("crossverse decimal %q exponent is out of range", s)
	}
	if neg {
		mantissa.Neg(mantissa)
	}
	num := new(big.Int).Mul(mantissa, big.NewInt(scale))
	den := big.NewInt(1)
	if exponent >= 0 {
		num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil))
	} else {
		den = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exponent)), nil)
	}
	quo := new(big.Int)
	rem := new(big.Int)
	quo.QuoRem(num, den, rem)
	if rem.Sign() != 0 {
		switch rounding {
		case RoundExact:
			return 0, fmt.Errorf("crossverse decimal %q is not exact at scale %d", s, scale)
		case RoundTrunc:
		case RoundHalfAwayFromZero:
			doubled := new(big.Int).Abs(rem)
			doubled.Mul(doubled, big.NewInt(2))
			if doubled.CmpAbs(den) >= 0 {
				if num.Sign() < 0 {
					quo.Sub(quo, big.NewInt(1))
				} else {
					quo.Add(quo, big.NewInt(1))
				}
			}
		default:
			return 0, fmt.Errorf("crossverse rounding %d is invalid", rounding)
		}
	}
	if quo.Cmp(maxInt64) > 0 || quo.Cmp(minInt64) < 0 {
		return 0, fmt.Errorf("crossverse decimal %q overflows int64 at scale %d", s, scale)
	}
	return quo.Int64(), nil
}

func ParsePriceCents(input string) (int64, error) {
	v, err := ParseScaled(input, 100, RoundHalfAwayFromZero)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("crossverse price %q must be positive", input)
	}
	return v, nil
}

func ParseExactPriceCents(input string) (int64, error) {
	v, err := ParseScaled(input, 100, RoundExact)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("crossverse book price %q must be positive", input)
	}
	return v, nil
}

func ParseContracts(input string) (int64, error) {
	v, err := ParseScaled(input, 1, RoundExact)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("crossverse contracts %q must not be negative", input)
	}
	return v, nil
}

func ParseSignedPpb(input string) (int64, error) {
	return ParseScaled(input, 1_000_000_000, RoundTrunc)
}

func ParseBpsToPpb(input string) (int64, error) {
	return ParseScaled(input, 100_000, RoundTrunc)
}

func ParseMicroUSDX(input string) (int64, error) {
	v, err := ParseScaled(input, 1_000_000, RoundTrunc)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("crossverse usd amount %q must not be negative", input)
	}
	return v, nil
}

func ParseTimestampMs(input string) (int64, error) {
	v, err := ParseScaled(input, 1, RoundTrunc)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("crossverse timestamp %q must not be negative", input)
	}
	return v, nil
}

// NextFundingAtMs normalizes a next-funding time to absolute epoch ms. The
// authoritative protocol states absolute epoch ms, but the deployed services
// still emit a relative countdown; values below the epoch-ms range are
// interpreted as relative to the frame timestamp.
func NextFundingAtMs(input string, sourceTimestampMs int64) (int64, error) {
	v, err := ParseTimestampMs(input)
	if err != nil {
		return 0, err
	}
	if v < 1_000_000_000_000 {
		return sourceTimestampMs + v, nil
	}
	return v, nil
}
