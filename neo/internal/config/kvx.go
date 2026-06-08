// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package config

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// kvxDoc is a parsed sectioned key/value document in the Matrix .kvx
// convention (mirrors tachyon/internal/config/kvx.go — copied rather than
// imported because that parser lives behind an internal/ boundary in a
// different module).
//
// Format (zero-dep, deterministic):
//
//	# comment
//	[section]
//	key = "string"            # double-quoted strings (per Matrix .mtx convention)
//	num = 50                  # bare ints / bools
//	list = ["shell", "git"]   # bracketed, comma-separated, quoted items
//	[section.sub]
//	ref = "${ENV_VAR}"        # ${ENV} interpolated from the process environment
//
// Values are stored as raw tokens; typed accessors parse on demand. Section
// names are case-sensitive; keys are trimmed. Later duplicate keys win.
type kvxDoc struct {
	sections map[string]map[string]string
	order    []string
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// parseKVXFile reads and parses a .kvx file. A missing file is not an error
// (returns an empty doc, ok=false).
func parseKVXFile(path string) (*kvxDoc, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newKVXDoc(), false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	doc, err := parseKVX(f)
	return doc, err == nil, err
}

func newKVXDoc() *kvxDoc {
	return &kvxDoc{sections: map[string]map[string]string{}}
}

func parseKVX(r interface{ Read([]byte) (int, error) }) (*kvxDoc, error) {
	doc := newKVXDoc()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	section := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := stripComment(strings.TrimSpace(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("kvx line %d: unterminated section header %q", lineNo, line)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := doc.sections[section]; !ok {
				doc.sections[section] = map[string]string{}
				doc.order = append(doc.order, section)
			}
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("kvx line %d: expected key = value, got %q", lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("kvx line %d: empty key", lineNo)
		}
		if doc.sections[section] == nil {
			doc.sections[section] = map[string]string{}
			doc.order = append(doc.order, section)
		}
		doc.sections[section][key] = strings.TrimSpace(val)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return doc, nil
}

// stripComment removes a trailing # comment that is not inside a quoted string.
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return strings.TrimSpace(line[:i])
			}
		}
	}
	return line
}

// has reports whether a section is present.
func (d *kvxDoc) has(section string) bool {
	_, ok := d.sections[section]
	return ok
}

// str returns the interpolated string value of section.key, or "".
func (d *kvxDoc) str(section, key string) string {
	sec, ok := d.sections[section]
	if !ok {
		return ""
	}
	raw, ok := sec[key]
	if !ok {
		return ""
	}
	return interpolate(unquote(raw))
}

// strOr returns the value of section.key, or fallback when absent/empty.
func (d *kvxDoc) strOr(section, key, fallback string) string {
	if v := d.str(section, key); v != "" {
		return v
	}
	return fallback
}

// list returns a bracketed list value as interpolated strings.
func (d *kvxDoc) list(section, key string) []string {
	sec, ok := d.sections[section]
	if !ok {
		return nil
	}
	raw, ok := sec[key]
	if !ok {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		if v := interpolate(unquote(raw)); v != "" {
			return []string{v}
		}
		return nil
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return nil
	}
	var out []string
	for _, part := range splitList(inner) {
		if v := interpolate(unquote(strings.TrimSpace(part))); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// intOr parses section.key as int, returning fallback when absent/invalid.
func (d *kvxDoc) intOr(section, key string, fallback int) int {
	v := d.str(section, key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func splitList(s string) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// interpolate replaces ${ENV_VAR} with the process environment value.
func interpolate(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		return os.Getenv(name)
	})
}
