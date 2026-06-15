package accumulator

import "testing"

func leaves(n int) [][32]byte {
	out := make([][32]byte, n)
	for i := 0; i < n; i++ {
		out[i] = LeafHash(CanonicalLeaf(int64(i+1), "did:matrix:a:0123456789abcdef", "did:matrix:b:fedcba9876543210", int64((i+1)*1000), int64(i+1)))
	}
	return out
}

func TestLeafDeterministic(t *testing.T) {
	a := LeafHashHex(CanonicalLeaf(7, "from", "to", 1234, 99))
	b := LeafHashHex(CanonicalLeaf(7, "from", "to", 1234, 99))
	if a != b {
		t.Fatal("leaf hashing must be deterministic")
	}
	c := LeafHashHex(CanonicalLeaf(8, "from", "to", 1234, 99))
	if a == c {
		t.Fatal("different seq must produce a different leaf")
	}
}

func TestProofVerifies(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 8, 13} {
		ls := leaves(n)
		root := Root(ls)
		for i := 0; i < n; i++ {
			path, err := Proof(ls, i)
			if err != nil {
				t.Fatalf("n=%d i=%d Proof: %v", n, i, err)
			}
			if !Verify(ls[i], path, root) {
				t.Fatalf("n=%d i=%d proof did not verify", n, i)
			}
			// round-trip the encoded path
			dec, err := DecodePath(EncodePath(path))
			if err != nil {
				t.Fatalf("DecodePath: %v", err)
			}
			if !Verify(ls[i], dec, root) {
				t.Fatalf("n=%d i=%d decoded proof did not verify", n, i)
			}
		}
	}
}

func TestTamperFails(t *testing.T) {
	ls := leaves(6)
	root := Root(ls)
	path, _ := Proof(ls, 2)
	// wrong leaf must not verify against the real root
	if Verify(ls[3], path, root) {
		t.Fatal("a different leaf must not verify against another leaf's path")
	}
}
