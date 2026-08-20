// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxPackageFiles = 256
	maxPackageBytes = 16 << 20
	maxRemoteBytes  = 20 << 20
)

var (
	safePart       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)\s*[:=]\s*["']?[A-Za-z0-9_./+=-]{12,}`),
		regexp.MustCompile(`\b(?:ghp|github_pat|sk)-[A-Za-z0-9_-]{16,}\b`),
	}
)

func (s *Store) ImportDirectory(ctx context.Context, request ImportRequest) (Capability, error) {
	request.SourceDir = strings.TrimSpace(request.SourceDir)
	request.SourceRef = strings.TrimSpace(request.SourceRef)
	if request.SourceDir == "" || !validSourceType(request.SourceType) {
		return Capability{}, fmt.Errorf("%w: invalid import request", ErrUnsafePackage)
	}
	if len(request.SourceRef) > 2048 {
		return Capability{}, fmt.Errorf("%w: source reference exceeds bounds", ErrUnsafePackage)
	}
	for _, pattern := range secretPatterns {
		if pattern.MatchString(request.SourceRef) {
			return Capability{}, fmt.Errorf("%w: possible secret in source reference", ErrUnsafePackage)
		}
	}
	stage, err := os.MkdirTemp(s.root, ".quarantine-")
	if err != nil {
		return Capability{}, fmt.Errorf("capability hub create quarantine: %w", err)
	}
	defer os.RemoveAll(stage)
	stagedPackage := filepath.Join(stage, "candidate")
	if err := copyPackage(request.SourceDir, stagedPackage); err != nil {
		return Capability{}, err
	}
	if err := scanPackage(stagedPackage); err != nil {
		return Capability{}, err
	}
	identity, err := readIdentity(filepath.Join(stagedPackage, "SKILL.mtx"))
	if err != nil {
		return Capability{}, err
	}
	if !safePart.MatchString(identity.slug) || !safePart.MatchString(identity.version) {
		return Capability{}, fmt.Errorf("%w: invalid skill id or version", ErrUnsafePackage)
	}
	validatedRoot := filepath.Join(stage, "validated")
	if err := os.MkdirAll(validatedRoot, 0o700); err != nil {
		return Capability{}, err
	}
	validatedPackage := filepath.Join(validatedRoot, identity.slug)
	if err := os.Rename(stagedPackage, validatedPackage); err != nil {
		return Capability{}, err
	}
	loaded, err := loadPackage(validatedRoot, identity.slug, identity.version)
	if err != nil {
		return Capability{}, err
	}
	digest, err := digestDirectory(validatedPackage)
	if err != nil {
		return Capability{}, err
	}
	finalRoot := filepath.Join(s.root, "packages", identity.slug, identity.version, digest)
	if err := os.MkdirAll(filepath.Dir(finalRoot), 0o700); err != nil {
		return Capability{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var existingDigest string
	err = s.db.QueryRowContext(ctx, `SELECT digest FROM capability_version WHERE slug = ? AND version = ?`, identity.slug, identity.version).Scan(&existingDigest)
	if err == nil {
		if existingDigest != digest {
			return Capability{}, ErrVersionConflict
		}
		return s.Get(ctx, identity.slug, identity.version)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Capability{}, err
	}
	if _, err := os.Stat(finalRoot); err == nil {
		return Capability{}, fmt.Errorf("%w: unregistered package path exists", ErrUnsafePackage)
	} else if !os.IsNotExist(err) {
		return Capability{}, err
	}
	if err := os.Rename(validatedRoot, finalRoot); err != nil {
		return Capability{}, fmt.Errorf("capability hub promote quarantine: %w", err)
	}
	promoted := true
	defer func() {
		if promoted {
			_ = os.RemoveAll(finalRoot)
		}
	}()
	toolsJSON, _ := json.Marshal(nonNil(loaded.DeclaredTools))
	subsJSON, _ := json.Marshal(nonNil(loaded.DeclaredSubSkills))
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Capability{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO capability_version(
		slug, version, digest, canonical_hash, display, description, publisher,
		source_type, source_ref, package_root, state, declared_tools,
		declared_subskills, created_at, updated_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		loaded.Slug, loaded.Version, digest, loaded.CanonicalHash, loaded.Display,
		loaded.Description, loaded.Author, request.SourceType, request.SourceRef,
		finalRoot, StateQuarantine, toolsJSON, subsJSON, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return Capability{}, fmt.Errorf("capability hub record import: %w", err)
	}
	if err := appendAudit(ctx, tx, loaded.Slug, loaded.Version, "import.quarantine", string(request.SourceType)+":"+request.SourceRef, now); err != nil {
		return Capability{}, err
	}
	if err := tx.Commit(); err != nil {
		return Capability{}, err
	}
	promoted = false
	return s.Get(ctx, loaded.Slug, loaded.Version)
}

func (s *Store) ImportLibrary(ctx context.Context, libraryRoot, slug string) (Capability, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if !safePart.MatchString(slug) {
		return Capability{}, fmt.Errorf("%w: invalid library capability id", ErrUnsafePackage)
	}
	root, err := filepath.Abs(strings.TrimSpace(libraryRoot))
	if err != nil || root == "" {
		return Capability{}, fmt.Errorf("capability hub library unavailable")
	}
	source := filepath.Join(root, slug)
	relative, err := filepath.Rel(root, source)
	if err != nil || relative != slug {
		return Capability{}, fmt.Errorf("%w: invalid library capability path", ErrUnsafePackage)
	}
	return s.ImportDirectory(ctx, ImportRequest{
		SourceDir:  source,
		SourceType: SourceLibrary,
		SourceRef:  "matrix-library/" + slug,
	})
}

func DiscoverLibrary(ctx context.Context, libraryRoot, query string, limit int) ([]LibraryItem, error) {
	root, err := filepath.Abs(strings.TrimSpace(libraryRoot))
	if err != nil || strings.TrimSpace(libraryRoot) == "" {
		return nil, fmt.Errorf("capability hub library unavailable")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("capability hub read library: %w", err)
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query = strings.ToLower(strings.TrimSpace(query))
	items := make([]LibraryItem, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || !safePart.MatchString(entry.Name()) {
			continue
		}
		identity, err := readIdentity(filepath.Join(root, entry.Name(), "SKILL.mtx"))
		if err != nil || identity.slug != entry.Name() {
			continue
		}
		loaded, err := loadPackage(root, identity.slug, identity.version)
		if err != nil {
			continue
		}
		haystack := strings.ToLower(loaded.Slug + " " + loaded.Display + " " + loaded.Description)
		if query != "" && !strings.Contains(haystack, query) {
			continue
		}
		items = append(items, LibraryItem{
			Slug: loaded.Slug, Version: loaded.Version, Display: loaded.Display,
			Description: loaded.Description, Publisher: loaded.Author,
			CanonicalHash: loaded.CanonicalHash, DeclaredTools: nonNil(loaded.DeclaredTools),
		})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func (s *Store) ImportAuthored(ctx context.Context, request AuthoredRequest) (Capability, error) {
	temp, err := os.MkdirTemp(s.root, ".authored-")
	if err != nil {
		return Capability{}, err
	}
	defer os.RemoveAll(temp)
	if err := os.WriteFile(filepath.Join(temp, "SKILL.mtx"), []byte(request.Manifest), 0o600); err != nil {
		return Capability{}, err
	}
	if strings.TrimSpace(request.Prose) != "" {
		if err := os.WriteFile(filepath.Join(temp, "SKILL.md"), []byte(request.Prose), 0o600); err != nil {
			return Capability{}, err
		}
	}
	ref := strings.TrimSpace(request.SourceRef)
	if ref == "" {
		ref = "neo-authored-proposal"
	}
	return s.ImportDirectory(ctx, ImportRequest{SourceDir: temp, SourceType: SourceAuthored, SourceRef: ref})
}

func (s *Store) ImportURL(ctx context.Context, rawURL string, sourceType SourceType) (Capability, error) {
	if sourceType != SourceURL && sourceType != SourceGitHub {
		return Capability{}, fmt.Errorf("%w: unsupported remote source type", ErrUnsafePackage)
	}
	resolved := strings.TrimSpace(rawURL)
	var archivePrefix string
	var err error
	if sourceType == SourceGitHub {
		resolved, archivePrefix, err = resolveGitHubURL(ctx, resolved)
		if err != nil {
			return Capability{}, err
		}
	}
	body, contentType, err := fetchRemote(ctx, resolved)
	if err != nil {
		return Capability{}, err
	}
	temp, err := os.MkdirTemp(s.root, ".download-")
	if err != nil {
		return Capability{}, err
	}
	defer os.RemoveAll(temp)
	packageDir := filepath.Join(temp, "package")
	if bytes.HasPrefix(body, []byte("PK\x03\x04")) || strings.Contains(strings.ToLower(contentType), "zip") {
		if err := extractZip(body, filepath.Join(temp, "archive")); err != nil {
			return Capability{}, err
		}
		found, err := locatePackage(filepath.Join(temp, "archive"), archivePrefix)
		if err != nil {
			return Capability{}, err
		}
		if err := copyPackage(found, packageDir); err != nil {
			return Capability{}, err
		}
	} else {
		if len(body) == 0 {
			return Capability{}, fmt.Errorf("%w: empty remote package", ErrUnsafePackage)
		}
		if err := os.MkdirAll(packageDir, 0o700); err != nil {
			return Capability{}, err
		}
		if err := os.WriteFile(filepath.Join(packageDir, "SKILL.mtx"), body, 0o600); err != nil {
			return Capability{}, err
		}
	}
	return s.ImportDirectory(ctx, ImportRequest{SourceDir: packageDir, SourceType: sourceType, SourceRef: rawURL})
}

type identity struct{ slug, version string }

func readIdentity(path string) (identity, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return identity{}, fmt.Errorf("%w: SKILL.mtx is required", ErrUnsafePackage)
	}
	var result identity
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "id=") && result.slug == "" {
			result.slug = trimMtxValue(strings.TrimPrefix(line, "id="))
		}
		if strings.HasPrefix(line, "version=") && result.version == "" {
			result.version = trimMtxValue(strings.TrimPrefix(line, "version="))
		}
	}
	if result.slug == "" || result.version == "" {
		return identity{}, fmt.Errorf("%w: SKILL id and version are required", ErrUnsafePackage)
	}
	return result, nil
}

func trimMtxValue(value string) string { return strings.Trim(strings.TrimSpace(value), `"`) }

func copyPackage(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: package source must be a directory", ErrUnsafePackage)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	count, total := 0, int64(0)
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink %s", ErrUnsafePackage, rel)
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(destination, rel), 0o700)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: non-regular file %s", ErrUnsafePackage, rel)
		}
		count++
		total += info.Size()
		if count > maxPackageFiles || total > maxPackageBytes {
			return fmt.Errorf("%w: package exceeds bounds", ErrUnsafePackage)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(filepath.Join(destination, rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, maxPackageBytes+1))
		inputCloseErr := input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		return closeErr
	})
}

