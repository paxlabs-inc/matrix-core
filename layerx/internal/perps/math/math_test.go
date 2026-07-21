package math

import (
	"errors"
	"testing"
)

func TestCheckedOps(t *testing.T) {
	if v, err := CheckedAdd(1<<62, 1<<62-1); err != nil || v != (1<<63)-1 {
		t.Fatalf("add max = %d %v", v, err)
	}
	if _, err := CheckedAdd(1<<62, 1<<62); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add overflow = %v", err)
	}
	if _, err := CheckedSub(-(1 << 62), (1<<62)+1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("sub overflow = %v", err)
	}
	if v, err := CheckedMul(-3, 7); err != nil || v != -21 {
		t.Fatalf("mul = %d %v", v, err)
	}
	if _, err := CheckedMul(1<<40, 1<<40); !errors.Is(err, ErrOverflow) {
		t.Fatalf("mul overflow = %v", err)
	}
	if v, err := CheckedMul(0, 1<<62); err != nil || v != 0 {
		t.Fatalf("mul zero = %d %v", v, err)
	}
}

func TestDivisionRoundingLaw(t *testing.T) {
	cases := []struct {
		a, b               int64
		floor, ceil, trunc int64
	}{
		{7, 2, 3, 4, 3},
		{-7, 2, -4, -3, -3},
		{7, -2, -4, -3, -3},
		{-7, -2, 3, 4, 3},
		{6, 3, 2, 2, 2},
		{0, 5, 0, 0, 0},
	}
	for _, tc := range cases {
		if v, err := FloorDiv(tc.a, tc.b); err != nil || v != tc.floor {
			t.Fatalf("FloorDiv(%d,%d) = %d %v, want %d", tc.a, tc.b, v, err, tc.floor)
		}
		if v, err := CeilDiv(tc.a, tc.b); err != nil || v != tc.ceil {
			t.Fatalf("CeilDiv(%d,%d) = %d %v, want %d", tc.a, tc.b, v, err, tc.ceil)
		}
		if v, err := TruncDiv(tc.a, tc.b); err != nil || v != tc.trunc {
			t.Fatalf("TruncDiv(%d,%d) = %d %v, want %d", tc.a, tc.b, v, err, tc.trunc)
		}
	}
	if _, err := FloorDiv(1, 0); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("div by zero = %v", err)
	}
}

func TestMulDivBigIntermediate(t *testing.T) {
	if v, err := MulDiv(1<<62, 4, 8, Trunc); err != nil || v != 1<<61 {
		t.Fatalf("128-bit intermediate = %d %v", v, err)
	}
	if _, err := MulDiv(1<<62, 4, 1, Trunc); !errors.Is(err, ErrOverflow) {
		t.Fatalf("result overflow = %v", err)
	}
}

func TestTicksClampAbs(t *testing.T) {
	if v, err := CeilToTick(101, 5); err != nil || v != 105 {
		t.Fatalf("CeilToTick = %d %v", v, err)
	}
	if v, err := FloorToTick(109, 5); err != nil || v != 105 {
		t.Fatalf("FloorToTick = %d %v", v, err)
	}
	if v, err := CeilToTick(105, 5); err != nil || v != 105 {
		t.Fatalf("CeilToTick exact = %d %v", v, err)
	}
	if Clamp(7, -3, 5) != 5 || Clamp(-7, -3, 5) != -3 || Clamp(2, -3, 5) != 2 {
		t.Fatal("Clamp broken")
	}
	if v, err := Abs(-9); err != nil || v != 9 {
		t.Fatalf("Abs = %d %v", v, err)
	}
	if _, err := Abs(-1 << 63); !errors.Is(err, ErrOverflow) {
		t.Fatalf("Abs min overflow = %v", err)
	}
}

func TestCmpMulMul(t *testing.T) {
	if CmpMulMul(3, 5, 4, 4) >= 0 {
		t.Fatal("15 < 16")
	}
	if CmpMulMul(1<<62, 4, 1<<61, 8) != 0 {
		t.Fatal("equal cross products")
	}
	if CmpMulMul(-3, 5, -4, 4) <= 0 {
		t.Fatal("-15 > -16")
	}
}
