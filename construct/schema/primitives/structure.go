// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package primitives

import (
	"encoding/json"
	"strings"
)

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

// UnmarshalJSON decodes a node while COERCING every table cell value to a
// string. The wire/render contract for cells is map[string]string, but models
// routinely fill table cells with raw numbers or booleans (a price, a count, a
// flag) instead of strings. Rather than fail the entire render with "cannot
// unmarshal number into Go value of type string", we accept any JSON scalar and
// keep its textual form. Children recurse through this same method.
func (n *StructureNode) UnmarshalJSON(b []byte) error {
	type alias StructureNode
	aux := struct {
		Cells map[string]json.RawMessage `json:"cells,omitempty"`
		*alias
	}{alias: (*alias)(n)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if len(aux.Cells) > 0 {
		n.Cells = make(map[string]string, len(aux.Cells))
		for k, raw := range aux.Cells {
			n.Cells[k] = coerceCellValue(raw)
		}
	}
	return nil
}

// coerceCellValue renders a JSON scalar as plain text: strings are unquoted;
// numbers and booleans keep their literal form; null/empty becomes "". Objects
// and arrays (not valid cell values) fall through as their raw JSON so the
// information is not silently dropped.
func coerceCellValue(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
	}
	return s
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
