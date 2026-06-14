// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package llm

import (
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// ModelSlot tests
// ----------------------------------------------------------------------------

func TestModelSlotString(t *testing.T) {
	tests := []struct {
		slot ModelSlot
		want string
	}{
		{SlotCompiler, "compiler"},
		{SlotPlanner, "planner"},
		{SlotExecutor, "executor"},
		{SlotLiaison, "liaison"},
		{ModelSlot(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.slot.String(); got != tt.want {
			t.Errorf("ModelSlot(%d).String() = %q, want %q", tt.slot, got, tt.want)
		}
	}
}

func TestModelSlotEnumStability(t *testing.T) {
	// Document the integer values for cross-language consumers (gateway,
	// future TS client). If these change, downstream consumers MUST be
	// updated in the same commit.
	if SlotCompiler != 0 {
		t.Errorf("SlotCompiler = %d, want 0", SlotCompiler)
	}
	if SlotPlanner != 1 {
		t.Errorf("SlotPlanner = %d, want 1 (NEW in Session 31a)", SlotPlanner)
	}
	if SlotExecutor != 2 {
		t.Errorf("SlotExecutor = %d, want 2 (was 1; bumped by SlotPlanner insertion)", SlotExecutor)
	}
	if SlotLiaison != 3 {
		t.Errorf("SlotLiaison = %d, want 3 (NEW: user-facing conversational agent)", SlotLiaison)
	}
}

// ----------------------------------------------------------------------------
// StepKind tests
// ----------------------------------------------------------------------------

func TestStepKindRoundtrip(t *testing.T) {
	// Every entry in AllStepKindNames must parse back to a StepKind whose
	// String() returns the same name. Guards against ordering drift.
	for i, name := range AllStepKindNames {
		k := ParseStepKind(name)
		if k == KindUnspecified {
			t.Errorf("ParseStepKind(%q) = KindUnspecified (unknown)", name)
			continue
		}
		if got := k.String(); got != name {
			t.Errorf("ParseStepKind(%q).String() = %q, want %q", name, got, name)
		}
		// AllStepKinds and AllStepKindNames must align by index.
		if i < len(AllStepKinds) && AllStepKinds[i] != k {
			t.Errorf("AllStepKinds[%d] = %v, but AllStepKindNames[%d]=%q parses to %v",
				i, AllStepKinds[i], i, name, k)
		}
	}
}

func TestStepKindParseUnknown(t *testing.T) {
	tests := []struct {
		in   string
		want StepKind
	}{
		{"", KindUnspecified},
		{"  ", KindUnspecified},
		{"unknown_kind", KindUnspecified},
		{"REASON", KindReason},   // case-insensitive
		{" Reason ", KindReason}, // trimmed
		{"hard_reason", KindHardReason},
		{"Hard_Reason", KindHardReason},
	}
	for _, tt := range tests {
		if got := ParseStepKind(tt.in); got != tt.want {
			t.Errorf("ParseStepKind(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestStepKindStringUnspecifiedReturnsReason(t *testing.T) {
	// Routing normalizes Unspecified -> Reason. String() must agree so
	// audit logs are self-consistent.
	if got := KindUnspecified.String(); got != "reason" {
		t.Errorf("KindUnspecified.String() = %q, want %q (routing default)", got, "reason")
	}
}

func TestValidStepKindName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", true}, // empty is allowed (defaults to reason)
		{"reason", true},
		{"code", true},
		{"summarize", true},
		{"write", true},
		{"transform", true},
		{"classify", true},
		{"hard_reason", true},
		{"REASON", false},      // case-sensitive — wire form is lowercase
		{"hard-reason", false}, // hyphen vs underscore
		{"think", false},       // not in closed set
	}
	for _, tt := range tests {
		if got := ValidStepKindName(tt.in); got != tt.want {
			t.Errorf("ValidStepKindName(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestAllStepKindsLengthMatchesNames(t *testing.T) {
	if len(AllStepKinds) != len(AllStepKindNames) {
		t.Fatalf("AllStepKinds len %d != AllStepKindNames len %d",
			len(AllStepKinds), len(AllStepKindNames))
	}
	if len(AllStepKinds) != 7 {
		t.Fatalf("AllStepKinds len %d, want 7 (closed enum size)", len(AllStepKinds))
	}
}

// ----------------------------------------------------------------------------
// ModelRegistry tests
// ----------------------------------------------------------------------------

func TestNewModelRegistryHonorsFallback(t *testing.T) {
	fb := Config{Model: "fb-model"}
	reg := NewModelRegistry(fb)
	got := reg.Resolve(RouteKey{Slot: SlotCompiler})
	if got.Model != "fb-model" {
		t.Errorf("Resolve on empty registry returned %q, want fallback %q", got.Model, "fb-model")
	}
	if reg.Fallback().Model != "fb-model" {
		t.Errorf("Fallback().Model = %q, want %q", reg.Fallback().Model, "fb-model")
	}
}

func TestRegistryRegisterAndResolve(t *testing.T) {
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotCompiler}, Config{Model: "compiler-m"}).
		Register(RouteKey{Slot: SlotPlanner}, Config{Model: "planner-m"}).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindReason}, Config{Model: "exec-reason"})

	if got := reg.Resolve(RouteKey{Slot: SlotCompiler}).Model; got != "compiler-m" {
		t.Errorf("compiler: got %q, want %q", got, "compiler-m")
	}
	if got := reg.Resolve(RouteKey{Slot: SlotPlanner}).Model; got != "planner-m" {
		t.Errorf("planner: got %q, want %q", got, "planner-m")
	}
	if got := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason}).Model; got != "exec-reason" {
		t.Errorf("executor reason: got %q, want %q", got, "exec-reason")
	}
}

func TestRegistryNormalizesKindForNonExecutor(t *testing.T) {
	// A caller that mistakenly passes Kind on SlotCompiler should still
	// get the compiler route (Kind is meaningless outside Executor).
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotCompiler}, Config{Model: "compiler"})

	if got := reg.Resolve(RouteKey{Slot: SlotCompiler, Kind: KindCode}).Model; got != "compiler" {
		t.Errorf("compiler with stray Kind: got %q, want %q (Kind should be ignored)", got, "compiler")
	}
}

