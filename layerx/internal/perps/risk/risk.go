package risk

import (
	"errors"
	"math/big"

	"github.com/paxlabs-inc/layerx/internal/perps/market"
	pmath "github.com/paxlabs-inc/layerx/internal/perps/math"
)

const (
	SideLong  = int64(1)
	SideShort = int64(-1)
)

// Notional is abs(contracts) * the locked 10.000000 USDX per-contract notional.
func Notional(contracts int64) (int64, error) {
	absContracts, err := pmath.Abs(contracts)
	if err != nil {
		return 0, err
	}
	return pmath.CheckedMul(absContracts, market.ContractNotionalMicroUSDX)
}

// InitialMargin rounds the user-required margin up.
func InitialMargin(notionalMicro, initialMarginBps int64) (int64, error) {
	return pmath.MulDiv(notionalMicro, initialMarginBps, 10_000, pmath.Ceil)
}

// MaintenanceMargin rounds up.
func MaintenanceMargin(notionalMicro, maintenanceMarginBps int64) (int64, error) {
	return pmath.MulDiv(notionalMicro, maintenanceMarginBps, 10_000, pmath.Ceil)
}

// Fee rounds the protocol fee up.
func Fee(fillNotionalMicro, feeBps int64) (int64, error) {
	return pmath.MulDiv(fillNotionalMicro, feeBps, 10_000, pmath.Ceil)
}

// UnrealizedPnL is trunc(side_sign * notional * (mark-entry) / entry).
func UnrealizedPnL(sideSign, notionalMicro, markPriceCents, entryPriceCents int64) (int64, error) {
	if entryPriceCents <= 0 {
		return 0, errors.New("risk: entry price must be positive")
	}
	if sideSign != SideLong && sideSign != SideShort {
		return 0, errors.New("risk: side sign must be +1 or -1")
	}
	diff, err := pmath.CheckedSub(markPriceCents, entryPriceCents)
	if err != nil {
		return 0, err
	}
	num := new(big.Int).Mul(big.NewInt(sideSign*notionalMicro), big.NewInt(diff))
	return pmath.DivBig(num, big.NewInt(entryPriceCents), pmath.Trunc)
}

// Equity is margin + unrealized PnL - unsettled funding debit + credit.
func Equity(marginMicro, unrealizedPnLMicro, fundingDebitMicro, fundingCreditMicro int64) (int64, error) {
	v, err := pmath.CheckedAdd(marginMicro, unrealizedPnLMicro)
	if err != nil {
		return 0, err
	}
	v, err = pmath.CheckedSub(v, fundingDebitMicro)
	if err != nil {
		return 0, err
	}
	return pmath.CheckedAdd(v, fundingCreditMicro)
}

// AvailableWithdrawal rounds the user-receivable amount down to what is
// provably free: max(0, spendable - open order initial margin -
// max(0, position initial margin - position equity without spendable)).
func AvailableWithdrawal(spendableMicro, openOrderInitialMarginMicro, positionInitialMarginMicro, positionEquityMicro int64) (int64, error) {
	shortfall, err := pmath.CheckedSub(positionInitialMarginMicro, positionEquityMicro)
	if err != nil {
		return 0, err
	}
	if shortfall < 0 {
		shortfall = 0
	}
	v, err := pmath.CheckedSub(spendableMicro, openOrderInitialMarginMicro)
	if err != nil {
		return 0, err
	}
	v, err = pmath.CheckedSub(v, shortfall)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		v = 0
	}
	return v, nil
}

// LiquidationFee rounds up.
func LiquidationFee(notionalMicro, liquidationFeeBps int64) (int64, error) {
	return pmath.MulDiv(notionalMicro, liquidationFeeBps, 10_000, pmath.Ceil)
}

// LiquidationEligible compares equity at the current mark against maintenance
// margin plus the estimated liquidation fee. Runtime eligibility always uses
// the live Crossverse mark, never a displayed trigger price.
func LiquidationEligible(equityMicro, maintenanceMarginMicro, estimatedLiquidationFeeMicro int64) (bool, error) {
	bound, err := pmath.CheckedAdd(maintenanceMarginMicro, estimatedLiquidationFeeMicro)
	if err != nil {
		return false, err
	}
	return equityMicro <= bound, nil
}

