package mode

import (
	"fmt"
	"sort"
	"strings"
)

type Mode string

const (
	Off        Mode = "OFF"
	Shadow     Mode = "SHADOW"
	Canary     Mode = "CANARY"
	Active     Mode = "ACTIVE"
	ReduceOnly Mode = "REDUCE_ONLY"
	Paused     Mode = "PAUSED"
)

var validModes = map[Mode]struct{}{
	Off: {}, Shadow: {}, Canary: {}, Active: {}, ReduceOnly: {}, Paused: {},
}

var marketSymbols = map[string]struct{}{
	"BTC": {}, "ETH": {}, "SOL": {}, "AVAX": {}, "LINK": {}, "TSLA": {},
	"NVDA": {}, "NAS100": {}, "XAU": {}, "SPX500": {}, "GOOGL": {},
	"PAX": {}, "SID": {}, "HYPE": {}, "XRP": {}, "ASTER": {}, "TRUMP": {},
	"BNB": {},
}

type Permissions struct {
	PublicReads     bool
	ShadowCalculate bool
	Increase        bool
	Reduce          bool
	Cancel          bool
	Withdraw        bool
}

func Parse(raw string) (Mode, error) {
	m := Mode(strings.ToUpper(strings.TrimSpace(raw)))
	if _, ok := validModes[m]; !ok {
		return "", fmt.Errorf("perps mode %q is invalid", raw)
	}
	return m, nil
}

func ParseMarketModes(raw string) (map[string]Mode, error) {
	out := make(map[string]Mode)
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, item := range strings.Split(raw, ",") {
		symbol, value, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("perps market mode %q must be SYMBOL=MODE", strings.TrimSpace(item))
		}
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if _, ok := marketSymbols[symbol]; !ok {
			return nil, fmt.Errorf("perps market mode symbol %q is unknown", symbol)
		}
		if _, exists := out[symbol]; exists {
			return nil, fmt.Errorf("perps market mode symbol %q is duplicated", symbol)
		}
		m, err := Parse(value)
		if err != nil {
			return nil, fmt.Errorf("perps market %s: %w", symbol, err)
		}
		out[symbol] = m
	}
	return out, nil
}

func (m Mode) Permissions() Permissions {
	switch m {
	case Shadow:
		return Permissions{PublicReads: true, ShadowCalculate: true, Withdraw: true}
	case Canary, Active:
		return Permissions{
			PublicReads: true, ShadowCalculate: true, Increase: true,
			Reduce: true, Cancel: true, Withdraw: true,
		}
	case ReduceOnly:
		return Permissions{
			PublicReads: true, ShadowCalculate: true, Reduce: true,
			Cancel: true, Withdraw: true,
		}
	case Paused:
		return Permissions{PublicReads: true, Cancel: true, Withdraw: true}
	default:
		return Permissions{Withdraw: true}
	}
}

func MoreRestrictive(a, b Mode) Mode {
	if restrictiveness(a) <= restrictiveness(b) {
		return a
	}
	return b
}

func restrictiveness(m Mode) int {
	switch m {
	case Off:
		return 0
	case Paused:
		return 1
	case Shadow:
		return 2
	case ReduceOnly:
		return 3
	case Canary:
		return 4
	case Active:
		return 5
	default:
		return 0
	}
}

type Registry struct {
	global  Mode
	markets map[string]Mode
}

func NewRegistry(global Mode, markets map[string]Mode) (*Registry, error) {
	if _, ok := validModes[global]; !ok {
		return nil, fmt.Errorf("perps global mode %q is invalid", global)
	}
	copied := make(map[string]Mode, len(markets))
	for symbol, m := range markets {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if _, ok := marketSymbols[symbol]; !ok {
			return nil, fmt.Errorf("perps market mode symbol %q is unknown", symbol)
		}
		if _, ok := validModes[m]; !ok {
			return nil, fmt.Errorf("perps market %s mode %q is invalid", symbol, m)
		}
		copied[symbol] = m
	}
	return &Registry{global: global, markets: copied}, nil
}

func (r *Registry) Global() Mode {
	return r.global
}

func (r *Registry) Market(symbol string) Mode {
	if m, ok := r.markets[strings.ToUpper(strings.TrimSpace(symbol))]; ok {
		return m
	}
	return Off
}

func (r *Registry) Effective(symbol string) Mode {
	market := Off
	if configured, ok := r.markets[strings.ToUpper(strings.TrimSpace(symbol))]; ok {
		market = configured
	}
	return MoreRestrictive(r.global, market)
}

func Symbols() []string {
	out := make([]string, 0, len(marketSymbols))
	for symbol := range marketSymbols {
		out = append(out, symbol)
	}
	sort.Strings(out)
	return out
}
