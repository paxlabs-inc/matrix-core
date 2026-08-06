// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"matrix/neo/internal/runtime/records"
)

type Item struct {
	SourceNamespace  string
	SourceID         string
	ConversationID   string
	SemanticKind     string
	RevisionIdentity string
	Content          string
	Sector           Sector
	NeverTrim        bool
}

func hash(content string) string {
	digest := sha256.Sum256([]byte(strings.Join(strings.Fields(content), " ")))
	return hex.EncodeToString(digest[:])
}

func normalizedOverlap(content string) string {
	content = strings.ToLower(strings.Join(strings.Fields(content), " "))
	for _, prefix := range []string{"user:", "assistant:", "tool result:", "memory:"} {
		content = strings.TrimSpace(strings.TrimPrefix(content, prefix))
	}
	return content
}

func identity(item Item) string {
	return strings.Join([]string{item.SourceNamespace, item.SourceID, item.ConversationID, item.SemanticKind, item.RevisionIdentity}, "\x00")
}

func source(item Item) string {
	return strings.Join([]string{item.SourceNamespace, item.SourceID}, "\x00")
}

func category(kind string) string {
	kind = strings.ToLower(kind)
	switch {
	case strings.Contains(kind, "transcript") || kind == "user" || kind == "assistant":
		return "transcript"
	case strings.Contains(kind, "tool"):
		return "tool"
	case strings.Contains(kind, "stable_identity"):
		return "stable_identity"
	case strings.Contains(kind, "memory") || strings.Contains(kind, "recall"):
		return "memory"
	default:
		return kind
	}
}

// Deduplicate applies the frozen six-step identity/content order. Input order
// is sector precedence order; the first valid representation is authoritative.
func Deduplicate(items []Item) ([]Item, records.ContextManifest) {
	included := make([]Item, 0, len(items))
	manifest := records.ContextManifest{Entries: make([]records.ContextManifestEntry, 0, len(items))}
	identities := make(map[string]struct{})
	sources := make(map[string]string)
	hashes := make(map[string]struct{})
	overlaps := make(map[string]string)
	for _, item := range items {
		entry := records.ContextManifestEntry{
			SourceNamespace: item.SourceNamespace, SourceID: item.SourceID,
			ConversationID: item.ConversationID, SemanticKind: item.SemanticKind,
			ContentHash: hash(item.Content), RevisionIdentity: item.RevisionIdentity,
		}
		reason := "included"
		id := identity(item)
		base := source(item)
		overlap := normalizedOverlap(item.Content)
		kind := category(item.SemanticKind)
		if _, exists := identities[id]; exists {
			reason = "duplicate_source_identity"
		} else if revision, exists := sources[base]; exists && revision != item.RevisionIdentity {
			reason = "superseded_revision"
		} else if _, exists := hashes[entry.ContentHash]; exists {
			reason = "identical_normalized_content"
		} else if prior, exists := overlaps[overlap]; exists && ((prior == "transcript" && kind == "memory") || (prior == "memory" && kind == "transcript")) {
			reason = "transcript_recall_overlap"
		} else if prior, exists := overlaps[overlap]; exists && ((prior == "tool" && kind == "memory") || (prior == "memory" && kind == "tool")) {
			reason = "tool_result_memory_overlap"
		} else if prior, exists := overlaps[overlap]; exists && ((prior == "stable_identity" && kind == "memory") || (prior == "memory" && kind == "stable_identity")) {
			reason = "stable_identity_memory_overlap"
		}
		entry.Included = reason == "included"
		entry.Reason = reason
		manifest.Entries = append(manifest.Entries, entry)
		if !entry.Included {
			continue
		}
		included = append(included, item)
		identities[id] = struct{}{}
		sources[base] = item.RevisionIdentity
		hashes[entry.ContentHash] = struct{}{}
		if overlap != "" {
			overlaps[overlap] = kind
		}
	}
	return included, manifest
}
