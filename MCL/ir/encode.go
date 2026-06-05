// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package ir

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalJSON encodes an Intent to canonical JSON with deterministic key ordering.
// This is the byte representation used for D11 hashing.
// Keys are sorted lexicographically at every nesting level.
func CanonicalJSON(intent *Intent) ([]byte, error) {
	return canonicalAny(intent)
}

// Hash computes the sha256 hash of the canonical JSON encoding.
// This is the Intent.Hash field value.
func Hash(intent *Intent) (string, error) {
	// Clear the hash field before hashing (self-referential)
	saved := intent.Hash
	intent.Hash = ""
	defer func() { intent.Hash = saved }()

	canonical, err := CanonicalJSON(intent)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", sum), nil
}

// CanonicalJSONPlan encodes a PlanTree to canonical JSON with deterministic
// key ordering. Mirrors CanonicalJSON for Intent.
func CanonicalJSONPlan(plan *PlanTree) ([]byte, error) {
	return canonicalAny(plan)
}

// HashPlan computes the sha256 hash of the plan's canonical JSON encoding
// with the self-referential Hash field cleared.
func HashPlan(plan *PlanTree) (string, error) {
	saved := plan.Hash
	plan.Hash = ""
	defer func() { plan.Hash = saved }()

	canonical, err := CanonicalJSONPlan(plan)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", sum), nil
}

// canonicalAny is the shared entry point for any JSON-marshalable value.
// Routes through encoding/json (to apply struct tags) then through the
// canonicalMarshal walker (to sort keys + drop zero values).
func canonicalAny(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ir: marshal failed: %w", err)
	}

	var obj interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("ir: unmarshal for canonicalisation failed: %w", err)
	}

	return canonicalMarshal(obj)
}

// canonicalMarshal recursively encodes a value to canonical JSON.
// Maps are emitted with keys in lexicographic order.
func canonicalMarshal(v interface{}) ([]byte, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		return canonicalMarshalMap(val)
	case []interface{}:
		return canonicalMarshalArray(val)
	default:
		// Scalars: use standard encoding
		return json.Marshal(val)
	}
}

func canonicalMarshalMap(m map[string]interface{}) ([]byte, error) {
	// Sort keys
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := []byte{'{'}
	first := true
	for _, k := range keys {
		v := m[k]

		// Skip zero/empty values for canonical compactness
		if isZeroValue(v) {
			continue
		}

		if !first {
			buf = append(buf, ',')
		}
		first = false

		// Key
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf = append(buf, kb...)
		buf = append(buf, ':')

		// Value
		vb, err := canonicalMarshal(v)
		if err != nil {
			return nil, err
		}
		buf = append(buf, vb...)
	}
	buf = append(buf, '}')
	return buf, nil
}

func canonicalMarshalArray(arr []interface{}) ([]byte, error) {
	buf := []byte{'['}
	for i, item := range arr {
		if i > 0 {
			buf = append(buf, ',')
		}
		ib, err := canonicalMarshal(item)
		if err != nil {
			return nil, err
		}
		buf = append(buf, ib...)
	}
	buf = append(buf, ']')
	return buf, nil
}

// isZeroValue returns true if the JSON value is a zero/empty that should be
// omitted from canonical encoding. This mirrors omitempty semantics.
func isZeroValue(v interface{}) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case string:
		return val == ""
	case float64:
		return val == 0
	case bool:
		return !val
	case []interface{}:
		return len(val) == 0
	case map[string]interface{}:
		return len(val) == 0
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
