// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"centra/packages/vault"
)

const schema = "neo.tool-artifact.v1"

var ErrNotFound = errors.New("artifacts: not found")

type Metadata struct {
	ArtifactID          string          `json:"artifact_id"`
	LogicalTurnID       string          `json:"logical_turn_id"`
	CycleIdentity       string          `json:"cycle_identity"`
	CallIdentity        string          `json:"call_identity"`
	Tool                string          `json:"tool"`
	NormalizedArgs      json.RawMessage `json:"normalized_arguments"`
	MIME                string          `json:"mime"`
	ByteSize            int64           `json:"byte_size"`
	ContentHash         string          `json:"content_hash"`
	CreatedAt           time.Time       `json:"created_at"`
	Retention           string          `json:"retention"`
	EffectStatus        string          `json:"effect_status"`
	AccessAuthorization string          `json:"access_authorization"`
}

type ToolResultProjection struct {
	ArtifactID         string         `json:"artifact_id"`
	Operation          string         `json:"operation"`
	Status             string         `json:"status"`
	Summary            string         `json:"summary"`
	ImportantFields    map[string]any `json:"important_fields,omitempty"`
	EvidenceReferences []string       `json:"evidence_references,omitempty"`
	Warnings           []string       `json:"warnings,omitempty"`
	ByteSize           int64          `json:"byte_size"`
	Truncated          bool           `json:"truncated"`
	AvailableSelectors []string       `json:"available_selectors"`
}

