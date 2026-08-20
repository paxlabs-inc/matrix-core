// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"centra/core/mcl/envelope"
	"centra/packages/vault"
)

const (
	envelopeStore   = "mcl.envelope"
	envelopeSchema1 = "envelope.v1"
)

// envelopeAD reconstructs the associated data for a persisted envelope from
// where it lives (this user, this intent, this position in the chain) — never
// stored, so an envelope moved between users, intents, or positions fails
// authentication.
func envelopeAD(user, intentID string, seq uint64) vault.AD {
	return vault.AD{User: user, Store: envelopeStore, Stream: intentID, Seq: seq, Schema: envelopeSchema1}
}

// actorIdentity carries the ed25519 keypair + DID URIs an executor uses
// to sign envelopes. Mirrors cmd/mcl-e2e/ActorIdentity in shape but
// supports BOTH loading a persistent key from disk (production path)
// AND deriving deterministic keys for testing.
//
// Persistent on-disk format: 64-hex-char ed25519 seed in
// <keyfile>. Created on first use if missing.
//
// Citations:
//   - A1 lock (research/06-agents.md): every agent has a stable DID
//   - matrix.kvx executor_locked_design Q22 (line 718): URIs versioned
type actorIdentity struct {
	DID      string
	Public   ed25519.PublicKey
	Private  ed25519.PrivateKey
	UserURI  string // matrix://user/<did>
	AgentURI string // matrix://agent/<did>
}

// loadOrCreateIdentity loads the ed25519 seed from path, or generates +
// persists one if path doesn't exist. The DID label is folded into the
// did:matrix:<label>:<key_prefix> identifier.
func loadOrCreateIdentity(path, didLabel string) (*actorIdentity, error) {
	if didLabel == "" {
		didLabel = "executor"
	}
	var seed [ed25519.SeedSize]byte

	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			// Persistent key found.
			raw, derr := hex.DecodeString(trimNL(string(b)))
			if derr != nil {
				return nil, fmt.Errorf("identity: decode key %s: %w", path, derr)
			}
			if len(raw) != ed25519.SeedSize {
				return nil, fmt.Errorf("identity: key %s wrong size %d (want %d)",
					path, len(raw), ed25519.SeedSize)
			}
			copy(seed[:], raw)
		} else if os.IsNotExist(err) {
			// Generate + persist.
			if _, rerr := rand.Read(seed[:]); rerr != nil {
				return nil, fmt.Errorf("identity: rand seed: %w", rerr)
			}
			if mderr := os.MkdirAll(filepath.Dir(path), 0o700); mderr != nil {
				return nil, fmt.Errorf("identity: mkdir for keyfile: %w", mderr)
			}
			if werr := os.WriteFile(path, []byte(hex.EncodeToString(seed[:])+"\n"), 0o600); werr != nil {
				return nil, fmt.Errorf("identity: persist key %s: %w", path, werr)
			}
		} else {
			return nil, fmt.Errorf("identity: read key %s: %w", path, err)
		}
	} else {
		// Ephemeral key (testing only).
		if _, err := rand.Read(seed[:]); err != nil {
			return nil, fmt.Errorf("identity: rand seed: %w", err)
		}
	}

	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	pubHex := hex.EncodeToString(pub)
	did := "did:matrix:" + didLabel + ":" + pubHex[:16]
	return &actorIdentity{
		DID:      did,
		Public:   pub,
		Private:  priv,
		UserURI:  "matrix://user/" + did,
		AgentURI: "matrix://agent/" + did,
	}, nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// staticKeyResolver implements envelope.KeyResolver against a fixed set
// of principals. In production, this would be replaced by a
// tools/registry-backed resolver that resolves matrix://agent/<did> via
// chain attestation. v1 ships static for the executor's own envelope
// self-verification needs.
type staticKeyResolver struct {
	principals map[string]ed25519.PublicKey
}

func (r *staticKeyResolver) ResolveKey(principal string) (ed25519.PublicKey, error) {
	if k, ok := r.principals[principal]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("identity: unknown principal %q", principal)
}

// envelopeStream is the per-intent signing + persistence machinery.
// Mirrors cmd/mcl-e2e/EnvelopeStream but lives in the production CLI.
//
// Every envelope is:
//  1. Stamped with ID/At/From/Intent + caller-supplied Correlation/Causation
//  2. Canonical-CBOR-encoded + ed25519-signed via envelope.Sign
//  3. Round-trip self-verified to catch encoding bugs
//  4. Persisted as pretty JSON under <journalDir>/<intentID>/<seq>-<kind>.json
//  5. Streamed to the transcript as an envelope.signed event
type envelopeStream struct {
	dir      string
	intentID string
	actor    *actorIdentity
	resolver *staticKeyResolver
	t        *transcript
	seq      uint64
	chain    []*envelope.Envelope
	vault    *vault.Session
	user     string
}

