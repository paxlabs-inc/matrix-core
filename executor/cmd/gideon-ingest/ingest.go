// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// ingest.go — the idempotent cortex upsert engine shared by every corpus
// pass. It opens nothing itself; main wires in an open *cortex.Cortex.
//
// Idempotency model (why this is not a plain cortex.Write loop):
//
//   - cortex.Write always mints a fresh ULID, so a naive re-run would
//     duplicate every node. cortex.AddEdge is already idempotent (no-op on
//     an existing live edge), so only NODES need de-dup logic here.
//   - We give every node a STABLE ingest key (e.g. "gideon:module:x/evm")
//     carried as a `gideon:key:<key>` tag. buildIndex scans the store once
//     and maps key -> existing memory ID.
//   - upsertNode then: (a) Writes when the key is unseen, (b) compares the
//     freshly-encoded typed Data against the stored version and SKIPS when
//     byte-identical (no journal churn), (c) Updates (new version) only when
//     the derived content actually changed.
//
// Determinism: anything that feeds Version.Data must be re-derivable byte
// for byte across runs, so we never stamp wall-clock into Data
// (CapabilityData.LastObserved uses a fixed sentinel). Head timestamps and
// edge CreatedAt DO move, but they live outside the Data comparison.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"matrix/cortex"
	"matrix/cortex/memory"
)

const (
	// ingestTag marks every memory this tool emits so an operator can list
	// the whole graph with one tag filter.
	ingestTag = "gideon-ingest"
	// ingestKeyTagNS namespaces the per-node stable de-dup key.
	ingestKeyTagNS = "gideon:key:"
	createdBy      = "gideon-ingest"
)

// stableObservedAt is the fixed timestamp used for CapabilityData.LastObserved.
// Using wall-clock would make Version.Data differ on every run and defeat the
// skip-if-unchanged path.
var stableObservedAt = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// keywordRef ties a RUNBOOK failure-mode Pattern to the distinctive keywords
// in its title so incident rows and chat logs can be linked back to it.
type keywordRef struct {
	id       memory.ID
	keywords []string
}

// patternRef is a fix/diagnosis Pattern plus the lowercased text we scan for
// module-name mentions during cross-linking.
type patternRef struct {
	id   memory.ID
	text string
}

type ingester struct {
	cx     *cortex.Cortex
	actor  string
	dryRun bool

	// moduleFacts maps a source module's base name (e.g. "evm") -> its Fact
	// node ID, for depends-on resolution and pattern cross-linking.
	moduleFacts map[string]memory.ID

	failurePatterns []keywordRef
	fixPatterns     []patternRef

	wrote   int
	updated int
	skipped int
	edges   int
}

func newIngester(actor string, dryRun bool) *ingester {
	return &ingester{
		actor:       actor,
		dryRun:      dryRun,
		moduleFacts: map[string]memory.ID{},
	}
}

// keyID derives a deterministic 16-byte memory ID from a stable ingest key
// (sha256 prefix). This is the load-bearing idempotency primitive: the same
// key always maps to the same memory ID across runs, so re-ingest resolves the
// existing node by ID and Updates-or-skips rather than minting a duplicate. It
// avoids any reliance on a lookup tag (tags are capped at 64 bytes and would
// truncate long keys).
func keyID(key string) memory.ID {
	sum := sha256.Sum256([]byte("gideon-ingest\x00" + key))
	var id memory.ID
	copy(id[:], sum[:16])
	return id
}

