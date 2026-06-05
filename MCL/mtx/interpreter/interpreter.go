// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package interpreter walks a parsed MatrixScript (.mtx) AST and executes it.
//
// The interpreter operates on the §PROCEDURE section of a SKILL.mtx file.
// It evaluates on-block conditions top-to-bottom (first-match-wins), then
// executes the matching block's entries: prompt blocks (via LLM interface),
// resolve statements (via Cortex interface), unknown/clarify blocks.
//
// The interpreter does NOT own the 6-stage pipeline — that is the mclc binary's
// job (step 8). The interpreter handles stage 4 (frame extraction) and parts of
// stages 5 (entity resolution) and 7 (gap detection) that are declared inline
// in a skill's §PROCEDURE.
//
// External dependencies are injected via interfaces (LLM, Cortex) so the MCL
// module never imports the cortex/ implementation.
package interpreter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"matrix/mcl/mtx/ast"
)

// LLM is the interface for grammar-constrained LLM decoding (D18).
// The compiler model is small, seedable, and grammar-constrained.
type LLM interface {
	// Decode sends messages to the LLM with an optional grammar constraint
	// and returns the raw text output.
	// grammar is a schema identifier (e.g. "intent_frame@1") or "" for unconstrained.
	Decode(ctx context.Context, messages []Message, grammar string) (string, error)
}

// StreamingLLM is the optional capability extension implemented by clients
// that can emit incremental tokens via OpenAI-compatible Server-Sent Events
// (text/event-stream with stream=true on /v1/chat/completions).
//
// Session 31c (model router P3a). Callers that want incremental UX should
// type-assert the LLM to this interface and fall back to Decode when the
// assertion fails:
//
//	if s, ok := llm.(interpreter.StreamingLLM); ok {
//	    text, err := s.Stream(ctx, msgs, grammar, onDelta)
//	} else {
//	    text, err := llm.Decode(ctx, msgs, grammar)
//	}
//
// Stream MUST return the same final text that an equivalent Decode call
// would have returned (so step canonicalization remains identical whether
// the call streamed or not). onDelta is invoked synchronously on the
// caller's goroutine for every received content fragment; an empty delta
// is permitted (some providers emit role-only opening chunks). Stream
// MUST NOT invoke onDelta after returning.
type StreamingLLM interface {
	LLM
	Stream(ctx context.Context, messages []Message, grammar string,
		onDelta func(delta string)) (string, error)
}

// Message is a single chat message sent to the LLM.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// Cortex is the interface for the three cortex primitives.
// The interpreter calls these when executing resolve statements.
type Cortex interface {
	// Find executes cortex.find with typed predicates.
	Find(ctx context.Context, args map[string]string) ([]CortexResult, error)

	// Resolve executes cortex.resolve for exact resolution.
	Resolve(ctx context.Context, expr string) (*CortexResult, error)

	// Context executes cortex.context for cold-start bundle retrieval.
	Context(ctx context.Context, args map[string]string) (string, error)
}

// CortexResult is a single memory returned from a cortex query.
type CortexResult struct {
	URI     string // matrix://cortex/<type>/<id>#<version>
	Type    string // memory type (e.g. "Fact", "Goal")
	Summary string // medium-form summary
}

// SlotStatus tracks the resolution state of a single slot.
type SlotStatus int

const (
	SlotEmpty    SlotStatus = iota // no value yet
	SlotRaw                        // has raw text, not yet resolved
	SlotResolved                   // resolved to a matrix:// URI
	SlotDefault                    // filled with default value
)

// Slot tracks the state of a single input slot during interpretation.
type Slot struct {
	Name     string
	TypeName string // from §INPUTS type annotation
	Status   SlotStatus
	Value    string // resolved URI or literal value
	RawProse string // original NL text before resolution
	Required bool
	Default  string // default value from §INPUTS (empty if none)
}

