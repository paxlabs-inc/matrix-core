// Package accumulator builds the per-batch Merkle commitment over accepted
// transfers and produces inclusion proofs for receipts. It reuses the cortex
// journal discipline: domain-separated SHA-256 leaves over a canonical, byte-
// stable preimage (see cortex/journal LeafHash), so two parties hashing the
// same transfer produce byte-identical leaves and the batch root is
// independently re-derivable. See layerx.frozen.kvx [receipts].
package accumulator

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// LeafDomain is prepended to the canonical transfer bytes before hashing.
// Version-prefixed so a format change forces a clean re-anchor.
const LeafDomain = "layerx.settlement.receipt.v1"

// nodePrefix domain-separates interior nodes from leaves (second-preimage
// resistance: an interior hash can never be mistaken for a leaf preimage).
const nodePrefix = 0x01

// CanonicalLeaf returns the byte-stable preimage for a transfer. Field order +
// fixed-width big-endian integers make the encoding deterministic across
// languages (the proxy can recompute it).
//
//	seq(uint64) | amount(int64) | ts_unixnano(int64) | len(from) | from | len(to) | to
func CanonicalLeaf(seq int64, fromDID, toDID string, amountMicro, tsUnixNano int64) []byte {
	from := []byte(fromDID)
	to := []byte(toDID)
	buf := make([]byte, 0, 8+8+8+4+len(from)+4+len(to))
	var u8 [8]byte
	binary.BigEndian.PutUint64(u8[:], uint64(seq))
	buf = append(buf, u8[:]...)
	binary.BigEndian.PutUint64(u8[:], uint64(amountMicro))
	buf = append(buf, u8[:]...)
	binary.BigEndian.PutUint64(u8[:], uint64(tsUnixNano))
	buf = append(buf, u8[:]...)
	var u4 [4]byte
	binary.BigEndian.PutUint32(u4[:], uint32(len(from)))
	buf = append(buf, u4[:]...)
	buf = append(buf, from...)
	binary.BigEndian.PutUint32(u4[:], uint32(len(to)))
	buf = append(buf, u4[:]...)
	buf = append(buf, to...)
	return buf
}

// LeafHash returns sha256(LeafDomain || canonical).
func LeafHash(canonical []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(LeafDomain))
	h.Write(canonical)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// LeafHashHex is the hex form, as stored on the transfer + returned in receipts.
func LeafHashHex(canonical []byte) string {
	h := LeafHash(canonical)
	return hex.EncodeToString(h[:])
}

func nodeHash(l, r [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	h.Write(l[:])
	h.Write(r[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Root computes the Merkle root over ordered leaves. An odd layer promotes its
// last node by hashing it with itself (standard duplicate-last). Empty -> zero.
func Root(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return [32]byte{}
	}
	layer := make([][32]byte, len(leaves))
	copy(layer, leaves)
	for len(layer) > 1 {
		next := make([][32]byte, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			if i+1 < len(layer) {
				next = append(next, nodeHash(layer[i], layer[i+1]))
			} else {
				next = append(next, nodeHash(layer[i], layer[i]))
			}
		}
		layer = next
	}
	return layer[0]
}

// ProofStep is one sibling on the path from a leaf to the root.
type ProofStep struct {
	Sibling       [32]byte
	SiblingIsLeft bool // true if the sibling is the LEFT input to the parent hash
}

// Proof returns the inclusion path for leaves[index].
func Proof(leaves [][32]byte, index int) ([]ProofStep, error) {
	if index < 0 || index >= len(leaves) {
		return nil, errors.New("accumulator: index out of range")
	}
	layer := make([][32]byte, len(leaves))
	copy(layer, leaves)
	idx := index
	var path []ProofStep
	for len(layer) > 1 {
		var sib [32]byte
		var sibLeft bool
		if idx%2 == 0 {
			// node is left; sibling is right (duplicate self if missing)
			if idx+1 < len(layer) {
				sib = layer[idx+1]
			} else {
				sib = layer[idx]
			}
			sibLeft = false
		} else {
			sib = layer[idx-1]
			sibLeft = true
		}
		path = append(path, ProofStep{Sibling: sib, SiblingIsLeft: sibLeft})
		next := make([][32]byte, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			if i+1 < len(layer) {
				next = append(next, nodeHash(layer[i], layer[i+1]))
			} else {
				next = append(next, nodeHash(layer[i], layer[i]))
			}
		}
		layer = next
		idx /= 2
	}
	return path, nil
}

// Verify recomputes the root from a leaf + path and compares to want.
func Verify(leaf [32]byte, path []ProofStep, want [32]byte) bool {
	cur := leaf
	for _, step := range path {
		if step.SiblingIsLeft {
			cur = nodeHash(step.Sibling, cur)
		} else {
			cur = nodeHash(cur, step.Sibling)
		}
	}
	return cur == want
}

// EncodePath renders a proof path as hex strings prefixed "l:" / "r:" (the
// sibling side), the form carried in a Receipt.inclusion_path.
func EncodePath(path []ProofStep) []string {
	out := make([]string, 0, len(path))
	for _, s := range path {
		side := "r"
		if s.SiblingIsLeft {
			side = "l"
		}
		out = append(out, side+":"+hex.EncodeToString(s.Sibling[:]))
	}
	return out
}

// DecodePath parses the hex/side encoding back into ProofSteps.
func DecodePath(enc []string) ([]ProofStep, error) {
	out := make([]ProofStep, 0, len(enc))
	for _, e := range enc {
		if len(e) < 2 || e[1] != ':' {
			return nil, fmt.Errorf("accumulator: malformed path step %q", e)
		}
		side := e[0]
		raw, err := hex.DecodeString(e[2:])
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("accumulator: bad sibling hex %q", e)
		}
		var sib [32]byte
		copy(sib[:], raw)
		out = append(out, ProofStep{Sibling: sib, SiblingIsLeft: side == 'l'})
	}
	return out, nil
}
