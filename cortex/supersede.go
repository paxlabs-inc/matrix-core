// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"errors"
	"fmt"
	"time"

	"matrix/cortex/forms"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/store"
)

// SupersedeOptions controls an atomic replacement. ReplacementURI is optional:
// when empty a new memory is created from Head; when present that distinct live
// memory is updated in the same transaction and becomes the replacement.
type SupersedeOptions struct {
	ReplacementURI memory.URI
	Head           memory.Head
	WriteMeta      WriteMeta
	EdgeMeta       AddEdgeMeta
}

type preparedSupersedeMemory struct {
	uri          memory.URI
	id           memory.ID
	typ          memory.Type
	headBytes    []byte
	version      uint64
	versionBytes []byte
	scoreBytes   []byte
	created      bool
	head         memory.Head
	journalKind  journal.Kind
	journalBy    string
	journalHash  [32]byte
}

// Supersede atomically writes or updates a replacement, records the
// replacement -> prior EdgeSupersedes provenance, and closes the prior
// memory's valid-time interval. No observable state is committed unless every
// part succeeds.
func (c *Cortex) Supersede(priorURI memory.URI, replacement memory.TypedData, opts SupersedeOptions) (memory.URI, error) {
	if replacement == nil {
		return "", memory.ErrEmptyData
	}
	priorType, priorID, _, err := ParseURI(priorURI)
	if err != nil {
		return "", err
	}
	if memory.TypeOf(replacement) != priorType {
		return "", memory.ErrTypeDataMismatch
	}
	prior, err := c.ResolveLatest(priorID)
	if err != nil {
		return "", fmt.Errorf("cortex.Supersede: resolve prior: %w", err)
	}
	if prior.Head.Tombstoned != nil {
		return "", memory.ErrTombstoned
	}
	if prior.Version.ValidUntil != nil {
		return "", errors.New("cortex.Supersede: prior memory is already superseded")
	}

	now := c.now()
	repl, err := c.prepareSupersedeReplacement(priorID, replacement, opts, now)
	if err != nil {
		return "", err
	}
	closedHead, closedVersion, closedHash, err := prepareValidityClose(prior, now, opts.WriteMeta.CreatedBy)
	if err != nil {
		return "", err
	}
	closedHeadBytes, err := memory.EncodeHead(&closedHead)
	if err != nil {
		return "", err
	}
	closedVersionBytes, err := memory.EncodeVersion(&closedVersion)
	if err != nil {
		return "", err
	}

	edge, edgeBytes, err := c.prepareSupersedesEdge(repl.id, priorID, opts.EdgeMeta, now)
	if err != nil {
		return "", err
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := c.stageSupersedeReplacement(wb, repl, now); err != nil {
		return "", err
	}
	if err := c.stageSupersedesEdge(wb, edge, edgeBytes, opts.EdgeMeta, now); err != nil {
		return "", err
	}
	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(priorID)), closedHeadBytes); err != nil {
		return "", err
	}
	if err := wb.Set(keys.MemoryVersionKey(toKeysULID(priorID), closedVersion.Version), closedVersionBytes); err != nil {
		return "", err
	}
	closePayload, err := journal.EncodeWritePayload(&journal.WritePayload{
		SchemaVersion: 1,
		ID:            priorID,
		Version:       closedVersion.Version,
		Type:          uint8(priorType),
		Hash:          closedHash,
	})
	if err != nil {
		return "", fmt.Errorf("cortex.Supersede: encode close payload: %w", err)
	}
	if err := wb.AppendJournal(&journal.Entry{
		Kind:      journal.KindUpdate,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(opts.WriteMeta.CreatedBy),
		Payload:   closePayload,
	}); err != nil {
		return "", err
	}
	if err := c.snap.StageMemoryUpdate(wb, priorID, closedHeadBytes); err != nil {
		return "", fmt.Errorf("cortex.Supersede: stage prior SMT: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.Supersede: commit: %w", err)
	}
	c.notifyEmbedder()
	return repl.uri, nil
}

