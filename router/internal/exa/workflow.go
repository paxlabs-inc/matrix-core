// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"fmt"
	"strings"
)

type FinanceRequest struct {
	Kind          string   `json:"kind"`
	Symbol        string   `json:"symbol"`
	AsOf          string   `json:"as_of,omitempty"`
	Effort        string   `json:"effort,omitempty"`
	RubricVersion string   `json:"rubric_version,omitempty"`
	Dimensions    []string `json:"dimensions,omitempty"`
}

type VerifyRequest struct {
	Symbol string   `json:"symbol"`
	Fields []string `json:"fields"`
	AsOf   string   `json:"as_of,omitempty"`
}

type FinanceNewsRequest struct {
	Symbol string   `json:"symbol"`
	URLs   []string `json:"urls"`
}

func (s *Service) StartFinance(ctx context.Context, user string, request FinanceRequest) (*RunEnvelope, error) {
	request.Symbol = strings.ToUpper(strings.TrimSpace(request.Symbol))
	if request.Symbol == "" || len(request.Symbol) > 16 {
		return nil, badRequest("finance/research", "A valid market symbol is required.")
	}
	if request.Effort == "" {
		request.Effort = "medium"
	}
	agent, workflow, err := financeAgentRequest(request)
	if err != nil {
		return nil, err
	}
	return s.StartRun(ctx, user, workflow, request.Symbol, agent)
}

func (s *Service) VerifyFinance(ctx context.Context, user string, request VerifyRequest) (*SearchEnvelope, error) {
	request.Symbol = strings.ToUpper(strings.TrimSpace(request.Symbol))
	if request.Symbol == "" || len(request.Fields) == 0 || len(request.Fields) > 8 {
		return nil, badRequest("finance/verify", "A symbol and between 1 and 8 fields are required.")
	}
	properties := map[string]any{}
	for _, field := range request.Fields {
		name := safeField(field)
		if name == "" {
			return nil, badRequest("finance/verify", "Verification field names may contain only letters, numbers, and underscores.")
		}
		properties[name] = typedFactSchema()
	}
	return s.Search(ctx, user, SearchRequest{
		Query: fmt.Sprintf("Verify the requested financial facts for %s as of %s. Prefer issuer investor relations, SEC filings, exchanges, and authoritative macro sources. Return inconclusive when sources do not establish a value.", request.Symbol, emptyAsCurrent(request.AsOf)),
		Type:  "deep", Category: "financial report", NumResults: 8,
		SystemPrompt: "Do not infer missing values. Preserve fiscal periods, units, currencies, and as-of dates. Use primary sources wherever available.",
		OutputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": properties},
		Contents:     ContentOptions{Highlights: true},
	})
}

func (s *Service) ExtractFinanceNews(ctx context.Context, user string, request FinanceNewsRequest) (*ContentsEnvelope, error) {
	request.Symbol = strings.ToUpper(strings.TrimSpace(request.Symbol))
	if request.Symbol == "" || len(request.URLs) == 0 || len(request.URLs) > 10 {
		return nil, badRequest("finance/news", "A symbol and between 1 and 10 news URLs are required.")
	}
	seen := make(map[string]bool, len(request.URLs))
	urls := make([]string, 0, len(request.URLs))
	for _, raw := range request.URLs {
		canonical := canonicalURL(raw)
		if canonical == "" {
			return nil, badRequest("finance/news", "Every news source must be a valid HTTP or HTTPS URL.")
		}
		if !seen[canonical] {
			seen[canonical] = true
			urls = append(urls, canonical)
		}
	}
	return s.Contents(ctx, user, ContentsRequest{
		URLs: urls,
		Highlights: map[string]any{
			"query":         fmt.Sprintf("Material facts, company statements, dates, and disclosed financial context about %s", request.Symbol),
			"maxCharacters": 4000,
		},
	})
}