func TestRegistryUnspecifiedNormalizesToReason(t *testing.T) {
	// A plan that omits step.kind (e.g. converted from a pre-P1 fixture)
	// should land on the KindReason route, not the fallback.
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindReason}, Config{Model: "reason-model"})

	got := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: KindUnspecified})
	if got.Model != "reason-model" {
		t.Errorf("executor with Unspecified Kind: got %q, want reason-model (backwards-compat)", got.Model)
	}
}

func TestRegistryUnknownKindFallsBackToReason(t *testing.T) {
	// Only KindReason is registered; a request for KindCode should fall
	// back to KindReason before reaching the global fallback.
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindReason}, Config{Model: "reason-model"})

	got := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: KindCode})
	if got.Model != "reason-model" {
		t.Errorf("executor KindCode fallback: got %q, want reason-model (executor fallback)", got.Model)
	}
}

func TestRegistryUnknownSlotHitsFallback(t *testing.T) {
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotCompiler}, Config{Model: "compiler"})

	got := reg.Resolve(RouteKey{Slot: SlotPlanner}) // not registered
	if got.Model != "fb" {
		t.Errorf("missing slot: got %q, want fb (global fallback)", got.Model)
	}
}

func TestRegistryLongCtxPrefersVariant(t *testing.T) {
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotCompiler}, Config{Model: "primary"}).
		Register(RouteKey{Slot: SlotCompiler, LongCtx: true}, Config{Model: "longctx"})

	if got := reg.Resolve(RouteKey{Slot: SlotCompiler}).Model; got != "primary" {
		t.Errorf("LongCtx=false: got %q, want primary", got)
	}
	if got := reg.Resolve(RouteKey{Slot: SlotCompiler, LongCtx: true}).Model; got != "longctx" {
		t.Errorf("LongCtx=true: got %q, want longctx", got)
	}
}

func TestRegistryLongCtxFallsThroughWhenNoVariant(t *testing.T) {
	// LongCtx=true on a route with no LongCtx variant should fall through
	// to the primary, not skip to the global fallback.
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotPlanner}, Config{Model: "planner"})

	got := reg.Resolve(RouteKey{Slot: SlotPlanner, LongCtx: true})
	if got.Model != "planner" {
		t.Errorf("LongCtx fall-through: got %q, want planner", got.Model)
	}
}

func TestRegistryNilSafeResolve(t *testing.T) {
	var reg *ModelRegistry
	got := reg.Resolve(RouteKey{Slot: SlotCompiler})
	if got.Model != "" {
		t.Errorf("nil registry Resolve: got %q, want zero Config", got.Model)
	}
}

func TestRegistryNilSafeRegister(t *testing.T) {
	var reg *ModelRegistry
	// Must not panic.
	got := reg.Register(RouteKey{Slot: SlotCompiler}, Config{})
	if got != nil {
		t.Errorf("nil registry Register: got non-nil %v", got)
	}
}

