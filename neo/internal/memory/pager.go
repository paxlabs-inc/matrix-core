// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package memory is Neo's memory-controller — the "pager" in the frozen
// spec's RAM/disk/pager model. The context window is RAM (scarce), cortex is
// disk (durable ground truth), and this package is the controller that:
//
//   - PINS a small high-salience block every turn (identity + inviolable
//     rules + active goal);
//   - PAGE-FAULTS the top-K relevant records into the window on demand
//     (semantic HNSW search when an embedder is running, else salience-ranked);
//   - writes durable learnings back to cortex (outcomes, facts, patterns).
//
// It is a thin, opinionated layer over matrix/cortex: cortex owns the typed,
// tamper-evident store; this package owns Neo's access patterns.
package memory

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/store"

	"matrix/neo/internal/config"
)

// Pager is Neo's memory controller over a single cortex actor store.
type Pager struct {
	cfg         config.Config
	cortex      *cortex.Cortex
	store       *store.Store
	embedder    embed.Embedder
	hasEmbedder bool

	mu         sync.RWMutex
	activeGoal string
}

// Snippet is a single retrieved memory rendered for injection into the window.
type Snippet struct {
	Text string
	URI  string
	Type string
	// Note carries an optional one-line advisory the pager attaches when a
	// memory needs reconciliation before it is trusted — currently set when a
	// surfaced memory is joined to another surfaced memory by a live
	// contradiction edge. Empty for ordinary memories.
	Note string
}

// Open opens (creating if needed) the cortex brain at cfg.CortexRoot for
// cfg.CortexActor and starts the embedding worker so semantic retrieval is
// available. A failed embedder is non-fatal — retrieval falls back to
// salience ranking.
func Open(cfg config.Config) (*Pager, error) {
	if err := os.MkdirAll(cfg.CortexRoot, 0o755); err != nil {
		return nil, fmt.Errorf("neo/memory: mkdir cortex root %s: %w", cfg.CortexRoot, err)
	}
	s, err := store.Open(cfg.CortexRoot, cfg.CortexActor, nil)
	if err != nil {
		return nil, fmt.Errorf("neo/memory: open store: %w", err)
	}
	c := cortex.New(s)

	emb := pickEmbedder(cfg)
	p := &Pager{cfg: cfg, cortex: c, store: s, embedder: emb}

	if serr := c.StartEmbedder(cortex.EmbedderOptions{Embedder: emb}); serr == nil {
		p.hasEmbedder = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = c.DrainEmbedder(ctx)
		cancel()
	}
	return p, nil
}

// Close stops the embedder and closes the store.
func (p *Pager) Close() error {
	if p == nil {
		return nil
	}
	if p.cortex != nil && p.hasEmbedder {
		_ = p.cortex.StopEmbedder()
	}
	if p.store != nil {
		return p.store.Close()
	}
	return nil
}

// SetActiveGoal records the task Neo is currently pursuing (pinned every turn).
func (p *Pager) SetActiveGoal(goal string) {
	p.mu.Lock()
	p.activeGoal = strings.TrimSpace(goal)
	p.mu.Unlock()
}

// ActiveGoal returns the current active goal.
func (p *Pager) ActiveGoal() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.activeGoal
}

// HasEmbedder reports whether semantic (HNSW) retrieval is available.
func (p *Pager) HasEmbedder() bool { return p.hasEmbedder }

// Embedder returns the embedding backend Neo selected at open time (gateway,
// direct provider, or the deterministic hash fallback — never nil in practice).
// Sibling read-lanes (e.g. conversational recall) reuse it so the whole agent
// shares one embedding model. Returns nil only on a nil pager.
func (p *Pager) Embedder() embed.Embedder {
	if p == nil {
		return nil
	}
	return p.embedder
}

