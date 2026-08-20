// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"encoding/json"
	"net/url"
	"strings"
)

type webSearchEnvelope struct {
	Results []struct {
		URL string `json:"url"`
	} `json:"results"`
}

func (a *Agent) noteWebEvidence(name string, args map[string]interface{}, evidence string, isErr bool) {
	if a == nil || a.turn == nil || isErr {
		return
	}
	base := name
	if i := strings.LastIndex(base, "__"); i >= 0 {
		base = base[i+2:]
	}
	switch base {
	case "web_search", "web_news":
		var envelope webSearchEnvelope
		if json.Unmarshal([]byte(strings.TrimSpace(evidence)), &envelope) != nil {
			return
		}
		if a.turn.webSourceURLs == nil {
			a.turn.webSourceURLs = make(map[string]struct{})
		}
		for _, result := range envelope.Results {
			if normalized := normalizeWebURL(result.URL); normalized != "" {
				a.turn.webSourceURLs[normalized] = struct{}{}
			}
		}
	case "fetch":
		raw, _ := args["url"].(string)
		normalized := normalizeWebURL(raw)
		if normalized == "" {
			return
		}
		if _, selected := a.turn.webSourceURLs[normalized]; !selected {
			return
		}
		if a.turn.webFetchedURLs == nil {
			a.turn.webFetchedURLs = make(map[string]struct{})
		}
		a.turn.webFetchedURLs[normalized] = struct{}{}
	}
}

func (a *Agent) webEvidenceReady() bool {
	return a == nil || a.turn == nil || len(a.turn.webSourceURLs) == 0 || len(a.turn.webFetchedURLs) > 0
}

func normalizeWebURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}
