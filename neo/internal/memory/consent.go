// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/cortex"
	cmemory "matrix/cortex/memory"
)

const (
	memoryConsentNoticeVersion = "2026-07-17"
	memoryConsentSidecarPrefix = "neo.memory.consent.v1:"
)

var memoryConsentTag cmemory.Tag = "memory-consent"

// MemoryConsentState is the auditable opt-in state for durable extraction.
// No record means the explicit default: disabled.
type MemoryConsentState struct {
	Enabled       bool   `json:"enabled"`
	Explicit      bool   `json:"explicit"`
	NoticeVersion string `json:"notice_version"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	UpdatedBy     string `json:"updated_by,omitempty"`
	ExistingData  string `json:"existing_data"`
}

type MemoryExportItem struct {
	URI       string      `json:"uri"`
	Type      string      `json:"type"`
	CreatedAt string      `json:"created_at,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
	Data      interface{} `json:"data"`
}

type MemoryExport struct {
	SchemaVersion int                `json:"schema_version"`
	ExportedAt    string             `json:"exported_at"`
	Consent       MemoryConsentState `json:"consent"`
	Items         []MemoryExportItem `json:"items"`
}

func defaultMemoryConsent() MemoryConsentState {
	return MemoryConsentState{
		Enabled:       false,
		Explicit:      false,
		NoticeVersion: memoryConsentNoticeVersion,
		ExistingData:  "retained until you edit, delete, or use the receipt-backed delete-all pipeline",
	}
}

func (p *Pager) MemoryConsent(_ context.Context) (MemoryConsentState, error) {
	mem, err := p.findMemoryConsent()
	if err != nil {
		return MemoryConsentState{}, err
	}
	if mem == nil {
		return defaultMemoryConsent(), nil
	}
	state := defaultMemoryConsent()
	data, err := cmemory.DecodeData(mem.Version.Type, mem.Version.Data)
	if err != nil {
		return MemoryConsentState{}, err
	}
	pref, ok := data.(cmemory.PreferenceData)
	if !ok || !strings.HasPrefix(pref.Rationale, memoryConsentSidecarPrefix) {
		return MemoryConsentState{}, errors.New("neo/memory: malformed consent record")
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(pref.Rationale, memoryConsentSidecarPrefix)), &state); err != nil {
		return MemoryConsentState{}, fmt.Errorf("neo/memory: decode consent: %w", err)
	}
	state.Explicit = true
	return state, nil
}

func (p *Pager) SetMemoryConsent(ctx context.Context, enabled bool, by string) (MemoryConsentState, error) {
	by = strings.TrimSpace(by)
	if by == "" {
		by = "user"
	}
	// Drain the async embedder before and after the versioned consent update,
	// matching the typed-mutation race protection: a queued embed of the prior
	// consent version must not land after the new one, and callers reading the
	// state immediately after must observe the settled index.
	if p.hasEmbedder {
		if err := p.cortex.DrainEmbedder(ctx); err != nil {
			return MemoryConsentState{}, fmt.Errorf("prepare consent indexes: %w", err)
		}
		defer func() { _ = p.cortex.DrainEmbedder(ctx) }()
	}
	state := defaultMemoryConsent()
	state.Enabled = enabled
	state.Explicit = true
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.UpdatedBy = by
	raw, err := json.Marshal(state)
	if err != nil {
		return MemoryConsentState{}, err
	}
	polarity := cmemory.PolarityAvoid
	if enabled {
		polarity = cmemory.PolarityPrefer
	}
	data := cmemory.PreferenceData{
		SchemaVersion: 1,
		Topic:         "durable personalization",
		Polarity:      polarity,
		StrengthVal:   1,
		Rationale:     memoryConsentSidecarPrefix + string(raw),
	}
	meta := cortex.WriteMeta{
		CreatedBy:  p.cfg.CortexActor,
		Confidence: 1,
		Provenance: cmemory.Provenance{Source: cmemory.SourceUserInput},
	}
	existing, err := p.findMemoryConsent()
	if err != nil {
		return MemoryConsentState{}, err
	}
	if existing != nil {
		uri := cortex.BuildURI(existing.Head.Type, existing.Head.ID, existing.Head.CurrentVersion)
		if _, err := p.cortex.Update(uri, data, meta); err != nil {
			return MemoryConsentState{}, err
		}
		return state, nil
	}
	_, err = p.cortex.Write(cmemory.Head{
		ActorScope:         p.cfg.CortexActor,
		Visibility:         cmemory.VisPrivate,
		DeclaredImportance: 10,
		Tags:               []cmemory.Tag{memoryConsentTag},
	}, data, meta)
	if err != nil {
		return MemoryConsentState{}, err
	}
	return state, nil
}

func (p *Pager) MemoryConsentEnabled() bool {
	state, err := p.MemoryConsent(context.Background())
	return err == nil && state.Enabled && state.Explicit
}

func (p *Pager) findMemoryConsent() (*cmemory.Memory, error) {
	ids, err := p.cortex.ListByType(cmemory.TypePreference, 0)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		mem, err := p.cortex.ResolveLatest(id)
		if err != nil {
			if errors.Is(err, cmemory.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if current, _ := cmemory.CurrentTruthAt(&mem.Head, &mem.Version, time.Now().UTC()); !current {
			continue
		}
		for _, tag := range mem.Head.Tags {
			if tag == memoryConsentTag {
				return mem, nil
			}
		}
	}
	return nil, nil
}

func (p *Pager) ExportMemories(ctx context.Context) (MemoryExport, error) {
	consent, err := p.MemoryConsent(ctx)
	if err != nil {
		return MemoryExport{}, err
	}
	out := MemoryExport{
		SchemaVersion: 1,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Consent:       consent,
		Items:         make([]MemoryExportItem, 0),
	}
	for _, typ := range timelineTypes {
		ids, err := p.cortex.ListByType(typ, 0)
		if err != nil {
			return MemoryExport{}, err
		}
		for _, id := range ids {
			mem, err := p.cortex.ResolveLatest(id)
			if err != nil {
				continue
			}
			if current, _ := cmemory.CurrentTruthAt(&mem.Head, &mem.Version, time.Now().UTC()); !current || hasMemoryTag(mem.Head, memoryConsentTag) {
				continue
			}
			data, err := cmemory.DecodeData(mem.Version.Type, mem.Version.Data)
			if err != nil {
				return MemoryExport{}, err
			}
			item := MemoryExportItem{
				URI:       string(cortex.BuildURI(mem.Head.Type, mem.Head.ID, mem.Head.CurrentVersion)),
				Type:      mem.Head.Type.String(),
				CreatedAt: mem.Version.CreatedAt.UTC().Format(time.RFC3339Nano),
				UpdatedAt: mem.Head.LastUpdatedAt.UTC().Format(time.RFC3339Nano),
				Data:      data,
			}
			out.Items = append(out.Items, item)
		}
	}
	return out, nil
}

func hasMemoryTag(head cmemory.Head, want cmemory.Tag) bool {
	for _, tag := range head.Tags {
		if tag == want {
			return true
		}
	}
	return false
}