func (c *Cortex) prepareSupersedeReplacement(priorID memory.ID, data memory.TypedData, opts SupersedeOptions, now time.Time) (preparedSupersedeMemory, error) {
	if opts.ReplacementURI == "" {
		h := opts.Head
		if h.ID.IsZero() {
			h.ID = c.idGen()
		}
		if h.ID == priorID {
			return preparedSupersedeMemory{}, memory.ErrSelfEdge
		}
		h.Type = memory.TypeOf(data)
		h.CurrentVersion = 1
		if h.Visibility == 0 {
			h.Visibility = memory.VisPrivate
		}
		h.LastUpdatedAt = now
		formsOut := opts.WriteMeta.Forms
		if !opts.WriteMeta.FormsOverride {
			formsOut = forms.Render(&h, data)
		}
		h.Forms = formsOut
		v := memory.Version{
			ID: h.ID, Version: 1, Type: h.Type,
			CreatedAt: now, CreatedBy: opts.WriteMeta.CreatedBy,
			Confidence: opts.WriteMeta.Confidence, Provenance: opts.WriteMeta.Provenance,
			Forms: formsOut, FormsOverride: opts.WriteMeta.FormsOverride,
			ValidFrom: opts.WriteMeta.ValidFrom, ValidUntil: opts.WriteMeta.ValidUntil,
		}
		if v.Confidence == 0 {
			v.Confidence = 1
		}
		var err error
		v.Data, err = memory.EncodeData(data)
		if err != nil {
			return preparedSupersedeMemory{}, fmt.Errorf("cortex.Supersede: encode replacement: %w", err)
		}
		if err := memory.ValidateMemory(&h, &v, data); err != nil {
			return preparedSupersedeMemory{}, fmt.Errorf("cortex.Supersede: validate replacement: %w", err)
		}
		v.Hash, err = memory.HashVersion(&v)
		if err != nil {
			return preparedSupersedeMemory{}, err
		}
		hb, err := memory.EncodeHead(&h)
		if err != nil {
			return preparedSupersedeMemory{}, err
		}
		vb, err := memory.EncodeVersion(&v)
		if err != nil {
			return preparedSupersedeMemory{}, err
		}
		score := salience.NewForWrite(h.DeclaredImportance, now)
		sb, err := salience.Encode(&score)
		if err != nil {
			return preparedSupersedeMemory{}, err
		}
		return preparedSupersedeMemory{
			uri: BuildURI(h.Type, h.ID, 1), id: h.ID, typ: h.Type,
			headBytes: hb, version: 1, versionBytes: vb, scoreBytes: sb,
			created: true, head: h, journalKind: journal.KindWrite,
			journalBy: opts.WriteMeta.CreatedBy, journalHash: v.Hash,
		}, nil
	}

	t, id, _, err := ParseURI(opts.ReplacementURI)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	if id == priorID {
		return preparedSupersedeMemory{}, memory.ErrSelfEdge
	}
	if t != memory.TypeOf(data) {
		return preparedSupersedeMemory{}, memory.ErrTypeDataMismatch
	}
	prev, err := c.ResolveLatest(id)
	if err != nil {
		return preparedSupersedeMemory{}, fmt.Errorf("cortex.Supersede: resolve replacement: %w", err)
	}
	if prev.Head.Tombstoned != nil {
		return preparedSupersedeMemory{}, memory.ErrTombstoned
	}
	h := prev.Head
	h.CurrentVersion++
	h.LastUpdatedAt = now
	formsOut := opts.WriteMeta.Forms
	if !opts.WriteMeta.FormsOverride {
		formsOut = forms.Render(&h, data)
	}
	h.Forms = formsOut
	encoded, err := memory.EncodeData(data)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	v := memory.Version{
		ID: id, Version: h.CurrentVersion, Type: t, Data: encoded,
		CreatedAt: now, CreatedBy: opts.WriteMeta.CreatedBy,
		Confidence: opts.WriteMeta.Confidence, Provenance: opts.WriteMeta.Provenance,
		Forms: formsOut, FormsOverride: opts.WriteMeta.FormsOverride,
		ValidFrom: opts.WriteMeta.ValidFrom, ValidUntil: opts.WriteMeta.ValidUntil,
	}
	if v.Confidence == 0 {
		v.Confidence = 1
	}
	if err := memory.ValidateMemory(&h, &v, data); err != nil {
		return preparedSupersedeMemory{}, fmt.Errorf("cortex.Supersede: validate replacement update: %w", err)
	}
	v.Hash, err = memory.HashVersion(&v)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	hb, err := memory.EncodeHead(&h)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	vb, err := memory.EncodeVersion(&v)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	scorePtr, ok, err := salience.Read(c.s, id)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	var score salience.Score
	if ok {
		score = *scorePtr
		salience.BumpForUpdate(&score, h.DeclaredImportance, now)
	} else {
		score = salience.NewForWrite(h.DeclaredImportance, now)
	}
	sb, err := salience.Encode(&score)
	if err != nil {
		return preparedSupersedeMemory{}, err
	}
	return preparedSupersedeMemory{
		uri: BuildURI(t, id, v.Version), id: id, typ: t,
		headBytes: hb, version: v.Version, versionBytes: vb, scoreBytes: sb,
		head: h, journalKind: journal.KindUpdate,
		journalBy: opts.WriteMeta.CreatedBy, journalHash: v.Hash,
	}, nil
}