func scanPackage(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key") || strings.Contains(name, "credential") || strings.Contains(name, "secret") {
			return fmt.Errorf("%w: secret-bearing filename %s", ErrUnsafePackage, info.Name())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pattern := range secretPatterns {
			if pattern.Match(body) {
				return fmt.Errorf("%w: possible secret in %s", ErrUnsafePackage, info.Name())
			}
		}
		return nil
	})
}

func digestDirectory(root string) (string, error) {
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, rel := range paths {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:%s:%d:", len(rel), rel, len(body)))
		_, _ = hash.Write(body)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func extractZip(body []byte, destination string) error {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("%w: invalid zip: %v", ErrUnsafePackage, err)
	}
	if len(reader.File) > maxPackageFiles {
		return fmt.Errorf("%w: archive has too many files", ErrUnsafePackage)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	var total uint64
	for _, file := range reader.File {
		clean := filepath.Clean(filepath.FromSlash(file.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: archive path traversal", ErrUnsafePackage)
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: archive symlink", ErrUnsafePackage)
		}
		total += file.UncompressedSize64
		if total > maxPackageBytes {
			return fmt.Errorf("%w: archive exceeds bounds", ErrUnsafePackage)
		}
		target := filepath.Join(destination, clean)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if !file.Mode().IsRegular() {
			return fmt.Errorf("%w: archive non-regular file", ErrUnsafePackage)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, maxPackageBytes+1))
		input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written > maxPackageBytes {
			return fmt.Errorf("%w: archive member exceeds bounds", ErrUnsafePackage)
		}
	}
	return nil
}

func locatePackage(root, preferredSuffix string) (string, error) {
	var matches []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "SKILL.mtx" {
			dir := filepath.Dir(path)
			if preferredSuffix == "" || strings.HasSuffix(filepath.ToSlash(dir), strings.Trim(preferredSuffix, "/")) {
				matches = append(matches, dir)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%w: archive must contain exactly one selected SKILL.mtx, found %d", ErrUnsafePackage, len(matches))
	}
	return matches[0], nil
}

func fetchRemote(ctx context.Context, rawURL string) ([]byte, string, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialPublic
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return validateRemoteURL(req.Context(), req.URL)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	if err := validateRemoteURL(ctx, parsed); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/zip, application/octet-stream, text/plain")
	response, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("capability hub fetch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("capability hub fetch: HTTP %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxRemoteBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxRemoteBytes {
		return nil, "", fmt.Errorf("%w: remote package exceeds bounds", ErrUnsafePackage)
	}
	return body, response.Header.Get("Content-Type"), nil
}

func validateRemoteURL(ctx context.Context, target *url.URL) error {
	if target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.RawQuery != "" {
		return fmt.Errorf("%w: remote imports require credential-free HTTPS URLs", ErrUnsafePackage)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, target.Hostname())
	if err != nil {
		return fmt.Errorf("capability hub resolve remote host: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("%w: remote host has no addresses", ErrUnsafePackage)
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("%w: remote host resolves to a non-public address", ErrUnsafePackage)
		}
	}
	return nil
}

func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		ip := address.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, fmt.Errorf("%w: remote host resolves to a non-public address", ErrUnsafePackage)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: remote host has no addresses", ErrUnsafePackage)
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
}

func resolveGitHubURL(ctx context.Context, raw string) (string, string, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if !strings.EqualFold(target.Hostname(), "github.com") || target.Scheme != "https" || target.User != nil {
		return "", "", fmt.Errorf("%w: expected an HTTPS github.com repository URL", ErrUnsafePackage)
	}
	parts := strings.Split(strings.Trim(target.Path, "/"), "/")
	if len(parts) < 2 || !safePart.MatchString(strings.ToLower(parts[0])) || !safePart.MatchString(strings.ToLower(strings.TrimSuffix(parts[1], ".git"))) {
		return "", "", fmt.Errorf("%w: invalid GitHub repository", ErrUnsafePackage)
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	ref, subpath := "", ""
	if len(parts) >= 4 && parts[2] == "tree" {
		ref = parts[3]
		if len(parts) > 4 {
			subpath = strings.Join(parts[4:], "/")
		}
	} else if len(parts) > 2 {
		return "", "", fmt.Errorf("%w: unsupported GitHub path", ErrUnsafePackage)
	}
	if ref == "" {
		body, _, err := fetchRemote(ctx, "https://api.github.com/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo))
		if err != nil {
			return "", "", err
		}
		var metadata struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := json.Unmarshal(body, &metadata); err != nil || metadata.DefaultBranch == "" {
			return "", "", fmt.Errorf("capability hub resolve GitHub default branch")
		}
		ref = metadata.DefaultBranch
	}
	if strings.ContainsAny(ref, "?#") || strings.Contains(ref, "..") {
		return "", "", fmt.Errorf("%w: invalid GitHub ref", ErrUnsafePackage)
	}
	archive := "https://codeload.github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/zip/" + url.PathEscape(ref)
	prefix := repo + "-" + strings.ReplaceAll(ref, "/", "-")
	if subpath != "" {
		prefix += "/" + subpath
	}
	return archive, prefix, nil
}

func validSourceType(value SourceType) bool {
	switch value {
	case SourceLibrary, SourceGitHub, SourceURL, SourceAuthored:
		return true
	default:
		return false
	}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
