package merkle

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// BannerPrefix is the first line of a serialized Merkle tree; the root hash
// follows it so a load can verify integrity and CI can detect staleness.
const BannerPrefix = "# GENERATED gen=codegraph-merkle do-not-edit root="

// Write serializes the tree as line-diffable .kvx: a banner carrying the root,
// then one "F <path> <hash>" line per file sorted by path. Directory hashes are
// recomputed on load, so only the leaves are stored.
func Write(w io.Writer, t *Tree) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "%s%s\n", BannerPrefix, t.Root)
	for _, p := range t.Files() {
		fmt.Fprintf(bw, "F %s %s\n", p, t.files[p])
	}
	return bw.Flush()
}

// Read parses a serialized tree and rebuilds it. It returns an error if the
// rebuilt root does not match the banner root, catching corruption or a
// hand-edit of the generated file.
func Read(r io.Reader) (*Tree, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	leaves := map[string]string{}
	banner := ""
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if !strings.HasPrefix(line, BannerPrefix) {
				return nil, fmt.Errorf("merkle: missing banner")
			}
			banner = strings.TrimPrefix(line, BannerPrefix)
			continue
		}
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "F ") {
			return nil, fmt.Errorf("merkle: unexpected line %q", line)
		}
		rest := line[2:]
		sp := strings.LastIndexByte(rest, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("merkle: malformed leaf %q", line)
		}
		leaves[rest[:sp]] = rest[sp+1:]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	t := FromLeaves(leaves)
	if banner != "" && t.Root != banner {
		return nil, fmt.Errorf("merkle: root mismatch (banner=%s rebuilt=%s) — store corrupt or hand-edited", banner, t.Root)
	}
	return t, nil
}
