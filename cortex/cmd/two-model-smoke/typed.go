// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Typed-data JSON parsing — mirrors cortex/cmd/cortex-shell/main.go's
// parseTypedJSON / parseTypeName. Duplicated here (~80 LOC) rather
// than extracted to a shared package because both consumers are
// `package main` smoke binaries; lifting the helpers would impose a
// new exported package surface for purely test-side code.
//
// Note: this uses encoding/json (the convenient ergonomic wrapper),
// not canonical CBOR. The cortex stores the canonical CBOR form
// internally; the JSON here is only the LLM-facing surface.

package main

import (
	"encoding/json"
	"fmt"

	"matrix/cortex/memory"
)

func parseTypedJSON(typeName, body string) (memory.TypedData, error) {
	switch typeName {
	case "Identity":
		var d memory.IdentityData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Fact":
		var d memory.FactData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Preference":
		var d memory.PreferenceData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Belief":
		var d memory.BeliefData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Event":
		var d memory.EventData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Goal":
		var d memory.GoalData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Constraint":
		var d memory.ConstraintData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Capability":
		var d memory.CapabilityData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	case "Pattern":
		var d memory.PatternData
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return nil, err
		}
		if d.SchemaVersion == 0 {
			d.SchemaVersion = 1
		}
		return d, nil
	}
	return nil, fmt.Errorf("unknown type %q", typeName)
}

func parseTypeName(name string) memory.Type {
	switch name {
	case "Identity":
		return memory.TypeIdentity
	case "Fact":
		return memory.TypeFact
	case "Preference":
		return memory.TypePreference
	case "Belief":
		return memory.TypeBelief
	case "Event":
		return memory.TypeEvent
	case "Goal":
		return memory.TypeGoal
	case "Constraint":
		return memory.TypeConstraint
	case "Capability":
		return memory.TypeCapability
	case "Pattern":
		return memory.TypePattern
	}
	return 0
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
