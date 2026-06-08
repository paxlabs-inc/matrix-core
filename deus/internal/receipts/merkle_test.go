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
