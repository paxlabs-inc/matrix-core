// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package query

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"time"

	"matrix/cortex/forms"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
	"matrix/cortex/store"
	"matrix/cortex/vector"
)

// Errors raised by Run.
var (
	// ErrUnbounded is returned when neither Limit nor BudgetTokens is set.
	// The §12 API forbids unbounded queries (load-bearing OOM guard).
	ErrUnbounded = errors.New("query: must set Limit or BudgetTokens")

	// ErrTooBroad is returned when the query has neither a Type filter nor
	// a HasTag predicate. We refuse to do a full-store scan because it
	// invalidates §15's latency targets.
	ErrTooBroad = errors.New("query: must specify at least one Type or HasTag predicate")

	// ErrUnsupported is returned when a Phase 4+ feature is requested.
	ErrUnsupported = errors.New("query: feature not yet implemented")
)

// FormKind selects which of {short, medium, full} the result should render.
//
// Phase 4: when Form is set, Run populates Result.Rendered with one entry
// per surviving memory (parallel-indexed with Result.Memories). Short and
// Medium are read from the persisted Forms on the Head/Version (no live
// recompute, byte-stable since write-time render). Full is rendered live
// from typed Data because storing it would double mv/ size with no win.
type FormKind string

const (
	FormShort  FormKind = "short"
	FormMedium FormKind = "medium"
	FormFull   FormKind = "full"
)

// OrderField names a sortable scalar in a result memory. Limited to fields
// the engine can sort on without a full Data decode (it has Head, Version,
// and the salience score in hand).
type OrderField string

const (
	OrderSalience      OrderField = "salience"           // default unless Near is set
	OrderCreatedAt     OrderField = "version.created_at" // version's created_at
	OrderLastUpdatedAt OrderField = "head.last_updated_at"
	OrderImportance    OrderField = "head.declared_importance"
	// OrderDistance is the implicit default order when a Near / NearURI
	// query is run: ascending HNSW distance (closest first). Explicit
	// OrderBy clauses override this default.
	OrderDistance OrderField = "near.distance"
	// OrderHop is the implicit default order when a From + Follow graph
	// traversal is run: ascending hop count (closest in the graph first).
	// Explicit OrderBy clauses override this default.
	OrderHop OrderField = "hop"
)

// Direction selects the traversal axis for an EdgeExpr.
type Direction string

const (
	DirOut  Direction = "out"  // walk e/from prefixes (default)
	DirIn   Direction = "in"   // walk e/to prefixes
	DirBoth Direction = "both" // both directions
)

// MaxHopsCap is the upper bound on EdgeExpr.MaxHops. Mirrors the skill-
// composition depth cap (research/05-skills-and-tools.md §S1) so a Find
// traversal can't go deeper than the deepest legal skill chain. A future
// per-actor budget may tune this; for now it is a hard ceiling enforced
// by Run regardless of caller request.
const MaxHopsCap = 6

// EdgeExpr describes a graph-traversal step. Spec: research/04-cortex.md
// §12.2 (Query.Follow).
//
//	Types      restricts the edges traversed (empty = any of the 14)
//	MinHops    smallest hop distance to include in results (default 1)
//	MaxHops    largest hop distance to traverse (default 1; cap MaxHopsCap)
//	Direction  out|in|both (default out)
//	IncludeTombstoned  surfaces edges with Tombstoned=true; default false
type EdgeExpr struct {
	Types             []memory.EdgeType
	MinHops           int
	MaxHops           int
	Direction         Direction
	IncludeTombstoned bool
}

// OrderDirection toggles ascending vs descending.
type OrderDirection string

const (
	OrderAsc  OrderDirection = "asc"
	OrderDesc OrderDirection = "desc"
)

// OrderClause is one entry in Query.OrderBy.
type OrderClause struct {
	Field     OrderField
	Direction OrderDirection
}

