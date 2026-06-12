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

// Retrieve page-faults the top-K records relevant to queryText. Semantic
// (HNSW) results lead when an embedder is running, ALWAYS merged with a
// salience-ranked lane over the durable types: the embedding worker is
// async, so a memory written seconds ago is invisible to the vector index —
// without the salience lane a "remember this" → "what do you know?" round
// trip inside one session comes back empty. Bounded by RetrievalTopK total.
func (p *Pager) Retrieve(ctx context.Context, queryText string) ([]Snippet, error) {
	var (
		out  []Snippet
		seen = map[string]bool{}
	)
	add := func(snips []Snippet) {
		for _, s := range snips {
			if len(out) >= p.cfg.RetrievalTopK {
				return
			}
			if s.URI == "" || seen[s.URI] {
				continue
			}
			seen[s.URI] = true
			out = append(out, s)
		}
	}

	if p.hasEmbedder && strings.TrimSpace(queryText) != "" {
		res, err := p.cortex.Find(query.Query{
			Near:         queryText,
			Limit:        p.cfg.RetrievalTopK,
			BudgetTokens: p.cfg.RetrievalBudgetTokens,
			Form:         query.FormMedium,
		})
		if err == nil {
			add(renderSnippets(res))
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
	if err != nil {
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}
	add(renderSnippets(res))
	return out, nil
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