// upsertNode is the single write path. It is idempotent: Write on first sight,
// skip when the encoded Data is unchanged, Update when content drifted.
func (ig *ingester) upsertNode(key string, data memory.TypedData, importance uint8, confidence float32, extraTags ...string) (memory.ID, error) {
	t := memory.TypeOf(data)
	id := keyID(key)

	if ig.dryRun {
		ig.wrote++
		return id, nil
	}

	encoded, err := memory.EncodeData(data)
	if err != nil {
		return memory.ID{}, fmt.Errorf("encode %s: %w", key, err)
	}
	meta := cortex.WriteMeta{
		CreatedBy:  createdBy,
		Confidence: confidence,
		Provenance: memory.Provenance{Source: memory.SourceImported},
	}

	mem, err := ig.cx.ResolveLatest(id)
	switch {
	case err == nil:
		// Already present — skip when unchanged, Update when content drifted.
		if mem.Head.Tombstoned == nil && bytes.Equal(mem.Version.Data, encoded) {
			ig.skipped++
			return id, nil
		}
		uri := cortex.BuildURI(t, id, mem.Head.CurrentVersion)
		if _, err := ig.cx.Update(uri, data, meta); err != nil {
			return memory.ID{}, fmt.Errorf("update %s: %w", key, err)
		}
		ig.updated++
		return id, nil
	case errors.Is(err, memory.ErrNotFound):
		head := memory.Head{
			ID:                 id,
			ActorScope:         ig.actor,
			Visibility:         memory.VisActorPublic,
			DeclaredImportance: importance,
			Tags:               buildTags(key, extraTags),
		}
		if _, err := ig.cx.Write(head, data, meta); err != nil {
			return memory.ID{}, fmt.Errorf("write %s: %w", key, err)
		}
		ig.wrote++
		return id, nil
	default:
		return memory.ID{}, fmt.Errorf("resolve %s: %w", key, err)
	}
}

// buildTags assembles the tag slice, always leading with the ingest marker and
// the stable key, then de-dupes and clamps to cortex's per-memory limits.
func buildTags(key string, extra []string) []memory.Tag {
	raw := append([]string{ingestTag, ingestKeyTagNS + key}, extra...)
	seen := map[string]struct{}{}
	out := make([]memory.Tag, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > memory.MaxTagLen {
			s = s[:memory.MaxTagLen]
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, memory.Tag(s))
		if len(out) >= memory.MaxTagsPerMemory {
			break
		}
	}
	return out
}

// linkEdge ensures a typed edge exists. label rides in EdgeRecord.Data as an
// opaque disambiguator (e.g. "depends-on" vs "resolved-by") since the 14
// canonical edge types are reused across several ops-graph relations.
func (ig *ingester) linkEdge(src memory.ID, t memory.EdgeType, dst memory.ID, label string) error {
	if src.IsZero() || dst.IsZero() || src == dst {
		return nil
	}
	ig.edges++
	if ig.dryRun {
		return nil
	}
	meta := cortex.AddEdgeMeta{CreatedBy: createdBy}
	if label != "" {
		meta.Data = []byte(label)
	}
	if err := ig.cx.AddEdge(src, t, dst, meta); err != nil {
		return fmt.Errorf("edge %s %s->%s: %w", t, src, dst, err)
	}
	return nil
}

// crossLinkPatternsToModules wires every fix/diagnosis Pattern to the source
// module Facts it names, by case-insensitive whole-token match.
func (ig *ingester) crossLinkPatternsToModules() error {
	names := make([]string, 0, len(ig.moduleFacts))
	for n := range ig.moduleFacts {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic edge emission order
	for _, p := range ig.fixPatterns {
		toks := tokenSet(p.text)
		for _, n := range names {
			if len(n) < 3 {
				continue
			}
			if _, ok := toks[n]; ok {
				if err := ig.linkEdge(p.id, memory.EdgeReferences, ig.moduleFacts[n], "touches-module"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// matchFailures returns RUNBOOK failure-mode Pattern IDs whose distinctive
// keywords appear (as substrings) in the lowercased text.
func (ig *ingester) matchFailures(text string) []memory.ID {
	lt := strings.ToLower(text)
	var out []memory.ID
	for _, f := range ig.failurePatterns {
		for _, kw := range f.keywords {
			if strings.Contains(lt, kw) {
				out = append(out, f.id)
				break
			}
		}
	}
	return out
}

func (ig *ingester) report(w io.Writer, root, actor string, dryRun bool) {
	mode := "live"
	if dryRun {
		mode = "dry-run (no writes)"
	}
	fmt.Fprintf(w, "gideon-ingest — %s\n", mode)
	fmt.Fprintf(w, "  cortex-root : %s\n", root)
	fmt.Fprintf(w, "  cortex-actor: %s\n", actor)
	fmt.Fprintf(w, "  nodes written=%d updated=%d skipped(unchanged)=%d\n", ig.wrote, ig.updated, ig.skipped)
	fmt.Fprintf(w, "  edges ensured=%d\n", ig.edges)
	fmt.Fprintf(w, "  module facts=%d  failure patterns=%d  fix patterns=%d\n",
		len(ig.moduleFacts), len(ig.failurePatterns), len(ig.fixPatterns))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
