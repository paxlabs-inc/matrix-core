// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package bridge wires the MCL interpreter to a live cortex.
//
// The MCL module (matrix/mcl) defines a narrow Cortex interface in
// mtx/interpreter consisting of Find / Resolve / Context. The cortex
// module (matrix/cortex) exposes the underlying typed query engine.
// Neither module imports the other — by design — so this bridge package
// is the third top-level glue that adapts cortex.Cortex to the MCL
// interpreter.Cortex interface.
//
// Architectural rules honored:
//
//   - The bridge is the ONLY place where matrix/mcl and matrix/cortex
//     are linked together; it lives in its own Go module so the two
//     core modules stay closed under their own dep graphs.
//   - Bridge calls are compile-time (D13 pre-resolution). LateBinding
//     defaults to false on every Find so the cortex journal does not
//     grow during compilation. Callers that genuinely want mid-execution
//     binding may opt in via WithLateBinding(true) on the Adapter.
//   - The bridge is a thin translator. It never invents query semantics
//     that the cortex package does not already implement; it parses
//     the MCL arg dict, builds a query.Query / context.ContextOpts, and
//     surfaces cortex's own errors verbatim.
//
// Spec citations:
//   - research/04-cortex.md §12 (cortex.find / resolve / context surface)
//   - research/03-retrieval-patterns.md §2 (cold-start composer)
//   - matrix.kvx mcl_go_module note ("MCL does NOT depend on cortex
//     implementation, only the interface")
package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/vector"
	"matrix/mcl/mtx/interpreter"
)

// Adapter implements interpreter.Cortex against a live *cortex.Cortex.
//
// Construct via New and pass to interpreter.New as the third argument.
// The same Adapter is safe to share across goroutines as long as the
// underlying Cortex is; cortex's own concurrency guarantees apply.
type Adapter struct {
	c *cortex.Cortex

	defaultLimit int
	defaultForm  query.FormKind
	lateBinding  bool
}

// Option configures an Adapter at construction.
type Option func(*Adapter)

// WithDefaultLimit sets the default Find limit when a SKILL.mtx call
// omits `limit=`. Zero or negative values are ignored. The package
// default is 10.
func WithDefaultLimit(n int) Option {
	return func(a *Adapter) {
		if n > 0 {
			a.defaultLimit = n
		}
	}
}

// WithDefaultForm sets the default Find FormKind when a SKILL.mtx call
// omits `form=`. The package default is FormMedium so CortexResult.Summary
// gets a non-empty rendering by default.
func WithDefaultForm(f query.FormKind) Option {
	return func(a *Adapter) {
		if f != "" {
			a.defaultForm = f
		}
	}
}

// WithLateBinding controls Query.LateBinding for every Find call. The
// MCL interpreter runs at compile time (D13) so the default is false:
// compile-time Find never journals. Callers that wire this Adapter into
// an executor-side path may set true to honor the cortex audit
// invariants for runtime binding.
func WithLateBinding(on bool) Option {
	return func(a *Adapter) { a.lateBinding = on }
}

