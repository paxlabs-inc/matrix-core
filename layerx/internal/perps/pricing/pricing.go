package pricing

import (
	"errors"
	"math/big"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	pmath "github.com/paxlabs-inc/layerx/internal/perps/math"
)

// ErrInsufficientBookDepth is returned when the book cannot fill the requested
// contracts; execution never extrapolates beyond real depth.
var ErrInsufficientBookDepth = errors.New("pricing: insufficient book depth")

type Side int

const (
	Buy Side = iota
	Sell
)

// BookVWAP walks the L2 book (asks ascending for a buy, bids descending for a
// sell — the caller passes the matching slice) consuming whole contracts, and
// returns the volume-weighted average price rounded against the taker: a buy
// rounds up, a sell rounds down.
func BookVWAP(levels []crossverse.Level, contracts int64, side Side) (int64, error) {
	if contracts <= 0 {
		return 0, errors.New("pricing: contracts must be positive")
	}
	remaining := contracts
	sum := new(big.Int)
	for _, lv := range levels {
		if remaining == 0 {
			break
		}
		take := lv.Contracts
		if take > remaining {
			take = remaining
		}
		px, err := pmath.CheckedMul(lv.PriceCents, take)
		if err != nil {
			return 0, err
		}
		sum.Add(sum, big.NewInt(px))
		remaining -= take
	}
	if remaining > 0 {
		return 0, ErrInsufficientBookDepth
	}
	rounding := pmath.Ceil
	if side == Sell {
		rounding = pmath.Floor
	}
	return pmath.DivBig(sum, big.NewInt(contracts), rounding)
}

// UtilizationBps is min(10_000, ceil(abs(projected_net_notional)*10_000 /
// effective_oi_cap)).
func UtilizationBps(projectedNetNotionalMicro, effectiveOICapMicro int64) (int64, error) {
	if effectiveOICapMicro <= 0 {
		return 0, errors.New("pricing: effective oi cap must be positive")
	}
	absNotional, err := pmath.Abs(projectedNetNotionalMicro)
	if err != nil {
		return 0, err
	}
	u, err := pmath.MulDiv(absNotional, 10_000, effectiveOICapMicro, pmath.Ceil)
	if err != nil {
		return 0, err
	}
	if u > 10_000 {
		u = 10_000
	}
	return u, nil
}

// SkewImpactBps is ceil(max_skew_impact_bps * u_bps^2 / 100_000_000).
func SkewImpactBps(maxSkewImpactBps, utilizationBps int64) (int64, error) {
	uSquared, err := pmath.CheckedMul(utilizationBps, utilizationBps)
	if err != nil {
		return 0, err
	}
	return pmath.MulDiv(maxSkewImpactBps, uSquared, 100_000_000, pmath.Ceil)
}

// TotalImpactBps is the protocol base spread plus the quadratic skew impact.
func TotalImpactBps(baseSpreadBps, skewImpactBps int64) (int64, error) {
	return pmath.CheckedAdd(baseSpreadBps, skewImpactBps)
}

// ExecutionPriceCents applies the total impact against the taker and rounds to
// a tick: a buy pays ceil_to_tick(vwap*(10_000+impact)/10_000), a sell receives
// floor_to_tick(vwap*(10_000-impact)/10_000). Impact always worsens the
// candidate user's price.
func ExecutionPriceCents(vwapCents, totalImpactBps, tickCents int64, side Side) (int64, error) {
	if side == Buy {
		raw, err := pmath.MulDiv(vwapCents, 10_000+totalImpactBps, 10_000, pmath.Ceil)
		if err != nil {
			return 0, err
		}
		return pmath.CeilToTick(raw, tickCents)
	}
	raw, err := pmath.MulDiv(vwapCents, 10_000-totalImpactBps, 10_000, pmath.Floor)
	if err != nil {
		return 0, err
	}
	return pmath.FloorToTick(raw, tickCents)
}
