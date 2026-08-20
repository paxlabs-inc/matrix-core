// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package codegen

import (
	"strings"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a != b {
		t.Fatal("codegen output is not deterministic across runs")
	}
}

func TestGenerateEmitsHeaderAndDoNotEdit(t *testing.T) {
	out, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(out, "DO NOT EDIT") {
		t.Fatal("generated file missing DO NOT EDIT marker")
	}
	if !strings.HasPrefix(out, "/* eslint-disable */") {
		t.Fatal("generated file should lead with eslint-disable")
	}
}

func TestGenerateEmitsEveryKindAndPrimitive(t *testing.T) {
	out, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	want := []string{
		// enums
		"export type Kind =",
		"export type Stakes =",
		"export type Temporality =",
		"export type AskKind =",
		"export type Shape =",
		"export type StepStatus =",
		// interfaces — the 8 primitives + envelope + attributes
		"export interface Surface {",
		"export interface Attributes {",
		"export interface Narration {",
		"export interface Metric {",
		"export interface Entity {",
		"export interface Structure {",
		"export interface Stream {",
		"export interface Timeline {",
		"export interface Canvas {",
		"export interface Ask {",
		// frozen kind literals present in the Kind union
		"'narration'",
		"'ask'",
		// field-type mapping spot-checks
		"kind: Kind",
		"attributes?: Attributes",
		"narration?: Narration",
		"records: StructureNode[]",
		"cells?: Record<string, string>",
		"confidence?: number",
		"required?: boolean",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("generated output missing %q", w)
		}
	}
}
