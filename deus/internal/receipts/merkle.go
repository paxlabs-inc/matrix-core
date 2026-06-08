package receipts

import (
	"encoding/hex"
	"fmt"
	"bytes"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
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
		layer = append(layer, b)
	}
	sort.Slice(layer, func(i, j int) bool {
		return bytes.Compare(layer[i], layer[j]) < 0
	})
	for len(layer) > 1 {
		var next [][]byte
		for i := 0; i < len(layer); i += 2 {
			if i+1 == len(layer) {
				next = append(next, layer[i])
				continue
			}
			pair := make([]byte, 0, len(layer[i])+len(layer[i+1]))
			pair = append(pair, layer[i]...)
			pair = append(pair, layer[i+1]...)
			h := crypto.Keccak256(pair)
			next = append(next, h)
		}
		layer = next
	}
	return "0x" + hex.EncodeToString(layer[0]), nil
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
