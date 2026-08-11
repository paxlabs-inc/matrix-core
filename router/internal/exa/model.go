// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Cost struct {
	Total float64 `json:"total"`
}

type Citation struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type Grounding struct {
	Field      string     `json:"field"`
	Citations  []Citation `json:"citations"`
	Confidence string     `json:"confidence,omitempty"`
}

type Evidence struct {
	URL         string     `json:"url"`
	Title       string     `json:"title,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	RetrievedAt time.Time  `json:"retrieved_at"`
	Excerpt     string     `json:"excerpt,omitempty"`
	Primary     bool       `json:"primary"`
	ContentHash string     `json:"content_hash"`
}

type ClaimState string

const (
	ClaimVerified     ClaimState = "verified"
	ClaimConflict     ClaimState = "conflict"
	ClaimInconclusive ClaimState = "inconclusive"
)

type GroundedClaim struct {
	Value        json.RawMessage `json:"value"`
	AsOf         string          `json:"as_of,omitempty"`
	FiscalPeriod string          `json:"fiscal_period,omitempty"`
	Unit         string          `json:"unit,omitempty"`
	Currency     string          `json:"currency,omitempty"`
	State        ClaimState      `json:"state"`
	Confidence   string          `json:"confidence,omitempty"`
	Evidence     []Evidence      `json:"evidence"`
}

type ContentOptions struct {
	Text             any      `json:"text,omitempty"`
	Highlights       any      `json:"highlights,omitempty"`
	Summary          any      `json:"summary,omitempty"`
	MaxAgeHours      *int     `json:"maxAgeHours,omitempty"`
	LivecrawlTimeout int      `json:"livecrawlTimeout,omitempty"`
	Subpages         int      `json:"subpages,omitempty"`
	SubpageTarget    []string `json:"subpageTarget,omitempty"`
}

type SearchRequest struct {
	Query              string         `json:"query"`
	Type               string         `json:"type,omitempty"`
	NumResults         int            `json:"numResults,omitempty"`
	Category           string         `json:"category,omitempty"`
	UserLocation       string         `json:"userLocation,omitempty"`
	IncludeDomains     []string       `json:"includeDomains,omitempty"`
	ExcludeDomains     []string       `json:"excludeDomains,omitempty"`
	StartPublishedDate string         `json:"startPublishedDate,omitempty"`
	EndPublishedDate   string         `json:"endPublishedDate,omitempty"`
	Moderation         bool           `json:"moderation,omitempty"`
	AdditionalQueries  []string       `json:"additionalQueries,omitempty"`
	SystemPrompt       string         `json:"systemPrompt,omitempty"`
	OutputSchema       map[string]any `json:"outputSchema,omitempty"`
	Contents           ContentOptions `json:"contents"`
}

type Result struct {
	Title           string    `json:"title"`
	URL             string    `json:"url"`
	ID              string    `json:"id,omitempty"`
	PublishedDate   string    `json:"publishedDate,omitempty"`
	Author          string    `json:"author,omitempty"`
	Image           string    `json:"image,omitempty"`
	Favicon         string    `json:"favicon,omitempty"`
	Text            string    `json:"text,omitempty"`
	Highlights      []string  `json:"highlights,omitempty"`
	HighlightScores []float64 `json:"highlightScores,omitempty"`
	Summary         any       `json:"summary,omitempty"`
}

type Output struct {
	Content    json.RawMessage `json:"content,omitempty"`
	Text       string          `json:"text,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Grounding  []Grounding     `json:"grounding,omitempty"`
}

type SearchResponse struct {
	RequestID  string   `json:"requestId"`
	SearchType string   `json:"searchType"`
	Results    []Result `json:"results"`
	Output     *Output  `json:"output,omitempty"`
	Cost       Cost     `json:"costDollars"`
}

type ContentsRequest struct {
	URLs             []string `json:"urls"`
	Text             any      `json:"text,omitempty"`
	Highlights       any      `json:"highlights,omitempty"`
	Summary          any      `json:"summary,omitempty"`
	MaxAgeHours      *int     `json:"maxAgeHours,omitempty"`
	LivecrawlTimeout int      `json:"livecrawlTimeout,omitempty"`
	Subpages         int      `json:"subpages,omitempty"`
	SubpageTarget    []string `json:"subpageTarget,omitempty"`
}

type ContentError struct {
	Tag            string `json:"tag"`
	HTTPStatusCode *int   `json:"httpStatusCode,omitempty"`
}

type ContentStatus struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Error  *ContentError `json:"error,omitempty"`
}

type ContentsResponse struct {
	RequestID string          `json:"requestId"`
	Results   []Result        `json:"results"`
	Statuses  []ContentStatus `json:"statuses"`
	Cost      Cost            `json:"costDollars"`
}

type AgentRequest struct {
	Query         string           `json:"query"`
	Effort        string           `json:"effort,omitempty"`
	OutputSchema  map[string]any   `json:"outputSchema,omitempty"`
	PreviousRunID string           `json:"previousRunId,omitempty"`
	Input         map[string]any   `json:"input,omitempty"`
	DataSources   []map[string]any `json:"dataSources,omitempty"`
}

type AgentError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type AgentRun struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"createdAt,omitempty"`
	UpdatedAt string          `json:"updatedAt,omitempty"`
	Output    *Output         `json:"output,omitempty"`
	Cost      Cost            `json:"costDollars"`
	Error     *AgentError     `json:"error,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

func (r AgentRun) Terminal() bool {
	switch r.Status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func EvidenceFromGrounding(grounding []Grounding, now time.Time) []Evidence {
	seen := map[string]bool{}
	out := make([]Evidence, 0)
	for _, group := range grounding {
		for _, citation := range group.Citations {
			canonical := canonicalURL(citation.URL)
			if canonical == "" || seen[canonical] {
				continue
			}
			seen[canonical] = true
			hash := sha256.Sum256([]byte(canonical + "\n" + citation.Title))
			out = append(out, Evidence{
				URL: canonical, Title: citation.Title, RetrievedAt: now.UTC(),
				Primary: primarySource(canonical), ContentHash: hex.EncodeToString(hash[:]),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func canonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	q := u.Query()
	for key := range q {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "ref" || lower == "source" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

func primarySource(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return host == "sec.gov" || strings.HasSuffix(host, ".sec.gov") ||
		host == "fred.stlouisfed.org" || strings.HasSuffix(host, ".gov") ||
		strings.Contains(host, "investor") || strings.Contains(host, "ir.")
}