// Pinned composes the always-injected pinned block: identity, the inviolable
// operating rules (Neo's invariants + any hard constraints in cortex), and
// the active goal. Bounded by cfg.PinnedBudgetTokens.
//
// goal is the caller's (per-conversation) active goal — passed in rather than
// read from shared pager state so that many conversations can share one cortex
// store without clobbering each other's goal. Empty falls back to any
// process-level ActiveGoal (CLI path) then to a neutral placeholder.
func (p *Pager) Pinned(ctx context.Context, goal string) string {
	var b strings.Builder

	name := p.cfg.AgentName
	if name == "" {
		name = "Neo"
	}
	did := p.identityDID()
	if did != "" {
		fmt.Fprintf(&b, "You are %s, Matrix's default agent (%s).\n", name, did)
	} else {
		fmt.Fprintf(&b, "You are %s, Matrix's default agent.\n", name)
	}

	b.WriteString("Inviolable operating rules:\n")
	for _, r := range invariantRules {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	for _, r := range p.hardConstraints(ctx) {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}

	if guide := p.LearnedGuidance(ctx); len(guide) > 0 {
		b.WriteString("Working guidance you've learned (apply it unless the user says otherwise):\n")
		for _, g := range guide {
			b.WriteString("- ")
			b.WriteString(g)
			b.WriteString("\n")
		}
	}

	if profile := p.UserProfile(ctx); len(profile) > 0 {
		b.WriteString("What you know about your user:\n")
		for _, line := range profile {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	goal = strings.TrimSpace(goal)
	if goal == "" {
		goal = p.ActiveGoal()
	}
	if goal == "" {
		goal = "(none set yet — infer it from the conversation)"
	}
	fmt.Fprintf(&b, "Current goal: %s\n", goal)

	return truncateTokens(b.String(), p.cfg.PinnedBudgetTokens)
}

// userProfileMax bounds the pinned profile so it can never crowd out the
// rules/goal inside the pinned token budget.
const userProfileMax = 12

// UserProfile returns the durable facts stored about the user themselves
// (subject matrix://knowledge/user), newest-versions-first, bounded.
func (p *Pager) UserProfile(ctx context.Context) []string {
	res, err := p.cortex.Find(query.Query{
		Type:  []memory.Type{memory.TypeFact},
		Limit: 64,
	})
	if err != nil || res == nil {
		return nil
	}
	var out []string
	for _, m := range res.Memories {
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		var fd memory.FactData
		switch x := data.(type) {
		case memory.FactData:
			fd = x
		case *memory.FactData:
			fd = *x
		default:
			continue
		}
		if fd.Subject != userFactSubject || strings.TrimSpace(fd.Statement) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(fd.Statement))
		if len(out) >= userProfileMax {
			break
		}
	}
	return out
}

// invariantRules are Neo's hard rules, lifted from the frozen spec invariants
// (i1–i6) and phrased for the model in human terms (transparency rule).
var invariantRules = []string{
	"You hold no signing key. Anything that moves or commits funds, or needs a wallet signature (sending value, swaps, token approvals, deploying for gas, funding/settling streams or channels), must go through the core_execute tool — never a direct tool. The user approves any spend inline.",
	"cortex is your durable memory and the ground truth; this conversation is a working cache that can be summarized and refreshed.",
	"Copy high-entropy tokens — addresses, tx hashes, IDs, file paths — verbatim. Never paraphrase or invent them.",
	"Explain what you are doing in plain, human terms. Hide the machinery (memory, hashing, replay); surface the intention.",
	"Never claim a success that did not happen. If you are blocked or only partly done, say so honestly.",
	"Act on reversible work by default; ask only on genuine ambiguity, a destructive non-monetary action, or scope expansion.",
}

// hardConstraints reads any hard-strength Constraint memories from cortex so
// operator/user-declared rules are pinned alongside the baked invariants.
func (p *Pager) hardConstraints(ctx context.Context) []string {
	res, err := p.cortex.Find(query.Query{
		Type:  []memory.Type{memory.TypeConstraint},
		Limit: 32,
	})
	if err != nil || res == nil {
		return nil
	}
	var out []string
	for _, m := range res.Memories {
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			continue
		}
		var stmt string
		var hard bool
		switch cd := data.(type) {
		case memory.ConstraintData:
			stmt, hard = cd.Statement, cd.StrengthVal == memory.StrengthHard
		case *memory.ConstraintData:
			stmt, hard = cd.Statement, cd.StrengthVal == memory.StrengthHard
		}
		if hard && strings.TrimSpace(stmt) != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// learnedGuidanceMax bounds the pinned learned-guidance block so it can never
// crowd out the rules/profile/goal inside the pinned token budget.
const learnedGuidanceMax = 8

// learnedPrefFloor is the minimum StrengthVal a learned Preference must carry
// to be pinned — weak preferences stay in salience-ranked retrieval rather
// than occupying scarce pinned space every turn.
const learnedPrefFloor float32 = 0.5

// LearnedGuidance returns short behavioral guidance lines Neo has LEARNED:
// non-hard learned/declared Constraints (hard ones are pinned verbatim by
// hardConstraints) followed by strong durable Preferences. This is the
// structural fix for "Neo keeps forgetting a learned behavior" — a correction
// or working preference (e.g. "render a Construct surface while working")
// becomes a line pinned to EVERY turn, changing behavior reliably instead of
// depending on salience-ranked retrieval luck. Ranked by the Find planner's
// salience-desc default and bounded by learnedGuidanceMax.
func (p *Pager) LearnedGuidance(ctx context.Context) []string {
	_ = ctx
	var out []string

	if res, err := p.cortex.Find(query.Query{Type: []memory.Type{memory.TypeConstraint}, Limit: 32}); err == nil && res != nil {
		for _, m := range res.Memories {
			data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
			if derr != nil {
				continue
			}
			var cd memory.ConstraintData
			switch x := data.(type) {
			case memory.ConstraintData:
				cd = x
			case *memory.ConstraintData:
				cd = *x
			default:
				continue
			}
			if cd.StrengthVal == memory.StrengthHard || strings.TrimSpace(cd.Statement) == "" {
				continue // hard rules already pinned verbatim by hardConstraints
			}
			out = append(out, strings.TrimSpace(cd.Statement))
			if len(out) >= learnedGuidanceMax {
				return out
			}
		}
	}

	if res, err := p.cortex.Find(query.Query{Type: []memory.Type{memory.TypePreference}, Limit: 32}); err == nil && res != nil {
		for _, m := range res.Memories {
			data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
			if derr != nil {
				continue
			}
			var pd memory.PreferenceData
			switch x := data.(type) {
			case memory.PreferenceData:
				pd = x
			case *memory.PreferenceData:
				pd = *x
			default:
				continue
			}
			if pd.StrengthVal < learnedPrefFloor || strings.TrimSpace(pd.Topic) == "" {
				continue
			}
			out = append(out, renderPreference(pd))
			if len(out) >= learnedGuidanceMax {
				return out
			}
		}
	}
	return out
}

// renderPreference turns a stored Preference into a one-line imperative for
// the pinned guidance block.
func renderPreference(pd memory.PreferenceData) string {
	verb := "Prefer to"
	switch pd.Polarity {
	case memory.PolarityAvoid, memory.PolarityDont:
		verb = "Avoid"
	case memory.PolarityDo:
		verb = "Always"
	}
	s := verb + " " + strings.TrimSpace(pd.Topic)
	if r := strings.TrimSpace(pd.Rationale); r != "" {
		s += " — " + r
	}
	return s
}

func (p *Pager) identityDID() string {
	ids, err := p.cortex.ListByType(memory.TypeIdentity, 1)
	if err != nil || len(ids) == 0 {
		return ""
	}
	m, err := p.cortex.ResolveLatest(ids[0])
	if err != nil {
		return ""
	}
	data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		return ""
	}
	switch id := data.(type) {
	case memory.IdentityData:
		return id.DID
	case *memory.IdentityData:
		return id.DID
	}
	return ""
}

// Retrieve page-faults the top-K records relevant to queryText. Seeds come
// from two lanes scored 1.0: semantic (HNSW) results when an embedder is
// running, ALWAYS merged with a salience-ranked lane over the durable types
// (the embedding worker is async, so a memory written seconds ago is invisible
// to the vector index — without the salience lane a "remember this" → "what do
// you know?" round trip inside one session comes back empty).
//
// The seeds then cascade over the relationship graph (cortex edges, depth 2,
// both directions) so connected memories surface with a hop-decayed score
// (0.7^hop). A supersession filter drops any candidate a newer memory replaces
// (live inbound EdgeSupersedes) so Neo sees the current version; contradiction
// edges annotate the conflicting pair so Neo reconciles before trusting. Sorted
// by score desc (stable on first-seen order) and bounded by RetrievalTopK.
func (p *Pager) Retrieve(ctx context.Context, queryText string) ([]Snippet, error) {
	type cand struct {
		snip  Snippet
		id    memory.ID
		score float32
		order int
	}
	var (
		cands []cand
		idx   = map[string]int{} // uri -> index into cands
	)
	addScored := func(s Snippet, score float32) {
		if s.URI == "" {
			return
		}
		if j, ok := idx[s.URI]; ok {
			if score > cands[j].score { // keep the strongest path to a memory
				cands[j].score = score
			}
			return
		}
		var id memory.ID
		if _, mid, _, err := cortex.ParseURI(memory.URI(s.URI)); err == nil {
			id = mid
		}
		idx[s.URI] = len(cands)
		cands = append(cands, cand{snip: s, id: id, score: score, order: len(cands)})
	}

	// --- seed lanes (base score 1.0): semantic HNSW + the always-on
	// salience lane (covers memories written this session that the async
	// embedder hasn't indexed yet). ---
	if p.hasEmbedder && strings.TrimSpace(queryText) != "" {
		if res, err := p.cortex.Find(query.Query{
			Near:         queryText,
			Limit:        p.cfg.RetrievalTopK,
			BudgetTokens: p.cfg.RetrievalBudgetTokens,
			Form:         query.FormMedium,
		}); err == nil {
			for _, s := range renderSnippets(res) {
				addScored(s, 1.0)
			}
		}
	}
	res, err := p.cortex.Find(query.Query{
		Type: []memory.Type{
			memory.TypeFact, memory.TypeEvent, memory.TypePattern,
			memory.TypePreference, memory.TypeGoal,
		},
		Limit:        p.cfg.RetrievalTopK,
		BudgetTokens: p.cfg.RetrievalBudgetTokens,
		Form:         query.FormMedium,
	})
	if err != nil && len(cands) == 0 {
		return nil, err
	}
	if err == nil {
		for _, s := range renderSnippets(res) {
			addScored(s, 1.0)
		}
	}

	// --- cascade: pull edge-connected neighbors of the seeds into the pool
	// with a hop-decayed score (0.7^hop, depth 2). Snapshot the seed URIs
	// first so neighbors added mid-loop aren't themselves re-expanded. ---
	seeds := make([]string, len(cands))
	for i := range cands {
		seeds[i] = cands[i].snip.URI
	}
	if len(seeds) > cascadeSeedCap {
		seeds = seeds[:cascadeSeedCap]
	}
	for _, seedURI := range seeds {
		for _, ns := range p.cascadeNeighbors(seedURI) {
			addScored(ns.snip, ns.score)
		}
	}

	// --- supersession filter: drop any candidate a newer memory supersedes
	// (a live inbound EdgeSupersedes) so Neo sees the current version, not the
	// stale one it corrects. ---
	kept := cands[:0]
	for _, c := range cands {
		if !c.id.IsZero() && p.isSuperseded(c.id) {
			continue
		}
		kept = append(kept, c)
	}
	cands = kept

	// --- contradiction surfacing: when two surviving candidates are joined by
	// a live contradiction edge, annotate them so Neo reconciles rather than
	// silently trusting one. ---
	liveIDs := make(map[memory.ID]bool, len(cands))
	for _, c := range cands {
		if !c.id.IsZero() {
			liveIDs[c.id] = true
		}
	}
	for i := range cands {
		if !cands[i].id.IsZero() && p.contradictsAnyOf(cands[i].id, liveIDs) {
			cands[i].snip.Note = contradictionNote
		}
	}

	// --- per-type half-life re-rank (Neo-side, read-time only): different
	// memory types live on different timescales, so scale each candidate's
	// score by a recency multiplier keyed by its type before the final top-K
	// cut. Cortex's stored Score, sc.Cached, and the journaled 90d half-life
	// are untouched — this only re-orders Neo's working set. ---
	now := time.Now().UTC()
	for i := range cands {
		cands[i].score *= recencyMultiplier(cands[i].snip.Type, p.lastUsedNano(cands[i].id), now)
	}

	// --- order by score desc (seeds before cascade neighbors, nearer hops
	// before farther), stable on insertion order, bounded to RetrievalTopK. ---
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].score != cands[b].score {
			return cands[a].score > cands[b].score
		}
		return cands[a].order < cands[b].order
	})
	out := make([]Snippet, 0, p.cfg.RetrievalTopK)
	for _, c := range cands {
		if len(out) >= p.cfg.RetrievalTopK {
			break
		}
		out = append(out, c.snip)
	}
	return out, nil
}

