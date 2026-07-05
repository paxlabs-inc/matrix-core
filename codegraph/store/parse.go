package store

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"matrix/codegraph/model"
)

// Parse reads a graph-store file and reconstructs the Index plus the Merkle root
// carried by the banner. Forward and reverse edge lines are both replayed into
// the index (reverse lines let a per-package shard record edges whose source
// lives in another shard); duplicates collapse.
func Parse(r io.Reader) (*model.Index, string, error) {
	ix := model.NewIndex()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	if !sc.Scan() {
		return nil, "", fmt.Errorf("store: empty input")
	}
	merkle, ok := strings.CutPrefix(sc.Text(), BannerPrefix)
	if !ok {
		return nil, "", fmt.Errorf("store: missing GENERATED banner")
	}

	var cur *model.Node
	inEdges := false
	flush := func() {
		if cur != nil {
			ix.AddNode(cur)
		}
	}
	line := 1
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" {
			continue
		}
		switch {
		case strings.HasPrefix(text, "NODE "):
			flush()
			n, err := parseNodeHeader(text)
			if err != nil {
				return nil, "", fmt.Errorf("store: line %d: %w", line, err)
			}
			cur = n
			inEdges = false
		case text == "EDGES":
			inEdges = true
		case strings.HasPrefix(text, "  "):
			if cur == nil {
				return nil, "", fmt.Errorf("store: line %d: attribute before NODE", line)
			}
			body := text[2:]
			if inEdges {
				if err := parseEdge(ix, cur.Id, body); err != nil {
					return nil, "", fmt.Errorf("store: line %d: %w", line, err)
				}
			} else if err := applyAttr(cur, body); err != nil {
				return nil, "", fmt.Errorf("store: line %d: %w", line, err)
			}
		default:
			return nil, "", fmt.Errorf("store: line %d: unexpected %q", line, text)
		}
	}
	flush()
	return ix, merkle, sc.Err()
}

func parseNodeHeader(text string) (*model.Node, error) {
	n := &model.Node{}
	for _, tok := range strings.Fields(strings.TrimPrefix(text, "NODE ")) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return nil, fmt.Errorf("bad NODE token %q", tok)
		}
		switch k {
		case "id":
			n.Id = v
		case "kind":
			n.Kind = model.Kind(v)
		default:
			return nil, fmt.Errorf("unknown NODE field %q", k)
		}
	}
	if n.Id == "" || n.Kind == "" {
		return nil, fmt.Errorf("NODE missing id or kind")
	}
	return n, nil
}

func applyAttr(n *model.Node, body string) error {
	// The meta line packs several space-free scalar fields; every other field is
	// a single key=value where the value is the rest of the line.
	if strings.HasPrefix(body, "digest=") {
		for _, tok := range strings.Fields(body) {
			k, v, _ := strings.Cut(tok, "=")
			if err := setScalar(n, k, v); err != nil {
				return err
			}
		}
		return nil
	}
	k, v, ok := strings.Cut(body, "=")
	if !ok {
		return fmt.Errorf("bad attribute %q", body)
	}
	switch k {
	case "sig":
		n.Sig = unescape(v)
	case "doc":
		n.Doc = unescape(v)
	case "summary":
		n.Enrich.Summary = unescape(v)
	case "summary_digest":
		n.Enrich.SummaryDigest = v
	case "salience":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("bad salience %q", v)
		}
		n.Enrich.Salience = f
	case "embed_ref":
		n.Enrich.EmbedRef = v
	default:
		return fmt.Errorf("unknown attribute %q", k)
	}
	return nil
}

func setScalar(n *model.Node, k, v string) error {
	switch k {
	case "digest":
		n.Digest = v
	case "file":
		n.File = v
	case "range":
		r, err := parseRange(v)
		if err != nil {
			return err
		}
		n.Range = r
	case "lang":
		n.Lang = v
	case "exported":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("bad exported %q", v)
		}
		n.Exported = b
	case "byte_range":
		a, b, ok := strings.Cut(v, ":")
		if !ok {
			return fmt.Errorf("bad byte_range %q", v)
		}
		sb, err1 := strconv.Atoi(a)
		eb, err2 := strconv.Atoi(b)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad byte_range %q", v)
		}
		n.ByteRange = model.ByteRange{StartByte: sb, EndByte: eb}
	default:
		return fmt.Errorf("unknown scalar %q", k)
	}
	return nil
}

func parseRange(v string) (model.Range, error) {
	a, b, ok := strings.Cut(v, ":")
	if !ok {
		return model.Range{}, fmt.Errorf("bad range %q", v)
	}
	sl, err1 := strconv.Atoi(a)
	el, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return model.Range{}, fmt.Errorf("bad range %q", v)
	}
	return model.Range{StartLine: sl, EndLine: el}, nil
}

func parseEdge(ix *model.Index, nodeID, body string) error {
	reverse := strings.HasPrefix(body, "^")
	body = strings.TrimPrefix(body, "^")
	typ, list, ok := strings.Cut(body, "=")
	if !ok {
		return fmt.Errorf("bad edge %q", body)
	}
	et := model.EdgeType(typ)
	for _, other := range strings.Split(list, ",") {
		if other == "" {
			continue
		}
		if reverse {
			ix.AddEdge(model.Edge{Src: other, Dst: nodeID, Type: et})
		} else {
			ix.AddEdge(model.Edge{Src: nodeID, Dst: other, Type: et})
		}
	}
	return nil
}
