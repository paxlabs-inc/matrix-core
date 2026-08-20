package model

import (
	"strings"
	"testing"
)

func TestNormalizeSource(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\nb\r\n", "a\nb\n"},
		{"a\rb", "a\nb"},
		{"trail   \nx\t\t\n", "trail\nx\n"},
		{"no newline", "no newline"},
		{"", ""},
	}
	for _, c := range cases {
		if got := string(NormalizeSource([]byte(c.in))); got != c.want {
			t.Fatalf("NormalizeSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDigest_FormatAndDeterminism(t *testing.T) {
	d := Digest([]byte("func F() {}"))
	if !strings.HasPrefix(d, "b3:") {
		t.Fatalf("digest %q missing b3: prefix", d)
	}
	if len(d) != len("b3:")+64 {
		t.Fatalf("digest %q not 256-bit hex", d)
	}
	if Digest([]byte("func F() {}")) != d {
		t.Fatal("digest not deterministic")
	}
}

func TestDigest_IgnoresTrailingWhitespaceAndCRLF(t *testing.T) {
	a := Digest([]byte("func F() {\n\treturn\n}"))
	b := Digest([]byte("func F() {   \r\n\treturn\t\r\n}"))
	if a != b {
		t.Fatalf("digest should ignore CRLF + trailing ws: %q vs %q", a, b)
	}
}

// Property 2: id stability + digest sensitivity. A body edit that leaves the
// qualified name and kind unchanged keeps the id stable and moves only the
// digest; a rename moves the id. id assignment never reads the body, and the
// digest never reads the id — the two are independent (req 1.4, 2.1, 2.2, 2.5).
func TestProperty_IdStableDigestSensitive(t *testing.T) {
	const pkg = "centra/core/cortex"
	file := "cortex/store.go"
	r := Range{StartLine: 10, EndLine: 12}

	idV1 := Id(KindFunc, pkg, pkg, "", "NewStore", file, r)
	body1 := []byte("// NewStore opens a store.\nfunc NewStore() *Store { return &Store{} }")
	digV1 := Digest(body1)

	// Body edit, same name/kind: id stable, digest moves.
	body2 := []byte("// NewStore opens a store.\nfunc NewStore() *Store { return &Store{ready: true} }")
	idV2 := Id(KindFunc, pkg, pkg, "", "NewStore", file, r)
	digV2 := Digest(body2)
	if idV1 != idV2 {
		t.Fatalf("body edit changed id: %q -> %q", idV1, idV2)
	}
	if digV1 == digV2 {
		t.Fatal("body edit did not change digest")
	}

	// Rename, same body: id moves, digest unchanged.
	idRenamed := Id(KindFunc, pkg, pkg, "", "OpenStore", file, r)
	if idRenamed == idV1 {
		t.Fatal("rename did not change id")
	}
	if Digest(body1) != digV1 {
		t.Fatal("digest depends on id/name — it must not")
	}
}