// New constructs an Adapter wrapping c.
func New(c *cortex.Cortex, opts ...Option) *Adapter {
	if c == nil {
		panic("bridge: nil cortex")
	}
	a := &Adapter{
		c:            c,
		defaultLimit: 10,
		defaultForm:  query.FormMedium,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Errors returned from the bridge boundary. Errors from the underlying
// cortex are wrapped with `bridge: <op>: %w` so callers can use
// errors.Is against cortex sentinels.
var (
	// ErrUnknownType is returned by Find when args["type"] is not a
	// valid memory.Type name.
	ErrUnknownType = errors.New("bridge: unknown type name")
	// ErrEmptyExpr is returned by Resolve when expr is empty after
	// trimming whitespace.
	ErrEmptyExpr = errors.New("bridge: empty resolve expression")
	// ErrNotResolvable is returned by Resolve when the expression is
	// neither a matrix:// URI nor produced any candidate via near
	// search. Callers should treat this as "no match" and surface a
	// blocking unknown.
	ErrNotResolvable = errors.New("bridge: expression did not resolve")
)

// Find translates the MCL arg dict into a query.Query and runs it
// against the bound cortex.
//
// Supported arg keys (all optional; see args.go for parser):
//
//	type=<TypeName>          e.g. Fact, Goal, Identity, ...
//	tag=<tag>                single tag; repeats not yet supported
//	near=<text>              NL phrase → vector recall (embedder req.)
//	limit=<int>              candidate cap (defaults to 10)
//	form=<short|medium|full> render granularity for Summary
//	late=true|false          override the Adapter's default LateBinding
//
// Any unrecognized arg returns an error so SKILL.mtx authors notice
// typos at compile time rather than silently dropping a filter.
func (a *Adapter) Find(ctx context.Context, args map[string]string) ([]interpreter.CortexResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	q, err := a.buildQuery(args)
	if err != nil {
		return nil, fmt.Errorf("bridge.Find: %w", err)
	}

	res, err := a.c.Find(q)
	if err != nil {
		// Empty-index search is a frequent edge case at compile time
		// (fresh cortex, no embeddings yet). Treat it as "no candidates"
		// so the SKILL.mtx unknown block can fire instead of erroring
		// out the whole compile.
		if errors.Is(err, vector.ErrEmptyIndex) {
			return nil, nil
		}
		return nil, fmt.Errorf("bridge.Find: %w", err)
	}

	out := make([]interpreter.CortexResult, 0, len(res.Memories))
	for i, m := range res.Memories {
		uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
		summary := selectSummary(m, res, i, q.Form)
		out = append(out, interpreter.CortexResult{
			URI:     string(uri),
			Type:    m.Head.Type.String(),
			Summary: summary,
		})
	}
	return out, nil
}

// Resolve maps to cortex.Resolve when expr is a matrix:// URI; falls
// back to a near-search top-1 when expr is natural-language text
// (typical pattern: `cortex.resolve(slot.target.prose)`).
//
// Returns ErrNotResolvable when no candidate was produced. Callers
// expect a nil return + nil error means "treat as unresolved but do
// not fail the on-block"; we follow that convention by returning
// (nil, nil) when the URI path returns memory.ErrNotFound.
func (a *Adapter) Resolve(ctx context.Context, expr string) (*interpreter.CortexResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, ErrEmptyExpr
	}

	// URI fast path.
	if strings.HasPrefix(expr, "matrix://cortex/") {
		m, err := a.c.Resolve(memory.URI(expr))
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("bridge.Resolve: %w", err)
		}
		return memoryToResult(m), nil
	}

	// NL fallback: best-match near-search.
	q := query.Query{
		Near:        expr,
		Limit:       1,
		Form:        a.defaultForm,
		LateBinding: a.lateBinding,
	}
	res, err := a.c.Find(q)
	if err != nil {
		// Empty-index → behave like "no match" so the on-block's
		// unknown can fire (mirrors the Find-path handling above).
		if errors.Is(err, vector.ErrEmptyIndex) {
			return nil, ErrNotResolvable
		}
		return nil, fmt.Errorf("bridge.Resolve: %w", err)
	}
	if len(res.Memories) == 0 {
		return nil, ErrNotResolvable
	}
	m := res.Memories[0]
	r := memoryToResult(m)
	if r.Summary == "" && len(res.Rendered) > 0 {
		r.Summary = res.Rendered[0]
	}
	return r, nil
}

// Context drives cortex.Context with parsed args and formats the
// returned Bundle into a single human/LLM-readable string suitable for
// `{cortex.bundle}` prompt interpolation (spec §8).
//
// Supported arg keys:
//
//	verb=<verb>              closed D7 verb (defaults to none)
//	objects=<k:r,k:r,...>    obj_kind:ref pairs; comma OR semicolon
//	                         separated; ref may contain anything except
//	                         the chosen separator
//	budget_tokens=<int>      total token cap (defaults to cortex default)
//	outcome_limit=<int>      Outcomes tier cap (defaults to 3)
//	form=<short|medium|full> rendered form (defaults to medium)
func (a *Adapter) Context(ctx context.Context, args map[string]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	opts, err := a.buildContextOpts(args)
	if err != nil {
		return "", fmt.Errorf("bridge.Context: %w", err)
	}

	bundle, err := a.c.Context(opts)
	if err != nil {
		return "", fmt.Errorf("bridge.Context: %w", err)
	}

	return FormatBundle(bundle), nil
}

// memoryToResult converts a *memory.Memory into a CortexResult.
// Summary falls through to Forms.Medium then Forms.Short.
func memoryToResult(m *memory.Memory) *interpreter.CortexResult {
	if m == nil {
		return nil
	}
	uri := cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)
	summary := m.Version.Forms.Medium
	if summary == "" {
		summary = m.Version.Forms.Short
	}
	return &interpreter.CortexResult{
		URI:     string(uri),
		Type:    m.Head.Type.String(),
		Summary: summary,
	}
}

// selectSummary picks the best summary string for a Find result. When
// the query was rendered (Form set), prefer res.Rendered[i]; otherwise
// fall back to the persisted Forms.Medium/Short.
func selectSummary(m *memory.Memory, res *query.Result, i int, form query.FormKind) string {
	if res != nil && i < len(res.Rendered) && res.Rendered[i] != "" {
		return res.Rendered[i]
	}
	if form == query.FormShort && m.Version.Forms.Short != "" {
		return m.Version.Forms.Short
	}
	if m.Version.Forms.Medium != "" {
		return m.Version.Forms.Medium
	}
	return m.Version.Forms.Short
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
