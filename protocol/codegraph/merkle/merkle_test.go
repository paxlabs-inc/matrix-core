package merkle

import (
	"bytes"
	"reflect"
	"testing"
)

func tree(files map[string]string) *Tree {
	contents := map[string][]byte{}
	for p, s := range files {
		contents[p] = []byte(s)
	}
	return FromContentMap(contents)
}

func TestDeterministicRoot(t *testing.T) {
	a := tree(map[string]string{"a/x.go": "package a", "b/y.go": "package b"})
	b := tree(map[string]string{"b/y.go": "package b", "a/x.go": "package a"})
	if a.Root != b.Root {
		t.Fatalf("root not order-independent: %s vs %s", a.Root, b.Root)
	}
	if a.Root == "" {
		t.Fatal("empty root")
	}
}

func TestLFNormalizationStableAcrossLineEndings(t *testing.T) {
	unix := tree(map[string]string{"a/x.go": "package a\nfunc F() {}\n"})
	win := tree(map[string]string{"a/x.go": "package a\r\nfunc F() {}\r\n"})
	if unix.Root != win.Root {
		t.Fatalf("CRLF changed the root: %s vs %s", unix.Root, win.Root)
	}
}

func TestDiffPinpointsChangedFile(t *testing.T) {
	old := tree(map[string]string{"a/x.go": "package a", "a/y.go": "package a", "b/z.go": "package b"})
	new := tree(map[string]string{"a/x.go": "package a CHANGED", "a/y.go": "package a", "b/z.go": "package b"})
	c := Diff(old, new)
	if !reflect.DeepEqual(c.Changed, []string{"a/x.go"}) {
		t.Fatalf("changed = %v, want [a/x.go]", c.Changed)
	}
	if len(c.Added) != 0 || len(c.Removed) != 0 {
		t.Fatalf("unexpected add/remove: %+v", c)
	}
}

func TestDiffAddRemove(t *testing.T) {
	old := tree(map[string]string{"a/x.go": "package a"})
	new := tree(map[string]string{"a/y.go": "package a"})
	c := Diff(old, new)
	if !reflect.DeepEqual(c.Added, []string{"a/y.go"}) || !reflect.DeepEqual(c.Removed, []string{"a/x.go"}) {
		t.Fatalf("add/remove wrong: %+v", c)
	}
}

func TestIdenticalTreesEmptyDiff(t *testing.T) {
	files := map[string]string{"a/x.go": "package a", "deep/nested/dir/f.go": "package d"}
	if c := Diff(tree(files), tree(files)); !c.Empty() {
		t.Fatalf("identical trees produced a delta: %+v", c)
	}
}

func TestNilOldTreeIsAllAdded(t *testing.T) {
	new := tree(map[string]string{"a/x.go": "package a", "b/y.go": "package b"})
	c := Diff(nil, new)
	want := []string{"a/x.go", "b/y.go"}
	if !reflect.DeepEqual(c.Added, want) {
		t.Fatalf("added = %v, want %v", c.Added, want)
	}
}

func TestUnchangedSiblingDirNotReported(t *testing.T) {
	// Change one file under a/ and assert b/ (a whole unchanged subtree) never
	// surfaces — this is the subtree-skip guarantee.
	old := tree(map[string]string{"a/x.go": "1", "a/y.go": "2", "b/m.go": "3", "b/n.go": "4"})
	new := tree(map[string]string{"a/x.go": "1x", "a/y.go": "2", "b/m.go": "3", "b/n.go": "4"})
	c := Diff(old, new)
	for _, p := range c.All() {
		if len(p) > 0 && p[0] == 'b' {
			t.Fatalf("unchanged subtree b/ leaked into diff: %v", c.All())
		}
	}
	if !reflect.DeepEqual(c.Changed, []string{"a/x.go"}) {
		t.Fatalf("changed = %v", c.Changed)
	}
	// The b/ subtree hash must be identical in both trees.
	if old.dirs["b"] != new.dirs["b"] {
		t.Fatal("unchanged subtree b/ has a different hash")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	orig := tree(map[string]string{"a/x.go": "package a", "b/c/y.go": "package c", "top.go": "package main"})
	var buf bytes.Buffer
	if err := Write(&buf, orig); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != orig.Root {
		t.Fatalf("round-trip root changed: %s vs %s", got.Root, orig.Root)
	}
	if !reflect.DeepEqual(got.Files(), orig.Files()) {
		t.Fatalf("round-trip files changed: %v vs %v", got.Files(), orig.Files())
	}
}

func TestStoreRejectsTamper(t *testing.T) {
	orig := tree(map[string]string{"a/x.go": "package a"})
	var buf bytes.Buffer
	if err := Write(&buf, orig); err != nil {
		t.Fatal(err)
	}
	// Corrupt the leaf hash on the "F a/x.go b3:..." line so the rebuilt root no
	// longer matches the banner root, which must be detected as tampering.
	tampered := bytes.Replace(buf.Bytes(), []byte("F a/x.go b3:"), []byte("F a/x.go b3:deadbeef"), 1)
	if _, err := Read(bytes.NewReader(tampered)); err == nil {
		t.Fatal("Read must reject a tree whose leaves no longer hash to the banner root")
	}
}
