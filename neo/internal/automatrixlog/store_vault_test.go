package automatrixlog

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"matrix/vault"
)

func vaultSessionFor(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected an encrypting session")
	}
	return sess
}

func sealedLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// TestVaultInboxRoundTrip proves every inbox record is an independently sealed
// line (record AEAD preserving O_APPEND) that reads back verbatim.
func TestVaultInboxRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	s := Open(dir)
	s.SetVault(sess, user)
	if _, err := s.Append(Record{OpportunitySummary: "secret opportunity", ResultSummary: "done well", ConversationID: "c1"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if _, err := s.Append(Record{OpportunitySummary: "second thing", ResultSummary: "also done", ConversationID: "c2"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	lines := sealedLines(t, s.path())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	for i, l := range lines {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("inbox line %d not sealed: %q", i, l)
		}
		if strings.Contains(l, "secret opportunity") || strings.Contains(l, "second thing") {
			t.Fatalf("plaintext leaked on line %d", i)
		}
	}

	// List returns newest-first.
	recs := s.List()
	if len(recs) != 2 || recs[0].OpportunitySummary != "second thing" || recs[1].ConversationID != "c1" {
		t.Fatalf("List mismatch: %+v", recs)
	}
}

// TestVaultInboxMarkReadReSeals proves the rewrite path (MarkRead) keeps every
// surviving record sealed and position-bound.
func TestVaultInboxMarkReadReSeals(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)
	s := Open(dir)
	s.SetVault(sess, user)
	first, err := s.Append(Record{OpportunitySummary: "one", ConversationID: "c1"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.Append(Record{OpportunitySummary: "two", ConversationID: "c2"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	ok, err := s.MarkRead(first.ID)
	if err != nil || !ok {
		t.Fatalf("MarkRead: ok=%v err=%v", ok, err)
	}
	for i, l := range sealedLines(t, s.path()) {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("post-rewrite line %d not sealed: %q", i, l)
		}
	}
	recs := s.List()
	if len(recs) != 2 || recs[0].Read || !recs[1].Read {
		t.Fatalf("MarkRead round trip mismatch: %+v", recs)
	}
	if s.Unread() != 1 {
		t.Fatalf("Unread = %d, want 1", s.Unread())
	}
}

// TestVaultInboxCrossPositionSwap proves records are position-bound: two
// sealed lines swapped on disk both fail authentication and are skipped.
func TestVaultInboxCrossPositionSwap(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)
	s := Open(dir)
	s.SetVault(sess, user)
	if _, err := s.Append(Record{OpportunitySummary: "one"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := s.Append(Record{OpportunitySummary: "two"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	lines := sealedLines(t, s.path())
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	swapped := lines[1] + "\n" + lines[0] + "\n"
	if err := os.WriteFile(s.path(), []byte(swapped), 0o600); err != nil {
		t.Fatalf("swap write: %v", err)
	}
	if recs := s.List(); len(recs) != 0 {
		t.Fatalf("swapped records still readable: %+v", recs)
	}
}

// TestVaultInboxWrongUser proves another user's key reads nothing.
func TestVaultInboxWrongUser(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if _, err := s.Append(Record{OpportunitySummary: "private"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	s2 := Open(dir)
	s2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if recs := s2.List(); len(recs) != 0 {
		t.Fatalf("wrong user read sealed inbox: %+v", recs)
	}
}

// TestVaultInboxLegacyPlaintext proves pre-vault plaintext lines stay readable
// alongside sealed appends (per-line sniffing), positions intact.
func TestVaultInboxLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := Open(dir)
	if _, err := plain.Append(Record{OpportunitySummary: "old plain"}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	user := "did:matrix:alice"
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, user), user)
	if _, err := s.Append(Record{OpportunitySummary: "new sealed"}); err != nil {
		t.Fatalf("append sealed: %v", err)
	}

	recs := s.List()
	if len(recs) != 2 || recs[0].OpportunitySummary != "new sealed" || recs[1].OpportunitySummary != "old plain" {
		t.Fatalf("mixed-file read mismatch: %+v", recs)
	}
	lines := sealedLines(t, s.path())
	if vault.IsSealedLine([]byte(lines[0])) || !vault.IsSealedLine([]byte(lines[1])) {
		t.Fatal("expected line 0 plaintext, line 1 sealed")
	}
}
