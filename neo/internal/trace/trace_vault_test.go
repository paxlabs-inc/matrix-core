package trace

import (
	"bufio"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"matrix/vault"
)

func traceSession(t *testing.T, user string) *vault.Session {
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

func rawTraceLines(t *testing.T, path string) []string {
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

func TestVaultTraceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openWithCap(dir, 2000)
	s.SetVault(traceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()

	s.Record("run1", Event{Seq: 1, Type: "tool.step", Fields: map[string]interface{}{"secret": "workspace path"}})
	s.Record("run1", Event{Seq: 2, Type: "tool.media"})
	s.Flush()

	path := filepath.Join(dir, "run1.trace.jsonl")
	lines := rawTraceLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("want 2 sealed lines, got %d", len(lines))
	}
	for i, l := range lines {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("trace line %d not sealed: %q", i, l)
		}
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "workspace path") {
		t.Fatal("plaintext leaked into sealed trace")
	}

	got := s.Load("run1")
	if len(got) != 2 || got[0].Type != "tool.step" || got[1].Type != "tool.media" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got[0].Fields["secret"] != "workspace path" {
		t.Fatalf("field not recovered: %+v", got[0].Fields)
	}
}

func TestVaultTraceRollupReSeals(t *testing.T) {
	dir := t.TempDir()
	s := openWithCap(dir, 3) // rolls at 2*3=6
	s.SetVault(traceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()

	for i := 0; i < 10; i++ {
		s.Record("run1", Event{Seq: i, Type: "tool.step"})
	}
	s.Flush()

	got := s.Load("run1")
	if len(got) != 3 {
		t.Fatalf("Load should present the retained cap of 3, got %d", len(got))
	}
	// The newest events survive, in order.
	if got[0].Seq != 7 || got[2].Seq != 9 {
		t.Fatalf("rollup kept wrong events: %+v", got)
	}
	for i, l := range rawTraceLines(t, filepath.Join(dir, "run1.trace.jsonl")) {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("post-rollup line %d not sealed", i)
		}
	}
}

func TestVaultTraceLegacyReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run1.trace.jsonl")
	legacy := `{"seq":1,"type":"tool.step"}` + "\n" + `{"seq":2,"type":"tool.media"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openWithCap(dir, 2000)
	s.SetVault(traceSession(t, "did:matrix:alice"), "did:matrix:alice")
	defer s.Close()

	got := s.Load("run1")
	if len(got) != 2 || got[0].Type != "tool.step" || got[1].Type != "tool.media" {
		t.Fatalf("legacy trace not read: %+v", got)
	}
}

func TestVaultTraceWrongUserSkips(t *testing.T) {
	dir := t.TempDir()
	a := openWithCap(dir, 2000)
	a.SetVault(traceSession(t, "did:matrix:alice"), "did:matrix:alice")
	a.Record("run1", Event{Seq: 1, Type: "tool.step"})
	a.Flush()
	a.Close()

	b := openWithCap(dir, 2000)
	b.SetVault(traceSession(t, "did:matrix:bob"), "did:matrix:bob")
	defer b.Close()
	if got := b.Load("run1"); got != nil {
		t.Fatalf("bob must not read alice's trace, got %+v", got)
	}
}