// Query is the typed find spec. See research/04-cortex.md §12.2.
//
// Phase 4 honors: Type, Where, OrderBy, Limit, Offset, IncludeTombstoned,
// LateBinding, Form, BudgetTokens. Near, NearURI, From, Follow are reserved
// fields; setting Near/NearURI/From/Follow returns ErrUnsupported.
//
// BudgetTokens semantics: when > 0, the engine sums memory.CountTokens over
// every rendered form and trims low-salience entries until the total fits.
// Trimming is by salience asc regardless of OrderBy (§12.1: "trimming
// low-salience items first"); surviving entries keep the user's OrderBy.
// If Form is unset but BudgetTokens > 0, FormMedium is used as the budgeted
// granularity (§12.1 default).
type Query struct {
	Type              []memory.Type
	Where             Predicate
	OrderBy           []OrderClause
	Limit             int
	Offset            int
	BudgetTokens      int // reserved for Phase 4 form-rendering
	Form              FormKind
	IncludeTombstoned bool
	// LateBinding marks this Find as issued mid-execution rather than at
	// compile time. When true, Run journals a KindFind audit entry. The D13
	// pre-resolution discipline expects compile-time binding; this flag is
	// the audit hook for cases that legitimately need late binding.
	LateBinding bool

	// Near is a natural-language phrase that the caller wants embedded
	// and used as the query vector. Setting Near requires NearIndex to
	// be set too; cortex.Find runs the embedder and populates both.
	Near string

	// NearURI selects a memory whose embedding is reused as the query
	// vector (semantic neighbours of an existing memory). cortex.Find
	// resolves the URI, loads vec/meta, and populates NearVector before
	// dispatching here.
	NearURI *memory.URI

	// NearVector is the query vector for HNSW search. Either supplied
	// directly by an advanced caller or filled in by cortex.Find from
	// Near / NearURI. When set together with NearIndex, the candidate
	// plan switches from idx/tag→idx/type to "HNSW top-K → optional
	// Where post-filter". Default ordering becomes distance ascending.
	NearVector []float32

	// NearIndex is the HNSW handle that Search will be run against.
	// Required when NearVector is set; cortex.Find threads c.Index() in
	// when an embedder is running.
	NearIndex *vector.Index

	// NearK controls the HNSW overshoot factor. The planner asks the
	// index for max(NearK, Limit, EfSearch) candidates so the Where
	// post-filter has slack to discard non-matching neighbours without
	// running out. Zero falls back to 4*Limit.
	NearK int

	// From is the entry vertex for graph traversal (Phase 6). When set,
	// Follow describes the walk; the candidate set becomes the BFS
	// reachable IDs from the resolved memory. Where/Type filters apply
	// post-traversal. From itself is excluded from results.
	From *memory.URI
	// Follow describes the traversal. nil with From set means "one hop
	// out, any edge type". Setting Follow without From returns
	// ErrUnsupported (no anchor to start from).
	Follow *EdgeExpr

	// Scope is the optional CortexScope (Phase 10). When non-nil the
	// run loop applies Scope.Allows(&head) to every candidate after the
	// tombstone + type filters and skips memories outside the scope.
	// Scope verification (signature, snapshot resolvability, multi-
	// proof) is the caller's responsibility — cortex.Find/Context call
	// VerifyScope once before invoking query.Run; raw query.Run callers
	// MUST do the same. query.Run treats q.Scope as already-verified.
	Scope *scope.Scope
}

// Result carries the output of one Find call.
type Result struct {
	Memories []*memory.Memory
	// Rendered is parallel-indexed with Memories. Empty unless Query.Form
	// was set or BudgetTokens triggered the medium fallback. Each entry is
	// the rendered form for the corresponding memory.
	Rendered []string
	// RenderedTokens is parallel-indexed with Rendered. memory.CountTokens
	// of each Rendered entry, captured pre-trim so callers can audit the
	// budget arithmetic.
	RenderedTokens []int
	// Scores maps memory ID → cold-salience value at query time. Useful for
	// debugging ranking; callers usually ignore it.
	Scores map[memory.ID]float32
	// Distances maps memory ID → HNSW distance from the query vector when
	// the call set Near / NearURI / NearVector. Empty otherwise. Distance
	// is in [0, 2] where 0 means "identical to query vector" (1 - cosine
	// under unit norm; see vector package).
	Distances map[memory.ID]float32
	// Hops maps memory ID → hop count from Query.From when the call ran a
	// graph traversal. Empty when From was unset. The From vertex itself
	// is NOT in the map (it is excluded from results by construction).
	Hops map[memory.ID]int
	// CandidatesScanned is the count BEFORE Where evaluation. Useful for
	// detecting too-broad type scans without a Where filter.
	CandidatesScanned int
	// Total is the count after Where + tombstone filter, before Offset/Limit.
	Total int
	// TrimmedByBudget is the count of memories dropped by BudgetTokens
	// enforcement after Limit. Zero when BudgetTokens is unset or fits.
	TrimmedByBudget int
	// Form is the FormKind used for Rendered; "" when no rendering ran.
	Form FormKind
	// LateBinding mirrors Query.LateBinding for audit-side reporting.
	LateBinding bool
}