// Unknown is a declared gap blocking or delaying execution.
type Unknown struct {
	SlotName string
	Severity string // "blocking", "preferred", "optional"
	Reason   string
	Options  string // optional: expression for suggested options
	Default  string // optional: fallback value
}

// ClarifyQuestion is a question to present to the user.
type ClarifyQuestion struct {
	SlotName string
	Prompt   string
	TypeName string // expected answer type
	Required bool
	Options  []string
	Default  string
}

// RunInput is the runtime state fed into the interpreter by the pipeline.
type RunInput struct {
	Prose      string            // original NL from intent.draft.prose (stage 1 output)
	Verb       string            // classified verb from stage 2
	Bundle     string            // formatted cortex.context() output from stage 3
	Grammar    string            // grammar constraint ID for LLM (e.g. "intent_frame@1")
	Confidence float64           // current overall confidence (0..1)
	SlotValues map[string]string // pre-filled slot values (e.g. from intent.answer)
}

// RunResult is what the interpreter produces after walking §PROCEDURE.
type RunResult struct {
	// FrameJSON is the raw JSON output from the LLM (if a prompt was executed).
	FrameJSON string

	// PromptMessages are the interpolated messages sent to the LLM.
	PromptMessages []Message

	// Slots is the final state of all input slots.
	Slots map[string]*Slot

	// Unknowns are declared gaps from unknown blocks.
	Unknowns []*Unknown

	// ClarifyQuestions are questions from clarify blocks.
	ClarifyQuestions []*ClarifyQuestion

	// MatchedCondition describes which on-block condition matched (for audit).
	MatchedCondition string

	// Executed is true if an on-block matched and its entries were executed.
	Executed bool

	// StepKindHint is the value of the `kind = "<value>"` KVPair from
	// the matched on-block, if present. Empty when the skill does not
	// declare a kind hint (the common case for the 159 bulk-converted
	// fixtures). Routes to llm.KindReason at executor time.
	//
	// Session 31b model router (P2b). The hint surfaces here for audit
	// and for the synthesizer's system prompt; the planner LLM consumes
	// the SKILL.mtx body directly and emits the corresponding
	// ir.StepPayload.Kind in plan_tree@1 output.
	StepKindHint string

	// OutputCardinalityHint is the value of the `output_cardinality = <N>`
	// KVPair from the matched on-block (positive integer). Tells the
	// planner LLM that this verb's procedure produces N independent items
	// per invocation and should be folded into a single multi-output
	// step (or into a parallel{} fan-out with N children) rather than
	// emitting N sequential step nodes. Zero / absent leaves the planner
	// free to choose the shape (default = single step or LLM-driven
	// expansion).
	//
	// Session 31c model router (P3c). The hint surfaces here for audit
	// and is forwarded into the planner system prompt by the executor.
	OutputCardinalityHint int
}

// Interpreter walks a SKILL.mtx AST and executes its §PROCEDURE.
type Interpreter struct {
	file   *ast.File
	llm    LLM
	cortex Cortex
}

// New creates an interpreter for the given parsed SKILL.mtx AST.
func New(file *ast.File, llm LLM, cortex Cortex) *Interpreter {
	return &Interpreter{
		file:   file,
		llm:    llm,
		cortex: cortex,
	}
}

// Run executes the §PROCEDURE section with the given runtime input.
func (interp *Interpreter) Run(ctx context.Context, input *RunInput) (*RunResult, error) {
	result := &RunResult{
		Slots: make(map[string]*Slot),
	}

	// Initialize slots from §INPUTS
	interp.initSlots(result, input)

	// Find §PROCEDURE section
	proc := interp.findSection("PROCEDURE")
	if proc == nil {
		return nil, fmt.Errorf("interpreter: no §PROCEDURE section found")
	}

	// Walk on-blocks top-to-bottom, first-match-wins
	for _, entry := range proc.Entries {
		ob, ok := entry.(*ast.OnBlock)
		if !ok {
			continue
		}

		matched, desc := interp.evalCondition(ob.Condition, input, result)
		if !matched {
			continue
		}

		result.MatchedCondition = desc
		result.Executed = true

		if err := interp.execOnBlock(ctx, ob, input, result); err != nil {
			return result, fmt.Errorf("interpreter: %w", err)
		}
		break // first-match-wins
	}

	return result, nil
}