func prepareValidityClose(prev *memory.Memory, until time.Time, by string) (memory.Head, memory.Version, [32]byte, error) {
	h := prev.Head
	h.CurrentVersion++
	h.LastUpdatedAt = until
	v := prev.Version
	if v.ValidFrom == nil {
		from := v.CreatedAt
		v.ValidFrom = &from
	}
	v.Version = h.CurrentVersion
	v.CreatedAt = until
	v.CreatedBy = by
	v.ValidUntil = &until
	v.Hash = [32]byte{}
	data, err := memory.DecodeData(v.Type, v.Data)
	if err != nil {
		return h, v, [32]byte{}, err
	}
	if err := memory.ValidateMemory(&h, &v, data); err != nil {
		return h, v, [32]byte{}, err
	}
	hash, err := memory.HashVersion(&v)
	if err != nil {
		return h, v, [32]byte{}, err
	}
	v.Hash = hash
	return h, v, hash, nil
}

func (c *Cortex) stageSupersedeReplacement(wb *store.WriteBatch, p preparedSupersedeMemory, now time.Time) error {
	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(p.id)), p.headBytes); err != nil {
		return err
	}
	if err := wb.Set(keys.MemoryVersionKey(toKeysULID(p.id), p.version), p.versionBytes); err != nil {
		return err
	}
	if err := wb.Set(keys.SalienceKey(toKeysULID(p.id)), p.scoreBytes); err != nil {
		return err
	}
	if p.created {
		if err := wb.Set(keys.IdxTypeKey(byte(p.typ), uint64(now.UnixNano()), toKeysULID(p.id)), nil); err != nil {
			return err
		}
		for _, tag := range p.head.Tags {
			if err := wb.Set(keys.IdxTagKey(hashTag(string(tag)), uint64(now.UnixNano()), toKeysULID(p.id)), nil); err != nil {
				return err
			}
		}
		for _, fr := range p.head.Frames {
			objHash := fr.Hash()
			if err := wb.Set(keys.IdxFrameKey(byte(fr.Verb), byte(fr.ObjKind), objHash, toKeysULID(p.id)), nil); err != nil {
				return err
			}
			if p.typ == memory.TypeEvent {
				if err := wb.Set(keys.IdxActorObjKey(byte(fr.Verb), objHash, uint64(now.UnixNano()), toKeysULID(p.id)), nil); err != nil {
					return err
				}
			}
		}
	}
	payload, err := journal.EncodeWritePayload(&journal.WritePayload{
		SchemaVersion: 1, ID: p.id, Version: p.version,
		Type: uint8(p.typ), Hash: p.journalHash,
	})
	if err != nil {
		return err
	}
	if err := wb.AppendJournal(&journal.Entry{
		Kind: p.journalKind, CreatedAt: now.UnixNano(),
		CreatedBy: []byte(p.journalBy), Payload: payload,
	}); err != nil {
		return err
	}
	if err := c.snap.StageMemoryUpdate(wb, p.id, p.headBytes); err != nil {
		return fmt.Errorf("cortex.Supersede: stage replacement SMT: %w", err)
	}
	return nil
}

func (c *Cortex) prepareSupersedesEdge(src, dst memory.ID, meta AddEdgeMeta, now time.Time) (memory.EdgeRecord, []byte, error) {
	if src == dst || src.IsZero() || dst.IsZero() {
		return memory.EdgeRecord{}, nil, memory.ErrSelfEdge
	}
	rec := memory.EdgeRecord{
		Type: memory.EdgeSupersedes, Src: src, Dst: dst,
		CreatedAt: now, CreatedBy: meta.CreatedBy,
		Weight: meta.Weight, Data: meta.Data,
	}
	enc, err := memory.EncodeEdge(&rec)
	return rec, enc, err
}

func (c *Cortex) stageSupersedesEdge(wb *store.WriteBatch, rec memory.EdgeRecord, enc []byte, meta AddEdgeMeta, now time.Time) error {
	srcU := toKeysULID(rec.Src)
	dstU := toKeysULID(rec.Dst)
	if err := wb.Set(keys.EdgeFromKey(srcU, byte(rec.Type), dstU), enc); err != nil {
		return err
	}
	if err := wb.Set(keys.EdgeToKey(dstU, byte(rec.Type), srcU), enc); err != nil {
		return err
	}
	payload, err := journal.EncodeEdgePayload(&journal.EdgePayload{
		SchemaVersion: 1, Type: uint8(rec.Type), Src: rec.Src,
		Dst: rec.Dst, Weight: meta.Weight,
	})
	if err != nil {
		return err
	}
	if err := wb.AppendJournal(&journal.Entry{
		Kind: journal.KindAddEdge, CreatedAt: now.UnixNano(),
		CreatedBy: []byte(meta.CreatedBy), Payload: payload,
	}); err != nil {
		return err
	}
	if err := c.snap.StageEdgeUpdate(wb, rec.Src, byte(rec.Type), rec.Dst, enc); err != nil {
		return fmt.Errorf("cortex.Supersede: stage edge SMT: %w", err)
	}
	return nil
}