func financeAgentRequest(request FinanceRequest) (AgentRequest, string, error) {
	symbol, asOf := request.Symbol, emptyAsCurrent(request.AsOf)
	sources := map[string]any{"type": "array", "maxItems": 6, "items": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string", "format": "uri"}, "title": map[string]any{"type": "string"}}}}
	sourced := map[string]any{"type": "object", "required": []string{"value", "sources"}, "properties": map[string]any{"value": map[string]any{"type": "string"}, "sources": sources}}
	switch request.Kind {
	case "equity_brief":
		schema := map[string]any{"type": "object", "required": []string{"ticker", "report_date", "key_debates", "kpis_to_watch"}, "properties": map[string]any{
			"ticker": map[string]any{"type": "string"}, "company_name": map[string]any{"type": "string"}, "report_date": map[string]any{"type": "string"}, "fiscal_period": map[string]any{"type": "string"}, "reporting_currency": map[string]any{"type": "string"},
			"key_debates":            map[string]any{"type": "array", "maxItems": 6, "items": map[string]any{"type": "object", "required": []string{"debate", "bull_case", "bear_case"}, "properties": map[string]any{"debate": map[string]any{"type": "string"}, "bull_case": sourced, "bear_case": sourced, "what_resolves_it": map[string]any{"type": "string"}}}},
			"kpis_to_watch":          map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{"type": "object", "required": []string{"kpi", "why_it_matters"}, "properties": map[string]any{"kpi": map[string]any{"type": "string"}, "why_it_matters": map[string]any{"type": "string"}, "last_reported_value": sourced}}},
			"peer_readthroughs":      map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "object", "properties": map[string]any{"peer": map[string]any{"type": "string"}, "signal": map[string]any{"type": "string"}, "direction": map[string]any{"type": "string"}, "sources": sources}}},
			"recent_mgmt_commentary": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "object", "properties": map[string]any{"date": map[string]any{"type": "string"}, "venue": map[string]any{"type": "string"}, "speaker": map[string]any{"type": "string"}, "takeaway": map[string]any{"type": "string"}, "sources": sources}}},
		}}
		return AgentRequest{Effort: request.Effort, Query: fmt.Sprintf("Produce a grounded equity research and earnings brief for %s as of %s. Cover bull and bear debates, KPIs, peer read-throughs, ownership or filing changes, and recent management commentary. Ground every factual and numeric claim in primary sources and explicitly mark unresolved conflicts or inconclusive evidence.", symbol, asOf), OutputSchema: schema}, "finance.equity_brief.v1", nil

	case "enrichment":
		properties := map[string]any{"company_name": map[string]any{"type": "string"}, "ticker": map[string]any{"type": "string"}, "market_cap": typedFactSchema(), "latest_revenue": typedFactSchema(), "latest_eps": typedFactSchema(), "next_earnings_date": typedFactSchema(), "recent_ownership_filings": map[string]any{"type": "array", "maxItems": 10, "items": sourced}}
		return AgentRequest{Effort: request.Effort, Query: fmt.Sprintf("Enrich public company %s as of %s using issuer materials, SEC filings, exchange data, and other authoritative primary sources. Values must be numeric where numeric, with currency, unit, fiscal period, and as-of date. Return inconclusive rather than guessing.", symbol, asOf), OutputSchema: map[string]any{"type": "object", "required": []string{"company_name", "ticker"}, "properties": properties}}, "finance.enrichment.v1", nil

	case "risk_rubric":
		dimensions := request.Dimensions
		if len(dimensions) == 0 {
			dimensions = []string{"labor_task_exposure", "ai_tailwind_dependence", "content_data_ip_exposure", "regulatory_legal_exposure", "business_model_substitutability"}
		}
		if len(dimensions) > 8 {
			return AgentRequest{}, "", badRequest("finance/research", "A risk rubric may contain at most 8 dimensions.")
		}
		dimensionProperties := map[string]any{}
		for _, raw := range dimensions {
			name := safeField(raw)
			if name == "" {
				return AgentRequest{}, "", badRequest("finance/research", "Risk dimensions may contain only letters, numbers, and underscores.")
			}
			dimensionProperties[name] = map[string]any{"type": "object", "required": []string{"score", "analysis", "inconclusive"}, "properties": map[string]any{"score": map[string]any{"type": "number", "minimum": 1, "maximum": 6}, "analysis": map[string]any{"type": "string"}, "inconclusive": map[string]any{"type": "boolean"}, "sources": sources}}
		}
		version := request.RubricVersion
		if version == "" {
			version = "v1"
		}
		schema := map[string]any{"type": "object", "required": []string{"company", "rubric_version", "dimensions", "executive_summary"}, "properties": map[string]any{"company": map[string]any{"type": "string"}, "ticker": map[string]any{"type": "string"}, "rubric_version": map[string]any{"type": "string"}, "dimensions": map[string]any{"type": "object", "properties": dimensionProperties}, "executive_summary": map[string]any{"type": "string"}}}
		return AgentRequest{Effort: request.Effort, Query: fmt.Sprintf("Assess %s as of %s against risk rubric %s. Score each named dimension from 1 to 6, explain it with evidence, and mark inconclusive whenever evidence is inadequate. This is evidence organization, not investment advice.", symbol, asOf, version), OutputSchema: schema}, "finance.risk_rubric." + safeField(version), nil
	default:
		return AgentRequest{}, "", badRequest("finance/research", "Research kind must be equity_brief, enrichment, or risk_rubric.")
	}
}

func typedFactSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"state", "evidence"}, "properties": map[string]any{"value": map[string]any{"type": []string{"number", "string", "null"}}, "as_of": map[string]any{"type": "string"}, "fiscal_period": map[string]any{"type": "string"}, "unit": map[string]any{"type": "string"}, "currency": map[string]any{"type": "string"}, "state": map[string]any{"type": "string", "enum": []string{"verified", "conflict", "inconclusive"}}, "confidence": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}}, "evidence": map[string]any{"type": "array", "maxItems": 6, "items": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string", "format": "uri"}, "title": map[string]any{"type": "string"}, "excerpt": map[string]any{"type": "string"}}}}}}
}

func safeField(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}
	for _, ch := range value {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' {
			return ""
		}
	}
	return value
}

func emptyAsCurrent(value string) string {
	if strings.TrimSpace(value) == "" {
		return "the current date"
	}
	return strings.TrimSpace(value)
}
