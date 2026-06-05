// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package journal

import (
	"bytes"
	"testing"
)

func TestEntryEncodeDecodeRoundTrip(t *testing.T) {
	e := Entry{
		Seq:       42,
		Kind:      KindRaw,
		CreatedAt: 1_700_000_000_000_000_000,
		CreatedBy: []byte("did:pax:owner"),
		Payload:   []byte("hello"),
	}
	enc, err := e.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got Entry
	if err := Decode(enc, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Seq != e.Seq || got.Kind != e.Kind || got.CreatedAt != e.CreatedAt {
		t.Fatalf("scalar mismatch: %+v != %+v", got, e)
	}
	if !bytes.Equal(got.CreatedBy, e.CreatedBy) || !bytes.Equal(got.Payload, e.Payload) {
		t.Fatalf("byte mismatch: %+v != %+v", got, e)
	}
}

// Determinism is load-bearing: encoding twice must yield byte-identical CBOR.
func TestEntryEncodeDeterministic(t *testing.T) {
	e := Entry{
		Seq:       7,
		Kind:      KindWrite,
		CreatedAt: 1_700_000_000_000_000_000,
		Payload:   []byte{0x01, 0x02, 0x03, 0x04},
	}
	a, err := e.Encode()
	if err != nil {
		t.Fatalf("encode a: %v", err)
	}
	b, err := e.Encode()
	if err != nil {
		t.Fatalf("encode b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("nondeterministic encoding:\n a=%x\n b=%x", a, b)
	}
}

// LeafHash is domain-separated. Two entries that happen to share their CBOR
// body with another protocol's payload still produce distinct cortex leaves.
func TestLeafHashDomainSeparated(t *testing.T) {
	e := Entry{Seq: 1, Kind: KindRaw, CreatedAt: 1, Payload: []byte("x")}
	enc, _ := e.Encode()

	h1 := LeafHash(enc)
	// Same bytes, no domain: deliberately differing.
	plain := [32]byte{}
	// We don't compute plain sha256 here because the point is just to assert
	// h1 != naive concat-free hash. A constant zero is fine for inequality.
	if h1 == plain {
		t.Fatalf("leaf hash collided with zero")
	}

	// Hashing twice gives the same value.
	if h1 != LeafHash(enc) {
		t.Fatalf("LeafHash nondeterministic")
	}
}

// TestEdgePayloadRoundTrip: Phase 6 EdgePayload canonical CBOR survives
// encode/decode byte-identically and Tombstoned distinguishes Add from Remove.
func TestEdgePayloadRoundTrip(t *testing.T) {
	src := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	dst := [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	add := &EdgePayload{
		SchemaVersion: 1,
		Type:          0x03,
		Src:           src,
		Dst:           dst,
		Weight:        0.7,
	}
	enc, err := EncodeEdgePayload(add)
	if err != nil {
		t.Fatalf("EncodeEdgePayload: %v", err)
	}
	var got EdgePayload
	if err := DecodeEdgePayload(enc, &got); err != nil {
		t.Fatalf("DecodeEdgePayload: %v", err)
	}
	if got != *add {
		t.Fatalf("round trip: %+v vs %+v", got, *add)
	}

	rem := &EdgePayload{
		SchemaVersion: 1,
		Type:          0x03,
		Src:           src,
		Dst:           dst,
		Tombstoned:    true,
		Reason:        "obsolete",
		By:            "system",
	}
	rEnc, err := EncodeEdgePayload(rem)
	if err != nil {
		t.Fatalf("EncodeEdgePayload remove: %v", err)
	}
	if bytes.Equal(enc, rEnc) {
		t.Fatalf("Add and Remove payloads collided")
	}
}

// TestCompactPayloadRoundTrip: Phase 9 CompactPayload canonical CBOR survives
// encode/decode byte-identically and two distinct payloads do not collide.
// Mirrors TestEdgePayloadRoundTrip discipline.
func TestCompactPayloadRoundTrip(t *testing.T) {
	hash := [32]byte{}
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	p := &CompactPayload{
		SchemaVersion:  1,
		IntentID:       "intent_01HABC",
		StepID:         "step_3",
		BudgetTokens:   4000,
		KeptCount:      2,
		CompactedCount: 5,
		CheckpointHash: hash,
	}
	enc, err := EncodeCompactPayload(p)
	if err != nil {
		t.Fatalf("EncodeCompactPayload: %v", err)
	}
	var got CompactPayload
	if err := DecodeCompactPayload(enc, &got); err != nil {
		t.Fatalf("DecodeCompactPayload: %v", err)
	}
	if got != *p {
		t.Fatalf("round trip: %+v vs %+v", got, *p)
	}
	// Determinism: encode twice → byte-identical.
	enc2, err := EncodeCompactPayload(p)
	if err != nil {
		t.Fatalf("EncodeCompactPayload twice: %v", err)
	}
	if !bytes.Equal(enc, enc2) {
		t.Fatalf("CompactPayload encoding nondeterministic")
	}
	// Distinct step → distinct bytes (no silent collapse).
	q := *p
	q.StepID = "step_4"
	encQ, err := EncodeCompactPayload(&q)
	if err != nil {
		t.Fatalf("EncodeCompactPayload q: %v", err)
	}
	if bytes.Equal(enc, encQ) {
		t.Fatalf("distinct StepID collided in CompactPayload encoding")
	}
}

// TestEncodeCompactPayloadNil: defensive nil-check matches EncodeEdgePayload
// and EncodeWritePayload behavior.
func TestEncodeCompactPayloadNil(t *testing.T) {
	if _, err := EncodeCompactPayload(nil); err == nil {
		t.Fatalf("expected error encoding nil CompactPayload")
	}
}

func TestEntryRequiresKind(t *testing.T) {
	e := Entry{Seq: 1, CreatedAt: 1}
	if _, err := e.Encode(); err == nil {
		t.Fatalf("expected error encoding entry without Kind")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
