package model

import (
	"encoding/hex"

	"lukechampine.com/blake3"
)

// Digest computes a node's content digest from its raw source-range bytes (the
// declaration plus its leading doc comment). The input is LF-normalized and has
// per-line trailing whitespace stripped before hashing; no enrichment field ever
// participates. Rendered as "b3:<hex>". The digest is independent of the id: a
// digest change never changes the id, and computing an id never reads a digest.
func Digest(src []byte) string {
	sum := blake3.Sum256(NormalizeSource(src))
	return "b3:" + hex.EncodeToString(sum[:])
}

// NormalizeSource canonicalizes source bytes for hashing: CRLF/CR line endings
// become LF and each line is stripped of trailing spaces and tabs. The leading
// doc comment (if present in src) is preserved.
func NormalizeSource(src []byte) []byte {
	out := make([]byte, 0, len(src))
	line := make([]byte, 0, 128)
	flush := func() {
		end := len(line)
		for end > 0 && (line[end-1] == ' ' || line[end-1] == '\t') {
			end--
		}
		out = append(out, line[:end]...)
		line = line[:0]
	}
	for i := 0; i < len(src); i++ {
		switch c := src[i]; c {
		case '\r':
			flush()
			out = append(out, '\n')
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
		case '\n':
			flush()
			out = append(out, '\n')
		default:
			line = append(line, c)
		}
	}
	flush()
	return out
}
