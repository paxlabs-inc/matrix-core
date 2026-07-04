// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Search tool bounds: results are capped so one call never floods the window,
// and matched lines are truncated so a minified bundle cannot blow the cap by
// itself.
const (
	grepMaxMatches  = 100
	grepMaxLineLen  = 500
	globMaxResults  = 100
	searchMaxSize   = 2 * 1024 * 1024 // per-file read ceiling for grep
)

// searchIgnoredDirs are never walked by grep/glob — the same exclusions the
// workspace indexer applies, plus Cody's own state dir.
var searchIgnoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	".cody": true,
}

// toolGrep is the worker's bounded content search: a regexp over the workspace
// tree without burning exec steps or spilling overflow files.
func (w *Worker) toolGrep(args map[string]interface{}) string {
	pattern := str(args, "pattern")
	if pattern == "" {
		return "error: pattern is required"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "error: invalid pattern: " + err.Error()
	}
	sub := str(args, "path")
	include := str(args, "include")
	var includeRe *regexp.Regexp
	if include != "" {
		includeRe, err = globRegexp(include)
		if err != nil {
			return "error: invalid include glob: " + err.Error()
		}
	}

	start := w.opts.Root
	if sub != "" {
		start = filepath.Join(w.opts.Root, filepath.FromSlash(sub))
	}
	var b strings.Builder
	matches, truncated := 0, false
	_ = filepath.WalkDir(start, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if path != start && searchIgnoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if matches >= grepMaxMatches {
			truncated = true
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(w.opts.Root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if includeRe != nil && !includeRe.MatchString(rel) && !includeRe.MatchString(filepath.Base(rel)) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > searchMaxSize {
			return nil
		}
		data, rerr2 := os.ReadFile(path)
		if rerr2 != nil || looksBinary(data) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if matches >= grepMaxMatches {
				truncated = true
				break
			}
			if !re.MatchString(line) {
				continue
			}
			if len(line) > grepMaxLineLen {
				line = line[:grepMaxLineLen] + "..."
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", rel, i+1, line)
			matches++
		}
		return nil
	})
	if matches == 0 {
		return "no matches"
	}
	if truncated {
		fmt.Fprintf(&b, "[grep: capped at %d matches — narrow the pattern or path for the rest]\n", grepMaxMatches)
	} else {
		fmt.Fprintf(&b, "[grep: %d match(es)]\n", matches)
	}
	return b.String()
}

// toolGlob finds files by path pattern (with ** support), newest first — the
// orientation primitive fs_list one-directory-at-a-time cannot provide.
func (w *Worker) toolGlob(args map[string]interface{}) string {
	pattern := str(args, "pattern")
	if pattern == "" {
		return "error: pattern is required"
	}
	re, err := globRegexp(pattern)
	if err != nil {
		return "error: invalid glob: " + err.Error()
	}
	type hit struct {
		rel string
		mod int64
	}
	var hits []hit
	_ = filepath.WalkDir(w.opts.Root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if path != w.opts.Root && searchIgnoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(w.opts.Root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !re.MatchString(rel) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		hits = append(hits, hit{rel: rel, mod: info.ModTime().UnixNano()})
		return nil
	})
	if len(hits) == 0 {
		return "no files match"
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mod != hits[j].mod {
			return hits[i].mod > hits[j].mod
		}
		return hits[i].rel < hits[j].rel
	})
	truncated := false
	if len(hits) > globMaxResults {
		hits = hits[:globMaxResults]
		truncated = true
	}
	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.rel + "\n")
	}
	if truncated {
		fmt.Fprintf(&b, "[glob: capped at %d files, newest first — narrow the pattern for the rest]\n", globMaxResults)
	} else {
		fmt.Fprintf(&b, "[glob: %d file(s), newest first]\n", len(hits))
	}
	return b.String()
}

// globRegexp compiles a glob pattern (supporting **) into an anchored regexp
// over slash-separated relative paths.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	p = strings.TrimPrefix(p, "./")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				// "**/" matches zero or more directories; a bare "**" matches
				// anything including separators.
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '(', ')', '+', '|', '^', '$', '[', ']', '{', '}', '\\':
			b.WriteString("\\" + string(c))
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// looksBinary reports whether a file's head contains a NUL byte — the cheap
// binary sniff grep uses to skip images/archives/compiled artifacts.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
