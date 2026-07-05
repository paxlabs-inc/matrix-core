// Package store serializes and parses the CodeGraph graph store: line-diffable
// .kvx sharded one file per package plus a root manifest. The format is a pure
// function of the graph — nodes sorted by id, edges grouped by type in a fixed
// order and sorted within a type — so "generate twice, assert byte-identical"
// holds and the store is git-resident and diff-reviewable.
package store

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"matrix/codegraph/model"
)

// BannerPrefix is the first-line marker; the repo Merkle root follows it so
// staleness is detectable.
const BannerPrefix = "# GENERATED gen=codegraph do-not-edit merkle="

// Write serializes every node owned by ix (those added via AddNode) and their
// forward + reverse edges to w, prefixed by the GENERATED banner carrying merkle.
func Write(w io.Writer, ix *model.Index, merkle string) error {
	return WriteNodes(w, ix, ix.Nodes(), merkle)
}

// WriteNodes serializes a chosen subset of nodes (already sorted by id) using
// ix as the global edge index, so a per-package shard still renders the reverse
// edges whose source lives in another shard. The subset must be sorted by id.
func WriteNodes(w io.Writer, ix *model.Index, nodes []*model.Node, merkle string) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "%s%s\n", BannerPrefix, merkle)
	for _, n := range nodes {
		bw.WriteByte('\n')
		writeNode(bw, ix, n)
	}
	return bw.Flush()
}

func writeNode(bw *bufio.Writer, ix *model.Index, n *model.Node) {
	fmt.Fprintf(bw, "NODE id=%s kind=%s\n", n.Id, n.Kind)

	fmt.Fprintf(bw, "  digest=%s file=%s range=%s lang=%s exported=%s",
		n.Digest, n.File, n.Range.String(), n.Lang, strconv.FormatBool(n.Exported))
	if !n.ByteRange.IsZero() {
		fmt.Fprintf(bw, " byte_range=%d:%d", n.ByteRange.StartByte, n.ByteRange.EndByte)
	}
	bw.WriteByte('\n')

	if n.Sig != "" {
		fmt.Fprintf(bw, "  sig=%s\n", escape(n.Sig))
	}
	if n.Doc != "" {
		fmt.Fprintf(bw, "  doc=%s\n", escape(n.Doc))
	}
	if n.Enrich.Summary != "" {
		fmt.Fprintf(bw, "  summary=%s\n", escape(n.Enrich.Summary))
	}
	if n.Enrich.SummaryDigest != "" {
		fmt.Fprintf(bw, "  summary_digest=%s\n", n.Enrich.SummaryDigest)
	}
	if n.Enrich.Salience != 0 {
		fmt.Fprintf(bw, "  salience=%s\n", formatSalience(n.Enrich.Salience))
	}
	if n.Enrich.EmbedRef != "" {
		fmt.Fprintf(bw, "  embed_ref=%s\n", n.Enrich.EmbedRef)
	}

	bw.WriteString("EDGES\n")
	for _, t := range model.EdgeTypes {
		if dsts := ix.Forward(n.Id, t); len(dsts) > 0 {
			fmt.Fprintf(bw, "  %s=%s\n", t, strings.Join(dsts, ","))
		}
	}
	for _, t := range model.EdgeTypes {
		if srcs := ix.Reverse(n.Id, t); len(srcs) > 0 {
			fmt.Fprintf(bw, "  ^%s=%s\n", t, strings.Join(srcs, ","))
		}
	}
}

// formatSalience renders with exactly 4 decimals, half-even rounding.
func formatSalience(f float64) string {
	return strconv.FormatFloat(math.RoundToEven(f*1e4)/1e4, 'f', 4, 64)
}

func escape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
