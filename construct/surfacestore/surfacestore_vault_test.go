package surfacestore

import (
	"bufio"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/construct/schema"
	"matrix/vault"
)

func surfaceSession(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected encrypting session")
	}
	return sess
}

func rawSurfaceLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		out = append(out, sc.Text())
	}
	return out
}

func TestVaultSurfaceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openWithCap(dir, 2000)
	s.SetVault(surfaceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()

	conv := "conv1"
	s.Record(conv, schema.Frame{Seq: 1, Type: "construct.surface", Fields: map[string]interface{}{"secret": "surface data"}})
	s.Record(conv, schema.Frame{Seq: 2, Type: "construct.surface.patch"})
	s.Flush()

	path := filepath.Join(dir, conv+".surfaces.jsonl")
	lines := rawSurfaceLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("want 2 sealed lines, got %d", len(lines))
	}
	for i, l := range lines {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("surface line %d not sealed: %q", i, l)
		}
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "surface data") {
		t.Fatal("plaintext leaked into sealed surface store")
	}

	got := s.Load(conv)
	if len(got) != 2 || got[0].Type != "construct.surface" || got[1].Type != "construct.surface.patch" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got[0].Fields["secret"] != "surface data" {
		t.Fatalf("field not recovered: %+v", got[0].Fields)
	}
}

func TestVaultSurfaceLegacyReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	conv := "conv1"
	path := filepath.Join(dir, conv+".surfaces.jsonl")
	legacy := `{"seq":1,"type":"construct.surface"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openWithCap(dir, 2000)
	s.SetVault(surfaceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()
	got := s.Load(conv)
	if len(got) != 1 || got[0].Type != "construct.surface" {
		t.Fatalf("legacy surface not read: %+v", got)
	}
}

func TestVaultSurfaceRollupReSeals(t *testing.T) {
	dir := t.TempDir()
	s := openWithCap(dir, 3) // rolls at 6
	s.SetVault(surfaceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()
	conv := "conv1"
	for i := 0; i < 10; i++ {
		s.Record(conv, schema.Frame{Seq: i, Type: "construct.surface.patch"})
	}
	s.Flush()
	got := s.Load(conv)
	if len(got) != 3 {
		t.Fatalf("Load should present retained cap 3, got %d", len(got))
	}
	if got[0].Seq != 7 || got[2].Seq != 9 {
		t.Fatalf("rollup kept wrong frames: %+v", got)
	}
	for i, l := range rawSurfaceLines(t, filepath.Join(dir, conv+".surfaces.jsonl")) {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("post-rollup line %d not sealed", i)
		}
	}
}
