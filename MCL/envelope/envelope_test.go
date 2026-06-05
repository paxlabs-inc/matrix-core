// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package envelope

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// makeKey generates a fresh ed25519 keypair with a fixed seed-style
// reader for test determinism (not the same as production key gen).
func makeKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// makeEnv produces a minimally-valid envelope for the given kind with
// the given body. Caller must populate ID/At/From/Intent before signing.
func makeEnv(t *testing.T, kind string, body interface{}) *Envelope {
	t.Helper()
	env, err := NewEnvelope(kind, body)
	if err != nil {
		t.Fatalf("NewEnvelope kind=%s: %v", kind, err)
	}
	env.ID = "01HZ0000000000000000000001"
	env.At = "2026-05-24T12:00:00Z"
	env.From = "matrix://agent/did:pax:0xabc"
	env.Intent = "matrix://intent/01HZ0000000000000000000002"
	return env
}

func TestKinds_AllFifteen(t *testing.T) {
	if len(AllKinds) != 15 {
		t.Fatalf("want 15 kinds, got %d", len(AllKinds))
	}
	seen := make(map[string]bool)
	for _, k := range AllKinds {
		if seen[k] {
			t.Errorf("duplicate kind %q in AllKinds", k)
		}
		seen[k] = true
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) should be true", k)
		}
	}
	// Each kind must have a body type mapping
	for _, k := range AllKinds {
		if BodyTypeOf(k) == nil {
			t.Errorf("kindBodyType missing entry for %q", k)
		}
	}
}

func TestKinds_NoChatMessage(t *testing.T) {
	// Spec: NO chat.message kind by design (research/02 §1)
	for _, k := range AllKinds {
		if k == "chat.message" || strings.Contains(k, "chat") {
			t.Errorf("forbidden chat kind in AllKinds: %q", k)
		}
	}
	if ValidKind("chat.message") {
		t.Fatalf("chat.message must not be a valid kind")
	}
}

func TestNewEnvelope_RejectsUnknownKind(t *testing.T) {
	_, err := NewEnvelope("intent.unknown", IntentDraftBody{Prose: "x"})
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("want ErrUnknownKind, got %v", err)
	}
}

func TestNewEnvelope_RejectsBodyKindMismatch(t *testing.T) {
	// IntentDraftBody passed under KindIntentAccept → mismatch
	_, err := NewEnvelope(KindIntentAccept, IntentDraftBody{Prose: "x"})
	if !errors.Is(err, ErrBodyTypeMismatch) {
		t.Fatalf("want ErrBodyTypeMismatch, got %v", err)
	}
}

func TestNewEnvelope_AcceptsPointerOrValueBody(t *testing.T) {
	v, err := NewEnvelope(KindIntentDraft, IntentDraftBody{Prose: "x"})
	if err != nil {
		t.Fatalf("value body: %v", err)
	}
	p, err := NewEnvelope(KindIntentDraft, &IntentDraftBody{Prose: "x"})
	if err != nil {
		t.Fatalf("pointer body: %v", err)
	}
	if !bytes.Equal(v.Body, p.Body) {
		t.Fatalf("value vs pointer body bytes differ")
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{
		Prose:      "build me a deployment pipeline",
		SlotValues: map[string]string{"target": "matrix://cortex/Fact/abc#1"},
	})

	enc, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}

	var got Envelope
	if err := Decode(enc, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindIntentDraft {
		t.Fatalf("kind drift: %s vs %s", got.Kind, KindIntentDraft)
	}
	if !bytes.Equal(got.Body, env.Body) {
		t.Fatalf("body bytes drift across CBOR round-trip")
	}

	// Typed body round-trip
	var body IntentDraftBody
	if err := got.DecodeBody(&body); err != nil {
		t.Fatal(err)
	}
	if body.Prose != "build me a deployment pipeline" {
		t.Fatalf("body content drift: %q", body.Prose)
	}
	if body.SlotValues["target"] != "matrix://cortex/Fact/abc#1" {
		t.Fatalf("slot value drift")
	}
}

