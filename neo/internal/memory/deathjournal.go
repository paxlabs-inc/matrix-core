// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// deathJournalTag marks a cortex Event as a supervised-loop death record, so the
// death journal is a FIRST-CLASS, directly-queryable set (self-model task 3.1,
// req.4.2) — the self-authoring consolidation pass (task 3.2) reads it by tag
// rather than guessing which failure Events were loop deaths. The tag is
// ADDITIVE: a death record is still a TypeEvent failure observation, so ordinary
// recall/Activate keeps surfacing it in the working set when salient (req.4.2).
// Unlike the opportunity tag it does NOT exclude the record from recall — it
// only makes the journal readable as a whole.
const deathJournalTag memory.Tag = "neo:death-journal"

// deathJournalScanCap bounds how many death records the journal reader scans
// before ordering. The journal is small by design (one entry per supervised
// respawn, importance-6 Events that the normal salience trim keeps bounded), so
// a generous fixed cap surfaces the whole recent journal.
const deathJournalScanCap = 256

// DeathRecord is one durable death-journal entry read back from cortex — the
// first-class read side of the durable death path (self-model task 3.1). The
// Summary is the full death-journal line written by RecordLoopDeath (objective,
// attempt, class, where-it-got-stuck digest, and the rich loop-state suffix), so
// a consumer (the self-authoring pass, the observability surface) can parse the
// failure MODE from it without a second fetch.
type DeathRecord struct {
	URI       string
	Summary   string
	CreatedAt time.Time
}

// deathJournalHead is p.head with the death-journal tag attached, so a death
// record is discoverable as a member of the journal while remaining an ordinary
// recallable failure Event.
func (p *Pager) deathJournalHead(importance uint8) memory.Head {
	h := p.head(importance)
	h.Tags = []memory.Tag{deathJournalTag}
	return h
}

// DeathJournal returns the durable death-journal entries newest-first, capped at
// limit (<=0 = all, up to the scan cap). It is the first-class read side of the
// durable death path (self-model task 3.1, req.4.2): where ordinary recall
// surfaces a salient death by semantic match, this returns the journal AS a
// journal — the accumulated set the self-authoring consolidation pass (task 3.2)
// reads to write durable how-I-fail memories, and the observability surface
// (req.13) inspects. Best-effort: a store error or a nil result yields an empty
// journal rather than failing a caller that treats the journal as advisory.
func (p *Pager) DeathJournal(ctx context.Context, limit int) ([]DeathRecord, error) {
	_ = ctx
	if p == nil || p.cortex == nil {
		return nil, nil
	}
	res, err := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(deathJournalTag)},
		Limit: deathJournalScanCap,
		Form:  query.FormMedium,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	out := make([]DeathRecord, 0, len(res.Memories))
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypeEvent || !headHasDeathJournalTag(m.Head) {
			continue
		}
		data, derr := memory.DecodeData(m.Version.Type, m.Version.Data)
		if derr != nil {
			continue
		}
		summary := strings.TrimSpace(eventSummary(data))
		if summary == "" {
			continue
		}
		out = append(out, DeathRecord{
			URI:       string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
			Summary:   summary,
			CreatedAt: m.Version.CreatedAt,
		})
	}
	// Newest first — a respawn cares most about how it just died, and the
	// consolidation pass reinforces the freshest recurrence. Deaths written in a
	// tight respawn burst can share a sub-resolution CreatedAt, so the URI (which
	// embeds a monotonic ULID that sorts chronologically) is the tiebreak.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].URI > out[j].URI
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// headHasDeathJournalTag reports whether a head carries the death-journal tag.
func headHasDeathJournalTag(h memory.Head) bool {
	for _, t := range h.Tags {
		if t == deathJournalTag {
			return true
		}
	}
	return false
}

// eventSummary extracts the one-line Summary from a decoded EventData (value or
// pointer form), or "" for any other decoded type.
func eventSummary(data any) string {
	switch e := data.(type) {
	case memory.EventData:
		return e.Summary
	case *memory.EventData:
		return e.Summary
	default:
		return ""
	}
}
