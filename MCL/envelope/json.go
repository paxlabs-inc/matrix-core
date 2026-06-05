// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package envelope

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// On-disk JSON representation per research/02-protocol.md §4
// ("On disk we store JSON for readability; on wire we use CBOR for
// compactness and signature-stability.")
//
// JSONEnvelope is the readable form persisted under
// journal/logs/<intent_id>/<seq>.envelope.json. The Body field is
// rendered as a typed object when the kind is known, falling back to
// hex-encoded raw CBOR for unknown kinds (forward compatibility).
//
// JSONEnvelope is NOT the signed form. The signed bytes are always the
// canonical CBOR encoding of Envelope. JSON is purely for human/
// debug/diff convenience.

// JSONEnvelope is the on-disk schema. Field names mirror the CBOR
// keyasint mapping but in human-readable form.
type JSONEnvelope struct {
	SchemaVersion   uint8           `json:"schema_version"`
	ProtocolVersion string          `json:"v"`
	Kind            string          `json:"kind"`
	ID              string          `json:"id"`
	At              string          `json:"at"`
	From            string          `json:"from"`
	To              string          `json:"to,omitempty"`
	Intent          string          `json:"intent"`
	CorrelationID   string          `json:"correlation_id,omitempty"`
	CausationID     string          `json:"causation_id,omitempty"`
	Body            json.RawMessage `json:"body"`
	BodyHex         string          `json:"body_hex,omitempty"`  // raw CBOR hex fallback
	Signature       string          `json:"signature,omitempty"` // base64
	SelfHash        string          `json:"self_hash,omitempty"` // sha256(UnsignedBytes) hex
}

// EnvelopeJSON renders env into its on-disk JSON form. If the kind is
// known the body is decoded into the typed struct + marshaled as JSON;
// otherwise the raw CBOR bytes are hex-encoded into BodyHex.
//
// SelfHash is always populated (it's free + useful for journal indexes).
func EnvelopeJSON(env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("envelope: nil Envelope")
	}

	out := &JSONEnvelope{
		SchemaVersion:   env.SchemaVersion,
		ProtocolVersion: env.ProtocolVersion,
		Kind:            env.Kind,
		ID:              env.ID,
		At:              env.At,
		From:            env.From,
		To:              env.To,
		Intent:          env.Intent,
		CorrelationID:   env.CorrelationID,
		CausationID:     env.CausationID,
	}

	if len(env.Signature) > 0 {
		out.Signature = base64.StdEncoding.EncodeToString(env.Signature)
	}

	// Self-hash is always computable (signed or not)
	sh, err := SelfHash(env)
	if err == nil {
		out.SelfHash = sh
	}

	if len(env.Body) == 0 {
		out.Body = json.RawMessage("null")
	} else if t := kindBodyType[env.Kind]; t != nil {
		// Decode body into typed struct, then marshal as JSON
		typed := NewTypedBody(env.Kind)
		if err := env.DecodeBody(typed); err != nil {
			// Fall through to hex fallback if typed decode fails
			out.BodyHex = hex.EncodeToString(env.Body)
			out.Body = json.RawMessage("null")
		} else {
			b, err := json.Marshal(typed)
			if err != nil {
				return nil, fmt.Errorf("envelope: marshal typed body: %w", err)
			}
			out.Body = b
		}
	} else {
		// Unknown kind: hex fallback
		out.BodyHex = hex.EncodeToString(env.Body)
		out.Body = json.RawMessage("null")
	}

	return json.MarshalIndent(out, "", "  ")
}

// EnvelopeFromJSON parses an on-disk JSONEnvelope and re-encodes the
// body into canonical CBOR so the resulting Envelope is signature-
// equivalent to the original. Bodies sourced via the BodyHex fallback
// are restored byte-for-byte; typed bodies are re-CBOR-encoded.
//
// This is the round-trip path for journal recovery and human edits.
// SelfHash is recomputed and cross-checked against the on-disk value;
// mismatch returns ErrSelfHashMismatch.
func EnvelopeFromJSON(b []byte) (*Envelope, error) {
	var in JSONEnvelope
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("envelope: parse JSON: %w", err)
	}

	env := &Envelope{
		SchemaVersion:   in.SchemaVersion,
		ProtocolVersion: in.ProtocolVersion,
		Kind:            in.Kind,
		ID:              in.ID,
		At:              in.At,
		From:            in.From,
		To:              in.To,
		Intent:          in.Intent,
		CorrelationID:   in.CorrelationID,
		CausationID:     in.CausationID,
	}

	if in.Signature != "" {
		sig, err := base64.StdEncoding.DecodeString(in.Signature)
		if err != nil {
			return nil, fmt.Errorf("envelope: decode signature: %w", err)
		}
		env.Signature = sig
	}

	// Body reconstruction
	switch {
	case in.BodyHex != "":
		// Hex fallback path (unknown kind or original-CBOR preserved)
		raw, err := hex.DecodeString(in.BodyHex)
		if err != nil {
			return nil, fmt.Errorf("envelope: decode body_hex: %w", err)
		}
		env.Body = raw
	case len(in.Body) > 0 && string(in.Body) != "null":
		// Typed JSON body — decode then re-CBOR-encode
		typed := NewTypedBody(in.Kind)
		if typed == nil {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, in.Kind)
		}
		if err := json.Unmarshal(in.Body, typed); err != nil {
			return nil, fmt.Errorf("envelope: decode typed body: %w", err)
		}
		raw, err := canonicalEnc.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("envelope: re-encode body to CBOR: %w", err)
		}
		env.Body = raw
	}

	// Cross-check SelfHash if the on-disk file recorded it
	if in.SelfHash != "" {
		got, err := SelfHash(env)
		if err != nil {
			return nil, err
		}
		if got != in.SelfHash {
			return nil, fmt.Errorf("%w: disk=%s recomputed=%s", ErrSelfHashMismatch, in.SelfHash, got)
		}
	}

	return env, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