// cascadeFollowTypes are the relationship edges Retrieve traverses out from
// each seed to pull connected context into the candidate pool.
var cascadeFollowTypes = []memory.EdgeType{
	memory.EdgeSupersedes, memory.EdgeContradicts, memory.EdgeDerivedFrom,
	memory.EdgeCorroborates, memory.EdgeReferences,
}

// cascadeSeedCap bounds how many seeds we expand so graph fan-out stays cheap
// (risk R4). The hop cap (2) and dedup do the rest.
const cascadeSeedCap = 6

// contradictionNote is the reconcile-first advisory attached to a surfaced
// memory that contradicts another surfaced one. Plain text (no symbols) per the
// output rules.
const contradictionNote = "conflicting memories surfaced — reconcile before trusting either"

// hopDecay returns the cascade score multiplier at hop distance hop: 0.7^hop
// (hop1 -> 0.7, hop2 -> 0.49). Cheap integer-power loop; hops are 1 or 2.
func hopDecay(hop int) float32 {
	m := float32(1)
	for i := 0; i < hop; i++ {
		m *= 0.7
	}
	return m
}

// scoredSnip pairs a rendered cascade neighbor with its hop-decayed score.
type scoredSnip struct {
	snip  Snippet
	score float32
}

// cascadeNeighbors runs a depth-2, both-direction BFS from seedURI over the
// relationship edges and returns the reachable memories rendered as snippets
// with a hop-decayed score. Best-effort context: any error or an unparseable
// seed yields no neighbors (cascade is never load-bearing on its own).
func (p *Pager) cascadeNeighbors(seedURI string) []scoredSnip {
	from := memory.URI(seedURI)
	res, err := p.cortex.Find(query.Query{
		From: &from,
		Follow: &query.EdgeExpr{
			Types:     cascadeFollowTypes,
			MaxHops:   2,
			Direction: query.DirBoth,
		},
		Limit:        p.cfg.RetrievalTopK,
		BudgetTokens: p.cfg.RetrievalBudgetTokens,
		Form:         query.FormMedium,
	})
	if err != nil || res == nil {
		return nil
	}
	out := make([]scoredSnip, 0, len(res.Memories))
	for i, m := range res.Memories {
		text := ""
		if i < len(res.Rendered) {
			text = res.Rendered[i]
		}
		if text == "" {
			text = m.Version.Forms.Medium
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, scoredSnip{
			snip: Snippet{
				Text: text,
				URI:  string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
				Type: m.Head.Type.String(),
			},
			score: hopDecay(res.Hops[m.Head.ID]),
		})
	}
	return out
}

