package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateFixture(t *testing.T) {
	path := filepath.Join("..", "..", "test", "fixtures", "proxy-weather.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := ValidateBytes(data)
	if err != nil {
		t.Fatalf("ValidateBytes: %v", err)
	}
	h, err := Hash(m)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h == "" || h[:2] != "0x" {
		t.Fatalf("unexpected hash %q", h)
	}
	canon, err := CanonicalJSON(m)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	canon2, err := CanonicalJSON(m)
	if err != nil {
		t.Fatalf("CanonicalJSON second: %v", err)
	}
	if !bytes.Equal(canon, canon2) {
		t.Fatal("canonical json not stable")
	}
}

func TestValidateRejectsProxyWithoutURL(t *testing.T) {
	m := &Manifest{
		SchemaVersion: "1",
		Slug:          "test.svc",
		Kind:          "data",
		DisplayName:   "Test",
		Summary:       "summary",
		Owner:         "0x1",
		PayoutAddress: "0x1",
		Mode:          "proxy",
		Operations: []Operation{{
			Name: "op", Method: "POST",
			InputSchema: map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
		}},
		Pricing: []Pricing{{
			Operation: "op", Model: "per_call", Unit: "call",
			PriceWei: "1000", MinChargeWei: "1000",
		}},
	}
	if err := Validate(m); err == nil {
		t.Fatal("expected validation error for missing proxy_url")
	}
}
