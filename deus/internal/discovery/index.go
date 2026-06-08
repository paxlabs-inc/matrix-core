package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paxlabs-inc/deus/pkg/manifest"
)

// BuildSearchDocument concatenates manifest fields for embedding and lexical index.
func BuildSearchDocument(m *manifest.Manifest) string {
	var b strings.Builder
	b.WriteString(m.DisplayName)
	b.WriteString(" ")
	b.WriteString(m.Summary)
	b.WriteString(" ")
	b.WriteString(m.Description)
	for _, t := range m.Tags {
		b.WriteString(t)
		b.WriteString(" ")
	}
	for _, op := range m.Operations {
		b.WriteString(op.Name)
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// IndexService embeds and indexes a listing for discovery.
func (s *Service) IndexService(ctx context.Context, serviceID string, raw json.RawMessage) error {
	m, err := manifest.ValidateBytes(raw)
	if err != nil {
		return fmt.Errorf("discovery: index validate: %w", err)
	}
	doc := BuildSearchDocument(m)
	if err := s.store.SetSearchDocument(ctx, serviceID, doc); err != nil {
		return err
	}
	if s.embed == nil {
		return nil
	}
	vec, err := s.embed.Embed(ctx, doc)
	if err != nil {
		return fmt.Errorf("discovery: embed: %w", err)
	}
	return s.store.UpsertEmbedding(ctx, serviceID, s.embed.Model(), vec)
}