// Hasher is a tiny helper holding a sha256 instance. Cheap to construct.
type tagHasher struct{}

// HashTag returns the 8-byte tag-hash prefix used in idx/tag keys.
func HashTag(tag string) [keys.TagHashSize]byte {
	sum := sha256.Sum256([]byte(tag))
	var out [keys.TagHashSize]byte
	copy(out[:], sum[:keys.TagHashSize])
	return out
}

// Run executes q against s and returns matching memories.
//
// The planner runs in five steps:
//  1. Validate q.
//  2. Plan candidate set (idx/tag if HasTag conjunction; else idx/type).
//  3. For each candidate: load head; tombstone-filter; load version, decode;
//     evaluate Where; collect.
//  4. Order (default: salience desc).
//  5. Offset / Limit / late-binding journal.
//
// Phase 3 keeps the implementation deliberately straightforward: a single
// pass, no parallel readers, no batching beyond Pebble's own block cache.
// Latency at small N is dominated by Pebble point reads (~30 µs warm), so
// the per-candidate overhead is small. Larger N awaits Phase 4 where
// streaming + budgeting come online.
func Run(s *store.Store, q Query) (*Result, error) {
	if s == nil {
		return nil, errors.New("query: nil store")
	}
	if q.Limit <= 0 && q.BudgetTokens <= 0 {
		return nil, ErrUnbounded
	}
	// Near (text) and NearURI are sugar for NearVector + NearIndex. The
	// cortex.Find facade resolves them and clears the sugar fields before
	// dispatching here, so reaching this point with Near set means the
	// caller bypassed cortex.Find without supplying a resolved vector —
	// that's a misuse.
	if (q.Near != "" || q.NearURI != nil) && q.NearVector == nil {
		return nil, fmt.Errorf("%w: Near/NearURI need cortex.Find to resolve them; raw query.Run requires NearVector", ErrUnsupported)
	}
	if q.NearVector != nil && q.NearIndex == nil {
		return nil, fmt.Errorf("%w: NearVector requires NearIndex (HNSW handle)", ErrUnsupported)
	}
	if q.Follow != nil && q.From == nil {
		return nil, fmt.Errorf("%w: Follow requires From", ErrUnsupported)
	}
	if q.From != nil {
		if err := validateEdgeExpr(q.Follow); err != nil {
			return nil, err
		}
	}
	if q.Where != nil {
		if err := validatePredicate(q.Where); err != nil {
			return nil, err
		}
	}

	var (
		candidates []memory.ID
		scanned    int
		distances  map[memory.ID]float32
		hops       map[memory.ID]int
		err        error
	)
	switch {
	case q.From != nil:
		candidates, scanned, hops, err = planCandidatesGraph(s, q)
	default:
		candidates, scanned, distances, err = planCandidates(s, q)
	}
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	// Phase 12: load per-actor learned weights once, reuse for every
	// candidate. Cold start (no key) falls back to DefaultWeights. Hot
	// path: O(1) Pebble Get per Find, then in-memory math per candidate.
	weights, _, err := salience.ReadWeights(s)
	if err != nil {
		return nil, fmt.Errorf("query: read weights: %w", err)
	}

	res := &Result{
		Scores:            map[memory.ID]float32{},
		Distances:         distances,
		Hops:              hops,
		CandidatesScanned: scanned,
		LateBinding:       q.LateBinding,
	}

	ev := newEvaluator()

	for _, id := range candidates {
		var u keys.ULID
		copy(u[:], id[:])

		headBytes, ok, err := s.Get(keys.MemoryHeadKey(u))
		if err != nil {
			return nil, fmt.Errorf("query: get head: %w", err)
		}
		if !ok {
			continue // candidate index entry without a head: stale (e.g. mid-rebuild)
		}
		var h memory.Head
		if err := memory.DecodeHead(headBytes, &h); err != nil {
			return nil, fmt.Errorf("query: decode head: %w", err)
		}

		if h.Tombstoned != nil && !q.IncludeTombstoned {
			continue
		}

		// Type post-filter: when scanning idx/tag, candidates carry mixed types
		// and we must respect q.Type.
		if len(q.Type) > 0 && !typeMatches(h.Type, q.Type) {
			continue
		}

		// Phase 10: scope post-filter. q.Scope is the already-verified
		// CortexScope (cortex.Find / Context call VerifyScope before
		// query.Run). Scope filtering is silent — Find / Context do not
		// raise per-candidate violations because the call as a whole
		// hasn't failed. Single-target reads (cortex.ResolveScoped) and
		// writes (cortex.UpdateHead) DO raise + journal violations
		// because there's a specific target the caller asked about.
		if q.Scope != nil && !q.Scope.Allows(&h) {
			continue
		}

		verBytes, ok, err := s.Get(keys.MemoryVersionKey(u, h.CurrentVersion))
		if err != nil {
			return nil, fmt.Errorf("query: get version: %w", err)
		}
		if !ok {
			continue
		}
		var v memory.Version
		if err := memory.DecodeVersion(verBytes, &v); err != nil {
			return nil, fmt.Errorf("query: decode version: %w", err)
		}

		mem := &memory.Memory{Head: h, Version: v}

		if q.Where != nil {
			data, err := memory.DecodeData(v.Type, v.Data)
			if err != nil {
				return nil, fmt.Errorf("query: decode data: %w", err)
			}
			match, err := ev.eval(q.Where, mem, data)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
		}

		// Phase 12: rank using the actor's learned weights, computed live
		// from the persisted Score's factor inputs (AccessCount, Citations,
		// LastUsed, Importance, Pinned). sc.Cached is intentionally NOT
		// consulted here — it was computed by BumpFor* helpers using the
		// DEFAULT cold weights, which diverges from the learned weights
		// after the first EMA step. Live recompute is O(1) per candidate
		// (a handful of float ops) so this is materially as cheap as the
		// previous cache-read path.
		sc, ok, err := salience.Read(s, h.ID)
		if err != nil {
			return nil, err
		}
		var score float32
		if ok {
			score = salience.ColdScoreWith(sc, weights, now)
		} else {
			seed := salience.Score{
				LastUsed:   v.CreatedAt.UnixNano(),
				Importance: h.DeclaredImportance,
			}
			score = salience.ColdScoreWith(&seed, weights, now)
		}
		// Tombstoned memories that slipped through (IncludeTombstoned=true)
		// always rank at zero per §8.2.
		if h.Tombstoned != nil {
			score = 0
		}
		res.Scores[h.ID] = score
		res.Memories = append(res.Memories, mem)
	}
	res.Total = len(res.Memories)

	if err := orderResults(res, q.OrderBy); err != nil {
		return nil, err
	}

	if q.Offset > 0 {
		if q.Offset >= len(res.Memories) {
			res.Memories = nil
		} else {
			res.Memories = res.Memories[q.Offset:]
		}
	}
	if q.Limit > 0 && len(res.Memories) > q.Limit {
		res.Memories = res.Memories[:q.Limit]
	}

	// Form rendering + BudgetTokens trim (§9.1 × §12.1). When Form is empty
	// AND BudgetTokens is unset, this is a no-op and callers walk
	// Memory.Version.Forms directly.
	form := q.Form
	if form == "" && q.BudgetTokens > 0 {
		form = FormMedium
	}
	if form != "" {
		if err := renderResults(res, form); err != nil {
			return nil, err
		}
		if q.BudgetTokens > 0 {
			trimByBudget(res, q.BudgetTokens)
		}
	}

	if q.LateBinding {
		if err := journalLateBinding(s, q, res, now); err != nil {
			return nil, err
		}
	}

	return res, nil
}

