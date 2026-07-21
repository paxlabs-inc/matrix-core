package market

import (
	"fmt"
	"sort"
	"strings"
)

const (
	USDXScale                 = 1_000_000
	PriceScale                = 100
	RateScale                 = 1_000_000_000
	QuantityScale             = 1
	ContractNotionalMicroUSDX = 10_000_000
	FundingIntervalSeconds    = 28_800
	BookFreshMs               = 2_000
	StatsFreshMs              = 45_000
	AggregateFreshMs          = 10_000
	QuoteTTLMs                = 1_000
	MaxSkewFundingPpb         = 250_000
	MaxFundingPpb             = 750_000
)

type Session string

const (
	Session24x7 Session = "24x7"
)

type Market struct {
	Symbol                 string
	Class                  string
	TickPriceUnits         int64
	MinOrderContracts      int64
	MinPositionContracts   int64
	InitialMarginBps       int64
	MaintenanceMarginBps   int64
	MaxLeverageX           int64
	MaxPositionMicroUSDX   int64
	MaxProtocolOIMicroUSDX int64
	MakerFeeBps            int64
	TakerFeeBps            int64
	LiquidationFeeBps      int64
	BaseSpreadBps          int64
	MaxSkewImpactBps       int64
	DivergenceLimitBps     int64
	StressLossBps          int64
	Session                Session
}

var markets = []Market{
	{Symbol: "BTC", Class: "crypto_major", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 2000, MaintenanceMarginBps: 1250, MaxLeverageX: 5, MaxPositionMicroUSDX: 25_000_000_000, MaxProtocolOIMicroUSDX: 100_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 50, BaseSpreadBps: 1, MaxSkewImpactBps: 20, DivergenceLimitBps: 75, StressLossBps: 2000, Session: Session24x7},
	{Symbol: "ETH", Class: "crypto_major", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 2000, MaintenanceMarginBps: 1250, MaxLeverageX: 5, MaxPositionMicroUSDX: 25_000_000_000, MaxProtocolOIMicroUSDX: 100_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 50, BaseSpreadBps: 1, MaxSkewImpactBps: 20, DivergenceLimitBps: 75, StressLossBps: 2000, Session: Session24x7},
	{Symbol: "SOL", Class: "crypto_liquid", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 3334, MaintenanceMarginBps: 2000, MaxLeverageX: 3, MaxPositionMicroUSDX: 10_000_000_000, MaxProtocolOIMicroUSDX: 40_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 75, BaseSpreadBps: 2, MaxSkewImpactBps: 30, DivergenceLimitBps: 100, StressLossBps: 3000, Session: Session24x7},
	{Symbol: "AVAX", Class: "crypto_liquid", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 3334, MaintenanceMarginBps: 2000, MaxLeverageX: 3, MaxPositionMicroUSDX: 10_000_000_000, MaxProtocolOIMicroUSDX: 40_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 75, BaseSpreadBps: 2, MaxSkewImpactBps: 30, DivergenceLimitBps: 100, StressLossBps: 3000, Session: Session24x7},
	{Symbol: "LINK", Class: "crypto_liquid", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 3334, MaintenanceMarginBps: 2000, MaxLeverageX: 3, MaxPositionMicroUSDX: 10_000_000_000, MaxProtocolOIMicroUSDX: 40_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 75, BaseSpreadBps: 2, MaxSkewImpactBps: 30, DivergenceLimitBps: 100, StressLossBps: 3000, Session: Session24x7},
	{Symbol: "TSLA", Class: "us_equity", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "NVDA", Class: "us_equity", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "NAS100", Class: "us_index", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "XAU", Class: "gold", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "SPX500", Class: "us_index", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "GOOGL", Class: "us_equity", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 75, StressLossBps: 4000, Session: Session24x7},
	{Symbol: "PAX", Class: "crypto_volatile", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 150, StressLossBps: 5000, Session: Session24x7},
	{Symbol: "SID", Class: "crypto_volatile", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 150, StressLossBps: 5000, Session: Session24x7},
	{Symbol: "HYPE", Class: "crypto_volatile", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 150, StressLossBps: 5000, Session: Session24x7},
	{Symbol: "XRP", Class: "crypto_volatile", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 5000, MaintenanceMarginBps: 3000, MaxLeverageX: 2, MaxPositionMicroUSDX: 5_000_000_000, MaxProtocolOIMicroUSDX: 20_000_000_000, MakerFeeBps: 3, TakerFeeBps: 7, LiquidationFeeBps: 100, BaseSpreadBps: 3, MaxSkewImpactBps: 40, DivergenceLimitBps: 150, StressLossBps: 5000, Session: Session24x7},
	{Symbol: "ASTER", Class: "crypto_restricted", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 10000, MaintenanceMarginBps: 6000, MaxLeverageX: 1, MaxPositionMicroUSDX: 2_500_000_000, MaxProtocolOIMicroUSDX: 10_000_000_000, MakerFeeBps: 5, TakerFeeBps: 10, LiquidationFeeBps: 100, BaseSpreadBps: 5, MaxSkewImpactBps: 60, DivergenceLimitBps: 200, StressLossBps: 7500, Session: Session24x7},
	{Symbol: "TRUMP", Class: "crypto_restricted", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 10000, MaintenanceMarginBps: 6000, MaxLeverageX: 1, MaxPositionMicroUSDX: 2_500_000_000, MaxProtocolOIMicroUSDX: 10_000_000_000, MakerFeeBps: 5, TakerFeeBps: 10, LiquidationFeeBps: 100, BaseSpreadBps: 5, MaxSkewImpactBps: 60, DivergenceLimitBps: 200, StressLossBps: 7500, Session: Session24x7},
	{Symbol: "BNB", Class: "crypto_liquid", TickPriceUnits: 1, MinOrderContracts: 1, MinPositionContracts: 1, InitialMarginBps: 3334, MaintenanceMarginBps: 2000, MaxLeverageX: 3, MaxPositionMicroUSDX: 10_000_000_000, MaxProtocolOIMicroUSDX: 40_000_000_000, MakerFeeBps: 2, TakerFeeBps: 5, LiquidationFeeBps: 75, BaseSpreadBps: 2, MaxSkewImpactBps: 30, DivergenceLimitBps: 100, StressLossBps: 3000, Session: Session24x7},
}

var bySymbol = func() map[string]Market {
	out := make(map[string]Market, len(markets))
	for _, m := range markets {
		out[m.Symbol] = m
	}
	return out
}()

func Lookup(symbol string) (Market, error) {
	m, ok := bySymbol[strings.ToUpper(strings.TrimSpace(symbol))]
	if !ok {
		return Market{}, fmt.Errorf("perps market %q is unknown", symbol)
	}
	return m, nil
}

func All() []Market {
	out := make([]Market, len(markets))
	copy(out, markets)
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func Symbols() []string {
	out := make([]string, 0, len(markets))
	for _, m := range markets {
		out = append(out, m.Symbol)
	}
	sort.Strings(out)
	return out
}

func Count() int {
	return len(markets)
}