func TestRegistryRoutesReturnsAllRegistered(t *testing.T) {
	reg := NewModelRegistry(Config{Model: "fb"}).
		Register(RouteKey{Slot: SlotCompiler}, Config{Model: "c"}).
		Register(RouteKey{Slot: SlotCompiler, LongCtx: true}, Config{Model: "c-long"}).
		Register(RouteKey{Slot: SlotExecutor, Kind: KindCode}, Config{Model: "code"})

	routes := reg.Routes()
	if len(routes) != 3 {
		t.Fatalf("Routes() len = %d, want 3", len(routes))
	}
	if routes[RouteKey{Slot: SlotCompiler}].Model != "c" {
		t.Errorf("missing compiler primary route")
	}
	if routes[RouteKey{Slot: SlotCompiler, LongCtx: true}].Model != "c-long" {
		t.Errorf("LongCtx route missing or wrong key shape (got %v)", routes[RouteKey{Slot: SlotCompiler, LongCtx: true}])
	}
	if routes[RouteKey{Slot: SlotExecutor, Kind: KindCode}].Model != "code" {
		t.Errorf("missing executor.code route")
	}
}

// ----------------------------------------------------------------------------
// DefaultRegistry coverage
// ----------------------------------------------------------------------------

func TestDefaultRegistryCompilerSlot(t *testing.T) {
	cfg := DefaultRegistry().Resolve(RouteKey{Slot: SlotCompiler})
	// Sess#31a pick: gpt-oss-20b on Fireworks.
	if !strings.Contains(cfg.Model, "gpt-oss-20b") {
		t.Errorf("compiler primary model = %q, want to contain gpt-oss-20b", cfg.Model)
	}
	if cfg.Temperature != 0 {
		t.Errorf("compiler temperature = %v, want 0 (D11 determinism)", cfg.Temperature)
	}
	if cfg.Seed != 42 {
		t.Errorf("compiler seed = %v, want 42 (D11)", cfg.Seed)
	}
	if cfg.GrammarMode != GrammarJSONSchema {
		t.Errorf("compiler grammar mode = %v, want GrammarJSONSchema", cfg.GrammarMode)
	}
	if cfg.MaxTokens > 1024 {
		t.Errorf("compiler max_tokens = %d, expected tight cap (<=1024)", cfg.MaxTokens)
	}
}

func TestDefaultRegistryCompilerLongCtxSlot(t *testing.T) {
	cfg := DefaultRegistry().Resolve(RouteKey{Slot: SlotCompiler, LongCtx: true})
	// LongCtx escalation: DeepSeek-V4-Flash for 1M context.
	if !strings.Contains(cfg.Model, "deepseek-v4-flash") {
		t.Errorf("compiler LongCtx model = %q, want deepseek-v4-flash", cfg.Model)
	}
	if cfg.GrammarMode != GrammarJSONSchema {
		t.Errorf("compiler LongCtx grammar mode = %v, want GrammarJSONSchema", cfg.GrammarMode)
	}
}

func TestDefaultRegistryPlannerSlot(t *testing.T) {
	cfg := DefaultRegistry().Resolve(RouteKey{Slot: SlotPlanner})
	// Sess#31a pick: gpt-oss-120b for plan_tree@1 recursive grammar.
	if !strings.Contains(cfg.Model, "gpt-oss-120b") {
		t.Errorf("planner model = %q, want to contain gpt-oss-120b", cfg.Model)
	}
	if cfg.GrammarMode != GrammarJSONSchema {
		t.Errorf("planner grammar mode = %v, want GrammarJSONSchema", cfg.GrammarMode)
	}
	if cfg.Seed != 42 {
		t.Errorf("planner seed = %v, want 42 (D11 determinism on grammar paths)", cfg.Seed)
	}
}

func TestDefaultRegistryExecutorReasonIsDefault(t *testing.T) {
	cfg := DefaultRegistry().Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason})
	// Sess#31a headline pick: GLM-5.1 for long-horizon agentic loops.
	if !strings.Contains(cfg.Model, "glm-5p1-fast") {
		t.Errorf("executor reason model = %q, want glm-5p1-fast", cfg.Model)
	}
	if cfg.GrammarMode != GrammarNone {
		t.Errorf("executor reason grammar mode = %v, want GrammarNone (free-form)", cfg.GrammarMode)
	}
	if cfg.Temperature != 0.4 {
		t.Errorf("executor reason temperature = %v, want 0.4 (matches sess#18 lock)", cfg.Temperature)
	}
}

func TestDefaultRegistryExecutorKindSpecialists(t *testing.T) {
	tests := []struct {
		kind        StepKind
		modelFrag   string
		grammarMode GrammarMode
	}{
		{KindCode, "Qwen3-Coder", GrammarNone},
		{KindSummarize, "deepseek-v4-flash", GrammarNone},
		{KindWrite, "kimi-k2p7-code", GrammarNone},
		{KindTransform, "gpt-oss-20b", GrammarNone},
		{KindClassify, "gpt-oss-20b", GrammarJSONSchema},
		{KindHardReason, "deepseek-v4-pro", GrammarNone},
	}
	reg := DefaultRegistry()
	for _, tt := range tests {
		cfg := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: tt.kind})
		if !strings.Contains(strings.ToLower(cfg.Model), strings.ToLower(tt.modelFrag)) {
			t.Errorf("kind=%v model = %q, want to contain %q", tt.kind, cfg.Model, tt.modelFrag)
		}
		if cfg.GrammarMode != tt.grammarMode {
			t.Errorf("kind=%v grammar = %v, want %v", tt.kind, cfg.GrammarMode, tt.grammarMode)
		}
	}
}