// LiquidationPriceCents is the displayed trigger-price candidate. Long rounds
// up to a tick (earlier liquidation); short rounds down.
func LiquidationPriceCents(sideSign, entryPriceCents, notionalMicro, maintenanceMicro, feeMicro,
	fundingDebitMicro, fundingCreditMicro, marginMicro, tickCents int64) (int64, error) {

	if notionalMicro <= 0 {
		return 0, errors.New("risk: notional must be positive")
	}
	delta, err := pmath.CheckedAdd(maintenanceMicro, feeMicro)
	if err != nil {
		return 0, err
	}
	delta, err = pmath.CheckedAdd(delta, fundingDebitMicro)
	if err != nil {
		return 0, err
	}
	delta, err = pmath.CheckedSub(delta, fundingCreditMicro)
	if err != nil {
		return 0, err
	}
	delta, err = pmath.CheckedSub(delta, marginMicro)
	if err != nil {
		return 0, err
	}
	switch sideSign {
	case SideLong:
		scaled, err := pmath.CheckedAdd(notionalMicro, delta)
		if err != nil {
			return 0, err
		}
		raw, err := pmath.MulDiv(entryPriceCents, scaled, notionalMicro, pmath.Ceil)
		if err != nil {
			return 0, err
		}
		if raw < 0 {
			return 0, nil
		}
		return pmath.CeilToTick(raw, tickCents)
	case SideShort:
		scaled, err := pmath.CheckedSub(notionalMicro, delta)
		if err != nil {
			return 0, err
		}
		raw, err := pmath.MulDiv(entryPriceCents, scaled, notionalMicro, pmath.Floor)
		if err != nil {
			return 0, err
		}
		if raw < 0 {
			return 0, nil
		}
		return pmath.FloorToTick(raw, tickCents)
	default:
		return 0, errors.New("risk: side sign must be +1 or -1")
	}
}

// SkewFundingPpb is trunc(net_notional * max_skew_funding_ppb /
// effective_oi_cap).
func SkewFundingPpb(netNotionalMicro, effectiveOICapMicro int64) (int64, error) {
	if effectiveOICapMicro <= 0 {
		return 0, errors.New("risk: effective oi cap must be positive")
	}
	return pmath.MulDiv(netNotionalMicro, market.MaxSkewFundingPpb, effectiveOICapMicro, pmath.Trunc)
}

// AppliedFundingPpb clamps external + skew funding to the locked market bound.
func AppliedFundingPpb(externalPpb, skewPpb int64) (int64, error) {
	v, err := pmath.CheckedAdd(externalPpb, skewPpb)
	if err != nil {
		return 0, err
	}
	return pmath.Clamp(v, -market.MaxFundingPpb, market.MaxFundingPpb), nil
}

// FundingTransfer is trunc(position_notional * applied_ppb * elapsed_ms /
// (1e9 * funding_interval_ms)) — one signed conserved transfer per interval.
func FundingTransfer(positionNotionalMicro, appliedPpb, elapsedMs, fundingIntervalMs int64) (int64, error) {
	if fundingIntervalMs <= 0 {
		return 0, errors.New("risk: funding interval must be positive")
	}
	if elapsedMs < 0 || elapsedMs > fundingIntervalMs {
		return 0, errors.New("risk: funding elapsed ms out of interval")
	}
	num := new(big.Int).Mul(big.NewInt(positionNotionalMicro), big.NewInt(appliedPpb))
	num.Mul(num, big.NewInt(elapsedMs))
	den := new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(fundingIntervalMs))
	return pmath.DivBig(num, den, pmath.Trunc)
}

// Capacity is the pool-capacity activation gate over funded capital.
type Capacity struct {
	UsableCapitalMicro   int64
	RequiredCapitalMicro int64
	CapacityOIMicro      int64
	EffectiveOICapMicro  int64
	ActivationAllowed    bool
}

// ComputeCapacity implements the locked capacity formulas: activation requires
// usable >= required and never silently lowers the advertised cap — the
// effective cap is min(configured, capacity).
func ComputeCapacity(liquidityMicro, insuranceMicro, insuranceFloorMicro, committedProfitReserveMicro,
	pendingWithdrawalsMicro, configuredMaxOIMicro, stressLossBps, liquidationFeeBps int64) (Capacity, error) {

	usable, err := pmath.CheckedAdd(liquidityMicro, insuranceMicro)
	if err != nil {
		return Capacity{}, err
	}
	for _, sub := range []int64{insuranceFloorMicro, committedProfitReserveMicro, pendingWithdrawalsMicro} {
		usable, err = pmath.CheckedSub(usable, sub)
		if err != nil {
			return Capacity{}, err
		}
	}
	stress, err := pmath.MulDiv(configuredMaxOIMicro, stressLossBps, 10_000, pmath.Ceil)
	if err != nil {
		return Capacity{}, err
	}
	liqFee, err := pmath.MulDiv(configuredMaxOIMicro, liquidationFeeBps, 10_000, pmath.Ceil)
	if err != nil {
		return Capacity{}, err
	}
	required, err := pmath.CheckedAdd(stress, liqFee)
	if err != nil {
		return Capacity{}, err
	}
	bpsSum, err := pmath.CheckedAdd(stressLossBps, liquidationFeeBps)
	if err != nil {
		return Capacity{}, err
	}
	capacityOI := int64(0)
	if usable > 0 {
		capacityOI, err = pmath.MulDiv(usable, 10_000, bpsSum, pmath.Floor)
		if err != nil {
			return Capacity{}, err
		}
	}
	effective := configuredMaxOIMicro
	if capacityOI < effective {
		effective = capacityOI
	}
	return Capacity{
		UsableCapitalMicro:   usable,
		RequiredCapitalMicro: required,
		CapacityOIMicro:      capacityOI,
		EffectiveOICapMicro:  effective,
		ActivationAllowed:    usable >= required,
	}, nil
}