// renderResults populates res.Rendered/RenderedTokens for the form kind.
// Short/Medium come from the persisted Version.Forms (byte-stable since
// write-time render); Full is rendered live from typed Data.
func renderResults(res *Result, form FormKind) error {
	res.Form = form
	res.Rendered = make([]string, len(res.Memories))
	res.RenderedTokens = make([]int, len(res.Memories))
	for i, m := range res.Memories {
		text, err := renderMemory(m, form)
		if err != nil {
			return err
		}
		res.Rendered[i] = text
		res.RenderedTokens[i] = memory.CountTokens(text)
	}
	return nil
}

func renderMemory(m *memory.Memory, form FormKind) (string, error) {
	switch form {
	case FormShort:
		return m.Version.Forms.Short, nil
	case FormMedium:
		return m.Version.Forms.Medium, nil
	case FormFull:
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			return "", fmt.Errorf("query: render full: decode data: %w", err)
		}
		return forms.RenderFull(&m.Head, data), nil
	}
	return "", fmt.Errorf("query: unknown FormKind %q", form)
}

// trimByBudget drops low-salience entries until the rendered token sum is
// ≤ budget. Trimming proceeds by salience asc regardless of OrderBy; the
// surviving entries are returned in their original (OrderBy-sorted) order.
// At least one memory is always retained when the input is non-empty so
// the caller doesn't get an empty result on a too-tight budget; the result
// may exceed budget by exactly that one item's tokens (the best we can do
// without rendering an even shorter form).
func trimByBudget(res *Result, budget int) {
	if len(res.Memories) == 0 {
		return
	}
	total := 0
	for _, n := range res.RenderedTokens {
		total += n
	}
	if total <= budget {
		return
	}
	// Order indices by salience asc (= drop priority).
	n := len(res.Memories)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa := res.Scores[res.Memories[order[a]].Head.ID]
		sb := res.Scores[res.Memories[order[b]].Head.ID]
		if sa != sb {
			return sa < sb
		}
		// Tie-break: higher original index first (later in OrderBy = lower
		// presentational priority). Keeps determinism when scores tie.
		return order[a] > order[b]
	})
	drop := map[int]struct{}{}
	for _, idx := range order {
		if total <= budget {
			break
		}
		// Always retain at least one entry.
		if len(drop) == n-1 {
			break
		}
		drop[idx] = struct{}{}
		total -= res.RenderedTokens[idx]
	}
	if len(drop) == 0 {
		return
	}
	keptM := make([]*memory.Memory, 0, n-len(drop))
	keptR := make([]string, 0, n-len(drop))
	keptT := make([]int, 0, n-len(drop))
	for i := 0; i < n; i++ {
		if _, dropped := drop[i]; dropped {
			continue
		}
		keptM = append(keptM, res.Memories[i])
		keptR = append(keptR, res.Rendered[i])
		keptT = append(keptT, res.RenderedTokens[i])
	}
	res.Memories = keptM
	res.Rendered = keptR
	res.RenderedTokens = keptT
	res.TrimmedByBudget = len(drop)
}

