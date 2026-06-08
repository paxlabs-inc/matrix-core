package discovery

import (
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/paxlabs-inc/deus/internal/store"
)

// RankingWeights configures blended discovery scoring (docs/07-discovery.md §7.4).
type RankingWeights struct {
	Semantic  float64 `yaml:"semantic"`
	Quality   float64 `yaml:"quality"`
	Uptime    float64 `yaml:"uptime"`
	Price     float64 `yaml:"price"`
	Freshness float64 `yaml:"freshness"`
}

// DefaultRankingWeights returns documented defaults.
func DefaultRankingWeights() RankingWeights {
	return RankingWeights{
		Semantic: 0.40, Quality: 0.30, Uptime: 0.15, Price: 0.10, Freshness: 0.05,
	}
}

// LoadRankingWeights reads configs/ranking.yaml when present.
func LoadRankingWeights(path string) RankingWeights {
	w := DefaultRankingWeights()
	if path == "" {
		return w
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return w
	}
	_ = yaml.Unmarshal(raw, &w)
	if w.Semantic+w.Quality+w.Uptime+w.Price+w.Freshness == 0 {
		return DefaultRankingWeights()
	}
	return w
}

// BlendScoreWithPrice includes resolved min price wei for the service.
func BlendScoreWithPrice(w RankingWeights, c store.DiscoverCandidate, maxPriceWei, minPriceWei string) float64 {
	sem := clamp01(c.SemanticSim)
	if c.LexicalRank > 0 {
		lex := clamp01(c.LexicalRank)
		if sem < lex {
			sem = lex
		}
	}
	qual := qualityNorm(c.QualityScore)
	up := uptimeNorm(c.UptimeBPS)
	price := priceAffinityWei(maxPriceWei, minPriceWei)
	return w.Semantic*sem + w.Quality*qual + w.Uptime*up + w.Price*price + w.Freshness*0.5
}

func qualityNorm(qs *string) float64 {
	if qs == nil || *qs == "" {
		return 0.5
	}
	v, ok := new(big.Float).SetString(*qs)
	if !ok {
		return 0.5
	}
	f, _ := v.Float64()
	if f > 1 {
		f /= 1e18
	}
	return clamp01(f)
}

func uptimeNorm(bps *int) float64 {
	if bps == nil {
		return 0.5
	}
	return clamp01(float64(*bps) / 10000.0)
}

func priceAffinityWei(maxPriceWei, minPriceWei string) float64 {
	if maxPriceWei == "" || minPriceWei == "" {
		return 0.5
	}
	maxP, ok1 := new(big.Int).SetString(maxPriceWei, 10)
	minP, ok2 := new(big.Int).SetString(minPriceWei, 10)
	if !ok1 || !ok2 {
		return 0.5
	}
	if minP.Cmp(maxP) <= 0 {
		return 1.0
	}
	diff := new(big.Int).Sub(minP, maxP)
	ratio := new(big.Float).Quo(new(big.Float).SetInt(diff), new(big.Float).SetInt(maxP))
	r, _ := ratio.Float64()
	return clamp01(1.0 - math.Min(r, 1.0))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func filterString(filters map[string]any, key string) string {
	v, ok := filters[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return strings.TrimSpace(strconv.FormatInt(int64(toIntAny(v)), 10))
	}
}

func toIntAny(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}