// initSlots populates result.Slots from §INPUTS declarations and pre-filled values.
func (interp *Interpreter) initSlots(result *RunResult, input *RunInput) {
	inputs := interp.findSection("INPUTS")
	if inputs == nil {
		return
	}

	for _, entry := range inputs.Entries {
		sd, ok := entry.(*ast.SlotDecl)
		if !ok {
			continue
		}

		slot := &Slot{
			Name:     sd.Name,
			TypeName: formatTypeRef(sd.TypeRef),
			Status:   SlotEmpty,
			Required: true, // default per spec §5.5
		}

		// Apply modifiers
		for _, mod := range sd.Modifiers {
			switch mod.Kind {
			case ast.ModOptional:
				slot.Required = false
			case ast.ModRequired:
				slot.Required = true
			case ast.ModDefault:
				slot.Default = valueToString(mod.Value)
			}
		}

		// Pre-fill from input
		if input.SlotValues != nil {
			if val, ok := input.SlotValues[sd.Name]; ok && val != "" {
				slot.Value = val
				slot.RawProse = val
				slot.Status = SlotRaw
			}
		}

		// Apply default if still empty
		if slot.Status == SlotEmpty && slot.Default != "" {
			slot.Value = slot.Default
			slot.Status = SlotDefault
		}

		result.Slots[sd.Name] = slot
	}
}

// evalCondition evaluates an on-block condition against the current state.
// Returns (matched, description).
func (interp *Interpreter) evalCondition(cond ast.Condition, input *RunInput, result *RunResult) (bool, string) {
	switch c := cond.(type) {
	case *ast.VerbCondition:
		matched := c.Verb == input.Verb
		return matched, fmt.Sprintf("verb=%s", c.Verb)

	case *ast.ConfidenceCondition:
		threshold, err := strconv.ParseFloat(c.Threshold, 64)
		if err != nil {
			return false, fmt.Sprintf("confidence%s%s (parse error)", c.Op, c.Threshold)
		}
		var matched bool
		switch c.Op {
		case "<":
			matched = input.Confidence < threshold
		case "<=":
			matched = input.Confidence <= threshold
		case ">":
			matched = input.Confidence > threshold
		case ">=":
			matched = input.Confidence >= threshold
		case "==":
			matched = input.Confidence == threshold
		}
		return matched, fmt.Sprintf("confidence%s%s", c.Op, c.Threshold)

	case *ast.SlotValCondition:
		slot := result.Slots[c.SlotName]
		if slot == nil {
			return false, fmt.Sprintf("slot.%s=? (slot not found)", c.SlotName)
		}
		expected := valueToString(c.Value)
		matched := slot.Value == expected
		return matched, fmt.Sprintf("slot.%s=%s", c.SlotName, expected)

	case *ast.UnknownCondition:
		hasBlocking := false
		for _, u := range result.Unknowns {
			if u.Severity == "blocking" {
				hasBlocking = true
				break
			}
		}
		return hasBlocking, "unknown"

	default:
		return false, "unknown_condition_type"
	}
}

