// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"matrix/mcl/envelope"
)

// EnvelopeStream signs envelopes for a single intent + persists them as JSON
// (one file per envelope) under JournalDir/<intentID>/<seq:04>-<kind>.json.
//
// Mirrors what a real executor does on every Apply: render canonical CBOR,
// sign with the actor's private key, persist on-disk in human-readable JSON
// per research/01 §4.10 ("journal/logs/ readability-first").
type EnvelopeStream struct {
	dir      string
	intentID string
	actor    *ActorIdentity
	resolver *staticKeyResolver
	t        *Transcript
	seq      uint64 // monotonic per intent
	chain    []*envelope.Envelope
}

// NewEnvelopeStream prepares a per-intent stream. dir is JournalDir/<intentID>/.
func NewEnvelopeStream(parent, intentID string, actor *ActorIdentity, t *Transcript) (*EnvelopeStream, error) {
	dir := filepath.Join(parent, intentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	resolver := &staticKeyResolver{principals: map[string]ed25519.PublicKey{}}
	resolver.principals[actor.UserURI] = actor.Public
	return &EnvelopeStream{
		dir:      dir,
		intentID: intentID,
		actor:    actor,
		resolver: resolver,
		t:        t,
	}, nil
}

// SignAndPersist signs an envelope of (kind, body), stamps headers, and writes
// JSON to disk. Returns the populated envelope (Signature set).
//
// `correlation` and `causation` are optional — pass "" to leave unset.
func (es *EnvelopeStream) SignAndPersist(kind string, body interface{}, correlation, causation string) (*envelope.Envelope, error) {
	env, err := envelope.NewEnvelope(kind, body)
	if err != nil {
		return nil, fmt.Errorf("envelopes: NewEnvelope %s: %w", kind, err)
	}

	atomic.AddUint64(&es.seq, 1)
	env.ID = newULIDLike()
	env.At = time.Now().UTC().Format(time.RFC3339Nano)
	env.From = es.actor.UserURI
	env.Intent = "matrix://intent/" + es.intentID
	env.CorrelationID = correlation
	env.CausationID = causation

	if err := envelope.Sign(env, es.actor.Private); err != nil {
		return nil, fmt.Errorf("envelopes: sign %s: %w", kind, err)
	}

	// Verify round-trips cleanly with our resolver (sanity).
	if err := envelope.Verify(env, es.resolver); err != nil {
		return nil, fmt.Errorf("envelopes: self-verify %s: %w", kind, err)
	}

	selfHash, _ := envelope.SelfHash(env)

	// Persist as JSON for journal/logs/.
	js, err := envelope.EnvelopeJSON(env)
	if err != nil {
		return nil, fmt.Errorf("envelopes: json %s: %w", kind, err)
	}
	pretty := bytesIndent(js)
	path := filepath.Join(es.dir, fmt.Sprintf("%04d-%s.json", es.seq, sanitiseKind(kind)))
	if err := os.WriteFile(path, pretty, 0o644); err != nil {
		return nil, fmt.Errorf("envelopes: write %s: %w", path, err)
	}

	es.chain = append(es.chain, env)
	es.t.Event("envelope.signed", "envelope", map[string]interface{}{
		"kind":      kind,
		"id":        env.ID,
		"self_hash": selfHash,
		"path":      path,
	})
	return env, nil
}

// Resolver exposes the EnvelopeStream's KeyResolver so other components
// (verification harness, lifecycle replay) can use the same one.
func (es *EnvelopeStream) Resolver() *staticKeyResolver { return es.resolver }

// Chain returns all signed envelopes in arrival order.
func (es *EnvelopeStream) Chain() []*envelope.Envelope { return es.chain }

// Last returns the most recently appended envelope (for CausationID chaining).
func (es *EnvelopeStream) Last() *envelope.Envelope {
	if len(es.chain) == 0 {
		return nil
	}
	return es.chain[len(es.chain)-1]
}

// LastID returns the most recent envelope's ID for CorrelationID/CausationID.
func (es *EnvelopeStream) LastID() string {
	if e := es.Last(); e != nil {
		return e.ID
	}
	return ""
}

// newULIDLike emits a ULID-shaped 26-char Crockford-base32 string. Not a
// strict ULID (no monotonic guarantee across the process) but cryptographically
// random and stable enough for envelope IDs. Mirrors the encoding used by
// oklog/ulid.
func newULIDLike() string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	// 6-byte timestamp
	ts := uint64(time.Now().UTC().UnixMilli())
	for i := 0; i < 6; i++ {
		raw[i] = byte((ts >> ((5 - i) * 8)) & 0xff)
	}
	out := make([]byte, 26)
	for i := 0; i < 26; i++ {
		out[i] = crockford[raw[i%16]&0x1f]
	}
	return string(out)
}

// sanitiseKind replaces '.' with '-' so kind names (e.g. "intent.draft")
// render as filesystem-safe filenames.
func sanitiseKind(kind string) string {
	out := []byte(kind)
	for i := range out {
		if out[i] == '.' {
			out[i] = '-'
		}
	}
	return string(out)
}

// bytesIndent re-indents already-marshaled JSON for human-readable
// journal logs (envelope.EnvelopeJSON returns compact bytes).
func bytesIndent(in []byte) []byte {
	var generic interface{}
	if err := json.Unmarshal(in, &generic); err != nil {
		return in // fall back on raw
	}
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return in
	}
	return append(out, '\n')
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
