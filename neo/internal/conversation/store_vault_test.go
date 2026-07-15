package conversation

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

// sealingStore boots a real encrypting vault for the user and wires it into a
// fresh store rooted at dir with the given retained-turn cap.
func sealingStore(t *testing.T, dir, user string, cap int) *Store {
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
	s := openWithCap(dir, cap)
	s.SetVault(sess, user)
	return s
}

func rawLines(t *testing.T, path string) []string {
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

// TestVaultEncryptedRoundTrip proves turns are sealed on disk (no plaintext) yet
// read back verbatim through the store.
func TestVaultEncryptedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := sealingStore(t, dir, "did:matrix:alice", 1000)
	convID := "c-round"

	s.AppendUser(convID, "i1", "the secret plan")
	s.AppendAssistant(convID, "i1", "acknowledged")

	// On disk: every line is a sealed vault record, and the plaintext never
	// appears in the bytes.
	path := filepath.Join(dir, convID+".jsonl")
	lines := rawLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("want 2 sealed lines, got %d", len(lines))
	}
	for i, l := range lines {
		if !vault.IsSealedLine([]byte(l)) {
			t.Fatalf("line %d is not sealed: %q", i, l)
		}
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "the secret plan") {
		t.Fatal("plaintext leaked into the sealed file")
	}

	rec := s.Get(convID)
	if rec == nil || len(rec.Turns) != 2 {
		t.Fatalf("want 2 turns back, got %+v", rec)
	}
	if rec.Turns[0].Text != "the secret plan" || rec.Turns[1].Text != "acknowledged" {
		t.Fatalf("round-trip mismatch: %+v", rec.Turns)
	}
}

// TestVaultReopenDecrypts proves a second store instance booting the SAME key
// decrypts records written by the first (durable across restart).
func TestVaultReopenDecrypts(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	kekDir := t.TempDir()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	boot := func() *Store {
		sess, err := vault.Boot(context.Background(), vault.Config{
			Required: true, DataDir: kekDir, UserDID: user, KEKHex: kek,
		})
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		s := Open(dir)
		s.SetVault(sess, user)
		return s
	}
	s1 := boot()
	s1.AppendUser("c", "", "persisted turn")

	s2 := boot()
	rec := s2.Get("c")
	if rec == nil || len(rec.Turns) != 1 || rec.Turns[0].Text != "persisted turn" {
		t.Fatalf("reopen did not decrypt prior write: %+v", rec)
	}
}

// TestVaultLegacyPlaintextReadable proves header-sniffing reads carry legacy
// plaintext lines written before migration, so no history is lost when the
// vault is wired on.
func TestVaultLegacyPlaintextReadable(t *testing.T) {
	dir := t.TempDir()
	convID := "c-legacy"
	// A pre-vault plaintext store: write two JSON turn lines directly.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, convID+".jsonl")
	legacy := `{"role":"user","text":"old turn one","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"role":"assistant","text":"old turn two","ts":"2026-01-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s := sealingStore(t, dir, "did:matrix:alice", 1000)
	rec := s.Get(convID)
	if rec == nil || len(rec.Turns) != 2 {
		t.Fatalf("legacy plaintext not read: %+v", rec)
	}
	if rec.Turns[0].Text != "old turn one" || rec.Turns[1].Text != "old turn two" {
		t.Fatalf("legacy content mismatch: %+v", rec.Turns)
	}

	// A new write after migration-on is sealed, and mixed reads still work.
	s.AppendUser(convID, "", "new sealed turn")
	lines := rawLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if vault.IsSealedLine([]byte(lines[0])) || vault.IsSealedLine([]byte(lines[1])) {
		t.Fatal("legacy lines must stay plaintext")
	}
	if !vault.IsSealedLine([]byte(lines[2])) {
		t.Fatal("new line must be sealed")
	}
	full := s.History(convID)
	if len(full) != 3 || full[2].Text != "new sealed turn" {
		t.Fatalf("mixed read failed: %+v", full)
	}
}

// TestVaultRollupReSeals proves the retained-turn rollup re-seals the kept hot
// set at its new positions and seals the rolled archive, with full history
// intact and every on-disk line sealed.
func TestVaultRollupReSeals(t *testing.T) {
	dir := t.TempDir()
	s := sealingStore(t, dir, "did:matrix:alice", 3)
	convID := "c-roll"
	for i := 0; i < 8; i++ {
		s.AppendUser(convID, "", turnText(i))
	}

	hot := s.Get(convID)
	if hot == nil || len(hot.Turns) != 3 {
		t.Fatalf("hot set should cap at 3, got %+v", hot)
	}
	archived := s.Archived(convID)
	if len(archived) != 5 {
		t.Fatalf("want 5 archived, got %d", len(archived))
	}
	full := s.History(convID)
	if len(full) != 8 {
		t.Fatalf("full history want 8, got %d", len(full))
	}
	for i := 0; i < 8; i++ {
		if full[i].Text != turnText(i) {
			t.Fatalf("history[%d]=%q want %q", i, full[i].Text, turnText(i))
		}
	}
	// Every on-disk line in both hot and archive files is sealed.
	for _, p := range []string{
		filepath.Join(dir, convID+".jsonl"),
		filepath.Join(dir, convID+".archive.jsonl"),
	} {
		for i, l := range rawLines(t, p) {
			if !vault.IsSealedLine([]byte(l)) {
				t.Fatalf("%s line %d not sealed", p, i)
			}
		}
	}
}

// TestVaultTornTailRecovery proves a crash mid-append (a truncated final sealed
// line) is skipped while every fully-written sealed record survives.
func TestVaultTornTailRecovery(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	kekDir := t.TempDir()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	boot := func() *Store {
		sess, err := vault.Boot(context.Background(), vault.Config{
			Required: true, DataDir: kekDir, UserDID: user, KEKHex: kek,
		})
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		s := Open(dir)
		s.SetVault(sess, user)
		return s
	}
	s := boot()
	s.AppendUser("c", "", "committed-1")
	s.AppendAssistant("c", "", "committed-2")

	// Simulate a crash: append a partial (no trailing newline) garbage line.
	path := filepath.Join(dir, "c.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("TVhWdGVhcnRvcnRvcnRvcg"); err != nil { // torn base64-ish
		t.Fatal(err)
	}
	f.Close()

	s2 := boot()
	rec := s2.Get("c")
	if rec == nil || len(rec.Turns) != 2 {
		t.Fatalf("torn tail must not corrupt store: got %+v", rec)
	}
	if rec.Turns[0].Text != "committed-1" || rec.Turns[1].Text != "committed-2" {
		t.Fatalf("committed turns corrupted: %+v", rec.Turns)
	}
}

// TestVaultWrongUserCannotRead proves a different user's vault cannot decrypt
// records (records are skipped rather than leaked).
func TestVaultWrongUserCannotRead(t *testing.T) {
	dir := t.TempDir()
	alice := sealingStore(t, dir, "did:matrix:alice", 1000)
	alice.AppendUser("c", "", "alice private")

	// A store wired with a DIFFERENT user's vault reconstructs different AD and
	// holds a different key: alice's records fail authentication and are skipped.
	bob := sealingStore(t, dir, "did:matrix:bob", 1000)
	if rec := bob.Get("c"); rec != nil {
		t.Fatalf("bob must not read alice's records, got %+v", rec)
	}
}

func turnText(i int) string { return "turn-" + string(rune('0'+i)) }
