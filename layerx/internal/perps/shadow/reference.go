// Package shadow contains the independently implemented fixed-point reference
// calculator used only for rollout comparison. It intentionally imports none
// of the production pricing, risk, or fixed-point math packages.
package shadow

import (
	"errors"
	"math/big"

	"github.com/paxlabs-inc/layerx/internal/marketdata/crossverse"
	"github.com/paxlabs-inc/layerx/internal/perps/market"
)

const (
	contractNotionalMicro = int64(10_000_000)
	rateScale             = int64(1_000_000_000)
	maxFundingPpb         = int64(750_000)
)

type Position struct {
	Side            string
	Contracts       int64
	EntryPriceCents int64
}

type Result struct {
	ExecutionPriceCents   int64
	MarginMicro           int64
	FeeMicro              int64
	FundingMicro          int64
	LiquidationPriceCents int64
	PnLMicro              int64
}

func div(num, den *big.Int, roundUp bool) (int64, error) {
	if den.Sign() <= 0 {
		return 0, errors.New("shadow reference: non-positive denominator")
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if roundUp && r.Sign() != 0 && num.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("shadow reference: integer overflow")
	}
	return q.Int64(), nil
}

func mulDiv(a, b, den int64, roundUp bool) (int64, error) {
	return div(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(den), roundUp)
}

func checkedMul(a, b int64) (int64, error) {
	v := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !v.IsInt64() {
		return 0, errors.New("shadow reference: integer overflow")
	}
	return v.Int64(), nil
}

func ceilTick(value, tick int64) int64 {
	if value <= 0 {
		return 0
	}
	return ((value + tick - 1) / tick) * tick
}

func floorTick(value, tick int64) int64 {
	if value <= 0 {
		return 0
	}
	return (value / tick) * tick
}

func vwap(levels []crossverse.Level, contracts int64, buy bool) (int64, error) {
	if contracts <= 0 {
		return 0, errors.New("shadow reference: contracts must be positive")
	}
	remaining := contracts
	sum := new(big.Int)
	for _, level := range levels {
		if remaining == 0 {
			break
		}
		take := level.Contracts
		if take > remaining {
			take = remaining
		}
		sum.Add(sum, new(big.Int).Mul(big.NewInt(level.PriceCents), big.NewInt(take)))
		remaining -= take
	}
	if remaining != 0 {
		return 0, errors.New("shadow reference: insufficient book depth")
	}
	return div(sum, big.NewInt(contracts), buy)
}

func clampFunding(value int64) int64 {
	if value > maxFundingPpb {
		return maxFundingPpb
	}
	if value < -maxFundingPpb {
		return -maxFundingPpb
	}
	return value
}

func Calculate(mkt market.Market, snap crossverse.NormalizedSnapshot, orderSide string,
	contracts, limitPriceCents int64, position *Position) (Result, error) {

	buy := orderSide == "BUY"
	levels := snap.Asks
	projectedSign := int64(1)
	if !buy {
		levels = snap.Bids
		projectedSign = -1
	}
	bookPrice, err := vwap(levels, contracts, buy)
	if err != nil {
		return Result{}, err
	}
	notional, err := checkedMul(contracts, contractNotionalMicro)
	if err != nil {
		return Result{}, err
	}
	projected := projectedSign * notional
	if position != nil {
		existingSign := int64(1)
		if position.Side == "SHORT" {
			existingSign = -1
		}
		existing, err := checkedMul(position.Contracts, contractNotionalMicro)
		if err != nil {
			return Result{}, err
		}
		projected += existingSign * existing
	}
	absoluteProjected := projected
	if absoluteProjected < 0 {
		absoluteProjected = -absoluteProjected
	}
	utilization, err := mulDiv(absoluteProjected, 10_000, mkt.MaxProtocolOIMicroUSDX, true)
	if err != nil {
		return Result{}, err
	}
	if utilization > 10_000 {
		utilization = 10_000
	}
	utilSquared := new(big.Int).Mul(big.NewInt(utilization), big.NewInt(utilization))
	skewNum := new(big.Int).Mul(big.NewInt(mkt.MaxSkewImpactBps), utilSquared)
	skew, err := div(skewNum, big.NewInt(100_000_000), true)
	if err != nil {
		return Result{}, err
	}
	impact := mkt.BaseSpreadBps + skew
	var execution int64
	if buy {
		raw, err := mulDiv(bookPrice, 10_000+impact, 10_000, true)
		if err != nil {
			return Result{}, err
		}
		execution = ceilTick(raw, mkt.TickPriceUnits)
		if limitPriceCents > 0 && execution > limitPriceCents {
			return Result{}, errors.New("shadow reference: execution exceeds buy limit")
		}
	} else {
		raw, err := mulDiv(bookPrice, 10_000-impact, 10_000, false)
		if err != nil {
			return Result{}, err
		}
		execution = floorTick(raw, mkt.TickPriceUnits)
		if limitPriceCents > 0 && execution < limitPriceCents {
			return Result{}, errors.New("shadow reference: execution below sell limit")
		}
	}
	margin, err := mulDiv(notional, mkt.InitialMarginBps, 10_000, true)
	if err != nil {
		return Result{}, err
	}
	fee, err := mulDiv(notional, mkt.TakerFeeBps, 10_000, true)
	if err != nil {
		return Result{}, err
	}
	maintenance, err := mulDiv(notional, mkt.MaintenanceMarginBps, 10_000, true)
	if err != nil {
		return Result{}, err
	}
	liqFee, err := mulDiv(notional, mkt.LiquidationFeeBps, 10_000, true)
	if err != nil {
		return Result{}, err
	}
	delta := maintenance + liqFee - margin
	var liquidation int64
	if buy {
		raw, err := mulDiv(execution, notional+delta, notional, true)
		if err != nil {
			return Result{}, err
		}
		liquidation = ceilTick(raw, mkt.TickPriceUnits)
	} else {
		raw, err := mulDiv(execution, notional-delta, notional, false)
		if err != nil {
			return Result{}, err
		}
		liquidation = floorTick(raw, mkt.TickPriceUnits)
	}
	fundingNum := new(big.Int).Mul(big.NewInt(notional), big.NewInt(clampFunding(snap.EstimatedFundingPpb)))
	funding, err := div(fundingNum, big.NewInt(rateScale), false)
	if err != nil {
		return Result{}, err
	}
	var pnl int64
	if position != nil {
		side := int64(1)
		if position.Side == "SHORT" {
			side = -1
		}
		positionNotional, err := checkedMul(position.Contracts, contractNotionalMicro)
		if err != nil {
			return Result{}, err
		}
		num := new(big.Int).Mul(big.NewInt(side*positionNotional),
			big.NewInt(snap.MarkPriceCents-position.EntryPriceCents))
		pnl, err = div(num, big.NewInt(position.EntryPriceCents), false)
		if err != nil {
			return Result{}, err
		}
	}
	return Result{
		ExecutionPriceCents: execution, MarginMicro: margin, FeeMicro: fee,
		FundingMicro: funding, LiquidationPriceCents: liquidation, PnLMicro: pnl,
	}, nil
}
