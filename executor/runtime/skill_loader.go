// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Skill loader — materializes a matrix://skill/<slug>@<version> URI into
// a parsed + validated SKILL.mtx AST plus its companion SKILL.md prose.
//
// Citations:
//
//   S23Q5 URI scheme              — matrix.kvx URI_SCHEME line 103
//                                   ("matrix://skill/{name}")
//   S23Q5 version pin             — research/05-skills-and-tools.md S4
//                                   (mirrored at matrix.kvx invariants
//                                   line 829: "Unquoted KV values ...")
//                                   and Q22 line 718 (URIs MUST be pinned)
//   S23Q5 SKILL.mtx vs SKILL.md   — matrix.kvx sess#22c invariant line 828
//                                   ("SKILL.md + SKILL.mtx coexist in every
//                                   skill dir; SKILL.md = prose body
//                                   (executor-LLM consumer), SKILL.mtx =
//                                   compiler manifest (MCL parser consumer)")
//   S23Q5 use existing parser/    — MCL/mtx/parser.New / .Parse
//         validator pipeline        and validator.ValidateSkill
//
// The loader is the single point of contact between the executor and the
// 159-skill corpus normalised in sess#22c. It does NOT cache; callers
// that need caching wrap a Loader behind their own LRU.

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrix/mcl/mtx/ast"
	"matrix/mcl/mtx/canonical"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/validator"
)

// DefaultSkillRepoRoot is the canonical filesystem home of the skill
// corpus (matrix.kvx FOLDERS line 60 — skills/). Tests override via
// SkillLoader.RepoRoot.
const DefaultSkillRepoRoot = "/root/matrix/skills"

// LoadedSkill is the resolved skill returned by SkillLoader.Load.
type LoadedSkill struct {
	// URI is the canonical matrix://skill/<slug>@<version> form. Matches
	// the input URI byte-for-byte modulo normalization (e.g. lowercasing).
	URI string

	// Slug is the path component (skills/<slug>/).
	Slug string

	// Version is the §SKILL.version value from the SKILL.mtx file. Verified
	// against the URI's @version pin at load time.
	Version string

	// ID is the §SKILL.id value. Verified against URI's <slug> at load.
	ID string

	// MtxPath is the absolute filesystem path of the SKILL.mtx file.
	MtxPath string

	// MdPath is the absolute filesystem path of the SKILL.md body. Empty
	// if no SKILL.md is present (skills published only via the compiler
	// manifest — research/05 §6 leaves prose body optional).
	MdPath string

	// File is the parsed + validated SKILL.mtx AST.
	File *ast.File

	// MtxBytes is the raw bytes of SKILL.mtx (canonical hashing input).
	MtxBytes []byte

	// MdBytes is the raw bytes of SKILL.md (executor-LLM prompt input).
	// nil when MdPath is empty.
	MdBytes []byte

	// CanonicalHash is the sha256 AST hash (canonical.Hash). Equals what
	// mclc hash would emit for this file. Used by D11 mtx_digest seed.
	CanonicalHash string

	// MclVerbs is the list of D7 verbs the skill declares it handles
	// (from §SKILL.mcl.verbs). Empty when not declared.
	MclVerbs []string

	// Description is §SKILL.description (used in skill-selection UIs).
	Description string

	// Display is §SKILL.display (human-friendly name).
	Display string

	// Author is §SKILL.author (DID).
	Author string

	// DeclaredTools is the §TOOLS allow-list as bare matrix://tool URIs
	// (each version-pinned per validator rule 10). The synthesizer uses
	// this to filter the agent's manifest tools so the executor LLM
	// only sees what the skill author sanctioned. Nil when §TOOLS is
	// `none` or the section is absent. An empty (non-nil) slice is
	// reserved for future "explicitly empty" semantics; v1 treats nil
	// and len==0 identically (no tool_call nodes allowed).
	DeclaredTools []string

	// DeclaredSubSkills is the §SUB_SKILLS allow-list as version-pinned
	// matrix://skill URIs. Same semantics as DeclaredTools: nil when
	// `none`/absent. Synthesizer rejects sub_dispatch nodes whose
	// skill_ref is not in this list.
	DeclaredSubSkills []string

	// ToolsNone is true when §TOOLS = `none`. Distinguishes
	// "explicitly forbidden" from "section parse error" so the
	// synthesizer can emit a stronger negative ("DO NOT EMIT TOOL
	// CALLS") instead of a soft empty list.
	ToolsNone bool

	// SubSkillsNone is true when §SUB_SKILLS = `none`. Mirrors
	// ToolsNone for the sub_dispatch path.
	SubSkillsNone bool
}