// execOnBlock executes all entries in a matched on-block.
func (interp *Interpreter) execOnBlock(ctx context.Context, ob *ast.OnBlock, input *RunInput, result *RunResult) error {
	for _, entry := range ob.Entries {
		switch e := entry.(type) {
		case *ast.PromptBlock:
			if err := interp.execPrompt(ctx, e, input, result); err != nil {
				return err
			}

		case *ast.ResolveStmt:
			if err := interp.execResolve(ctx, e, input, result); err != nil {
				return err
			}

		case *ast.UnknownBlock:
			interp.execUnknown(e, result)

		case *ast.ClarifyBlock:
			interp.execClarify(e, result)

		case *ast.OnBlock:
			// Nested on-block — evaluate and recurse if matched
			matched, _ := interp.evalCondition(e.Condition, input, result)
			if matched {
				if err := interp.execOnBlock(ctx, e, input, result); err != nil {
					return err
				}
			}

		case *ast.KVPair:
			// KV pairs inside on-blocks are metadata. Recognized keys:
			//   kind = "<value>"            -> routes the synthesized step to a
			//                                  specialist model (Session 31b model router);
			//                                  see ir.StepKindNames closed enum.
			//   output_cardinality = <int>  -> declares this on-block produces N
			//                                  independent outputs; planner folds
			//                                  into one multi-output step or a
			//                                  parallel{} fan-out (Session 31c P3c).
			//   skip = true                 -> legacy sentinel; no action.
			// Unrecognized keys are tolerated (forward-compat).
			if len(e.Key) == 1 {
				switch e.Key[0] {
				case "kind":
					if v := ExtractKindValue(e.Value); v != "" {
						result.StepKindHint = v
					}
				case "output_cardinality":
					if n, ok := ExtractPositiveIntValue(e.Value); ok {
						result.OutputCardinalityHint = n
					}
				}
			}

		default:
			// Ignore unrecognized entries (comments, etc.)
		}
	}
	return nil
}

// ExtractKindValue returns the wire-form text of a `kind = ...` value
// node from an on-block KVPair. Accepts both quoted strings (the
// canonical form) and bare identifiers (an ergonomic short-hand that
// mirrors how `verb=` is written in conditions). Returns "" for
// unsupported value types (number, bool, list, URI) so the validator's
// V11 rule can flag the call site instead of silently dropping a
// malformed annotation.
func ExtractKindValue(v ast.Value) string {
	switch x := v.(type) {
	case *ast.StringValue:
		return strings.TrimSpace(x.Text)
	case *ast.IdentValue:
		return x.Name
	}
	return ""
}

// ExtractPositiveIntValue returns the integer value of an *ast.IntValue
// node when it parses cleanly and is strictly positive. Returns
// (0, false) otherwise. Used by the on-block `output_cardinality = <N>`
// metadata KV (Session 31c P3c). Strict positivity matches the
// validator's V12 contract and prevents zero/negative values from
// silently disabling the planner hint.
func ExtractPositiveIntValue(v ast.Value) (int, bool) {
	iv, ok := v.(*ast.IntValue)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(iv.Raw))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// execPrompt interpolates a prompt block and calls the LLM.
func (interp *Interpreter) execPrompt(ctx context.Context, pb *ast.PromptBlock, input *RunInput, result *RunResult) error {
	var messages []Message
	for _, role := range pb.Roles {
		content := interp.interpolate(role.Text, input, result)
		messages = append(messages, Message{
			Role:    role.Role,
			Content: content,
		})
	}

	result.PromptMessages = messages

	if interp.llm == nil {
		return nil // dry-run mode: no LLM, just collect messages
	}

	output, err := interp.llm.Decode(ctx, messages, input.Grammar)
	if err != nil {
		return fmt.Errorf("prompt execution failed: %w", err)
	}

	result.FrameJSON = output
	return nil
}