func TestEncode_Deterministic(t *testing.T) {
	env := makeEnv(t, KindIntentAttest, IntentAttestBody{
		Outcome:     "success",
		CitedURIs:   []string{"matrix://cortex/Fact/a#1", "matrix://cortex/Fact/b#2"},
		CompletedAt: "2026-05-24T12:00:00Z",
	})

	a, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("Encode not deterministic across calls")
	}
}

func TestSign_Verify_RoundTrip(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{
		IntentHash: "abc123",
		AcceptedAt: "2026-05-24T12:00:00Z",
	})

	if err := Sign(env, priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(env.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature size: %d", len(env.Signature))
	}

	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(env, resolver); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerify_RejectsTamperedBody(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{
		IntentHash: "abc123",
		AcceptedAt: "2026-05-24T12:00:00Z",
	})
	if err := Sign(env, priv); err != nil {
		t.Fatal(err)
	}

	// Tamper the body — flip a byte
	env.Body[0] ^= 0xff
	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(env, resolver); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
}

func TestVerify_RejectsTamperedHeader(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{
		IntentHash: "abc123",
		AcceptedAt: "2026-05-24T12:00:00Z",
	})
	if err := Sign(env, priv); err != nil {
		t.Fatal(err)
	}

	env.Intent = "matrix://intent/different"
	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(env, resolver); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
}

func TestVerify_RejectsWrongKey(t *testing.T) {
	_, priv := makeKey(t)
	wrongPub, _ := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{
		IntentHash: "abc",
		AcceptedAt: "2026-05-24T12:00:00Z",
	})
	_ = Sign(env, priv)
	resolver := StaticKeyResolver{env.From: wrongPub}
	if err := Verify(env, resolver); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
}

func TestVerify_RejectsUnknownPrincipal(t *testing.T) {
	_, priv := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{IntentHash: "x", AcceptedAt: "t"})
	_ = Sign(env, priv)
	resolver := StaticKeyResolver{} // empty
	if err := Verify(env, resolver); !errors.Is(err, ErrUnknownPrincipal) {
		t.Fatalf("want ErrUnknownPrincipal, got %v", err)
	}
}

func TestVerify_RejectsSchemaMismatch(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentAccept, IntentAcceptBody{IntentHash: "x", AcceptedAt: "t"})
	_ = Sign(env, priv)
	env.SchemaVersion = 99
	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(env, resolver); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("want ErrSchemaVersion, got %v", err)
	}
}

func TestVerify_RejectsMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Envelope)
		want error
	}{
		{"no_id", func(e *Envelope) { e.ID = "" }, ErrIDMissing},
		{"no_at", func(e *Envelope) { e.At = "" }, ErrAtMissing},
		{"no_from", func(e *Envelope) { e.From = "" }, ErrFromMissing},
		{"no_intent", func(e *Envelope) { e.Intent = "" }, ErrIntentMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv := makeKey(t)
			env := makeEnv(t, KindIntentAccept, IntentAcceptBody{IntentHash: "x", AcceptedAt: "t"})
			// Sign requires all fields to be set; missing field is detected at Sign or Verify
			tc.mut(env)
			err := Sign(env, priv)
			if err == nil {
				// If sign passed, verify must catch it
				env2 := *env
				env2.Signature = make([]byte, ed25519.SignatureSize)
				resolver := StaticKeyResolver{env.From: pub}
				err = Verify(&env2, resolver)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestVerify_RejectsMissingSignature(t *testing.T) {
	pub, _ := makeKey(t)
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})
	// not signed
	resolver := StaticKeyResolver{env.From: pub}
	err := Verify(env, resolver)
	if !errors.Is(err, ErrSignatureMissing) {
		t.Fatalf("want ErrSignatureMissing, got %v", err)
	}
}

func TestSelfHash_StableAndDifferentiates(t *testing.T) {
	env1 := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "a"})
	env2 := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "b"})

	h1a, err := SelfHash(env1)
	if err != nil {
		t.Fatal(err)
	}
	h1b, err := SelfHash(env1)
	if err != nil {
		t.Fatal(err)
	}
	if h1a != h1b {
		t.Fatalf("SelfHash not stable: %s vs %s", h1a, h1b)
	}

	h2, err := SelfHash(env2)
	if err != nil {
		t.Fatal(err)
	}
	if h1a == h2 {
		t.Fatalf("SelfHash did not differentiate distinct bodies")
	}
}

