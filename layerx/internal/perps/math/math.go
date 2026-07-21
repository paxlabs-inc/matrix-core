package math

import (
	"errors"
	"math/big"
)

var ErrOverflow = errors.New("perps math: integer overflow")
var ErrDivideByZero = errors.New("perps math: division by zero")

func CheckedAdd(a, b int64) (int64, error) {
	c := a + b
	if (b > 0 && c < a) || (b < 0 && c > a) {
		return 0, ErrOverflow
	}
	return c, nil
}

func CheckedSub(a, b int64) (int64, error) {
	c := a - b
	if (b < 0 && c < a) || (b > 0 && c > a) {
		return 0, ErrOverflow
	}
	return c, nil
}

func CheckedMul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	c := a * b
	if c/b != a {
		return 0, ErrOverflow
	}
	return c, nil
}

type Rounding int

const (
	Floor Rounding = iota
	Ceil
	Trunc
)

func toInt64(v *big.Int) (int64, error) {
	if !v.IsInt64() {
		return 0, ErrOverflow
	}
	return v.Int64(), nil
}

// MulDiv computes a*b/c through a big-integer intermediate with the named
// rounding direction, rejecting overflow of the final int64.
func MulDiv(a, b, c int64, rounding Rounding) (int64, error) {
	if c == 0 {
		return 0, ErrDivideByZero
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return DivBig(num, big.NewInt(c), rounding)
}

// DivBig divides num by den with the named rounding direction.
func DivBig(num, den *big.Int, rounding Rounding) (int64, error) {
	if den.Sign() == 0 {
		return 0, ErrDivideByZero
	}
	quo := new(big.Int)
	rem := new(big.Int)
	quo.QuoRem(num, den, rem)
	if rem.Sign() != 0 {
		negative := (num.Sign() < 0) != (den.Sign() < 0)
		switch rounding {
		case Floor:
			if negative {
				quo.Sub(quo, big.NewInt(1))
			}
		case Ceil:
			if !negative {
				quo.Add(quo, big.NewInt(1))
			}
		case Trunc:
		default:
			return 0, errors.New("perps math: invalid rounding")
		}
	}
	return toInt64(quo)
}

// FloorDiv is mathematical floor division.
func FloorDiv(a, b int64) (int64, error) {
	return MulDiv(a, 1, b, Floor)
}

// CeilDiv is mathematical ceiling division.
func CeilDiv(a, b int64) (int64, error) {
	return MulDiv(a, 1, b, Ceil)
}

// TruncDiv rounds the quotient toward zero.
func TruncDiv(a, b int64) (int64, error) {
	return MulDiv(a, 1, b, Trunc)
}

// CeilToTick rounds a positive price up to the next tick multiple.
func CeilToTick(priceUnits, tick int64) (int64, error) {
	if tick <= 0 {
		return 0, ErrDivideByZero
	}
	q, err := CeilDiv(priceUnits, tick)
	if err != nil {
		return 0, err
	}
	return CheckedMul(q, tick)
}

// FloorToTick rounds a positive price down to the previous tick multiple.
func FloorToTick(priceUnits, tick int64) (int64, error) {
	if tick <= 0 {
		return 0, ErrDivideByZero
	}
	q, err := FloorDiv(priceUnits, tick)
	if err != nil {
		return 0, err
	}
	return CheckedMul(q, tick)
}

func Clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func Abs(v int64) (int64, error) {
	if v == -1<<63 {
		return 0, ErrOverflow
	}
	if v < 0 {
		return -v, nil
	}
	return v, nil
}

// CmpMulMul compares a*b against c*d without division or overflow (the
// cross-product bound comparison used by percentage/rate checks).
func CmpMulMul(a, b, c, d int64) int {
	lhs := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	rhs := new(big.Int).Mul(big.NewInt(c), big.NewInt(d))
	return lhs.Cmp(rhs)
}