// execResolve calls the cortex to resolve a slot value.
func (interp *Interpreter) execResolve(ctx context.Context, rs *ast.ResolveStmt, input *RunInput, result *RunResult) error {
	if interp.cortex == nil {
		return nil // dry-run mode
	}

	slot := result.Slots[rs.SlotName]
	if slot == nil {
		// Slot not declared in §INPUTS — still create an entry
		slot = &Slot{Name: rs.SlotName, Status: SlotEmpty}
		result.Slots[rs.SlotName] = slot
	}

	// Already resolved? Skip.
	if slot.Status == SlotResolved {
		return nil
	}

	// Build args map from AST args, interpolating slot expressions
	args := make(map[string]string)
	var positional string
	for _, arg := range rs.Args {
		val := interp.interpolateValue(arg.Value, input, result)
		if arg.Name == "" {
			positional = val
		} else {
			args[arg.Name] = val
		}
	}

	switch rs.CortexFn {
	case "cortex.find":
		results, err := interp.cortex.Find(ctx, args)
		if err != nil {
			return fmt.Errorf("cortex.find for slot.%s: %w", rs.SlotName, err)
		}
		if len(results) > 0 {
			slot.Value = results[0].URI
			slot.Status = SlotResolved
		}

	case "cortex.resolve":
		expr := positional
		if expr == "" {
			// Fallback: use slot's raw prose
			expr = slot.RawProse
		}
		res, err := interp.cortex.Resolve(ctx, expr)
		if err != nil {
			return fmt.Errorf("cortex.resolve for slot.%s: %w", rs.SlotName, err)
		}
		if res != nil {
			slot.Value = res.URI
			slot.Status = SlotResolved
		}

	case "cortex.context":
		bundle, err := interp.cortex.Context(ctx, args)
		if err != nil {
			return fmt.Errorf("cortex.context for slot.%s: %w", rs.SlotName, err)
		}
		if bundle != "" {
			slot.Value = bundle
			slot.Status = SlotResolved
		}
	}

	return nil
}

// execUnknown registers an unknown gap from an unknown block.
func (interp *Interpreter) execUnknown(ub *ast.UnknownBlock, result *RunResult) {
	// Only register if the slot is not already resolved
	slot := result.Slots[ub.SlotName]
	if slot != nil && slot.Status == SlotResolved {
		return
	}

	u := &Unknown{SlotName: ub.SlotName}
	for _, mod := range ub.Modifiers {
		switch mod.Key {
		case "severity":
			u.Severity = valueToString(mod.Value)
		case "reason":
			u.Reason = valueToString(mod.Value)
		case "default":
			u.Default = valueToString(mod.Value)
		case "options":
			u.Options = valueToString(mod.Value)
		}
	}

	// Default severity
	if u.Severity == "" {
		u.Severity = "blocking"
	}

	result.Unknowns = append(result.Unknowns, u)
}

// execClarify generates a clarify question from a clarify block.
func (interp *Interpreter) execClarify(cb *ast.ClarifyBlock, result *RunResult) {
	q := &ClarifyQuestion{SlotName: cb.SlotName}
	for _, mod := range cb.Modifiers {
		switch mod.Key {
		case "prompt":
			q.Prompt = valueToString(mod.Value)
		case "type":
			q.TypeName = valueToString(mod.Value)
		case "required":
			q.Required = valueToString(mod.Value) == "true"
		case "default":
			q.Default = valueToString(mod.Value)
		case "options":
			if ol, ok := mod.Value.(*ast.OptionListValue); ok {
				for _, item := range ol.Items {
					q.Options = append(q.Options, valueToString(item))
				}
			} else {
				q.Options = append(q.Options, valueToString(mod.Value))
			}
		}
	}

	result.ClarifyQuestions = append(result.ClarifyQuestions, q)
}

// interpolate expands {variables} in a prompt string per spec §8.
func (interp *Interpreter) interpolate(text string, input *RunInput, result *RunResult) string {
	var b strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '{' {
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				b.WriteByte(text[i])
				i++
				continue
			}
			varName := text[i+1 : i+end]
			b.WriteString(interp.resolveVar(varName, input, result))
			i += end + 1
		} else {
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String()
}

