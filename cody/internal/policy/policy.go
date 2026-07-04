// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package policy injects Cody's standards and playbooks: rules/common/* plus
// the applicable rules/<language>/* for the detected stack become the working
// policy (and every task sheet's rules_refs constraints), and the skills/
// library is surfaced as an on-demand index + loader, mirroring how Neo
// surfaces skills (names in the prompt, bodies pulled on demand).
package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/cody/internal/workspace"
)

// langRulesDir maps a detected workspace language to its rules/<dir>.
var langRulesDir = map[string]string{
	"go":         "golang",
	"typescript": "typescript",
	"javascript": "typescript",
	"python":     "python",
	"rust":       "rust",
	"java":       "java",
	"kotlin":     "kotlin",
	"php":        "php",
	"csharp":     "csharp",
	"swift":      "swift",
	"dart":       "dart",
	"cpp":        "cpp",
	"c":          "cpp",
	"vue":        "web",
	"svelte":     "web",
	"css":        "web",
	"html":       "web",
}

// Rules is the resolved standards set for a workspace: the common rule files
// plus the language-specific ones for the detected stack.
type Rules struct {
	Root string
	// Common lists rules/common/* files (paths relative to Root).
	Common []string
	// Language maps language dir -> its rule files (paths relative to Root).
	Language map[string][]string
}

// LoadRules resolves the applicable rule files for a workspace model.
func LoadRules(rulesRoot string, m *workspace.Model) (*Rules, error) {
	abs, err := filepath.Abs(rulesRoot)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("rules root %q is not a directory", rulesRoot)
	}
	r := &Rules{Root: abs, Language: map[string][]string{}}
	r.Common, err = listMarkdown(abs, "common")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	seen := map[string]bool{}
	for _, lang := range m.PrimaryLanguages() {
		dir, ok := langRulesDir[lang]
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		files, err := listMarkdown(abs, dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if len(files) > 0 {
			r.Language[dir] = files
		}
	}
	return r, nil
}

func listMarkdown(root, sub string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, sub))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Join(sub, e.Name())))
	}
	sort.Strings(out)
	return out, nil
}

// Refs returns every applicable rule file (relative to the rules root) —
// the value task sheets carry as constraints.rules_refs.
func (r *Rules) Refs() []string {
	out := append([]string{}, r.Common...)
	dirs := make([]string, 0, len(r.Language))
	for d := range r.Language {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		out = append(out, r.Language[d]...)
	}
	return out
}

// AbsRefs returns the refs as absolute paths (for workers reading them).
func (r *Rules) AbsRefs() []string {
	refs := r.Refs()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, filepath.Join(r.Root, filepath.FromSlash(ref)))
	}
	return out
}

// Render produces the working-policy section for prompts: the applicable
// standards by stack, referenced (not inlined) so the window stays lean.
func (r *Rules) Render() string {
	var b strings.Builder
	b.WriteString("Engineering standards in force (read a file for the details before working in its area):\n")
	for _, f := range r.Common {
		b.WriteString("- " + f + "\n")
	}
	dirs := make([]string, 0, len(r.Language))
	for d := range r.Language {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		fmt.Fprintf(&b, "- %s (detected stack):\n", d)
		for _, f := range r.Language[d] {
			b.WriteString("  - " + f + "\n")
		}
	}
	return b.String()
}

// Skills surfaces the skills/ library: an index of names + one-line
// descriptions for prompts (metadata resident) and an on-demand body loader
// (bodies pulled only when a task matches — never resident).
type Skills struct {
	Root  string
	Names []string
	// Descriptions maps skill name -> its one-line description (from the
	// SKILL.md frontmatter), capped for prompt economy. Missing when the
	// playbook carries none.
	Descriptions map[string]string
}

// skillDescCap bounds one description line in the prompt index.
const skillDescCap = 100

// LoadSkills indexes the skills library (one directory per skill carrying a
// SKILL.md playbook).
func LoadSkills(skillsRoot string) (*Skills, error) {
	abs, err := filepath.Abs(skillsRoot)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	s := &Skills{Root: abs, Descriptions: map[string]string{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(abs, e.Name(), "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		s.Names = append(s.Names, e.Name())
		if desc := skillDescription(path); desc != "" {
			s.Descriptions[e.Name()] = desc
		}
	}
	sort.Strings(s.Names)
	return s, nil
}

// skillDescription extracts the one-line description from a SKILL.md
// frontmatter block ("description: ..."), capped for the prompt index.
func skillDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	lines := strings.Split(string(head[:n]), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if rest, ok := strings.CutPrefix(trimmed, "description:"); ok {
			desc := strings.Trim(strings.TrimSpace(rest), `"'`)
			if len(desc) > skillDescCap {
				desc = strings.TrimSpace(desc[:skillDescCap]) + "..."
			}
			return desc
		}
	}
	return ""
}

// Load returns the full SKILL.md playbook body for a named skill.
func (s *Skills) Load(name string) (string, error) {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", fmt.Errorf("invalid skill name %q", name)
		}
	}
	data, err := os.ReadFile(filepath.Join(s.Root, name, "SKILL.md"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Has reports whether a skill exists in the index.
func (s *Skills) Has(name string) bool {
	i := sort.SearchStrings(s.Names, name)
	return i < len(s.Names) && s.Names[i] == name
}

// RenderIndex produces the metadata prompt section: one line per skill (name +
// one-line description when the playbook carries one). Metadata is cheap and
// resident; bodies are pulled on demand.
func (s *Skills) RenderIndex() string {
	if len(s.Names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Skill playbooks available on demand (load one when the task matches):\n")
	for _, name := range s.Names {
		if desc := s.Descriptions[name]; desc != "" {
			b.WriteString("- " + name + " — " + desc + "\n")
		} else {
			b.WriteString("- " + name + "\n")
		}
	}
	return b.String()
}
