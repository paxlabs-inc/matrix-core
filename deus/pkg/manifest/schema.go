package manifest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed schema.json
var schemaBytes []byte

var (
	schemaOnce sync.Once
	schemaInst *jsonschema.Schema
	schemaErr  error
)

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("schema.json", bytes.NewReader(schemaBytes)); err != nil {
			schemaErr = fmt.Errorf("manifest: add schema resource: %w", err)
			return
		}
		schemaInst, schemaErr = compiler.Compile("schema.json")
	})
	return schemaInst, schemaErr
}

// Validate checks m against the embedded JSON Schema.
func Validate(m *Manifest) error {
	sch, err := compiledSchema()
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("manifest: validate marshal: %w", err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("manifest: validate doc: %w", err)
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("manifest: schema: %w", err)
	}
	if err := validateBusinessRules(m); err != nil {
		return err
	}
	return nil
}

// ValidateBytes parses and validates raw JSON.
func ValidateBytes(data []byte) (*Manifest, error) {
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := Validate(m); err != nil {
		return nil, err
	}
	return m, nil
}

func validateBusinessRules(m *Manifest) error {
	if m.Mode == "proxy" {
		if m.Endpoint == nil || m.Endpoint.ProxyURL == "" {
			return fmt.Errorf("manifest: proxy mode requires endpoint.proxy_url")
		}
	}
	opNames := make(map[string]struct{}, len(m.Operations))
	for _, op := range m.Operations {
		opNames[op.Name] = struct{}{}
	}
	for _, p := range m.Pricing {
		if _, ok := opNames[p.Operation]; !ok {
			return fmt.Errorf("manifest: pricing references unknown operation %q", p.Operation)
		}
		if p.PriceWei == "0" && p.MinChargeWei == "0" {
			return fmt.Errorf("manifest: pricing for %q must be positive", p.Operation)
		}
	}
	if m.Attestation != nil && !m.Confidential {
		return fmt.Errorf("manifest: attestation requires confidential=true")
	}
	return nil
}
