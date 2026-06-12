// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package recall is Neo's conversational recall lane: the read-side that
// surfaces the most RELEVANT past turns of a (now unbounded) conversation
// thread — reaching PAST what the live working transcript (RAM) and the resume
// seed already hold.
//
// It is the leanest form of the Context Assembler's recall stage
// (MCL/assembler.frozen.kvx [side_lane]): turns are embedded + cosine-ranked
// IN MEMORY, recomputed lazily from the durable conversation store on demand —
// a derivable, disposable index that never touches cortex or the replay chain
// (preserving the conversation side-channel invariant). When the real Loom
// pipeline lands this becomes the cortex_recall stage's conversational source.
//
// Why this matters: Neo's RAM tier (the live transcript) and the 16-turn resume
// seed cover RECENT context, but once a turn scrolls out of RAM / is compacted
// into the lossy summary, the verbatim original is unreachable. This lane pulls
// a specific, relevant OLD turn back by semantic similarity — relevance over
// raw recency, the fix for thread-length dilution.
package recall

import (
	"context"
	"sort"
	"strings"
	"sync"

	"matrix/cortex/embed"
	"matrix/neo/internal/conversation"
)

// Hit is one recalled past turn, rendered verbatim (high-entropy tokens are
// never paraphrased — the trust contract).
type Hit struct {
	Role string // "user" | "assistant"
	Text string
}

// turnVec caches one embedded turn in thread order.
type turnVec struct {
	role string
	text string
	vec  []float32
}

// Recaller surfaces relevant past turns of ONE conversation thread. It is
// session-scoped (one per conversation) and serialized by a mutex, so the
// opening fault and the mid-turn refault never race the embedding cache. The
// cache is lazy + incremental: only turns appended since the last call are
// embedded, so steady-state cost is one embed per new turn.
type Recaller struct {
	conv   *conversation.Store
	convID string
	emb    embed.Embedder
	topK   int
	budget int // token ceiling (bytes/4 heuristic) for the recalled block

	mu       sync.Mutex
	cache    []turnVec // embedded turns, thread order
	embedded int       // count of source turns already folded into cache
}

// New builds a Recaller for convID. A nil embedder or disabled store yields a
// safe no-op recaller (Relevant returns nil). topK / budgetTokens fall back to
// sane defaults when non-positive.
func New(conv *conversation.Store, convID string, emb embed.Embedder, topK, budgetTokens int) *Recaller {
	if topK <= 0 {
		topK = 6
	}
	if budgetTokens <= 0 {
		budgetTokens = 2500
	}
	return &Recaller{
		conv:   conv,
		convID: strings.TrimSpace(convID),
		emb:    emb,
		topK:   topK,
		budget: budgetTokens,
	}
}

func (r *Recaller) enabled() bool {
	return r != nil && r.emb != nil && r.conv != nil && r.conv.Enabled() && r.convID != ""
}

// Relevant returns up to topK past turns most similar to queryText, ranked by
// cosine similarity and bounded by the token budget. Best-effort: any embed
// failure degrades to fewer/no hits rather than erroring (the RAM tier + recent
// tail still carry the turn). The caller is expected to drop any hit already
// present in the live transcript (dedup lives at the agent boundary).
func (r *Recaller) Relevant(ctx context.Context, queryText string) []Hit {
	_ = ctx // reserved: the real Loom recall stage is context-aware
	if !r.enabled() {
		return nil
	}
	queryText = strings.TrimSpace(queryText)
	if queryText == "" {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.refreshLocked()
	if len(r.cache) == 0 {
		return nil
	}

	qv, err := r.emb.Embed(queryText)
	if err != nil {
		return nil
	}

	type scored struct {
		idx   int
		score float32
	}
	scores := make([]scored, len(r.cache))
	for i := range r.cache {
		scores[i] = scored{i, embed.Cosine(qv, r.cache[i].vec)}
	}
	sort.SliceStable(scores, func(a, b int) bool { return scores[a].score > scores[b].score })

	var (
		out  []Hit
		used int
	)
	for _, s := range scores {
		if len(out) >= r.topK {
			break
		}
		tv := r.cache[s.idx]
		cost := (len(tv.text) + 3) / 4
		if used+cost > r.budget && len(out) > 0 {
			break
		}
		out = append(out, Hit{Role: tv.role, Text: tv.text})
		used += cost
	}
	return out
}

// refreshLocked embeds any turns appended since the last call, extending the
// cache. On an embed error it stops and leaves the remaining turns for a later
// pass (embedded is not advanced past the failed turn), so transient embedder
// hiccups self-heal. Caller MUST hold r.mu.
func (r *Recaller) refreshLocked() {
	rec := r.conv.Get(r.convID)
	if rec == nil {
		return
	}
	turns := rec.Turns
	for i := r.embedded; i < len(turns); i++ {
		text := strings.TrimSpace(turns[i].Text)
		if text == "" {
			r.embedded = i + 1
			continue
		}
		vec, err := r.emb.Embed(text)
		if err != nil {
			return // retry from i on the next pass
		}
		r.cache = append(r.cache, turnVec{role: turns[i].Role, text: text, vec: vec})
		r.embedded = i + 1
	}
}