// resolveVar resolves a single interpolation variable name.
func (interp *Interpreter) resolveVar(name string, input *RunInput, result *RunResult) string {
	switch name {
	case "prose":
		return input.Prose
	case "verb":
		return input.Verb
	case "cortex.bundle":
		return input.Bundle
	case "slots":
		return interp.formatSlots(result)
	case "unknowns":
		return interp.formatUnknowns(result)
	default:
		// slot.X or slot.X.prose
		if strings.HasPrefix(name, "slot.") {
			parts := strings.SplitN(name, ".", 3)
			if len(parts) >= 2 {
				slotName := parts[1]
				slot := result.Slots[slotName]
				if slot == nil {
					return "{" + name + "}"
				}
				if len(parts) == 3 && parts[2] == "prose" {
					if slot.RawProse != "" {
						return slot.RawProse
					}
					return slot.Value
				}
				return slot.Value
			}
		}
		// Unknown variable — preserve as-is for debugging
		return "{" + name + "}"
	}
}

// interpolateValue resolves a Value node to a string, expanding slot references.
func (interp *Interpreter) interpolateValue(v ast.Value, input *RunInput, result *RunResult) string {
	switch val := v.(type) {
	case *ast.SlotExprValue:
		// slot.target.prose → resolve via slot state
		if len(val.Parts) >= 2 {
			slotName := val.Parts[1]
			slot := result.Slots[slotName]
			if slot == nil {
				return strings.Join(val.Parts, ".")
			}
			if len(val.Parts) >= 3 && val.Parts[2] == "prose" {
				if slot.RawProse != "" {
					return slot.RawProse
				}
				return slot.Value
			}
			return slot.Value
		}
		return strings.Join(val.Parts, ".")
	default:
		return valueToString(v)
	}
}

// formatSlots builds a summary of all currently-filled slots.
func (interp *Interpreter) formatSlots(result *RunResult) string {
	if len(result.Slots) == 0 {
		return "(none)"
	}
	var parts []string
	for _, slot := range result.Slots {
		var status string
		switch slot.Status {
		case SlotEmpty:
			status = "empty"
		case SlotRaw:
			status = "raw"
		case SlotResolved:
			status = "resolved"
		case SlotDefault:
			status = "default"
		}
		parts = append(parts, fmt.Sprintf("%s=%s (%s)", slot.Name, slot.Value, status))
	}
	return strings.Join(parts, "\n")
}

// formatUnknowns builds a summary of declared unknowns.
func (interp *Interpreter) formatUnknowns(result *RunResult) string {
	if len(result.Unknowns) == 0 {
		return "(none)"
	}
	var parts []string
	for _, u := range result.Unknowns {
		parts = append(parts, fmt.Sprintf("slot.%s [%s]: %s", u.SlotName, u.Severity, u.Reason))
	}
	return strings.Join(parts, "\n")
}

// findSection returns the first section with the given name, or nil.
func (interp *Interpreter) findSection(name string) *ast.Section {
	for _, sec := range interp.file.Sections {
		if sec.Name == name {
			return sec
		}
	}
	return nil
}

// formatTypeRef formats a TypeRef back to string form.
func formatTypeRef(tr ast.TypeRef) string {
	name := tr.Name
	if name == "enum" && len(tr.EnumSet) > 0 {
		name = "enum<" + strings.Join(tr.EnumSet, "|") + ">"
	}
	if tr.IsList {
		name += "[]"
	}
	return name
}

// valueToString extracts a string representation from an AST Value node.
func valueToString(v ast.Value) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case *ast.StringValue:
		return val.Text
	case *ast.IntValue:
		return val.Raw
	case *ast.FloatValue:
		return val.Raw
	case *ast.BoolValue:
		if val.Val {
			return "true"
		}
		return "false"
	case *ast.IdentValue:
		return val.Name
	case *ast.URIValue:
		return val.URI
	case *ast.SpaceListValue:
		return strings.Join(val.Items, " ")
	case *ast.SlotExprValue:
		return strings.Join(val.Parts, ".")
	case *ast.OptionListValue:
		var items []string
		for _, item := range val.Items {
			items = append(items, valueToString(item))
		}
		return "[" + strings.Join(items, " ") + "]"
	default:
		return ""
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
