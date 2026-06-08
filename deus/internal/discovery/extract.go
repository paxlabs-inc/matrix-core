package discovery

import (
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// ExtractedQuery is the parsed discovery request after constraint extraction.
type ExtractedQuery struct {
	SemanticQuery string
	Filters       map[string]any
}

var (
	reMaxPAX   = regexp.MustCompile(`(?i)under\s+(\d+(?:\.\d+)?)\s*pax`)
	reMaxWei   = regexp.MustCompile(`(?i)under\s+(\d+)\s*wei`)
	reUptime   = regexp.MustCompile(`(?i)(?:>|at\s+least\s+)(\d+(?:\.\d+)?)\s*%\s*uptime`)
	reKindData = regexp.MustCompile(`(?i)\bdata\s+service\b`)
	reKindAgent = regexp.MustCompile(`(?i)\bagent\s+service\b`)
)

// ExtractConstraints pulls structured filters from plain-language query text.
func ExtractConstraints(query string, explicit map[string]any) ExtractedQuery {
	out := ExtractedQuery{Filters: map[string]any{}}
	for k, v := range explicit {
		out.Filters[k] = v
	}
	q := strings.TrimSpace(query)
	if m := reMaxPAX.FindStringSubmatch(q); len(m) == 2 && out.Filters["max_price_wei"] == nil {
		if wei := paxToWei(m[1]); wei != "" {
			out.Filters["max_price_wei"] = wei
		}
		q = reMaxPAX.ReplaceAllString(q, "")
	}
	if m := reMaxWei.FindStringSubmatch(q); len(m) == 2 && out.Filters["max_price_wei"] == nil {
		out.Filters["max_price_wei"] = m[1]
		q = reMaxWei.ReplaceAllString(q, "")
	}
	if m := reUptime.FindStringSubmatch(q); len(m) == 2 && out.Filters["min_uptime_bps"] == nil {
		if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
			out.Filters["min_uptime_bps"] = int(pct * 100)
		}
		q = reUptime.ReplaceAllString(q, "")
	}
	if out.Filters["kind"] == nil {
		switch {
		case reKindAgent.MatchString(q):
			out.Filters["kind"] = "agent"
			q = reKindAgent.ReplaceAllString(q, "")
		case reKindData.MatchString(q):
			out.Filters["kind"] = "data"
			q = reKindData.ReplaceAllString(q, "")
		}
	}
	out.SemanticQuery = strings.TrimSpace(strings.Join(strings.Fields(q), " "))
	if out.SemanticQuery == "" {
		out.SemanticQuery = strings.TrimSpace(query)
	}
	return out
}

func paxToWei(pax string) string {
	f, err := strconv.ParseFloat(pax, 64)
	if err != nil {
		return ""
	}
	wei := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18))
	i, _ := wei.Int(nil)
	return i.Text(10)
}
