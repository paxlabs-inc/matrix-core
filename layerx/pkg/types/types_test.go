package types

import (
	"math"
	"testing"
)

func TestParseUSDXValid(t *testing.T) {
	cases := map[string]int64{
		"0":          0,
		"1":          1_000_000,
		"1.5":        1_500_000,
		"0.000001":   1,
		".5":         500_000,
		"10.000000":  10_000_000,
		"  2.25  ":   2_250_000,
		"-0.000001":  -1,
		"+3":         3_000_000,
		"123456.789": 123_456_789_000,
		"0.05":       50_000,
	}
	for in, want := range cases {
		got, err := ParseUSDX(in)
		if err != nil {
			t.Errorf("ParseUSDX(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUSDX(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseUSDXInvalid(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"abc",
		"12abc",                  // trailing garbage must NOT be silently accepted
		"1.2.3",                  // multiple separators
		"1.2345678",              // > 6 dp precision
		"1e6",                    // no scientific notation
		"1.+5",                   // sign inside fraction
		"--1",                    // double sign
		".",                      // bare dot
		"-",                      // bare sign
		"0x10",                   // hex
		"1 000",                  // internal space
		"9999999999999999999999", // overflow
	}
	for _, in := range bad {
		if v, err := ParseUSDX(in); err == nil {
			t.Errorf("ParseUSDX(%q) = %d, want error", in, v)
		}
	}
}

func TestParseUSDXOverflowBoundary(t *testing.T) {
	// Just under the int64 micro-USDX ceiling must parse; the ceiling+ must fail.
	maxWhole := math.MaxInt64 / MicroPerUSDX // 9223372036854
	if _, err := ParseUSDX("9223372036854"); err != nil {
		t.Fatalf("max whole should parse: %v", err)
	}
	_ = maxWhole
	if _, err := ParseUSDX("92233720368540"); err == nil {
		t.Fatal("10x over max whole must overflow")
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	vals := []int64{0, 1, 999_999, 1_000_000, 1_500_000, 123_456_789_000, -1, -42_000_000}
	for _, v := range vals {
		s := FormatUSDX(v)
		got, err := ParseUSDX(s)
		if err != nil {
			t.Errorf("round-trip ParseUSDX(FormatUSDX(%d)=%q) error: %v", v, s, err)
			continue
		}
		if got != v {
			t.Errorf("round-trip %d -> %q -> %d", v, s, got)
		}
	}
}

func TestFormatUSDX(t *testing.T) {
	cases := map[int64]string{
		0:          "0.000000",
		1:          "0.000001",
		1_000_000:  "1.000000",
		1_500_000:  "1.500000",
		-1:         "-0.000001",
		-1_000_000: "-1.000000",
	}
	for in, want := range cases {
		if got := FormatUSDX(in); got != want {
			t.Errorf("FormatUSDX(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvelopeHelpers(t *testing.T) {
	ok := OK(map[string]int{"a": 1})
	if !ok.Ok || ok.Error != nil {
		t.Fatal("OK envelope malformed")
	}
	f := Fail(NewError(CodeInvalidRequest, "bad", true))
	if f.Ok || f.Error == nil || f.Error.Code != CodeInvalidRequest || !f.Error.Retryable {
		t.Fatal("Fail envelope malformed")
	}
	if f.Error.Error() != "invalid_request: bad" {
		t.Fatalf("Error() = %q", f.Error.Error())
	}
}
