package config

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// kvxDoc is a parsed chronos.config.kvx file: a sectioned key/value document.
//
// Format (zero-dep, deterministic), shared with tachyon/uwac's config loaders:
//
//	# comment
//	[section]
//	key = "string"            # double-quoted strings (per Matrix .mtx convention)
//	num = 9096                # bare ints / bools
//	list = ["a", "b"]         # bracketed, comma-separated, quoted items
//	[section.sub]
//	ref = "${ENV_VAR}"        # ${ENV} interpolated from the process environment
//
// Values are stored as raw tokens; typed accessors parse on demand. Later
// duplicate keys win.
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

// uint64Or parses section.key as uint64, returning fallback when absent/invalid.
func (d *kvxDoc) uint64Or(section, key string, fallback uint64) uint64 {
	v := d.str(section, key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
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
