// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package edit is Cody's anchored edit engine. Every mutation is gated on a
// prior read: an edit or overwrite fails with ErrStale if the target file
// changed since it was read (content-hash comparison), and anchored
// find/replace fails on a missing or ambiguous anchor. The engine never blind
// overwrites — the only unconditional write is the creation of a file that
// does not exist yet.
package edit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// ErrStale means the file on disk no longer matches the content the
	// engine read: someone (or something) changed it, so the edit is refused
	// and the caller must re-read.
	ErrStale = errors.New("file changed since it was read")
	// ErrNotRead means a mutation was attempted on a file the engine has not
	// read this session — reads gate writes, always.
	ErrNotRead = errors.New("file was not read before writing")
	// ErrAnchorNotFound means the anchored old text does not occur in the file.
	ErrAnchorNotFound = errors.New("anchor text not found in file")
	// ErrAnchorAmbiguous means the anchored old text occurs more than once and
	// the edit did not request replace-all.
	ErrAnchorAmbiguous = errors.New("anchor text occurs more than once; widen the anchor or use replace-all")
	// ErrExists means Create was called for a file that already exists.
	ErrExists = errors.New("file already exists; read it and edit instead")
	// ErrOutsideRoot means the path escapes the engine's workspace root.
	ErrOutsideRoot = errors.New("path escapes the workspace root")
)

// Engine tracks read state per file and applies gated mutations under a
// workspace root. It is safe for concurrent use.
type Engine struct {
	root string

	mu    sync.Mutex
	reads map[string]string // abs path -> sha256 of content at read time
}

// New creates an edit engine rooted at the workspace directory.
func New(root string) (*Engine, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("edit engine root %q is not a directory", root)
	}
	return &Engine{root: abs, reads: map[string]string{}}, nil
}

// Root reports the engine's workspace root.
func (e *Engine) Root() string { return e.root }

// resolveIn turns a workspace-relative (or absolute) path into a verified
// absolute path inside root. root must already be absolute and clean. This is
// the single resolution core: both the Engine (via its own root) and callers
// that only hold a root string (e.g. the acceptance gate) share it, so path
// normalization can never drift between the tool boundary and adjudication.
func resolveIn(root, path string) (string, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, path)
	}
	abs = filepath.Clean(abs)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	return abs, nil
}

// Rel is the single workspace-path normalization seam, usable without an
// Engine: it resolves a model-supplied path against root, rejects escapes, and
// returns a clean, slash-separated workspace-relative path. An absolute in-root
// path and its relative form normalize to the identical result, so callers
// never carry an absolute path forward and never double-join.
func Rel(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	abs, err := resolveIn(absRoot, path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// resolve turns a workspace-relative (or absolute) path into a verified
// absolute path inside the engine's root.
func (e *Engine) resolve(path string) (string, error) {
	return resolveIn(e.root, path)
}

// Rel normalizes a model-supplied path against the engine's root. See the
// package-level Rel for the contract.
func (e *Engine) Rel(path string) (string, error) {
	return Rel(e.root, path)
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Read returns the file content and records its hash as the freshness anchor
// for subsequent edits.
func (e *Engine) Read(path string) (string, error) {
	abs, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.reads[abs] = hashOf(data)
	e.mu.Unlock()
	return string(data), nil
}

// checkFresh verifies the file still matches the recorded read. Returns the
// current content when fresh.
func (e *Engine) checkFresh(abs string) ([]byte, error) {
	e.mu.Lock()
	readHash, ok := e.reads[abs]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotRead, abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if hashOf(data) != readHash {
		return nil, fmt.Errorf("%w: %s", ErrStale, abs)
	}
	return data, nil
}

// commit writes content atomically and refreshes the read anchor so chained
// edits by the same holder keep working.
func (e *Engine) commit(abs string, content []byte) error {
	tmp := abs + ".cody-edit.tmp"
	if err := os.WriteFile(tmp, content, filePerm(abs)); err != nil {
		return err
	}
	if err := os.Rename(tmp, abs); err != nil {
		return err
	}
	e.mu.Lock()
	e.reads[abs] = hashOf(content)
	e.mu.Unlock()
	return nil
}

func filePerm(abs string) os.FileMode {
	if fi, err := os.Stat(abs); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}

// Apply performs an anchored find/replace: oldText must occur exactly once
// (or replaceAll must be set), and the file must be unchanged since Read.
func (e *Engine) Apply(path, oldText, newText string, replaceAll bool) error {
	if oldText == "" {
		return errors.New("empty anchor text")
	}
	if oldText == newText {
		return errors.New("anchor and replacement are identical")
	}
	abs, err := e.resolve(path)
	if err != nil {
		return err
	}
	data, err := e.checkFresh(abs)
	if err != nil {
		return err
	}
	content := string(data)
	switch n := strings.Count(content, oldText); {
	case n == 0:
		return fmt.Errorf("%w: %s", ErrAnchorNotFound, abs)
	case n > 1 && !replaceAll:
		return fmt.Errorf("%w (%d occurrences): %s", ErrAnchorAmbiguous, n, abs)
	}
	var next string
	if replaceAll {
		next = strings.ReplaceAll(content, oldText, newText)
	} else {
		next = strings.Replace(content, oldText, newText, 1)
	}
	return e.commit(abs, []byte(next))
}

// Overwrite replaces the full content of an EXISTING file that was read and
// is still fresh. It refuses files never read and files that drifted.
func (e *Engine) Overwrite(path, content string) error {
	abs, err := e.resolve(path)
	if err != nil {
		return err
	}
	if _, err := e.checkFresh(abs); err != nil {
		return err
	}
	return e.commit(abs, []byte(content))
}

// Create writes a NEW file. It fails if the file already exists — existing
// content is only reachable through Read + Apply/Overwrite.
func (e *Engine) Create(path, content string) error {
	abs, err := e.resolve(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return e.commit(abs, []byte(content))
}

// Delete removes a file, gated the same way as an edit: it must have been
// read and still be fresh, so nothing is deleted sight-unseen.
func (e *Engine) Delete(path string) error {
	abs, err := e.resolve(path)
	if err != nil {
		return err
	}
	if _, err := e.checkFresh(abs); err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.reads, abs)
	e.mu.Unlock()
	return nil
}