// isSuperseded reports whether a live EdgeSupersedes points AT id (i.e. a newer
// memory supersedes it). Edges are written new -> old, so an inbound supersedes
// edge marks id as the stale side. Best-effort: an iter error reads as "not
// superseded" so a transient fault never hides a real memory.
func (p *Pager) isSuperseded(id memory.ID) bool {
	found := false
	_ = p.cortex.IterEdgesIn(id, cortex.IterEdgesOptions{
		Types: []memory.EdgeType{memory.EdgeSupersedes},
	}, func(*memory.EdgeRecord) error {
		found = true
		return nil
	})
	return found
}

// contradictsAnyOf reports whether id is joined by a live contradiction edge to
// any OTHER id in set. Contradiction is symmetric, so both directions are
// scanned.
func (p *Pager) contradictsAnyOf(id memory.ID, set map[memory.ID]bool) bool {
	hit := false
	visit := func(rec *memory.EdgeRecord) error {
		other := rec.Dst
		if other == id {
			other = rec.Src
		}
		if other != id && set[other] {
			hit = true
		}
		return nil
	}
	opts := cortex.IterEdgesOptions{Types: []memory.EdgeType{memory.EdgeContradicts}}
	_ = p.cortex.IterEdgesOut(id, opts, visit)
	_ = p.cortex.IterEdgesIn(id, opts, visit)
	return hit
}

