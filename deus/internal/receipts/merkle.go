package receipts

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

// Domain-separation prefixes guard against second-preimage attacks: a leaf hash
// can never be reinterpreted as an internal node and vice versa.
const (
	merkleLeafPrefix byte = 0x00
	merkleNodePrefix byte = 0x01
)

// MerkleRoot builds a deterministic binary merkle root over receipt digests.
// Leaves are domain-separated (0x00) and internal nodes (0x01); on an odd layer
// the trailing node is duplicated (hashed with itself) rather than promoted, so
// the tree shape is unambiguous and not malleable.
func MerkleRoot(digests []string) (string, error) {
	if len(digests) == 0 {
		return "", fmt.Errorf("receipts: empty merkle input")
	}
	layer := make([][]byte, 0, len(digests))
	for _, d := range digests {
		b, err := decodeHash(d)
		if err != nil {
			return "", err
		}
		layer = append(layer, hashLeaf(b))
	}
	sort.Slice(layer, func(i, j int) bool {
		return bytes.Compare(layer[i], layer[j]) < 0
	})
	for len(layer) > 1 {
		next := make([][]byte, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			left := layer[i]
			right := left
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			next = append(next, hashNode(left, right))
		}
		layer = next
	}
	return "0x" + hex.EncodeToString(layer[0]), nil
}

func hashLeaf(b []byte) []byte {
	buf := make([]byte, 0, 1+len(b))
	buf = append(buf, merkleLeafPrefix)
	buf = append(buf, b...)
	return crypto.Keccak256(buf)
}

func hashNode(left, right []byte) []byte {
	buf := make([]byte, 0, 1+len(left)+len(right))
	buf = append(buf, merkleNodePrefix)
	buf = append(buf, left...)
	buf = append(buf, right...)
	return crypto.Keccak256(buf)
}

func decodeHash(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("receipts: invalid digest %q", s)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("receipts: digest len %d", len(b))
	}
	return b, nil
}
