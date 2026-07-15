package vault

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func jsonlSession(t *testing.T, user string) *Session {
	t.Helper()
	s, err := Boot(context.Background(), Config{Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: devKEKHex()})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return s
}

func jsonlAD(user, conv string, seq uint64) AD {
	return AD{User: user, Store: "neo.conversation", Stream: conv, Seq: seq, Schema: "turn.v1"}
}

func TestEncodeLineRoundTrip(t *testing.T) {
	s := jsonlSession(t, "did:matrix:alice")
	rec := []byte(`{"role":"user","text":"hello there"}`)
	ad := jsonlAD("did:matrix:alice", "c1", 0)

	line, err := s.EncodeLine(ad, rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if bytes.ContainsRune(line, '\n') {
		t.Fatal("encoded line must be newline-free")
	}
	if !IsSealedLine(line) {
		t.Fatal("encoded line must sniff as sealed")
	}
	got, err := s.DecodeLine(ad, line)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, rec) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, rec)
	}
}

func TestDecodeLineLegacyPlaintextPassthrough(t *testing.T) {
	s := jsonlSession(t, "did:matrix:alice")
	// A pre-migration JSON line begins with '{' and is not base64 -> read as-is.
	legacy := []byte(`{"role":"assistant","text":"legacy"}`)
	got, err := s.DecodeLine(jsonlAD("did:matrix:alice", "c1", 0), legacy)
	if err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if !bytes.Equal(got, legacy) {
		t.Fatalf("legacy line must pass through unchanged: got %q", got)
	}
}

func TestDecodeLineCrossContextFailsAuth(t *testing.T) {
	s := jsonlSession(t, "did:matrix:alice")
	rec := []byte(`{"role":"user","text":"secret"}`)
	line, err := s.EncodeLine(jsonlAD("did:matrix:alice", "c1", 3), rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Same bytes read back under a different sequence, conversation, or store all
	// reconstruct to a different AD and must fail authentication.
	for name, ad := range map[string]AD{
		"wrong-seq":   jsonlAD("did:matrix:alice", "c1", 4),
		"wrong-conv":  jsonlAD("did:matrix:alice", "c2", 3),
		"wrong-store": {User: "did:matrix:alice", Store: "neo.trace", Stream: "c1", Seq: 3, Schema: "turn.v1"},
	} {
		if _, err := s.DecodeLine(ad, line); !errors.Is(err, ErrAuth) {
			t.Fatalf("%s: want ErrAuth, got %v", name, err)
		}
	}
}

func TestDecodeLineWrongUserFailsAuth(t *testing.T) {
	alice := jsonlSession(t, "did:matrix:alice")
	line, err := alice.EncodeLine(jsonlAD("did:matrix:alice", "c1", 0), []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A different user's vault cannot decrypt alice's record: different UK and,
	// even were the bytes forced, different AD.User.
	bob := jsonlSession(t, "did:matrix:bob")
	if _, err := bob.DecodeLine(jsonlAD("did:matrix:bob", "c1", 0), line); err == nil {
		t.Fatal("bob must not decode alice's record")
	}
}

func TestDecodeLineTamperFailsAuth(t *testing.T) {
	s := jsonlSession(t, "did:matrix:alice")
	ad := jsonlAD("did:matrix:alice", "c1", 0)
	line, err := s.EncodeLine(ad, []byte(`{"role":"user","text":"authentic"}`))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Flip a byte in the middle of the encoded ciphertext body.
	tampered := append([]byte(nil), line...)
	mid := len(tampered) / 2
	tampered[mid] ^= 0x01
	if _, err := s.DecodeLine(ad, tampered); err == nil {
		t.Fatal("tampered line must not decode")
	}
}

func TestEncodeLineDevPassthrough(t *testing.T) {
	// An explicitly-disabled dev session writes plaintext and reads it back.
	s, err := Boot(context.Background(), Config{Required: false, DataDir: t.TempDir(), UserDID: "did:matrix:alice"})
	if err != nil {
		t.Fatalf("dev boot: %v", err)
	}
	rec := []byte(`{"role":"user","text":"dev"}`)
	line, err := s.EncodeLine(jsonlAD("did:matrix:alice", "c1", 0), rec)
	if err != nil || !bytes.Equal(line, rec) {
		t.Fatalf("dev encode passthrough: got %q err %v", line, err)
	}
	got, err := s.DecodeLine(jsonlAD("did:matrix:alice", "c1", 0), line)
	if err != nil || !bytes.Equal(got, rec) {
		t.Fatalf("dev decode passthrough: got %q err %v", got, err)
	}
}

func TestEncodeLineRequiredNoKeyHardErrors(t *testing.T) {
	s := &Session{required: true}
	if _, err := s.EncodeLine(jsonlAD("u", "c", 0), []byte(`{"x":1}`)); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("want ErrVaultRequired, got %v", err)
	}
}

func TestDecodeLineSealedWithoutKeyHardErrors(t *testing.T) {
	s := jsonlSession(t, "did:matrix:alice")
	line, err := s.EncodeLine(jsonlAD("did:matrix:alice", "c1", 0), []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A keyless session must not silently return sealed bytes as content.
	dev := &Session{}
	if _, err := dev.DecodeLine(jsonlAD("did:matrix:alice", "c1", 0), line); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("want ErrKeyUnavailable, got %v", err)
	}
}
