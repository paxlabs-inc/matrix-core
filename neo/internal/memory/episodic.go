// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
)

type EpisodicTimeWindow struct {
	From  time.Time
	Until time.Time
}

type EpisodicBudget struct {
	Tokens              int
	Hits                int
	Deadline            time.Duration
	Radius              int
	ExcludeConversation string
}

type EpisodicCurrentHit struct {
	Role string
	Text string
}

type EpisodicExcerpt struct {
	Ref             string
	ConversationID  string
	Date            time.Time
	SeqLo           uint64
	SeqHi           uint64
	Exact           bool
	Text            string
	RelatedMemories []Snippet
}

func (p *Pager) EpisodicRetrieve(ctx context.Context, referent string, window EpisodicTimeWindow, budget EpisodicBudget, current []EpisodicCurrentHit) (out []EpisodicExcerpt) {
	if p == nil || p.cortex == nil || strings.TrimSpace(referent) == "" {
		return nil
	}
	if budget.Tokens <= 0 {
		budget.Tokens = p.cfg.RetrievalBudgetTokens
	}
	if budget.Hits <= 0 {
		budget.Hits = p.cfg.RetrievalTopK
	}
	if budget.Deadline <= 0 {
		budget.Deadline = 1500 * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, budget.Deadline)
	defer cancel()

	type fused struct {
		ex    EpisodicExcerpt
		score float64
		order int
	}
	pool := map[string]*fused{}
	order := 0
	addLane := func(ex EpisodicExcerpt, rank int) {
		if strings.TrimSpace(ex.Text) == "" {
			return
		}
		row := pool[ex.Ref]
		if row == nil {
			row = &fused{ex: ex, order: order}
			pool[ex.Ref] = row
			order++
		} else {
			row.ex.RelatedMemories = append(row.ex.RelatedMemories, ex.RelatedMemories...)
		}
		row.score += 1 / float64(60+rank)
	}

	snips, err := p.Retrieve(callCtx, referent)
	if err == nil {
		for rank, snip := range snips {
			if callCtx.Err() != nil {
				return nil
			}
			slice, xerr := p.ExpandToTranscript(snip.URI, budget.Radius)
			if xerr != nil || slice == nil || len(slice.Messages) == 0 {
				continue
			}
			if !withinEpisodicWindow(slice.Date, window) {
				continue
			}
			var b strings.Builder
			for _, msg := range slice.Messages {
				line := strings.TrimSpace(msg.Content)
				if line == "" {
					continue
				}
				fmt.Fprintf(&b, "%s: %s\n", msg.Role, line)
			}
			ref := fmt.Sprintf("session:%s:%d-%d", slice.ConversationID, slice.SeqLo, slice.SeqHi)
			addLane(EpisodicExcerpt{Ref: ref, ConversationID: slice.ConversationID, Date: slice.Date, SeqLo: slice.SeqLo, SeqHi: slice.SeqHi, Exact: slice.Exact, Text: strings.TrimSpace(b.String()), RelatedMemories: []Snippet{snip}}, rank+1)
		}
	}

	if lexical, err := p.cortex.QueryLexical(referent, window.From, window.Until, budget.Hits); err == nil {
		for rank, hit := range lexical {
			if hit.ConversationID == budget.ExcludeConversation {
				continue
			}
			if ex, ok := p.lexicalExcerpt(hit, budget.Radius); ok {
				for ref, row := range pool {
					if row.ex.ConversationID == hit.ConversationID && hit.Seq >= row.ex.SeqLo && hit.Seq <= row.ex.SeqHi {
						ex.Ref = ref
						break
					}
				}
				addLane(ex, rank+1)
			}
		}
	}
	for rank, hit := range current {
		addLane(EpisodicExcerpt{Ref: fmt.Sprintf("current:%d:%s", rank, strings.TrimSpace(hit.Text)), ConversationID: "current", Exact: true, Text: strings.TrimSpace(hit.Role) + ": " + strings.TrimSpace(hit.Text)}, rank+1)
	}

	ranked := make([]*fused, 0, len(pool))
	for _, row := range pool {
		ranked = append(ranked, row)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].order < ranked[j].order
	})
	used := 0
	for _, row := range ranked {
		if callCtx.Err() != nil {
			return nil
		}
		cost := (len(row.ex.Text) + 3) / 4
		if used+cost > budget.Tokens && len(out) > 0 {
			continue
		}
		out = append(out, row.ex)
		used += cost
		if len(out) >= budget.Hits {
			break
		}
	}
	return out
}

func (p *Pager) lexicalExcerpt(hit cortex.LexicalHit, radius int) (EpisodicExcerpt, bool) {
	from := hit.Seq
	if uint64(radius) > from {
		from = 0
	} else {
		from -= uint64(radius)
	}
	msgs, err := p.Transcript(hit.ConversationID, from, 2*radius+1)
	if err != nil || len(msgs) == 0 {
		return EpisodicExcerpt{}, false
	}
	var b strings.Builder
	for _, msg := range msgs {
		if line := strings.TrimSpace(msg.Content); line != "" {
			fmt.Fprintf(&b, "%s: %s\n", msg.Role, line)
		}
	}
	return EpisodicExcerpt{Ref: fmt.Sprintf("session:%s:%d-%d", hit.ConversationID, from, from+uint64(len(msgs))-1), ConversationID: hit.ConversationID, Date: hit.Date, SeqLo: from, SeqHi: from + uint64(len(msgs)) - 1, Exact: true, Text: strings.TrimSpace(b.String())}, b.Len() > 0
}

func withinEpisodicWindow(at time.Time, w EpisodicTimeWindow) bool {
	if w.From.IsZero() || w.Until.IsZero() || !w.From.Before(w.Until) {
		return true
	}
	return !at.Before(w.From) && at.Before(w.Until)
}
