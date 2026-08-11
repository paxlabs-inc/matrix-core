// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const knowledgeDocumentLimit = 2 << 20

type KnowledgeDocumentUpdate struct {
	Title         *string `json:"title,omitempty"`
	Content       *string `json:"content,omitempty"`
	TopicID       *string `json:"topic_id,omitempty"`
	Archived      *bool   `json:"archived,omitempty"`
	RetentionDays *int    `json:"retention_days,omitempty"`
}

func cloneKnowledge(state KnowledgeState) KnowledgeState {
	encoded, _ := json.Marshal(state)
	var cloned KnowledgeState
	_ = json.Unmarshal(encoded, &cloned)
	cloned.initialize()
	return cloned
}

func knowledgeID(kind string, values ...string) string {
	hash := sha256.Sum256([]byte(kind + "\x00" + strings.Join(values, "\x00") + "\x00" + time.Now().UTC().Format(time.RFC3339Nano)))
	return kind + ":" + hex.EncodeToString(hash[:12])
}

func (p *Pager) mutateKnowledge(ctx context.Context, apply func(*KnowledgeState) error) error {
	if p == nil || p.client == nil {
		return ErrNeocortexUnavailable
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	candidate := cloneKnowledge(p.state.Knowledge)
	if err := apply(&candidate); err != nil {
		return err
	}
	if err := p.saveDomainLocked(ctx, knowledgeCheckpoint, candidate); err != nil {
		return err
	}
	p.state.Knowledge = candidate
	return nil
}

func (p *Pager) KnowledgeSnapshot() KnowledgeState {
	if p == nil {
		state := KnowledgeState{}
		state.initialize()
		return state
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneKnowledge(p.state.Knowledge)
}

func (p *Pager) CreateKnowledgeTopic(ctx context.Context, name, parentID string) (KnowledgeTopic, error) {
	name, parentID = strings.TrimSpace(name), strings.TrimSpace(parentID)
	if name == "" || len(name) > 200 {
		return KnowledgeTopic{}, errors.New("knowledge topic name is required and must not exceed 200 characters")
	}
	now := time.Now().UTC()
	topic := KnowledgeTopic{ID: knowledgeID("topic", parentID, name), ParentID: parentID, Name: name, CreatedAt: now, UpdatedAt: now}
	err := p.mutateKnowledge(ctx, func(state *KnowledgeState) error {
		if parentID != "" {
			parent, ok := state.Topics[parentID]
			if !ok || parent.Archived {
				return errors.New("knowledge parent topic does not exist")
			}
		}
		state.Topics[topic.ID] = topic
		return nil
	})
	return topic, err
}

func (p *Pager) UpdateKnowledgeTopic(ctx context.Context, id, name, parentID string, archived *bool) (KnowledgeTopic, error) {
	id, name, parentID = strings.TrimSpace(id), strings.TrimSpace(name), strings.TrimSpace(parentID)
	var updated KnowledgeTopic
	err := p.mutateKnowledge(ctx, func(state *KnowledgeState) error {
		topic, ok := state.Topics[id]
		if !ok {
			return errors.New("knowledge topic does not exist")
		}
		if name != "" {
			if len(name) > 200 {
				return errors.New("knowledge topic name exceeds 200 characters")
			}
			topic.Name = name
		}
		if parentID != "" && parentID != id {
			if _, ok := state.Topics[parentID]; !ok {
				return errors.New("knowledge parent topic does not exist")
			}
			topic.ParentID = parentID
		}
		if archived != nil {
			topic.Archived = *archived
		}
		topic.UpdatedAt = time.Now().UTC()
		state.Topics[id], updated = topic, topic
		return nil
	})
	return updated, err
}

func (p *Pager) ImportKnowledge(ctx context.Context, request KnowledgeImportRequest) (KnowledgeDocument, error) {
	request.Title, request.Content = strings.TrimSpace(request.Title), strings.TrimSpace(request.Content)
	request.TopicID, request.TopicName = strings.TrimSpace(request.TopicID), strings.TrimSpace(request.TopicName)
	request.SourceKind, request.SourceTitle, request.SourceURL = strings.TrimSpace(request.SourceKind), strings.TrimSpace(request.SourceTitle), strings.TrimSpace(request.SourceURL)
	if request.Title == "" || request.Content == "" {
		return KnowledgeDocument{}, errors.New("knowledge title and content are required")
	}
	if len(request.Content) > knowledgeDocumentLimit {
		return KnowledgeDocument{}, errors.New("knowledge document exceeds the 2 MiB limit")
	}
	if request.RetentionDays < 0 || request.RetentionDays > 36500 {
		return KnowledgeDocument{}, errors.New("knowledge retention_days must be between 0 and 36500")
	}
	if request.SourceKind == "" {
		request.SourceKind = "document"
	}
	if request.SourceURL != "" {
		parsed, err := url.Parse(request.SourceURL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return KnowledgeDocument{}, errors.New("knowledge source_url must be an absolute HTTP(S) URL")
		}
	}
	now := time.Now().UTC()
	var document KnowledgeDocument
	err := p.mutateKnowledge(ctx, func(state *KnowledgeState) error {
		topicID := request.TopicID
		if topicID == "" {
			if request.TopicName == "" {
				request.TopicName = "Imported"
			}
			for id, topic := range state.Topics {
				if !topic.Archived && strings.EqualFold(topic.Name, request.TopicName) {
					topicID = id
					break
				}
			}
			if topicID == "" {
				topicID = knowledgeID("topic", request.TopicName)
				state.Topics[topicID] = KnowledgeTopic{ID: topicID, Name: request.TopicName, CreatedAt: now, UpdatedAt: now}
			}
		}
		topic, ok := state.Topics[topicID]
		if !ok || topic.Archived {
			return errors.New("knowledge topic does not exist or is archived")
		}
		digest := sha256.Sum256([]byte(request.Content))
		sourceID := knowledgeID("source", request.SourceKind, request.SourceURL, hex.EncodeToString(digest[:]))
		state.Sources[sourceID] = KnowledgeSource{ID: sourceID, Kind: request.SourceKind, Title: firstKnowledgeNonEmpty(request.SourceTitle, request.Title), URL: request.SourceURL, ContentHash: hex.EncodeToString(digest[:]), ImportedAt: now}
		documentID := knowledgeID("document", topicID, request.Title, sourceID)
		version := KnowledgeDocumentVersion{Version: 1, Title: request.Title, Content: request.Content, SourceID: sourceID, CreatedAt: now}
		document = KnowledgeDocument{ID: documentID, TopicID: topicID, Title: request.Title, Content: request.Content, SourceID: sourceID, Version: 1, Versions: []KnowledgeDocumentVersion{version}, RetentionDays: request.RetentionDays, CreatedAt: now, UpdatedAt: now}
		state.Documents[documentID] = document
		entityIDs := map[string]string{}
		for _, input := range request.Entities {
			name, kind := strings.TrimSpace(input.Name), strings.TrimSpace(input.Kind)
			if name == "" {
				continue
			}
			id := knowledgeID("entity", strings.ToLower(name), kind)
			state.Entities[id] = KnowledgeEntity{ID: id, Name: name, Kind: kind, SourceIDs: []string{sourceID}}
			entityIDs[strings.ToLower(name)] = id
		}
		for _, input := range request.Relationships {
			from, to := entityIDs[strings.ToLower(strings.TrimSpace(input.From))], entityIDs[strings.ToLower(strings.TrimSpace(input.To))]
			if from == "" || to == "" || strings.TrimSpace(input.Kind) == "" {
				return errors.New("knowledge relationships must reference entities in the same import")
			}
			id := knowledgeID("relationship", from, to, input.Kind)
			state.Relationships[id] = KnowledgeRelationship{ID: id, FromID: from, ToID: to, Kind: strings.TrimSpace(input.Kind), SourceID: sourceID, Contradicts: strings.TrimSpace(input.Contradicts), Supersedes: strings.TrimSpace(input.Supersedes)}
		}
		return nil
	})
	return document, err
}

func (p *Pager) UpdateKnowledgeDocument(ctx context.Context, id string, update KnowledgeDocumentUpdate) (KnowledgeDocument, error) {
	var result KnowledgeDocument
	err := p.mutateKnowledge(ctx, func(state *KnowledgeState) error {
		document, ok := state.Documents[strings.TrimSpace(id)]
		if !ok {
			return errors.New("knowledge document does not exist")
		}
		changed := false
		if update.TopicID != nil {
			topic, ok := state.Topics[strings.TrimSpace(*update.TopicID)]
			if !ok || topic.Archived {
				return errors.New("knowledge topic does not exist or is archived")
			}
			document.TopicID, changed = strings.TrimSpace(*update.TopicID), true
		}
		if update.Title != nil {
			value := strings.TrimSpace(*update.Title)
			if value == "" {
				return errors.New("knowledge title cannot be empty")
			}
			document.Title, changed = value, true
		}
		if update.Content != nil {
			value := strings.TrimSpace(*update.Content)
			if value == "" || len(value) > knowledgeDocumentLimit {
				return errors.New("knowledge content is empty or exceeds the 2 MiB limit")
			}
			document.Content, changed = value, true
		}
		if update.Archived != nil {
			document.Archived, changed = *update.Archived, true
		}
		if update.RetentionDays != nil {
			if *update.RetentionDays < 0 || *update.RetentionDays > 36500 {
				return errors.New("knowledge retention_days must be between 0 and 36500")
			}
			document.RetentionDays, changed = *update.RetentionDays, true
		}
		if changed {
			previous := document.Version
			document.Version++
			document.UpdatedAt = time.Now().UTC()
			document.Versions = append(document.Versions, KnowledgeDocumentVersion{Version: document.Version, Title: document.Title, Content: document.Content, SourceID: document.SourceID, CreatedAt: document.UpdatedAt, Supersedes: previous})
		}
		state.Documents[document.ID], result = document, document
		return nil
	})
	return result, err
}

func (p *Pager) SearchKnowledge(query string, limit int) ([]KnowledgeSearchHit, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, errors.New("knowledge search query is required")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	state := p.KnowledgeSnapshot()
	now := time.Now().UTC()
	queryTokens := knowledgeTokens(query)
	hits := make([]KnowledgeSearchHit, 0)
	for _, document := range state.Documents {
		if document.Archived || (document.RetentionDays > 0 && now.After(document.CreatedAt.Add(time.Duration(document.RetentionDays)*24*time.Hour))) {
			continue
		}
		haystack := strings.ToLower(document.Title + "\n" + document.Content)
		exact := strings.Contains(haystack, query)
		score := 0.0
		if exact {
			score = 2
		}
		docTokens := knowledgeTokens(haystack)
		overlap := 0
		for token := range queryTokens {
			if _, ok := docTokens[token]; ok {
				overlap++
			}
		}
		if len(queryTokens) > 0 {
			score += float64(overlap) / float64(len(queryTokens))
		}
		if score == 0 {
			continue
		}
		hits = append(hits, KnowledgeSearchHit{DocumentID: document.ID, TopicID: document.TopicID, Title: document.Title, Snippet: knowledgeSnippet(document.Content, query), Source: state.Sources[document.SourceID], Score: score, Exact: exact})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Title < hits[j].Title
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (p *Pager) ExportKnowledge() KnowledgeExport {
	return KnowledgeExport{SchemaVersion: 1, ExportedAt: time.Now().UTC(), State: p.KnowledgeSnapshot()}
}

func knowledgeTokens(value string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return r < '0' || (r > '9' && r < 'a') || r > 'z' }) {
		if len(token) > 1 {
			out[token] = struct{}{}
		}
	}
	return out
}
func knowledgeSnippet(content, query string) string {
	const maximum = 320
	lower := strings.ToLower(content)
	index := strings.Index(lower, query)
	if index < 0 {
		index = 0
	}
	start := index - 80
	if start < 0 {
		start = 0
	}
	end := start + maximum
	if end > len(content) {
		end = len(content)
	}
	return strings.TrimSpace(content[start:end])
}
func firstKnowledgeNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Imported source"
}

func (s KnowledgeState) Validate() error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("unsupported knowledge schema version %d", s.SchemaVersion)
	}
	return nil
}