// validatePredicate walks the AST checking field refs are well-formed and
// every And/Or has at least zero children (vacuous OK). The eval-time
// resolver handles unknown-but-shaped fields per its rules.
func validatePredicate(p Predicate) error {
	if p == nil {
		return nil
	}
	switch x := p.(type) {
	case Eq:
		return x.Field.Validate()
	case Ne:
		return x.Field.Validate()
	case Gt:
		return x.Field.Validate()
	case Gte:
		return x.Field.Validate()
	case Lt:
		return x.Field.Validate()
	case Lte:
		return x.Field.Validate()
	case In:
		return x.Field.Validate()
	case HasTag:
		if x.Tag == "" {
			return errors.New("query: HasTag with empty tag")
		}
		return nil
	case Matches:
		return x.Field.Validate()
	case And:
		for _, c := range x.Children {
			if err := validatePredicate(c); err != nil {
				return err
			}
		}
		return nil
	case Or:
		for _, c := range x.Children {
			if err := validatePredicate(c); err != nil {
				return err
			}
		}
		return nil
	case Not:
		return validatePredicate(x.Inner)
	}
	return fmt.Errorf("query: unknown predicate %T", p)
}

func typeMatches(t memory.Type, allowed []memory.Type) bool {
	for _, a := range allowed {
		if t == a {
			return true
		}
	}
	return false
}

