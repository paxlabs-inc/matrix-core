// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// AffordanceKind is the interaction an affordance exposes on an entity.
type AffordanceKind string

const (
	// AffordanceLink opens an external/internal reference (uses Href).
	AffordanceLink AffordanceKind = "link"
	// AffordanceAsk triggers a back-channel request (uses AskRef to point at
	// an Ask surface — e.g. an irreversible action's sign/confirm).
	AffordanceAsk AffordanceKind = "ask"
	// AffordanceCopy copies the entity identity/field to the clipboard.
	AffordanceCopy AffordanceKind = "copy"
)

// EntityField is one labelled attribute of an entity. A field may itself carry
// a ref to link to another surface/entity (composition by reference).
type EntityField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Ref optionally links this field's value to another surface by id.
	Ref string `json:"ref,omitempty"`
}

// Affordance is an action the human may take on an entity. Affordances NEVER
// embed behaviour — they are typed intents the trusted renderer wires to a
// link, a clipboard copy, or an Ask back-channel surface.
type Affordance struct {
	ID    string         `json:"id"`
	Label string         `json:"label"`
	Kind  AffordanceKind `json:"kind,omitempty"`
	// Href is the target for a link affordance.
	Href string `json:"href,omitempty"`
	// AskRef points at an Ask surface id for an ask affordance.
	AskRef string `json:"ask_ref,omitempty"`
}

// Entity is a referenceable typed object: type + identity + fields +
// affordances (axis: structured + reference). It gives world-state an IDENTITY
// so it can be re-referenced and acted on later (frozen [vocabulary.entity]).
type Entity struct {
	// Type is the entity class (e.g. "tx", "token", "file", "sub-agent").
	Type string `json:"type"`
	// Identity is the stable id (address, hash, path, did).
	Identity string `json:"identity"`
	// Label is an optional human title for the entity.
	Label string `json:"label,omitempty"`
	// Fields are the entity's labelled attributes.
	Fields []EntityField `json:"fields,omitempty"`
	// Affordances are the actions exposed on the entity.
	Affordances []Affordance `json:"affordances,omitempty"`
}
