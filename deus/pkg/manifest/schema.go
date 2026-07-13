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
		hasWei := p.PriceWei != "" || p.MinChargeWei != ""
		hasUSDX := p.UnitPriceUSDX != "" || p.MinChargeUSDX != ""
		if !hasWei && !hasUSDX {
			return fmt.Errorf("manifest: pricing for %q needs unit_price_usdx/min_charge_usdx (or legacy price_wei/min_charge_wei)", p.Operation)
		}
		if hasWei && (p.PriceWei == "" || p.MinChargeWei == "") {
			return fmt.Errorf("manifest: pricing for %q must carry both price_wei and min_charge_wei", p.Operation)
		}
		if hasUSDX && (p.UnitPriceUSDX == "" || p.MinChargeUSDX == "") {
			return fmt.Errorf("manifest: pricing for %q must carry both unit_price_usdx and min_charge_usdx", p.Operation)
		}
		if hasWei && p.PriceWei == "0" && p.MinChargeWei == "0" {
			return fmt.Errorf("manifest: pricing for %q must be positive", p.Operation)
		}
		if hasUSDX && isZeroUSDX(p.UnitPriceUSDX) && isZeroUSDX(p.MinChargeUSDX) {
			return fmt.Errorf("manifest: pricing for %q must be positive", p.Operation)
		}
	}
	if m.Attestation != nil && !m.Confidential {
		return fmt.Errorf("manifest: attestation requires confidential=true")
	}
	if m.HoldTTLS != 0 && m.SettlementMode != "hold" {
		return fmt.Errorf("manifest: hold_ttl_s requires settlement_mode=hold")
	}
	return nil
}

// ValidateUSDXOnly runs Validate and additionally requires every pricing plan
// to be USDX-denominated with NO legacy wei fields — the posture once the
// LayerX rail flag is on (DEUS-LAYERX req.7.3).
func ValidateUSDXOnly(m *Manifest) error {
	if err := Validate(m); err != nil {
		return err
	}
	if m.PayeeDID == "" {
		return fmt.Errorf("manifest: payee_did required — LayerX settles a priced service's earnings directly to its DID")
	}
	for _, p := range m.Pricing {
		if p.PriceWei != "" || p.MinChargeWei != "" {
			return fmt.Errorf("manifest: pricing for %q is wei-denominated; the LayerX rail requires unit_price_usdx/min_charge_usdx only", p.Operation)
		}
		if p.UnitPriceUSDX == "" || p.MinChargeUSDX == "" {
			return fmt.Errorf("manifest: pricing for %q must carry unit_price_usdx and min_charge_usdx", p.Operation)
		}
	}
	return nil
}

// isZeroUSDX reports whether a schema-valid USDX decimal string is zero.
func isZeroUSDX(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' && s[i] != '.' {
			return false
		}
	}
	return true
}
