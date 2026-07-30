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

// deathJournalTag identifies legacy failure Events written before the death
// journal moved out of ordinary memory. Read paths filter these old records.
const deathJournalTag memory.Tag = "neo:death-journal"

// deathJournalConversation is a dedicated derived session lane. It is
// journaled and vault-sealed by cortex.AppendMessage, but is not an anchored
// memory and never enters another conversation's ambient activation.
const deathJournalConversation = "neo-death-journal"

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
	IntentID  string
	Summary   string
	CreatedAt time.Time
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
	messages, err := p.cortex.Transcript(deathJournalConversation, 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]DeathRecord, 0, len(messages))
	for _, message := range messages {
		if message.Role != cortex.RoleToolResult {
			continue
		}
		summary := strings.TrimSpace(message.Content)
		if summary == "" {
			continue
		}
		out = append(out, DeathRecord{
			URI:       string(cortex.BuildSessionURI(deathJournalConversation, message.Seq)),
			IntentID:  strings.TrimSpace(message.ToolName),
			Summary:   summary,
			CreatedAt: time.Unix(0, message.TS).UTC(),
		})
	}
	legacy, legacyErr := p.cortex.Find(query.Query{
		Where: query.HasTag{Tag: string(deathJournalTag)},
		Limit: deathJournalScanCap,
		Form:  query.FormMedium,
	})
	if legacyErr == nil && legacy != nil {
		for _, item := range legacy.Memories {
			data, decodeErr := memory.DecodeData(item.Version.Type, item.Version.Data)
			if decodeErr != nil {
				continue
			}
			event, ok := asEvent(data)
			if !ok || strings.TrimSpace(event.Summary) == "" {
				continue
			}
			out = append(out, DeathRecord{
				URI:       string(cortex.BuildURI(item.Head.Type, item.Head.ID, item.Head.CurrentVersion)),
				IntentID:  strings.TrimSpace(event.IntentRef),
				Summary:   strings.TrimSpace(event.Summary),
				CreatedAt: item.Version.CreatedAt,
			})
		}
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

// headHasDeathJournalTag reports whether a head carries the legacy tag.
func headHasDeathJournalTag(h memory.Head) bool {
	for _, t := range h.Tags {
		if t == deathJournalTag {
			return true
		}
	}
	return false
}
