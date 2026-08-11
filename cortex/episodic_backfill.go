// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"bytes"
	"fmt"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

var episodicBackfillKey = []byte("chk/deja-vu/backfill")

type episodicBackfillCheckpoint struct {
	SchemaVersion uint8  `cbor:"0,keyasint"`
	Phase         string `cbor:"1,keyasint"`
	LastKey       []byte `cbor:"2,keyasint,omitempty"`
	Complete      bool   `cbor:"3,keyasint"`
}

type EpisodicBackfillResult struct {
	Processed int
	Indexed   int
	Linked    int
	Complete  bool
}

func (c *Cortex) EpisodicBackfill(batch int, createdBy string) (EpisodicBackfillResult, error) {
	if batch <= 0 {
		batch = 128
	}
	cp := episodicBackfillCheckpoint{SchemaVersion: 1, Phase: "sessions"}
	if raw, ok, err := c.s.Get(episodicBackfillKey); err != nil {
		return EpisodicBackfillResult{}, err
	} else if ok {
		if err := sessDec.Unmarshal(raw, &cp); err != nil {
			return EpisodicBackfillResult{}, fmt.Errorf("cortex.EpisodicBackfill: checkpoint: %w", err)
		}
	}
	if cp.Complete {
		return EpisodicBackfillResult{Complete: true}, nil
	}
	if cp.Phase == "sessions" {
		return c.backfillSessionBatch(cp, batch)
	}
	return c.backfillMemoryBatch(cp, batch, createdBy)
}

func (c *Cortex) backfillSessionBatch(cp episodicBackfillCheckpoint, batch int) (EpisodicBackfillResult, error) {
	type row struct{ key, value []byte }
	var selected []row
	if err := c.s.PrefixIter(keys.PrefixSession, func(k, v []byte) error {
		if len(cp.LastKey) > 0 && bytes.Compare(k, cp.LastKey) <= 0 {
			return nil
		}
		if len(selected) < batch {
			selected = append(selected, row{append([]byte(nil), k...), append([]byte(nil), v...)})
		}
		return nil
	}); err != nil {
		return EpisodicBackfillResult{}, err
	}
	rows := map[string][]byte{}
	for _, item := range selected {
		var rec SessionRecord
		if err := DecodeSessionRecord(item.value, &rec); err != nil {
			return EpisodicBackfillResult{}, err
		}
		add, err := lexicalRows(&rec)
		if err != nil {
			return EpisodicBackfillResult{}, err
		}
		for k, v := range add {
			rows[k] = v
		}
		cp.LastKey = item.key
	}
	if err := c.s.SetDerivedRows(rows); err != nil {
		return EpisodicBackfillResult{}, err
	}
	if len(selected) < batch {
		cp.Phase, cp.LastKey = "memories", nil
	}
	if err := c.saveEpisodicBackfill(cp); err != nil {
		return EpisodicBackfillResult{}, err
	}
	return EpisodicBackfillResult{Processed: len(selected), Indexed: len(selected)}, nil
}

func (c *Cortex) backfillMemoryBatch(cp episodicBackfillCheckpoint, batch int, createdBy string) (EpisodicBackfillResult, error) {
	type row struct {
		key  []byte
		head memory.Head
	}
	var selected []row
	if err := c.s.PrefixIter(keys.PrefixMemoryHead, func(k, v []byte) error {
		if len(cp.LastKey) > 0 && bytes.Compare(k, cp.LastKey) <= 0 {
			return nil
		}
		if len(selected) >= batch {
			return nil
		}
		var head memory.Head
		if err := memory.DecodeHead(v, &head); err != nil {
			return err
		}
		selected = append(selected, row{append([]byte(nil), k...), head})
		return nil
	}); err != nil {
		return EpisodicBackfillResult{}, err
	}
	linked := 0
	for _, item := range selected {
		has, err := c.hasSessionProvenance(item.head.ID)
		if err != nil {
			return EpisodicBackfillResult{}, err
		}
		if !has {
			slice, _ := c.transcriptSliceApprox(item.head.ID, 0)
			if slice != nil && len(slice.Messages) > 0 {
				if err := c.AddSessionProvenanceHeuristic(item.head.ID, slice.ConversationID, slice.SeqLo, slice.SeqHi, createdBy); err != nil {
					return EpisodicBackfillResult{}, err
				}
				linked++
			}
		}
		cp.LastKey = item.key
	}
	if len(selected) < batch {
		cp.Complete = true
	}
	if err := c.saveEpisodicBackfill(cp); err != nil {
		return EpisodicBackfillResult{}, err
	}
	return EpisodicBackfillResult{Processed: len(selected), Linked: linked, Complete: cp.Complete}, nil
}

func (c *Cortex) hasSessionProvenance(id memory.ID) (bool, error) {
	found := false
	err := c.IterEdgesOut(id, IterEdgesOptions{Types: []memory.EdgeType{memory.EdgeDerivedFrom}}, func(rec *memory.EdgeRecord) error {
		var sp sessionProvenance
		if len(rec.Data) > 0 && sessDec.Unmarshal(rec.Data, &sp) == nil && sp.SchemaVersion == SessionProvenanceSchemaVersion && sp.ConversationID != "" {
			found = true
		}
		return nil
	})
	return found, err
}

func (c *Cortex) saveEpisodicBackfill(cp episodicBackfillCheckpoint) error {
	raw, err := sessEnc.Marshal(cp)
	if err != nil {
		return err
	}
	return c.s.SetDerivedRows(map[string][]byte{string(episodicBackfillKey): raw})
}
