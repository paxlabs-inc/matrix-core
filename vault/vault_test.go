package vault

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// testVault builds a Vault over a static KEK provider whose KEK bytes the test
// knows, so it can assert raw key material never leaks into serialized output.
func testVault(t *testing.T) (*Vault, []byte) {
	t.Helper()
	kek := bytes.Repeat([]byte{0xA5}, keyLen)
	kp, err := NewStaticKeyProvider(map[string][]byte{"kek1": kek}, "kek1")
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return New(kp), kek
}

func mustUser(t *testing.T, v *Vault, did string) *UserVault {
	t.Helper()
	kf, err := v.ProvisionUser(context.Background(), did)
	if err != nil {
		t.Fatalf("provision %s: %v", did, err)
	}
	uv, err := v.OpenUser(context.Background(), kf)
	if err != nil {
		t.Fatalf("open %s: %v", did, err)
	}
	return uv
}

func recAD(user string, seq uint64) AD {
	return AD{User: user, Store: "neo.conversation", Stream: "conv-1", Seq: seq, Schema: "turn.v1"}
}

func TestRecordFileRoundTrip(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:alice")
	pt := []byte(`{"role":"user","content":"hello sealed world"}`)

	obj, err := uv.SealRecord(recAD("did:matrix:alice", 7), pt)
	if err != nil {
		t.Fatalf("seal record: %v", err)
	}
	if !IsVault(obj) {
		t.Fatal("sealed record is not recognized as a vault object")
	}
	if bytes.Contains(obj, pt) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	got, err := uv.OpenRecord(recAD("did:matrix:alice", 7), obj)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open record: got %q err %v", got, err)
	}

	fad := AD{User: "did:matrix:alice", Store: "neo.task", Stream: "tasks.json", Schema: "ledger.v1"}
	fobj, err := uv.SealFile(fad, pt)
	if err != nil {
		t.Fatalf("seal file: %v", err)
	}
	fgot, err := uv.OpenFile(fad, fobj)
	if err != nil || !bytes.Equal(fgot, pt) {
		t.Fatalf("open file: got %q err %v", fgot, err)
	}
	// A record and a file are not interchangeable even under identical AD.
	if _, err := uv.OpenRecord(fad, fobj); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("cross-shape open want ErrUnsupported got %v", err)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:bob")
	ad := AD{User: "did:matrix:bob", Store: "neo.media", Stream: "screenshot-9", Schema: "png"}
	payload := bytes.Repeat([]byte("streaming-payload-0123456789"), 5000) // ~140 KiB, multi-chunk

	var buf bytes.Buffer
	sw, err := uv.StreamWriter(&buf, ad, 4096)
	if err != nil {
		t.Fatalf("stream writer: %v", err)
	}
	if _, err := sw.Write(payload); err != nil {
		t.Fatalf("stream write: %v", err)
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("stream close: %v", err)
	}
	if bytes.Contains(buf.Bytes(), payload[:64]) {
		t.Fatal("plaintext leaked into stream ciphertext")
	}
	sr, err := uv.StreamReader(bytes.NewReader(buf.Bytes()), ad)
	if err != nil {
		t.Fatalf("stream reader: %v", err)
	}
	got, err := io.ReadAll(sr)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("stream read: len(got)=%d err %v", len(got), err)
	}
}

func TestStreamEmptyAndExactMultiple(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:bob")
	ad := AD{User: "did:matrix:bob", Store: "neo.media", Stream: "x", Schema: "bin"}
	for _, payload := range [][]byte{nil, bytes.Repeat([]byte("A"), 4096), bytes.Repeat([]byte("B"), 8192)} {
		var buf bytes.Buffer
		sw, _ := uv.StreamWriter(&buf, ad, 4096)
		if _, err := sw.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := sw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		sr, err := uv.StreamReader(bytes.NewReader(buf.Bytes()), ad)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		got, err := io.ReadAll(sr)
		if err != nil {
			t.Fatalf("read len=%d: %v", len(payload), err)
		}
		if len(got) != len(payload) || !bytes.Equal(got, payload) {
			t.Fatalf("mismatch len=%d got=%d", len(payload), len(got))
		}
	}
}

func TestWrongUserKeyFailsAuth(t *testing.T) {
	// Two users seal independent objects; neither UserVault can open the other's
	// object (the header names a user-key id the other does not hold).
	v, _ := testVault(t)
	alice := mustUser(t, v, "did:matrix:alice")
	bob := mustUser(t, v, "did:matrix:bob")
	obj, _ := alice.SealRecord(recAD("did:matrix:alice", 1), []byte("secret"))
	if _, err := bob.OpenRecord(recAD("did:matrix:bob", 1), obj); err == nil {
		t.Fatal("bob decrypted alice's record")
	}

	// Force the true wrong-key crypto path: a vault that holds alice's key id but
	// the wrong key bytes. Unwrapping the DEK must collapse to ErrAuth.
	o, _ := alice.SealRecord(recAD("did:matrix:alice", 1), []byte("secret"))
	h, _, _ := unmarshalHeader(o)
	wrongKey, _ := randBytes(keyLen)
	uvWrong := &UserVault{user: "did:matrix:alice", activeUKID: h.UKID, uks: map[string][]byte{h.UKID: wrongKey}}
	if _, err := uvWrong.OpenRecord(recAD("did:matrix:alice", 1), o); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong-key open want ErrAuth got %v", err)
	}
}

