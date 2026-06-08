package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/crypto"
)

const CanonicalVersion = "1"

// CanonicalJSON returns stable UTF-8 JSON bytes for hashing (sorted object keys).
func CanonicalJSON(m *Manifest) ([]byte, error) {
	var raw any
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal: %w", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("manifest: unmarshal raw: %w", err)
	}
	sorted := sortValue(raw)
	out, err := json.Marshal(sorted)
	if err != nil {
		return nil, fmt.Errorf("manifest: marshal canonical: %w", err)
	}
	return out, nil
}

// Hash returns keccak256(canonical_json(manifest)) as 0x-prefixed hex.
func Hash(m *Manifest) (string, error) {
	b, err := CanonicalJSON(m)
	if err != nil {
		return "", err
	}
	sum := crypto.Keccak256Hash(b)
	return sum.Hex(), nil
}

// PricingCommitmentHash returns keccak256(canonical_json(pricing only)).
func PricingCommitmentHash(m *Manifest) (string, error) {
	payload := struct {
		SchemaVersion string    `json:"schema_version"`
		Pricing       []Pricing `json:"pricing"`
	}{
		SchemaVersion: m.SchemaVersion,
		Pricing:       m.Pricing,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("manifest: pricing marshal: %w", err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return "", err
	}
	out, err := json.Marshal(sortValue(raw))
	if err != nil {
		return "", err
	}
	sum := crypto.Keccak256Hash(out)
	return sum.Hex(), nil
}

func sortValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			out[k] = sortValue(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = sortValue(item)
		}
		return out
	default:
		return v
	}
}

// Parse decodes JSON into Manifest without validation.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	return &m, nil
}