func TestSelfHash_IgnoresSignature(t *testing.T) {
	_, priv := makeKey(t)
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})
	pre, err := SelfHash(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(env, priv); err != nil {
		t.Fatal(err)
	}
	post, err := SelfHash(env)
	if err != nil {
		t.Fatal(err)
	}
	if pre != post {
		t.Fatalf("SelfHash should ignore signature: %s vs %s", pre, post)
	}
}

func TestEveryKind_RoundTripSignVerify(t *testing.T) {
	// Build a minimal canonical body for each kind, sign with a fresh
	// key, encode → decode → verify. Catches encoding bugs in body.go.
	bodies := map[string]interface{}{
		KindIntentDraft:       IntentDraftBody{Prose: "x"},
		KindIntentCompiled:    IntentCompiledBody{IntentJSON: []byte(`{"id":"x"}`)},
		KindIntentClarify:     IntentClarifyBody{Questions: []ClarifyQuestion{{UnknownID: "u1", Field: "f", Prompt: "?"}}},
		KindIntentAnswer:      IntentAnswerBody{Patches: []byte(`[]`), AnswerOf: "01HZ"},
		KindIntentAccept:      IntentAcceptBody{IntentHash: "abc", AcceptedAt: "t"},
		KindPlanProposed:      PlanProposedBody{PlanJSON: []byte(`{"id":"p"}`)},
		KindPlanStep:          PlanStepBody{PlanID: "p", NodeID: "n", Status: "completed"},
		KindPlanOutput:        PlanOutputBody{PlanID: "p", NodeID: "n", Sequence: 1, Chunk: []byte("hello")},
		KindIntentCorrect:     IntentCorrectBody{Target: "intent", Patches: []byte(`[]`)},
		KindIntentDispatch:    IntentDispatchBody{SubIntentJSON: []byte(`{"id":"sub"}`)},
		KindIntentAttest:      IntentAttestBody{Outcome: "success", CompletedAt: "t"},
		KindIntentFail:        IntentFailBody{Reason: "tool_error", FailedAt: "t"},
		KindIntentCancel:      IntentCancelBody{CancelledAt: "t"},
		KindPolicyGate:        PolicyGateBody{RuleRef: "matrix://rule/x", Question: "?"},
		KindPolicyGateResolve: PolicyGateResolveBody{GateOf: "g", Decision: "approve", ResolvedAt: "t"},
	}

	for _, kind := range AllKinds {
		body, ok := bodies[kind]
		if !ok {
			t.Errorf("test missing body for kind %q", kind)
			continue
		}
		t.Run(kind, func(t *testing.T) {
			pub, priv := makeKey(t)
			env := makeEnv(t, kind, body)
			if err := Sign(env, priv); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			enc, err := Encode(env)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}

			var got Envelope
			if err := Decode(enc, &got); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			resolver := StaticKeyResolver{env.From: pub}
			if err := Verify(&got, resolver); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			// Strict body kind/type check
			typed, err := ValidateBody(&got)
			if err != nil {
				t.Fatalf("ValidateBody: %v", err)
			}
			if typed == nil {
				t.Fatalf("ValidateBody returned nil")
			}
		})
	}
}

func TestValidateBody_RejectsUnknownKind(t *testing.T) {
	env := &Envelope{Kind: "nope", Body: []byte{0xa0}}
	_, err := ValidateBody(env)
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("want ErrUnknownKind, got %v", err)
	}
}

func TestUnsignedBytes_ExcludesSignature(t *testing.T) {
	_, priv := makeKey(t)
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})

	pre, err := UnsignedBytes(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(env, priv); err != nil {
		t.Fatal(err)
	}
	post, err := UnsignedBytes(env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pre, post) {
		t.Fatalf("UnsignedBytes differs after signing — signature leaking into unsigned form")
	}
}

func TestSign_RejectsBadKey(t *testing.T) {
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})
	short := ed25519.PrivateKey(make([]byte, 16))
	if err := Sign(env, short); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("want ErrSignatureInvalid, got %v", err)
	}
}