// Recall renders an explicit, user-visible memory lookup: the pinned user
// profile plus the merged retrieval for the query. This backs Neo's
// memory_recall tool so "check your memory" is an action, not an apology.
func (p *Pager) Recall(ctx context.Context, queryText string) (string, error) {
	var b strings.Builder
	if profile := p.UserProfile(ctx); len(profile) > 0 {
		b.WriteString("User profile (durable):\n")
		for _, line := range profile {
			b.WriteString("- ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	snips, err := p.Retrieve(ctx, queryText)
	if err != nil && b.Len() == 0 {
		return "", err
	}
	if len(snips) > 0 {
		b.WriteString("Relevant memories:\n")
		for _, s := range snips {
			b.WriteString("- [")
			b.WriteString(s.Type)
			b.WriteString("] ")
			b.WriteString(strings.TrimSpace(s.Text))
			if s.Note != "" {
				b.WriteString(" [")
				b.WriteString(s.Note)
				b.WriteString("]")
			}
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "(no durable memories stored yet)", nil
	}
	return b.String(), nil
}

// Procedural returns proven how-to patterns whose trigger matches the goal,
// gated by the anti-overfit guard (coverage >= cfg.MinPatternSuccesses).
func (p *Pager) Procedural(ctx context.Context, goal string) ([]Pattern, error) {
	q := query.Query{
		Type:  []memory.Type{memory.TypePattern},
		Limit: p.cfg.RetrievalTopK,
	}
	if p.hasEmbedder && strings.TrimSpace(goal) != "" {
		q.Near = goal
		q.Type = nil
	}
	res, err := p.cortex.Find(q)
	if err != nil {
		return nil, err
	}
	var out []Pattern
	for _, m := range res.Memories {
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			continue
		}
		var pd memory.PatternData
		switch x := data.(type) {
		case memory.PatternData:
			pd = x
		case *memory.PatternData:
			pd = *x
		default:
			continue
		}
		if pd.Coverage < p.cfg.MinPatternSuccesses {
			continue // still a candidate; not yet proven enough to inject
		}
		out = append(out, Pattern{
			Spec:       DecodePatternSpec(pd.Statement),
			Confidence: pd.Strength,
			Coverage:   pd.Coverage,
			URI:        string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	return out, nil
}

func renderSnippets(res *query.Result) []Snippet {
	if res == nil {
		return nil
	}
	out := make([]Snippet, 0, len(res.Memories))
	for i, m := range res.Memories {
		text := ""
		if i < len(res.Rendered) {
			text = res.Rendered[i]
		}
		if text == "" {
			text = m.Version.Forms.Medium
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, Snippet{
			Text: text,
			URI:  string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
			Type: m.Head.Type.String(),
		})
	}
	return out
}

func truncateTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return s
	}
	maxBytes := maxTokens * memory.BytesPerToken
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "\n…(pinned block truncated)\n"
}

// EstimateTokens approximates token count with cortex's bytes/4 heuristic.
// Deterministic and dependency-free; matches the budget math elsewhere.
func EstimateTokens(s string) int {
	return (len(s) + memory.BytesPerToken - 1) / memory.BytesPerToken
}
