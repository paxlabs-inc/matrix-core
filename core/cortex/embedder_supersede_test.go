// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"errors"
	"testing"

	"centra/core/cortex/embed"
	"centra/core/cortex/forms"
	"centra/core/cortex/journal"
	"centra/core/cortex/memory"
	"centra/core/cortex/query"
	"centra/core/cortex/vector"
)

func TestEmbedderCommitDoesNotOverwriteConcurrentSupersede(t *testing.T) {
	c := openCortex(t)
	priorURI := writePref(t, c, "legacy tone", 7)
	_, priorID, _, err := ParseURI(priorURI)
	if err != nil {
		t.Fatalf("ParseURI prior: %v", err)
	}
	prior, err := c.Resolve(priorURI)
	if err != nil {
		t.Fatalf("Resolve prior: %v", err)
	}
	data, err := memory.DecodeData(prior.Version.Type, prior.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData prior: %v", err)
	}

	embedder := embed.NewHashEmbedder()
	vec, err := embedder.Embed(forms.RenderFull(&prior.Head, data))
	if err != nil {
		t.Fatalf("Embed prior: %v", err)
	}
	vectorStore := newPebbleVectorStore(c.s)
	index := vector.NewIndex(vector.Params{
		Dim: embedder.Dim(), Model: embedder.Model(),
	})
	index.BindStore(vectorStore)
	state := &embedderState{
		c: c, embedder: embedder, index: index, store: vectorStore,
		vertexNext: 1,
	}
	payload := journal.WritePayload{
		SchemaVersion: 1, ID: priorID, Version: prior.Version.Version,
		Type: uint8(prior.Version.Type), Hash: prior.Version.Hash,
	}

	replacementURI, err := c.Supersede(
		priorURI,
		memory.PreferenceData{
			SchemaVersion: 1,
			Topic:         "current tone",
			Polarity:      memory.PolarityPrefer,
			StrengthVal:   0.9,
			Rationale:     "the current preference",
		},
		SupersedeOptions{
			Head: memory.Head{
				ActorScope: "andrew", DeclaredImportance: 7,
			},
			WriteMeta: WriteMeta{
				CreatedBy: "andrew",
				Provenance: memory.Provenance{
					Source: memory.SourceUserInput,
				},
			},
			EdgeMeta: AddEdgeMeta{CreatedBy: "andrew"},
		},
	)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	err = state.commitEmbedding(
		payload, priorID, prior.Version.Version, vec, memory.HashVector(vec),
	)
	if !errors.Is(err, errEmbedderHeadChanged) {
		t.Fatalf("stale commit error = %v, want %v", err, errEmbedderHeadChanged)
	}
	closed, err := c.ResolveLatest(priorID)
	if err != nil {
		t.Fatalf("ResolveLatest closed prior: %v", err)
	}
	if closed.Head.CurrentVersion != 2 || closed.Version.ValidUntil == nil ||
		closed.Head.EmbeddingRef != nil {
		t.Fatalf("stale embed changed superseded head: %+v", closed)
	}

	encoded, err := journal.EncodeWritePayload(&payload)
	if err != nil {
		t.Fatalf("EncodeWritePayload: %v", err)
	}
	if err := state.processWriteEntry(&journal.Entry{
		Kind: journal.KindWrite, Payload: encoded,
	}); err != nil {
		t.Fatalf("retry processWriteEntry: %v", err)
	}
	closed, err = c.ResolveLatest(priorID)
	if err != nil {
		t.Fatalf("ResolveLatest embedded prior: %v", err)
	}
	if closed.Head.CurrentVersion != 2 || closed.Version.ValidUntil == nil ||
		closed.Head.EmbeddingRef == nil {
		t.Fatalf("retry did not merge embedding into superseded head: %+v", closed)
	}
	if closed.Head.EmbeddingRef.Model != embedder.Model() {
		t.Fatalf("embedded model = %q, want %q",
			closed.Head.EmbeddingRef.Model, embedder.Model())
	}

	current, err := c.Find(query.Query{
		Type: []memory.Type{memory.TypePreference}, Limit: 8,
	})
	if err != nil {
		t.Fatalf("Find current preferences: %v", err)
	}
	_, replacementID, _, err := ParseURI(replacementURI)
	if err != nil {
		t.Fatalf("ParseURI replacement: %v", err)
	}
	if current == nil || len(current.Memories) != 1 ||
		current.Memories[0].Head.ID != replacementID {
		t.Fatalf("current preferences after embed retry = %+v", current)
	}
}