func TestJSON_RoundTripPreservesSignature(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentAttest, IntentAttestBody{
		Outcome:     "success",
		CitedURIs:   []string{"matrix://cortex/Fact/a#1"},
		CompletedAt: "2026-05-24T12:00:00Z",
	})
	if err := Sign(env, priv); err != nil {
		t.Fatal(err)
	}

	js, err := EnvelopeJSON(env)
	if err != nil {
		t.Fatal(err)
	}

	// Spot-check the JSON has the expected fields readable
	var raw map[string]interface{}
	if err := json.Unmarshal(js, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["kind"] != KindIntentAttest {
		t.Fatalf("JSON kind: %v", raw["kind"])
	}
	if _, ok := raw["body"].(map[string]interface{}); !ok {
		t.Fatalf("JSON body should be typed object, got %T", raw["body"])
	}

	got, err := EnvelopeFromJSON(js)
	if err != nil {
		t.Fatalf("EnvelopeFromJSON: %v", err)
	}
	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(got, resolver); err != nil {
		t.Fatalf("Verify after JSON round-trip: %v", err)
	}
	// Body bytes must round-trip byte-for-byte
	if !bytes.Equal(got.Body, env.Body) {
		t.Fatalf("body bytes drift across JSON round-trip")
	}
}

func TestJSON_RejectsSelfHashMismatch(t *testing.T) {
	pub, priv := makeKey(t)
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})
	_ = Sign(env, priv)
	js, err := EnvelopeJSON(env)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the JSON: change the prose field to invalidate self-hash
	corrupted := bytes.Replace(js, []byte(`"prose": "x"`), []byte(`"prose": "y"`), 1)
	if bytes.Equal(corrupted, js) {
		t.Fatal("test setup did not corrupt JSON")
	}
	_, err = EnvelopeFromJSON(corrupted)
	if !errors.Is(err, ErrSelfHashMismatch) {
		t.Fatalf("want ErrSelfHashMismatch, got %v", err)
	}

	// Sanity: original parses + verifies
	got, err := EnvelopeFromJSON(js)
	if err != nil {
		t.Fatal(err)
	}
	resolver := StaticKeyResolver{env.From: pub}
	if err := Verify(got, resolver); err != nil {
		t.Fatalf("Verify uncorrupted: %v", err)
	}
}

func TestBodyTypeOf_AndNewTypedBody(t *testing.T) {
	if BodyTypeOf("bogus") != nil {
		t.Fatal("BodyTypeOf must return nil for unknown")
	}
	if NewTypedBody("bogus") != nil {
		t.Fatal("NewTypedBody must return nil for unknown")
	}
	for _, k := range AllKinds {
		got := NewTypedBody(k)
		if got == nil {
			t.Errorf("NewTypedBody(%q) nil", k)
			continue
		}
		// Must be a pointer to the kind's struct type
		gotType := reflect.TypeOf(got)
		if gotType.Kind() != reflect.Ptr {
			t.Errorf("NewTypedBody(%q) not a pointer: %v", k, gotType)
		}
		if gotType.Elem() != BodyTypeOf(k) {
			t.Errorf("NewTypedBody(%q) elem mismatch: %v vs %v", k, gotType.Elem(), BodyTypeOf(k))
		}
	}
}

func TestEnvelope_BodyKindMismatchOnDecode(t *testing.T) {
	// Build a draft envelope, then attempt to decode body as the wrong type.
	// DecodeBody itself doesn't enforce kind↔type — ValidateBody does.
	env := makeEnv(t, KindIntentDraft, IntentDraftBody{Prose: "x"})

	// ValidateBody routes through the correct typed struct (no error)
	if _, err := ValidateBody(env); err != nil {
		t.Fatalf("ValidateBody: %v", err)
	}

	// DecodeBody into a wrong type: CBOR will populate whatever fields
	// match by tag, but won't return error. This is the documented
	// permissive contract — strict callers use ValidateBody.
	var wrong IntentAcceptBody
	_ = env.DecodeBody(&wrong) // No-error-but-empty is acceptable
}

func TestNewEnvelope_RejectsNilBody(t *testing.T) {
	_, err := NewEnvelope(KindIntentDraft, nil)
	if !errors.Is(err, ErrBodyTypeMismatch) {
		t.Fatalf("want ErrBodyTypeMismatch on nil body, got %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