// SkillLoader resolves matrix://skill/... URIs against a filesystem repo.
//
// Construction is cheap; Load is pure (no caching). Embed inside a
// caching layer if your workload demands it.
type SkillLoader struct {
	// RepoRoot is the absolute filesystem root containing per-skill
	// directories. Empty → DefaultSkillRepoRoot.
	RepoRoot string
}

// NewSkillLoader constructs a loader. repoRoot empty → default.
func NewSkillLoader(repoRoot string) *SkillLoader {
	if repoRoot == "" {
		repoRoot = DefaultSkillRepoRoot
	}
	return &SkillLoader{RepoRoot: repoRoot}
}

// Load resolves a matrix://skill/<slug>@<version> URI to a LoadedSkill.
//
// Steps:
//
//  1. Parse the URI (S23Q5 + Q22: version pin REQUIRED).
//  2. Read <RepoRoot>/<slug>/SKILL.mtx + (optional) SKILL.md.
//  3. Lex + parse the SKILL.mtx (MCL/mtx/parser.Parse).
//  4. Run ValidateSkill against all 10 rules (MCL/mtx/validator.ValidateSkill).
//  5. Extract §SKILL.id and §SKILL.version; verify they match the URI.
//  6. Compute the canonical AST hash (canonical.Hash).
//
// Each step that fails returns a wrapped error naming the URI + step.
func (l *SkillLoader) Load(uri string) (*LoadedSkill, error) {
	su, err := ParseSkillURI(uri)
	if err != nil {
		return nil, err
	}

	skillDir := filepath.Join(l.RepoRoot, su.Slug)
	mtxPath := filepath.Join(skillDir, "SKILL.mtx")
	mtxBytes, err := os.ReadFile(mtxPath)
	if err != nil {
		return nil, fmt.Errorf("skill_loader: read %s: %w", mtxPath, err)
	}

	// SKILL.md is optional per S23Q5; missing file is not an error.
	mdPath := filepath.Join(skillDir, "SKILL.md")
	mdBytes, err := os.ReadFile(mdPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("skill_loader: read %s: %w", mdPath, err)
		}
		mdBytes = nil
		mdPath = ""
	}

	p := parser.New(mtxBytes)
	file, perrs := p.Parse()
	if len(perrs) > 0 {
		// Surface first error verbatim; loader is not a debugger.
		return nil, fmt.Errorf("skill_loader: parse %s: %s", mtxPath, perrs[0])
	}

	verrs := validator.ValidateSkill(file)
	if len(verrs) > 0 {
		return nil, fmt.Errorf("skill_loader: validate %s: %s", mtxPath, verrs[0])
	}

	meta, err := extractSkillMeta(file)
	if err != nil {
		return nil, fmt.Errorf("skill_loader: %s: %w", mtxPath, err)
	}

	if meta.ID != su.Slug {
		return nil, fmt.Errorf("skill_loader: %s: §SKILL.id=%q does not match URI slug=%q",
			mtxPath, meta.ID, su.Slug)
	}
	if meta.Version != su.Version {
		return nil, fmt.Errorf("skill_loader: %s: §SKILL.version=%q does not match URI version=%q (S4 hard pin)",
			mtxPath, meta.Version, su.Version)
	}

	tools, toolsNone := extractRefList(file, "TOOLS")
	subs, subsNone := extractRefList(file, "SUB_SKILLS")

	return &LoadedSkill{
		URI:               uri,
		Slug:              su.Slug,
		Version:           meta.Version,
		ID:                meta.ID,
		MtxPath:           mtxPath,
		MdPath:            mdPath,
		File:              file,
		MtxBytes:          mtxBytes,
		MdBytes:           mdBytes,
		CanonicalHash:     canonical.Hash(file),
		MclVerbs:          meta.MclVerbs,
		Description:       meta.Description,
		Display:           meta.Display,
		Author:            meta.Author,
		DeclaredTools:     tools,
		DeclaredSubSkills: subs,
		ToolsNone:         toolsNone,
		SubSkillsNone:     subsNone,
	}, nil
}

// extractRefList walks a §TOOLS or §SUB_SKILLS section and returns
// the URI list along with a `none` flag. Both `none` and an absent
// section yield (nil, true|false): `none` is explicit forbidden,
// absent is the same v1 semantics (validator rule 1 normally requires
// the section, so absent only happens in non-skill files).
func extractRefList(file *ast.File, section string) ([]string, bool) {
	var sec *ast.Section
	for _, s := range file.Sections {
		if s.Name == section {
			sec = s
			break
		}
	}
	if sec == nil {
		return nil, false
	}
	var uris []string
	none := false
	for _, entry := range sec.Entries {
		switch e := entry.(type) {
		case *ast.NoneEntry:
			none = true
		case *ast.RefEntry:
			if e.URI != "" {
				uris = append(uris, e.URI)
			}
		}
	}
	if none {
		return nil, true
	}
	return uris, false
}