// SetVault wires the fail-closed data-at-rest session and owning user DID so
// persisted envelopes are sealed. Called once right after construction in the
// daemon pipelines; a nil session leaves legacy plaintext (dev/CLI walk).
// Signatures stay over the envelope's canonical logical encoding — sealing
// changes persisted bytes only, and readers decrypt before verify.
func (es *envelopeStream) SetVault(sess *vault.Session, user string) {
	es.vault = sess
	es.user = user
}

func newEnvelopeStream(parentDir, intentID string, actor *actorIdentity, t *transcript) (*envelopeStream, error) {
	dir := filepath.Join(parentDir, intentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("envelope_stream: mkdir %s: %w", dir, err)
	}
	res := &staticKeyResolver{principals: map[string]ed25519.PublicKey{}}
	res.principals[actor.UserURI] = actor.Public
	return &envelopeStream{
		dir:      dir,
		intentID: intentID,
		actor:    actor,
		resolver: res,
		t:        t,
	}, nil
}

// SignAndPersist implements runtime.EnvelopeSink.
func (es *envelopeStream) SignAndPersist(kind string, body interface{}, correlation, causation string) (*envelope.Envelope, error) {
	env, err := envelope.NewEnvelope(kind, body)
	if err != nil {
		return nil, fmt.Errorf("envelope_stream: NewEnvelope %s: %w", kind, err)
	}
	seq := atomic.AddUint64(&es.seq, 1)
	env.ID = newULIDLike()
	env.At = time.Now().UTC().Format(time.RFC3339Nano)
	env.From = es.actor.UserURI
	env.Intent = "matrix://intent/" + es.intentID
	env.CorrelationID = correlation
	env.CausationID = causation

	if err := envelope.Sign(env, es.actor.Private); err != nil {
		return nil, fmt.Errorf("envelope_stream: sign %s: %w", kind, err)
	}
	if err := envelope.Verify(env, es.resolver); err != nil {
		return nil, fmt.Errorf("envelope_stream: self-verify %s: %w", kind, err)
	}

	selfHash, _ := envelope.SelfHash(env)
	js, err := envelope.EnvelopeJSON(env)
	if err != nil {
		return nil, fmt.Errorf("envelope_stream: json %s: %w", kind, err)
	}
	pretty := prettyJSON(js)
	// Seal the whole file at rest (fail-closed when the vault is required);
	// nil session = legacy plaintext for dev/CLI. The AD binds the position
	// (seq) so a sealed envelope re-ordered inside the chain fails to open.
	pretty, err = es.vault.MaybeSealFile(envelopeAD(es.user, es.intentID, seq), pretty)
	if err != nil {
		return nil, fmt.Errorf("envelope_stream: seal %s: %w", kind, err)
	}
	path := filepath.Join(es.dir, fmt.Sprintf("%04d-%s.json", seq, sanitiseKind(kind)))
	if err := os.WriteFile(path, pretty, 0o600); err != nil {
		return nil, fmt.Errorf("envelope_stream: write %s: %w", path, err)
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

// LastID implements runtime.EnvelopeSink for chaining.
func (es *envelopeStream) LastID() string {
	if len(es.chain) == 0 {
		return ""
	}
	return es.chain[len(es.chain)-1].ID
}

// Chain returns the envelope sequence (audit access).
func (es *envelopeStream) Chain() []*envelope.Envelope { return es.chain }

// IntentURI returns matrix://intent/<id> form for cross-references.
func (es *envelopeStream) IntentURI() string { return "matrix://intent/" + es.intentID }

// Resolver exposes the verification key set.
func (es *envelopeStream) Resolver() *staticKeyResolver { return es.resolver }

// AcceptCorrespondent adds another principal to the resolver. Used when
// the executor receives an envelope from the user (different DID) and
// needs to verify it.
func (es *envelopeStream) AcceptCorrespondent(uri string, pub ed25519.PublicKey) {
	es.resolver.principals[uri] = pub
}

// --- helpers ---

// newULIDLike emits a ULID-shaped 26-char Crockford-base32 string. Not
// a strict ULID (no in-process monotonic guarantee) but cryptographically
// random and stable enough for envelope IDs. Mirrors oklog/ulid
// encoding.
func newULIDLike() string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var raw [16]byte
	_, _ = rand.Read(raw[:])
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

func sanitiseKind(kind string) string {
	out := []byte(kind)
	for i := range out {
		if out[i] == '.' {
			out[i] = '-'
		}
	}
	return string(out)
}

func prettyJSON(in []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return in
	}
	return append(out, '\n')
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