func TestTamperAndCrossContextFailAuth(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:alice")
	obj, _ := uv.SealRecord(recAD("did:matrix:alice", 5), []byte("payload"))

	// Flip a ciphertext-body byte -> ErrAuth (indistinguishable from disclosure).
	tampered := append([]byte(nil), obj...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 5), tampered); !errors.Is(err, ErrAuth) {
		t.Fatalf("body tamper want ErrAuth got %v", err)
	}

	// Flip a header byte (the shape) -> header no longer matches; open fails.
	hdrTamper := append([]byte(nil), obj...)
	hdrTamper[6] ^= 0x01 // shape byte
	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 5), hdrTamper); err == nil {
		t.Fatal("header tamper decrypted")
	}

	// Cross-context: same bytes, reconstructed under a different sequence/user/store.
	for _, bad := range []AD{
		recAD("did:matrix:alice", 6), // wrong seq
		recAD("did:matrix:bob", 5),   // wrong user
		{User: "did:matrix:alice", Store: "neo.trace", Stream: "conv-1", Seq: 5, Schema: "turn.v1"}, // wrong store
	} {
		if _, err := uv.OpenRecord(bad, obj); !errors.Is(err, ErrAuth) {
			t.Fatalf("cross-context %+v want ErrAuth got %v", bad, err)
		}
	}
}

func TestTruncationDetected(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:alice")
	obj, _ := uv.SealRecord(recAD("did:matrix:alice", 1), []byte("some longer payload here"))

	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 1), obj[:len(obj)-4]); err == nil {
		t.Fatal("truncated record decrypted")
	}
	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 1), obj[:6]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short header want ErrTruncated got %v", err)
	}

	// Stream truncation: drop the final chunk -> ErrTruncated.
	ad := AD{User: "did:matrix:alice", Store: "neo.media", Stream: "s", Schema: "bin"}
	var buf bytes.Buffer
	sw, _ := uv.StreamWriter(&buf, ad, 32)
	_, _ = sw.Write(bytes.Repeat([]byte("z"), 200))
	_ = sw.Close()
	full := buf.Bytes()
	sr, _ := uv.StreamReader(bytes.NewReader(full[:len(full)-8]), ad)
	if _, err := io.ReadAll(sr); !errors.Is(err, ErrTruncated) {
		t.Fatalf("stream truncation want ErrTruncated got %v", err)
	}
}

func TestStreamReorderFailsAuth(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:alice")
	ad := AD{User: "did:matrix:alice", Store: "neo.media", Stream: "s", Schema: "bin"}
	var buf bytes.Buffer
	sw, _ := uv.StreamWriter(&buf, ad, 16)
	_, _ = sw.Write(bytes.Repeat([]byte("abcdefgh"), 8)) // 64 bytes -> 4 non-final + 1 final chunk
	_ = sw.Close()

	_, hb, err := readHeader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	frames := splitFrames(t, buf.Bytes()[len(hb):])
	if len(frames) < 3 {
		t.Fatalf("need >=3 frames, got %d", len(frames))
	}
	frames[0], frames[1] = frames[1], frames[0] // swap two non-final chunks
	var re bytes.Buffer
	re.Write(hb)
	for _, f := range frames {
		re.Write(f)
	}
	sr, _ := uv.StreamReader(bytes.NewReader(re.Bytes()), ad)
	if _, err := io.ReadAll(sr); !errors.Is(err, ErrAuth) {
		t.Fatalf("reordered stream want ErrAuth got %v", err)
	}
}

func splitFrames(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 5 {
			t.Fatal("dangling frame header")
		}
		n := int(binary.BigEndian.Uint32(b[1:5]))
		end := 5 + n
		if end > len(b) {
			t.Fatal("frame overruns buffer")
		}
		out = append(out, append([]byte(nil), b[:end]...))
		b = b[end:]
	}
	return out
}

func TestHeaderVersioningAndSniff(t *testing.T) {
	v, _ := testVault(t)
	uv := mustUser(t, v, "did:matrix:alice")

	legacy := []byte(`{"role":"user","content":"legacy plaintext line"}`)
	if IsVault(legacy) {
		t.Fatal("plaintext misdetected as vault object")
	}
	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 1), legacy); !errors.Is(err, ErrNotVault) {
		t.Fatalf("legacy line want ErrNotVault got %v", err)
	}

	obj, _ := uv.SealRecord(recAD("did:matrix:alice", 1), []byte("x"))
	bad := append([]byte(nil), obj...)
	bad[4] = 0x02 // unsupported format version
	if _, err := uv.OpenRecord(recAD("did:matrix:alice", 1), bad); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("bad version want ErrUnsupported got %v", err)
	}
}