// planCandidates picks an index strategy and returns the deduped candidate
// ID list plus a scan count and (when applicable) a per-ID distance map.
//
// Strategy:
//   - When NearVector is set, HNSW search gives the candidate set; ordered
//     ascending by distance. ErrTooBroad does NOT fire (HNSW is already
//     selective). Where filters apply as a post-filter.
//   - Else, HasTag predicates in the top-level And-conjunction are the
//     most selective signal — intersect across all such tags.
//   - Else, scan idx/type for each Type in q.Type, union.
//   - Else, ErrTooBroad.
func planCandidates(s *store.Store, q Query) ([]memory.ID, int, map[memory.ID]float32, error) {
	if q.NearVector != nil {
		return scanByNear(q)
	}
	tags := collectHasTags(q.Where)
	if len(tags) > 0 {
		ids, scanned, err := scanByTags(s, tags)
		return ids, scanned, nil, err
	}
	if len(q.Type) > 0 {
		ids, scanned, err := scanByTypes(s, q.Type)
		return ids, scanned, nil, err
	}
	return nil, 0, nil, ErrTooBroad
}

// scanByNear runs HNSW search and returns the resulting memory IDs in
// distance-ascending order along with a per-ID distance map for ranking
// downstream.
func scanByNear(q Query) ([]memory.ID, int, map[memory.ID]float32, error) {
	k := q.NearK
	if k <= 0 {
		k = 4 * q.Limit
	}
	// Always request at least Limit so a fully successful Where pass-
	// through still has K results. Cap at a sane upper bound so a tiny
	// index doesn't end up scanning everything multiple times.
	if k < q.Limit {
		k = q.Limit
	}
	if k <= 0 {
		k = 16
	}
	hits, err := q.NearIndex.Search(q.NearVector, k)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("query: HNSW search: %w", err)
	}
	ids := make([]memory.ID, 0, len(hits))
	dists := make(map[memory.ID]float32, len(hits))
	for _, h := range hits {
		mid := memory.ID(h.MemoryID)
		ids = append(ids, mid)
		dists[mid] = h.Distance
	}
	return ids, len(hits), dists, nil
}