func TestDefaultRegistryAllExecutorKindsResolve(t *testing.T) {
	reg := DefaultRegistry()
	for _, kind := range AllStepKinds {
		cfg := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: kind})
		if cfg.Model == "" {
			t.Errorf("DefaultRegistry has no route for executor.%s", kind)
		}
	}
}

func TestDefaultRegistryUnspecifiedKindEqualsReason(t *testing.T) {
	// Critical backwards-compat: plans without an explicit step.kind
	// (the 159 bulk-converted SKILL.mtx fixtures) must resolve to the
	// executor reason model byte-equal.
	reg := DefaultRegistry()
	a := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: KindUnspecified})
	b := reg.Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason})
	if a.Model != b.Model {
		t.Errorf("KindUnspecified (%q) != KindReason (%q) — breaks backwards compat", a.Model, b.Model)
	}
	if a.Temperature != b.Temperature {
		t.Errorf("KindUnspecified temp (%v) != KindReason temp (%v)", a.Temperature, b.Temperature)
	}
}

// ----------------------------------------------------------------------------
// Backwards-compat shim tests
// ----------------------------------------------------------------------------

func TestDefaultCompilerModelEqualsRegistryResolve(t *testing.T) {
	a := DefaultCompilerModel()
	b := DefaultRegistry().Resolve(RouteKey{Slot: SlotCompiler})
	if a.Model != b.Model || a.Temperature != b.Temperature || a.Seed != b.Seed {
		t.Errorf("DefaultCompilerModel drift: shim=%+v registry=%+v", a, b)
	}
}

func TestDefaultPlannerModelEqualsRegistryResolve(t *testing.T) {
	a := DefaultPlannerModel()
	b := DefaultRegistry().Resolve(RouteKey{Slot: SlotPlanner})
	if a.Model != b.Model || a.Temperature != b.Temperature {
		t.Errorf("DefaultPlannerModel drift: shim=%+v registry=%+v", a, b)
	}
}

func TestDefaultExecutorModelEqualsReasonResolve(t *testing.T) {
	a := DefaultExecutorModel()
	b := DefaultRegistry().Resolve(RouteKey{Slot: SlotExecutor, Kind: KindReason})
	if a.Model != b.Model || a.Temperature != b.Temperature {
		t.Errorf("DefaultExecutorModel drift: shim=%+v registry=%+v", a, b)
	}
}

// ----------------------------------------------------------------------------
// Grammar schema sanity: plan_tree@1 must declare the new step.kind enum
// ----------------------------------------------------------------------------

func TestPlanTreeGrammarCarriesStepKind(t *testing.T) {
	g := DefaultGrammars()["plan_tree@1"]
	if g == nil {
		t.Fatal("plan_tree@1 grammar missing")
	}
	// Walk schema down to $defs.plan_node.properties.step.properties.kind.enum
	defs, ok := g.JSONSchema["$defs"].(map[string]interface{})
	if !ok {
		t.Fatal("plan_tree@1 missing $defs")
	}
	planNode, ok := defs["plan_node"].(map[string]interface{})
	if !ok {
		t.Fatal("$defs.plan_node missing")
	}
	props, ok := planNode["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("plan_node.properties missing")
	}
	step, ok := props["step"].(map[string]interface{})
	if !ok {
		t.Fatal("plan_node.properties.step missing")
	}
	stepProps, ok := step["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("step.properties missing")
	}
	kindField, ok := stepProps["kind"].(map[string]interface{})
	if !ok {
		t.Fatal("step.properties.kind missing (P1 plan_tree@1 extension not landed)")
	}
	kindEnum, ok := kindField["enum"].([]string)
	if !ok {
		t.Fatalf("step.kind.enum is not []string: %T", kindField["enum"])
	}
	if len(kindEnum) != len(AllStepKindNames) {
		t.Errorf("step.kind.enum len = %d, want %d (AllStepKindNames)",
			len(kindEnum), len(AllStepKindNames))
	}
	// Each enum entry must match the canonical wire name.
	for i, want := range AllStepKindNames {
		if i >= len(kindEnum) {
			t.Errorf("step.kind.enum missing index %d (%q)", i, want)
			continue
		}
		if kindEnum[i] != want {
			t.Errorf("step.kind.enum[%d] = %q, want %q", i, kindEnum[i], want)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
