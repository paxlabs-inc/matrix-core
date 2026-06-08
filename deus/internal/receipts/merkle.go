package receipts

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

const (
	merkleLeafPrefix = 0x00
	merkleNodePrefix = 0x01
)

// MerkleRoot builds a deterministic binary merkle root over receipt digests.
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
		var next [][]byte
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

func hashLeaf(leaf []byte) []byte {
	buf := make([]byte, 0, 1+len(leaf))
	buf = append(buf, merkleLeafPrefix)
	buf = append(buf, leaf...)
	return crypto.Keccak256(buf)
}

func hashNode(left, right []byte) []byte {
	if bytes.Compare(left, right) > 0 {
		left, right = right, left
	}
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