type Selector struct {
	ByteOffset  int64    `json:"byte_offset,omitempty"`
	ByteLength  int64    `json:"byte_length,omitempty"`
	LineStart   int      `json:"line_start,omitempty"`
	LineEnd     int      `json:"line_end,omitempty"`
	JSONPointer string   `json:"json_pointer,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	Search      string   `json:"search,omitempty"`
	Limit       int      `json:"limit,omitempty"`
	Page        int      `json:"page,omitempty"`
	PageSize    int      `json:"page_size,omitempty"`
	Child       string   `json:"child,omitempty"`
}

type Store struct {
	dir   string
	vault *vault.UserVault
	user  string
}

func Open(dir string, session *vault.Session) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("artifacts: directory is required")
	}
	if session == nil || !session.Encrypting() || session.UserVault() == nil {
		return nil, vault.ErrVaultRequired
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, vault: session.UserVault(), user: session.UserVault().User()}, nil
}

func (s *Store) ad(id, part string) vault.AD {
	return vault.AD{User: s.user, Store: "neo.artifacts", Stream: id + ":" + part, Schema: schema}
}

func id() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "artifact-" + hex.EncodeToString(raw), nil
}

func normalizeArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return encoded
}

func (s *Store) Put(_ context.Context, meta Metadata, content []byte) (Metadata, ToolResultProjection, error) {
	if strings.TrimSpace(meta.LogicalTurnID) == "" || strings.TrimSpace(meta.CallIdentity) == "" || strings.TrimSpace(meta.Tool) == "" {
		return Metadata{}, ToolResultProjection{}, fmt.Errorf("artifacts: turn, call, and tool identities are required")
	}
	if meta.ArtifactID == "" {
		var err error
		meta.ArtifactID, err = id()
		if err != nil {
			return Metadata{}, ToolResultProjection{}, err
		}
	}
	meta.NormalizedArgs = normalizeArgs(meta.NormalizedArgs)
	if meta.MIME == "" {
		meta.MIME = "application/octet-stream"
	}
	meta.ByteSize = int64(len(content))
	digest := sha256.Sum256(content)
	meta.ContentHash = hex.EncodeToString(digest[:])
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	if meta.Retention == "" {
		meta.Retention = "turn-plus-30d"
	}
	if meta.EffectStatus == "" {
		meta.EffectStatus = "completed"
	}
	if meta.AccessAuthorization == "" {
		meta.AccessAuthorization = "same-person-runtime"
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return Metadata{}, ToolResultProjection{}, err
	}
	sealedContent, err := s.vault.SealFile(s.ad(meta.ArtifactID, "content"), content)
	if err != nil {
		return Metadata{}, ToolResultProjection{}, err
	}
	sealedMeta, err := s.vault.SealFile(s.ad(meta.ArtifactID, "metadata"), encodedMeta)
	if err != nil {
		return Metadata{}, ToolResultProjection{}, err
	}
	if err := atomicWrite(filepath.Join(s.dir, meta.ArtifactID+".content.vault"), sealedContent); err != nil {
		return Metadata{}, ToolResultProjection{}, err
	}
	if err := atomicWrite(filepath.Join(s.dir, meta.ArtifactID+".metadata.vault"), sealedMeta); err != nil {
		return Metadata{}, ToolResultProjection{}, err
	}
	return meta, project(meta, content), nil
}

func atomicWrite(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *Store) load(id string) (Metadata, []byte, error) {
	sealedMeta, err := os.ReadFile(filepath.Join(s.dir, id+".metadata.vault"))
	if errors.Is(err, os.ErrNotExist) {
		return Metadata{}, nil, ErrNotFound
	}
	if err != nil {
		return Metadata{}, nil, err
	}
	plainMeta, err := s.vault.OpenFile(s.ad(id, "metadata"), sealedMeta)
	if err != nil {
		return Metadata{}, nil, err
	}
	var meta Metadata
	if err := json.Unmarshal(plainMeta, &meta); err != nil {
		return Metadata{}, nil, err
	}
	sealedContent, err := os.ReadFile(filepath.Join(s.dir, id+".content.vault"))
	if err != nil {
		return Metadata{}, nil, err
	}
	content, err := s.vault.OpenFile(s.ad(id, "content"), sealedContent)
	if err != nil {
		return Metadata{}, nil, err
	}
	digest := sha256.Sum256(content)
	if int64(len(content)) != meta.ByteSize || hex.EncodeToString(digest[:]) != meta.ContentHash {
		return Metadata{}, nil, fmt.Errorf("artifacts: integrity mismatch")
	}
	return meta, content, nil
}

func (s *Store) Projection(_ context.Context, artifactID string) (ToolResultProjection, error) {
	meta, content, err := s.load(artifactID)
	if err != nil {
		return ToolResultProjection{}, err
	}
	return project(meta, content), nil
}

func project(meta Metadata, content []byte) ToolResultProjection {
	projection := ToolResultProjection{
		ArtifactID: meta.ArtifactID, Operation: meta.Tool, Status: meta.EffectStatus,
		Summary:  "Complete " + family(meta.Tool) + " result stored as an encrypted artifact.",
		ByteSize: meta.ByteSize, Truncated: meta.ByteSize > 16<<10,
		AvailableSelectors: []string{"byte_range", "line_range", "json_pointer", "field_selection", "search", "table_page", "child_object"},
	}
	var value any
	if json.Unmarshal(content, &value) == nil {
		projection.ImportantFields = importantJSON(value)
		if object, ok := value.(map[string]any); ok {
			for _, key := range []string{"url", "path", "status", "exit_code", "title", "count"} {
				if scalar, exists := object[key]; exists {
					projection.EvidenceReferences = append(projection.EvidenceReferences, key+"="+fmt.Sprint(scalar))
				}
			}
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(content))
		lines := 0
		for scanner.Scan() {
			lines++
		}
		projection.ImportantFields = map[string]any{"lines": lines, "mime": meta.MIME}
	}
	if projection.Truncated {
		projection.Warnings = []string{"A bounded evidence preview is included; additional detail remains durable in the encrypted artifact."}
	}
	return projection
}

func family(tool string) string {
	tool = strings.ToLower(tool)
	switch {
	case strings.Contains(tool, "file") || strings.Contains(tool, "read"):
		return "file-read"
	case strings.Contains(tool, "search"):
		return "search"
	case strings.Contains(tool, "shell") || strings.Contains(tool, "exec"):
		return "shell"
	case strings.Contains(tool, "browser"):
		return "browser"
	case strings.Contains(tool, "git"):
		return "git"
	case strings.Contains(tool, "service"):
		return "service"
	default:
		return "JSON/API"
	}
}

func importantJSON(value any) map[string]any {
	result := make(map[string]any)
	object, ok := value.(map[string]any)
	if !ok {
		if array, ok := value.([]any); ok {
			result["items"] = len(array)
		}
		return result
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch typed := object[key].(type) {
		case string, float64, bool, nil:
			if len(result) < 16 {
				result[key] = typed
			}
		case []any:
			result[key+"_count"] = len(typed)
			if key == "results" || key == "sources" || key == "items" {
				if preview := boundedArrayPreview(typed); len(preview) > 0 {
					result[key+"_preview"] = preview
				}
			}
		case map[string]any:
			result[key+"_fields"] = len(typed)
		}
	}
	return result
}

func boundedArrayPreview(items []any) []map[string]any {
	const (
		maxItems      = 12
		maxStringSize = 700
	)
	preview := make([]map[string]any, 0, min(len(items), maxItems))
	for _, item := range items {
		if len(preview) == maxItems {
			break
		}
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entry := make(map[string]any)
		for _, key := range []string{
			"title", "url", "published", "publishedDate", "author",
			"status", "snippet", "excerpt", "summary",
		} {
			value, exists := object[key]
			if !exists {
				continue
			}
			switch typed := value.(type) {
			case string:
				if len(typed) > maxStringSize {
					typed = typed[:maxStringSize] + "…"
				}
				entry[key] = typed
			case float64, bool, nil:
				entry[key] = typed
			}
		}
		if len(entry) > 0 {
			preview = append(preview, entry)
		}
	}
	return preview
}

func (s *Store) Rehydrate(_ context.Context, artifactID string, selector Selector) ([]byte, error) {
	_, content, err := s.load(artifactID)
	if err != nil {
		return nil, err
	}
	selected := 0
	for _, active := range []bool{selector.ByteLength > 0, selector.LineStart > 0 || selector.LineEnd > 0, selector.JSONPointer != "", len(selector.Fields) > 0, selector.Search != "", selector.PageSize > 0, selector.Child != ""} {
		if active {
			selected++
		}
	}
	if selected != 1 {
		return nil, fmt.Errorf("artifacts: exactly one selector is required")
	}
	switch {
	case selector.ByteLength > 0:
		if selector.ByteOffset < 0 || selector.ByteLength > 1<<20 || selector.ByteOffset >= int64(len(content)) {
			return nil, fmt.Errorf("artifacts: invalid bounded byte range")
		}
		end := min(selector.ByteOffset+selector.ByteLength, int64(len(content)))
		return append([]byte(nil), content[selector.ByteOffset:end]...), nil
	case selector.LineStart > 0 || selector.LineEnd > 0:
		return selectLines(content, selector.LineStart, selector.LineEnd)
	case selector.JSONPointer != "":
		return selectJSON(content, selector.JSONPointer)
	case len(selector.Fields) > 0:
		return selectFields(content, selector.Fields)
	case selector.Search != "":
		return search(content, selector.Search, selector.Limit)
	case selector.PageSize > 0:
		return tablePage(content, selector.Page, selector.PageSize)
	default:
		return selectJSON(content, selector.Child)
	}
}

func selectLines(content []byte, start, end int) ([]byte, error) {
	if start < 1 || end < start || end-start+1 > 5000 {
		return nil, fmt.Errorf("artifacts: invalid bounded line range")
	}
	lines := strings.Split(string(content), "\n")
	if start > len(lines) {
		return nil, fmt.Errorf("artifacts: line range starts past end")
	}
	end = min(end, len(lines))
	return []byte(strings.Join(lines[start-1:end], "\n")), nil
}

func pointer(value any, raw string) (any, error) {
	if raw == "" || raw == "/" {
		return value, nil
	}
	if !strings.HasPrefix(raw, "/") {
		return nil, fmt.Errorf("artifacts: JSON Pointer must start with /")
	}
	current := value
	for _, token := range strings.Split(strings.TrimPrefix(raw, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[token]
			if !ok {
				return nil, ErrNotFound
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, ErrNotFound
			}
			current = typed[index]
		default:
			return nil, ErrNotFound
		}
	}
	return current, nil
}

func selectJSON(content []byte, path string) ([]byte, error) {
	var value any
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, err
	}
	selected, err := pointer(value, path)
	if err != nil {
		return nil, err
	}
	return json.Marshal(selected)
}

func selectFields(content []byte, fields []string) ([]byte, error) {
	if len(fields) > 100 {
		return nil, fmt.Errorf("artifacts: too many fields")
	}
	var object map[string]any
	if err := json.Unmarshal(content, &object); err != nil {
		return nil, err
	}
	result := make(map[string]any)
	for _, field := range fields {
		if value, ok := object[field]; ok {
			result[field] = value
		}
	}
	return json.Marshal(result)
}

func search(content []byte, query string, limit int) ([]byte, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, fmt.Errorf("artifacts: search query is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var hits []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.Contains(strings.ToLower(line), query) {
			hits = append(hits, line)
			if len(hits) == limit {
				break
			}
		}
	}
	return json.Marshal(hits)
}

func tablePage(content []byte, page, size int) ([]byte, error) {
	if page < 0 || size < 1 || size > 1000 {
		return nil, fmt.Errorf("artifacts: invalid table page")
	}
	lines := strings.Split(string(content), "\n")
	start := page * size
	if start >= len(lines) {
		return []byte{}, nil
	}
	end := min(start+size, len(lines))
	return []byte(strings.Join(lines[start:end], "\n")), nil
}