func scanByTags(s *store.Store, tags []string) ([]memory.ID, int, error) {
	// Scan each tag's idx/tag bucket. Intersect: an ID must appear in every
	// bucket. Insertion order in Pebble is byte-order; we collect into maps.
	buckets := make([]map[memory.ID]struct{}, len(tags))
	scanned := 0
	for i, tag := range tags {
		hash := HashTag(tag)
		ids, count, err := scanIdxTag(s, hash)
		if err != nil {
			return nil, 0, err
		}
		scanned += count
		buckets[i] = ids
	}
	// Intersect — pick the smallest bucket as base.
	base := buckets[0]
	for _, b := range buckets[1:] {
		if len(b) < len(base) {
			base = b
		}
	}
	out := make([]memory.ID, 0, len(base))
	for id := range base {
		ok := true
		for _, b := range buckets {
			if _, hit := b[id]; !hit {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	// Stable order (ULID byte order = creation time order) for reproducible
	// downstream sorting.
	sort.Slice(out, func(i, j int) bool {
		for k := 0; k < len(out[i]); k++ {
			if out[i][k] != out[j][k] {
				return out[i][k] < out[j][k]
			}
		}
		return false
	})
	return out, scanned, nil
}

func scanIdxTag(s *store.Store, hash [keys.TagHashSize]byte) (map[memory.ID]struct{}, int, error) {
	prefix := keys.IdxTagPrefix(hash)
	out := map[memory.ID]struct{}{}
	count := 0
	err := s.PrefixIter(prefix, func(k, _ []byte) error {
		count++
		// key = idx/tag/<hash:8>/<created:8>/<id:16>
		_, id, err := keys.ParseIdxTagKey(k)
		if err != nil {
			return fmt.Errorf("query: parse idx/tag: %w", err)
		}
		var mid memory.ID
		copy(mid[:], id[:])
		out[mid] = struct{}{}
		return nil
	})
	return out, count, err
}

func scanByTypes(s *store.Store, types []memory.Type) ([]memory.ID, int, error) {
	seen := map[memory.ID]struct{}{}
	out := []memory.ID{}
	scanned := 0
	for _, t := range types {
		if !t.Valid() {
			return nil, 0, fmt.Errorf("query: invalid memory type %d", t)
		}
		prefix := keys.IdxTypePrefix(byte(t))
		err := s.PrefixIter(prefix, func(k, _ []byte) error {
			scanned++
			_, _, id, err := keys.ParseIdxTypeKey(k)
			if err != nil {
				return fmt.Errorf("query: parse idx/type: %w", err)
			}
			var mid memory.ID
			copy(mid[:], id[:])
			if _, dup := seen[mid]; dup {
				return nil
			}
			seen[mid] = struct{}{}
			out = append(out, mid)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return out, scanned, nil
}

// orderResults sorts res.Memories in place. Default ordering:
//   - if res.Hops is populated (From query) → OrderHop ascending
//   - else if res.Distances is populated (Near query) → OrderDistance ascending
//   - else → OrderSalience descending
//
// ULID ascending is the deterministic tie-break in every case.
func orderResults(res *Result, clauses []OrderClause) error {
	if len(clauses) == 0 {
		switch {
		case len(res.Hops) > 0:
			clauses = []OrderClause{{Field: OrderHop, Direction: OrderAsc}}
		case len(res.Distances) > 0:
			clauses = []OrderClause{{Field: OrderDistance, Direction: OrderAsc}}
		default:
			clauses = []OrderClause{{Field: OrderSalience, Direction: OrderDesc}}
		}
	}
	mems := res.Memories
	scores := res.Scores
	dists := res.Distances
	hops := res.Hops
	var sortErr error
	sort.SliceStable(mems, func(i, j int) bool {
		for _, c := range clauses {
			cmp, err := compareForOrderFull(c.Field, scores, dists, hops, mems[i], mems[j])
			if err != nil {
				sortErr = err
				return false
			}
			if cmp == 0 {
				continue
			}
			if c.Direction == OrderAsc {
				return cmp < 0
			}
			return cmp > 0
		}
		// stable tie-break: lower ULID first
		for k := 0; k < len(mems[i].Head.ID); k++ {
			if mems[i].Head.ID[k] != mems[j].Head.ID[k] {
				return mems[i].Head.ID[k] < mems[j].Head.ID[k]
			}
		}
		return false
	})
	return sortErr
}

func compareForOrderFull(field OrderField, scores, dists map[memory.ID]float32, hops map[memory.ID]int, a, b *memory.Memory) (int, error) {
	switch field {
	case OrderSalience, "":
		sa, sb := scores[a.Head.ID], scores[b.Head.ID]
		switch {
		case sa < sb:
			return -1, nil
		case sa > sb:
			return 1, nil
		}
		return 0, nil
	case OrderHop:
		ha, okA := hops[a.Head.ID]
		hb, okB := hops[b.Head.ID]
		if !okA {
			ha = int(^uint(0) >> 1)
		}
		if !okB {
			hb = int(^uint(0) >> 1)
		}
		switch {
		case ha < hb:
			return -1, nil
		case ha > hb:
			return 1, nil
		}
		return 0, nil
	case OrderDistance:
		// Memories without a distance entry sort to the end (treated as
		// +Inf). This shouldn't happen for Near queries since every
		// candidate came from HNSW, but the guard protects mixed-source
		// future plans (e.g. union of Near and tag scans).
		da, okA := dists[a.Head.ID]
		db, okB := dists[b.Head.ID]
		if !okA {
			da = float32(1e30)
		}
		if !okB {
			db = float32(1e30)
		}
		switch {
		case da < db:
			return -1, nil
		case da > db:
			return 1, nil
		}
		return 0, nil
	case OrderCreatedAt:
		switch {
		case a.Version.CreatedAt.Before(b.Version.CreatedAt):
			return -1, nil
		case a.Version.CreatedAt.After(b.Version.CreatedAt):
			return 1, nil
		}
		return 0, nil
	case OrderLastUpdatedAt:
		switch {
		case a.Head.LastUpdatedAt.Before(b.Head.LastUpdatedAt):
			return -1, nil
		case a.Head.LastUpdatedAt.After(b.Head.LastUpdatedAt):
			return 1, nil
		}
		return 0, nil
	case OrderImportance:
		switch {
		case a.Head.DeclaredImportance < b.Head.DeclaredImportance:
			return -1, nil
		case a.Head.DeclaredImportance > b.Head.DeclaredImportance:
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("query: unknown OrderField %q", field)
}

// LateBindingPayload is the canonical shape of the audit record journaled
// for late-binding Find calls. Type definition lives in
// matrix/cortex/journal so the replay harness can decode without a
// reverse dependency on this package; we re-export the alias here so
// existing call sites that referenced query.LateBindingPayload keep
// compiling.
type LateBindingPayload = journal.LateBindingPayload

// journalLateBinding writes the KindFind audit entry AND bumps the
// salience.AccessCount of every returned candidate in a single atomic
// Pebble batch (Phase 11.5).
//
// Phase 3 wrote the journal entry via the standalone Store.AppendJournal
// path with no salience mutation. Phase 11.5 moved both to a WriteBatch
// so the salience bumps and the audit entry commit-or-abort together —
// the replay invariant (research/04-cortex.md §13.4) requires that every
// derived-state mutation be reproducible from journal+canonical, and the
// AccessCount bumps are now derived from the AccessedIDs field on the
// journal payload.
//
// AccessedIDs is the set of res.Memories[*].Head.ID after Offset/Limit/
// BudgetTokens trim; BudgetTokens trim happens before this call returns
// per Run's existing ordering, so the count matches what the caller
// actually receives. Tombstoned candidates are filtered upstream in
// Run, so we never bump a tombstone salience here.
func journalLateBinding(s *store.Store, q Query, res *Result, now time.Time) error {
	pred := ""
	if q.Where != nil {
		pred = q.Where.String()
	}
	types := make([]byte, 0, len(q.Type))
	for _, t := range q.Type {
		types = append(types, byte(t))
	}
	accessed := make([][16]byte, 0, len(res.Memories))
	for _, m := range res.Memories {
		var id [16]byte
		copy(id[:], m.Head.ID[:])
		accessed = append(accessed, id)
	}
	payload := journal.LateBindingPayload{
		SchemaVersion: 1,
		Predicate:     pred,
		Types:         types,
		Limit:         q.Limit,
		ResultCount:   len(res.Memories),
		Tags:          collectHasTags(q.Where),
		AccessedIDs:   accessed,
	}
	body, err := journal.EncodeLateBindingPayload(&payload)
	if err != nil {
		return fmt.Errorf("query: encode late-binding payload: %w", err)
	}

	wb := s.BeginWrite()
	defer wb.Abort()

	// Bump salience.AccessCount per returned candidate. Cache-miss falls
	// back to NewForWrite at v/1.CreatedAt — same posture as Run's score
	// resolution path (lines 384-396): a memory written before salience
	// instrumentation existed shouldn't be skipped on its first observed
	// access. We don't read v/1.CreatedAt here because the access bump
	// is what's important; LastUsed becomes `now` either way.
	for _, m := range res.Memories {
		var u keys.ULID
		copy(u[:], m.Head.ID[:])
		var sc salience.Score
		raw, ok, err := s.Get(keys.SalienceKey(u))
		if err != nil {
			return fmt.Errorf("query: read salience: %w", err)
		}
		if ok {
			if err := salience.Decode(raw, &sc); err != nil {
				return fmt.Errorf("query: decode salience: %w", err)
			}
		} else {
			sc = salience.NewForWrite(m.Head.DeclaredImportance, now)
		}
		salience.BumpForAccess(&sc, now)
		encoded, err := salience.Encode(&sc)
		if err != nil {
			return fmt.Errorf("query: encode salience: %w", err)
		}
		if err := wb.Set(keys.SalienceKey(u), encoded); err != nil {
			return fmt.Errorf("query: set salience: %w", err)
		}
		// Reflect the post-bump score in the live Result so callers
		// see the freshly-bumped value (matches the cortex.go:566-578
		// posture where BumpForUpdate's result is what the caller
		// observes).
		res.Scores[m.Head.ID] = sc.Cached
	}

	if err := wb.AppendJournal(&journal.Entry{
		Kind:      journal.KindFind,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte("query.Run"),
		Payload:   body,
	}); err != nil {
		return fmt.Errorf("query: append late-binding journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return fmt.Errorf("query: commit late-binding batch: %w", err)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
