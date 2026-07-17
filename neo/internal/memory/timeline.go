// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	cmemory "matrix/cortex/memory"
	"matrix/cortex/query"
)

// TimelineEntry is the read-only memory summary exposed to the client.
type TimelineEntry struct {
	URI                string   `json:"uri"`
	Type               string   `json:"type"`
	Version            uint64   `json:"version"`
	CreatedAt          string   `json:"created_at,omitempty"`
	UpdatedAt          string   `json:"updated_at,omitempty"`
	CreatedBy          string   `json:"created_by,omitempty"`
	Confidence         float32  `json:"confidence,omitempty"`
	Salience           float32  `json:"salience,omitempty"`
	DeclaredImportance uint8    `json:"declared_importance,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	FormShort          string   `json:"form_short,omitempty"`
	FormMedium         string   `json:"form_medium,omitempty"`
	Tombstoned         bool     `json:"tombstoned,omitempty"`
}

// TimelineTypeCount is one populated memory type and its current count.
type TimelineTypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// TimelineQuery describes a bounded read over Neo's own durable memory.
type TimelineQuery struct {
	Near  string
	Types []string
	AsOf  *time.Time
	Limit int
}

var timelineTypes = []cmemory.Type{
	cmemory.TypeIdentity,
	cmemory.TypeFact,
	cmemory.TypePreference,
	cmemory.TypeBelief,
	cmemory.TypeEvent,
	cmemory.TypeGoal,
	cmemory.TypeConstraint,
	cmemory.TypeCapability,
	cmemory.TypePattern,
}

// Timeline returns memories from the same Cortex actor Neo uses for recall and
// writeback. Empty searches are newest-first; semantic searches retain the
// Cortex relevance order.
func (p *Pager) Timeline(spec TimelineQuery) ([]TimelineEntry, int, error) {
	if p == nil || p.cortex == nil {
		return nil, 0, fmt.Errorf("neo/memory: pager unavailable")
	}
	limit := spec.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	types, err := parseTimelineTypes(spec.Types)
	if err != nil {
		return nil, 0, err
	}
	near := strings.TrimSpace(spec.Near)
	if len(types) == 0 && near == "" {
		types = append([]cmemory.Type(nil), timelineTypes...)
	}
	q := query.Query{
		Type:  types,
		Near:  near,
		AsOf:  spec.AsOf,
		Limit: limit,
		Form:  query.FormMedium,
	}
	if near == "" {
		q.OrderBy = []query.OrderClause{{Field: query.OrderCreatedAt, Direction: query.OrderDesc}}
	}
	res, err := p.cortex.Find(q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]TimelineEntry, 0, len(res.Memories))
	total := res.Total
	for i, m := range res.Memories {
		if hasMemoryTag(m.Head, memoryConsentTag) {
			// The consent state is an internal control carrier, not a
			// user-facing memory; never surface it (or its rationale JSON).
			if total > 0 {
				total--
			}
			continue
		}
		entry := TimelineEntry{
			URI:                string(cortex.BuildURI(m.Head.Type, m.Head.ID, m.Head.CurrentVersion)),
			Type:               m.Head.Type.String(),
			Version:            m.Head.CurrentVersion,
			CreatedBy:          m.Version.CreatedBy,
			Confidence:         m.Version.Confidence,
			Salience:           res.Scores[m.Head.ID],
			DeclaredImportance: m.Head.DeclaredImportance,
			FormShort:          m.Version.Forms.Short,
			FormMedium:         m.Version.Forms.Medium,
			Tombstoned:         m.Head.Tombstoned != nil,
		}
		if i < len(res.Rendered) {
			entry.FormMedium = res.Rendered[i]
		}
		if !m.Version.CreatedAt.IsZero() {
			entry.CreatedAt = m.Version.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		if !m.Head.LastUpdatedAt.IsZero() {
			entry.UpdatedAt = m.Head.LastUpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		for _, tag := range m.Head.Tags {
			entry.Tags = append(entry.Tags, string(tag))
		}
		out = append(out, entry)
	}
	return out, total, nil
}

// TimelineTypes reports counts from Neo's own Cortex actor.
func (p *Pager) TimelineTypes() ([]TimelineTypeCount, error) {
	if p == nil || p.cortex == nil {
		return nil, fmt.Errorf("neo/memory: pager unavailable")
	}
	out := make([]TimelineTypeCount, 0, len(timelineTypes))
	for _, typ := range timelineTypes {
		ids, err := p.cortex.ListByType(typ, 0)
		if err != nil {
			return nil, err
		}
		count := len(ids)
		if typ == cmemory.TypePreference {
			// The internal consent carrier is a tagged Preference; exclude it
			// so the count reflects only user-facing memory.
			for _, id := range ids {
				mem, rerr := p.cortex.ResolveLatest(id)
				if rerr != nil || mem == nil {
					continue
				}
				if hasMemoryTag(mem.Head, memoryConsentTag) {
					count--
				}
			}
		}
		out = append(out, TimelineTypeCount{Type: typ.String(), Count: count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

func parseTimelineTypes(names []string) ([]cmemory.Type, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]cmemory.Type, 0, len(names))
	seen := make(map[cmemory.Type]bool, len(names))
	for _, name := range names {
		typ, ok := recallTypeAliases[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, fmt.Errorf("neo/memory: unknown memory type %q", name)
		}
		if !seen[typ] {
			out = append(out, typ)
			seen[typ] = true
		}
	}
	return out, nil
}