// SkillURI carries the parsed components of a matrix://skill/... URI.
type SkillURI struct {
	Slug    string
	Version string
}

// ParseSkillURI validates a matrix://skill/<slug>@<version> URI.
//
// Rejects:
//   - missing scheme
//   - any non-skill path
//   - missing @version (S4 hard rule)
//   - empty slug or version
//
// Spec: matrix.kvx URI_SCHEME (line 103) declares matrix://skill/{name};
// the @version suffix is the S4 pin and is not strictly part of URI_SCHEME
// but is mandatory at every consumer per research/05 S4 + Q22.
func ParseSkillURI(uri string) (*SkillURI, error) {
	const prefix = "matrix://skill/"
	if !strings.HasPrefix(uri, prefix) {
		return nil, fmt.Errorf("%w: %q (must start with %s)", ErrInvalidSkillURI, uri, prefix)
	}
	rest := uri[len(prefix):]
	atIdx := strings.LastIndex(rest, "@")
	if atIdx <= 0 || atIdx == len(rest)-1 {
		return nil, fmt.Errorf("%w: %q (missing @version pin)", ErrUnpinnedSkillURI, uri)
	}
	slug := rest[:atIdx]
	version := rest[atIdx+1:]
	if slug == "" {
		return nil, fmt.Errorf("%w: %q (empty slug)", ErrInvalidSkillURI, uri)
	}
	if version == "" {
		return nil, fmt.Errorf("%w: %q (empty version)", ErrUnpinnedSkillURI, uri)
	}
	if strings.ContainsAny(slug, "/?#") {
		return nil, fmt.Errorf("%w: %q (slug contains reserved char)", ErrInvalidSkillURI, uri)
	}
	return &SkillURI{Slug: slug, Version: version}, nil
}

// Sentinel errors emitted by the loader.
var (
	// ErrInvalidSkillURI fires when the URI doesn't conform to the
	// matrix://skill/<slug>@<version> grammar.
	ErrInvalidSkillURI = errors.New("skill_loader: invalid skill URI")

	// ErrUnpinnedSkillURI fires when the URI omits @version. S4 hard rule.
	ErrUnpinnedSkillURI = errors.New("skill_loader: skill URI missing @version pin")
)

// ---- internal: extract §SKILL section fields from the AST ----

type skillMeta struct {
	ID          string
	Version     string
	Display     string
	Description string
	Author      string
	MclVerbs    []string
}

func extractSkillMeta(file *ast.File) (*skillMeta, error) {
	var skill *ast.Section
	for _, sec := range file.Sections {
		if sec.Name == "SKILL" {
			skill = sec
			break
		}
	}
	if skill == nil {
		return nil, errors.New("§SKILL section missing")
	}

	m := &skillMeta{}
	for _, entry := range skill.Entries {
		kv, ok := entry.(*ast.KVPair)
		if !ok {
			continue
		}
		key := strings.Join(kv.Key, ".")
		switch key {
		case "id":
			m.ID = kvString(kv.Value)
		case "version":
			m.Version = kvString(kv.Value)
		case "display":
			m.Display = kvString(kv.Value)
		case "description":
			m.Description = kvString(kv.Value)
		case "author":
			m.Author = kvString(kv.Value)
		case "mcl.verbs":
			// mcl.verbs is unquoted space-separated identifiers; the
			// parser emits each as a separate IdentValue inside a
			// ListValue, OR a single IdentValue when there's one verb.
			m.MclVerbs = kvIdentList(kv.Value)
		}
	}
	if m.ID == "" {
		return nil, errors.New("§SKILL.id missing")
	}
	if m.Version == "" {
		return nil, errors.New("§SKILL.version missing")
	}
	return m, nil
}

// kvString returns the string content of a KV value (StringValue or
// IdentValue). For SpaceListValue (the parser shape produced by an
// unquoted multi-word value like `display=Writing Plans`) it joins the
// items with single spaces. Returns "" for any other shape.
func kvString(v ast.Value) string {
	switch val := v.(type) {
	case *ast.StringValue:
		return val.Text
	case *ast.IdentValue:
		return val.Name
	case *ast.SpaceListValue:
		return strings.Join(val.Items, " ")
	}
	return ""
}

// kvIdentList returns the list of bare identifiers from a value
// position. Handles both single-ident and space-separated-list shapes
// emitted by the MCL parser (parser.go parseValueList → SpaceListValue).
func kvIdentList(v ast.Value) []string {
	switch val := v.(type) {
	case *ast.IdentValue:
		return splitOnSpace(val.Name)
	case *ast.SpaceListValue:
		out := make([]string, 0, len(val.Items))
		for _, item := range val.Items {
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	return nil
}

func splitOnSpace(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