func TestKEKRotationNoDataRewrite(t *testing.T) {
	kek1 := bytes.Repeat([]byte{0x11}, keyLen)
	kek2 := bytes.Repeat([]byte{0x22}, keyLen)
	kp, _ := NewStaticKeyProvider(map[string][]byte{"kek1": kek1}, "kek1")
	v := New(kp)
	ctx := context.Background()

	kf, _ := v.ProvisionUser(ctx, "did:matrix:alice")
	uv, _ := v.OpenUser(ctx, kf)
	ad := recAD("did:matrix:alice", 1)
	obj, _ := uv.SealRecord(ad, []byte("survives rotation"))
	objCopy := append([]byte(nil), obj...)
	wrappedBefore := append([]byte(nil), kf.Keys[0].Wrapped...)

	// Rotate the KEK: install kek2 as active and re-wrap the user key.
	if err := kp.AddKEK("kek2", kek2, true); err != nil {
		t.Fatalf("add kek: %v", err)
	}
	if err := v.RotateKEK(ctx, kf); err != nil {
		t.Fatalf("rotate kek: %v", err)
	}
	if kf.Keys[0].KEKID != "kek2" {
		t.Fatalf("kek id not advanced: %s", kf.Keys[0].KEKID)
	}
	if bytes.Equal(kf.Keys[0].Wrapped, wrappedBefore) {
		t.Fatal("wrapped user key unchanged after KEK rotation")
	}
	// The data object bytes were never touched.
	if !bytes.Equal(obj, objCopy) {
		t.Fatal("data object was rewritten during KEK rotation")
	}
	// Reopen under the rotated keyfile and decrypt the untouched object.
	uv2, err := v.OpenUser(ctx, kf)
	if err != nil {
		t.Fatalf("open after rotate: %v", err)
	}
	got, err := uv2.OpenRecord(ad, obj)
	if err != nil || string(got) != "survives rotation" {
		t.Fatalf("post-rotation open: got %q err %v", got, err)
	}
}

func TestUKRotationRetainsOldReads(t *testing.T) {
	v, _ := testVault(t)
	ctx := context.Background()
	kf, _ := v.ProvisionUser(ctx, "did:matrix:alice")
	uv, _ := v.OpenUser(ctx, kf)
	ad1 := recAD("did:matrix:alice", 1)
	old, _ := uv.SealRecord(ad1, []byte("written under uk1"))
	h1, _, _ := unmarshalHeader(old)

	if err := v.RotateUK(ctx, kf); err != nil {
		t.Fatalf("rotate uk: %v", err)
	}
	uv2, _ := v.OpenUser(ctx, kf)
	ad2 := recAD("did:matrix:alice", 2)
	fresh, _ := uv2.SealRecord(ad2, []byte("written under uk2"))
	h2, _, _ := unmarshalHeader(fresh)

	if h1.UKID == h2.UKID {
		t.Fatal("UK rotation did not change the active key id")
	}
	// Old object still readable under retained uk1.
	got, err := uv2.OpenRecord(ad1, old)
	if err != nil || string(got) != "written under uk1" {
		t.Fatalf("old read after uk rotation: got %q err %v", got, err)
	}
	// New object readable under uk2.
	got2, err := uv2.OpenRecord(ad2, fresh)
	if err != nil || string(got2) != "written under uk2" {
		t.Fatalf("new read after uk rotation: got %q err %v", got2, err)
	}
}

func TestNoRawKeyBytesInOutput(t *testing.T) {
	v, kek := testVault(t)
	ctx := context.Background()
	kf, _ := v.ProvisionUser(ctx, "did:matrix:alice")
	uv, _ := v.OpenUser(ctx, kf)

	kfJSON, err := kf.Marshal()
	if err != nil {
		t.Fatalf("marshal keyfile: %v", err)
	}
	if bytes.Contains(kfJSON, kek) {
		t.Fatal("raw KEK bytes present in serialized keyfile")
	}
	rec, _ := uv.SealRecord(recAD("did:matrix:alice", 1), []byte("data"))
	file, _ := uv.SealFile(AD{User: "did:matrix:alice", Store: "s", Stream: "f", Schema: "v"}, []byte("data"))
	var sbuf bytes.Buffer
	sw, _ := uv.StreamWriter(&sbuf, AD{User: "did:matrix:alice", Store: "s", Stream: "f", Schema: "v"}, 32)
	_, _ = sw.Write([]byte("data"))
	_ = sw.Close()
	for name, out := range map[string][]byte{"record": rec, "file": file, "stream": sbuf.Bytes(), "keyfile": kfJSON} {
		if bytes.Contains(out, kek) {
			t.Fatalf("raw KEK bytes present in %s output", name)
		}
	}

	// Error messages must not carry key material — they are static sentinels.
	for _, e := range []error{ErrAuth, ErrNotVault, ErrTruncated, ErrUnsupported, ErrKeyUnavailable, ErrVaultRequired} {
		if bytes.Contains([]byte(e.Error()), kek) {
			t.Fatalf("key bytes in error %q", e.Error())
		}
	}
}

func TestKMSProviderFailsClosedOnNilClient(t *testing.T) {
	if _, err := NewKMSKeyProvider(nil); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("nil KMS want ErrKeyUnavailable got %v", err)
	}
}
