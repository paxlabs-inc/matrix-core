package receipts_test

import (
	"testing"

	"github.com/paxlabs-inc/deus/internal/receipts"
)

func TestMerkleRootDeterministic(t *testing.T) {
	digests := []string{
		"0x0000000000000000000000000000000000000000000000000000000000000001",
		"0x0000000000000000000000000000000000000000000000000000000000000002",
	}
	a, err := receipts.MerkleRoot(digests)
	if err != nil {
		t.Fatal(err)
	}
	b, err := receipts.MerkleRoot([]string{digests[1], digests[0]})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("merkle not order-independent: %s vs %s", a, b)
	}
}

func TestMerkleRootOddLeaves(t *testing.T) {
	digests := []string{
		"0x0000000000000000000000000000000000000000000000000000000000000001",
		"0x0000000000000000000000000000000000000000000000000000000000000002",
		"0x0000000000000000000000000000000000000000000000000000000000000003",
	}
	a, err := receipts.MerkleRoot(digests)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic across reordering (trailing node is duplicated, not promoted).
	b, err := receipts.MerkleRoot([]string{digests[2], digests[0], digests[1]})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("odd-leaf merkle not deterministic: %s vs %s", a, b)
	}
	// Domain separation: a single raw 32-byte digest is never its own root.
	single, err := receipts.MerkleRoot([]string{digests[0]})
	if err != nil {
		t.Fatal(err)
	}
	if single == digests[0] {
		t.Fatal("leaf must be domain-separated, not passed through as the root")
	}
}
