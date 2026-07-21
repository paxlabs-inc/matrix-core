package crossverse

import (
	"strings"
	"testing"
)

func TestParseScaled(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		scale    int64
		rounding Rounding
		want     int64
		wantErr  bool
	}{
		{name: "exact integer price", input: "77173.0", scale: 100, rounding: RoundExact, want: 7717300},
		{name: "exact one decimal", input: "77182.4", scale: 100, rounding: RoundExact, want: 7718240},
		{name: "exact two decimals", input: "77185.12", scale: 100, rounding: RoundExact, want: 7718512},
		{name: "exact rejects third decimal", input: "77185.123", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "half away rounds up", input: "0.005", scale: 100, rounding: RoundHalfAwayFromZero, want: 1},
		{name: "half away rounds negative away", input: "-0.005", scale: 100, rounding: RoundHalfAwayFromZero, want: -1},
		{name: "half away below half rounds down", input: "0.0049", scale: 100, rounding: RoundHalfAwayFromZero, want: 0},
		{name: "trunc drops fraction", input: "0.009", scale: 100, rounding: RoundTrunc, want: 0},
		{name: "trunc negative toward zero", input: "-0.009", scale: 100, rounding: RoundTrunc, want: 0},
		{name: "positive exponent", input: "1.21e2", scale: 100, rounding: RoundExact, want: 12100},
		{name: "negative exponent", input: "121e-2", scale: 100, rounding: RoundExact, want: 121},
		{name: "uppercase exponent with sign", input: "1.21E+2", scale: 100, rounding: RoundExact, want: 12100},
		{name: "leading plus", input: "+5", scale: 100, rounding: RoundExact, want: 500},
		{name: "funding to ppb", input: "0.00012", scale: 1_000_000_000, rounding: RoundTrunc, want: 120_000},
		{name: "negative funding to ppb", input: "-0.00009", scale: 1_000_000_000, rounding: RoundTrunc, want: -90_000},
		{name: "many decimals trunc", input: "0.000123456789123", scale: 1_000_000_000, rounding: RoundTrunc, want: 123_456},
		{name: "empty", input: "", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "no digits", input: ".", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "bad digit", input: "12a4", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "two points", input: "1.2.3", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "empty exponent", input: "1e", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "exponent out of range", input: "1e99999", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "exponent above cap", input: "1e31", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "length cap", input: strings.Repeat("9", 65), scale: 100, rounding: RoundExact, wantErr: true},
		{name: "overflow", input: "92233720368547758075", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "boundary max", input: "92233720368547758.07", scale: 100, rounding: RoundExact, want: 9223372036854775807},
		{name: "boundary overflow by one cent", input: "92233720368547758.08", scale: 100, rounding: RoundExact, wantErr: true},
		{name: "invalid scale", input: "1", scale: 0, rounding: RoundExact, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseScaled(tc.input, tc.scale, tc.rounding)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseScaled(%q) = %d, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseScaled(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseScaled(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParsePriceCents(t *testing.T) {
	got, err := ParsePriceCents("77182.436")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7718244 {
		t.Fatalf("got %d, want 7718244", got)
	}
	if _, err := ParsePriceCents("0"); err == nil {
		t.Fatal("zero price must be rejected")
	}
	if _, err := ParsePriceCents("-5"); err == nil {
		t.Fatal("negative price must be rejected")
	}
}

func TestParseExactPriceCents(t *testing.T) {
	got, err := ParseExactPriceCents("77185")
	if err != nil {
		t.Fatal(err)
	}
	if got != 7718500 {
		t.Fatalf("got %d, want 7718500", got)
	}
	if _, err := ParseExactPriceCents("77185.123"); err == nil {
		t.Fatal("sub-cent book price must be rejected")
	}
	if _, err := ParseExactPriceCents("0"); err == nil {
		t.Fatal("zero book price must be rejected")
	}
}

func TestParseContracts(t *testing.T) {
	got, err := ParseContracts("316")
	if err != nil {
		t.Fatal(err)
	}
	if got != 316 {
		t.Fatalf("got %d, want 316", got)
	}
	if got, err := ParseContracts("316.000"); err != nil || got != 316 {
		t.Fatalf("got %d err %v, want 316", got, err)
	}
	if _, err := ParseContracts("0.42"); err == nil {
		t.Fatal("fractional contracts must be rejected")
	}
	if _, err := ParseContracts("-3"); err == nil {
		t.Fatal("negative contracts must be rejected")
	}
}

func TestParseBpsToPpb(t *testing.T) {
	got, err := ParseBpsToPpb("1.21")
	if err != nil {
		t.Fatal(err)
	}
	if got != 121_000 {
		t.Fatalf("got %d, want 121000", got)
	}
	got, err = ParseBpsToPpb("-3.999999")
	if err != nil {
		t.Fatal(err)
	}
	if got != -399_999 {
		t.Fatalf("got %d, want -399999", got)
	}
}

func TestParseMicroUSDX(t *testing.T) {
	got, err := ParseMicroUSDX("12345678.912345678")
	if err != nil {
		t.Fatal(err)
	}
	if got != 12_345_678_912_345 {
		t.Fatalf("got %d, want 12345678912345", got)
	}
	if _, err := ParseMicroUSDX("-1"); err == nil {
		t.Fatal("negative usd must be rejected")
	}
}

func TestNextFundingAtMs(t *testing.T) {
	got, err := NextFundingAtMs("1747555200000", 1_752_950_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1_747_555_200_000 {
		t.Fatalf("absolute got %d, want 1747555200000", got)
	}
	got, err = NextFundingAtMs("27700000", 1_752_950_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1_752_977_700_000 {
		t.Fatalf("relative got %d, want 1752977700000", got)
	}
	if _, err := NextFundingAtMs("-1", 0); err == nil {
		t.Fatal("negative next funding must be rejected")
	}
}
