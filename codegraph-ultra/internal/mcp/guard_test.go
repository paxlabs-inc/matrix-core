package mcp

import (
	"testing"
)

func TestGuardFragmentValid(t *testing.T) {
	valid := `# FRAGMENT tool=test
NODE id=pkg.Foo kind=func loc=foo.go:10 sig=func Foo()
NODE id=pkg.Bar kind=type loc=bar.go:5
`
	result, err := guardFragment(valid)
	if err != nil {
		t.Errorf("guardFragment rejected valid fragment: %v", err)
	}
	if result != valid {
		t.Error("guardFragment modified valid fragment")
	}
}

func TestGuardFragmentRejectsIndentedSource(t *testing.T) {
	malicious := `# FRAGMENT tool=test
NODE id=pkg.Foo kind=func
	This line looks like source code because it's indented with a tab
`
	_, err := guardFragment(malicious)
	if err == nil {
		t.Error("guardFragment should reject indented source code")
	}
}

func TestGuardFragmentRejectsFourSpaceIndent(t *testing.T) {
	malicious := `# FRAGMENT tool=test
NODE id=pkg.Foo kind=func
    This line looks like source code because it's indented with 4 spaces
`
	_, err := guardFragment(malicious)
	if err == nil {
		t.Error("guardFragment should reject 4-space indented source code")
	}
}

func TestGuardFragmentAllowsEdgeData(t *testing.T) {
	fragment := `# FRAGMENT tool=neighbors
NODE id=pkg.Foo kind=func
EDGES
  calls=pkg.Bar,pkg.Baz
  ^references=pkg.Qux
`
	result, err := guardFragment(fragment)
	if err != nil {
		t.Errorf("guardFragment rejected edge data: %v", err)
	}
	if result != fragment {
		t.Error("guardFragment modified valid edge data")
	}
}

func TestGuardFragmentAllowsEmpty(t *testing.T) {
	result, err := guardFragment("")
	if err != nil {
		t.Errorf("guardFragment rejected empty: %v", err)
	}
	if result != "" {
		t.Error("guardFragment modified empty fragment")
	}
}

func TestGuardFragmentSafeReturnsErrorFragment(t *testing.T) {
	malicious := `# FRAGMENT tool=test
NODE id=pkg.Foo kind=func
	secret source code here
`
	result := guardFragmentSafe(malicious)
	if result == "" {
		t.Error("guardFragmentSafe returned empty")
	}
	// Should contain the error message
	if len(result) < 10 {
		t.Error("guardFragmentSafe result too short")
	}
}

func TestSanitizeKVXValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"hello\nworld", "hello\\nworld"},
		{"hello\rworld", "hello\\rworld"},
		{"hello\tworld", "hello\\tworld"},
	}
	for _, tt := range tests {
		got := sanitizeKVXValue(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeKVXValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizeKVXValueTruncates(t *testing.T) {
	long := string(make([]byte, 300))
	got := sanitizeKVXValue(long)
	if len(got) > 200 {
		t.Errorf("sanitizeKVXValue did not truncate: len = %d", len(got))
	}
}
