// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

// Shape is the composite form a Structure takes (frozen [vocabulary.structure]:
// "shape in {list | table | tree}").
type Shape string

const (
	ShapeList  Shape = "list"
	ShapeTable Shape = "table"
	ShapeTree  Shape = "tree"
)

// ShapeValues is the frozen, ordered set of structure shapes.
var ShapeValues = []Shape{ShapeList, ShapeTable, ShapeTree}

// ValidShape reports whether s is a known structure shape.
func ValidShape(s Shape) bool {
	switch s {
	case ShapeList, ShapeTable, ShapeTree:
		return true
	default:
		return false
	}
}

// StructureNode is one record in a structure. The same node serves all three
// shapes: a list uses Label; a table uses Cells (column -> value); a tree uses
// Children. A node may carry a Ref to link to another surface/entity.
type StructureNode struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Ref   string `json:"ref,omitempty"`
	// Cells maps a column name to its value for table rows.
	Cells map[string]string `json:"cells,omitempty"`
	// Children are nested nodes for tree shapes.
	Children []StructureNode `json:"children,omitempty"`
}

// Structure is a composite of records (axis: tree / structured). It covers
// collections, filesystem trees, plan DAGs, the DOM (frozen
// [vocabulary.structure]).
type Structure struct {
	// Shape is the composite form (list|table|tree).
	Shape Shape `json:"shape"`
	// Columns names the ordered table columns (table shape only).
	Columns []string `json:"columns,omitempty"`
	// Records are the structure's nodes.
	Records []StructureNode `json:"records"`
}
