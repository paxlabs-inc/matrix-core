package crossverse

import "math/big"

type Health string

const (
	HealthConnecting       Health = "CONNECTING"
	HealthAwaitingSnapshot Health = "AWAITING_SNAPSHOT"
	HealthHealthy          Health = "HEALTHY"
	HealthStaleGap         Health = "STALE_GAP"
	HealthStaleTime        Health = "STALE_TIME"
	HealthStaleDivergence  Health = "STALE_DIVERGENCE"
	HealthStopped          Health = "STOPPED"
)

const (
	bookFreshMs      = 2_000
	statsFreshMs     = 45_000
	aggregateFreshMs = 10_000
)

func (f *feed) healthLocked(nowMs int64) Health {
	if f.phase != HealthHealthy {
		return f.phase
	}
	if f.bookReceivedTsMs == 0 || nowMs-f.bookReceivedTsMs > bookFreshMs {
		return HealthStaleTime
	}
	if f.statsReceivedTsMs == 0 || nowMs-f.statsReceivedTsMs > statsFreshMs {
		return HealthStaleTime
	}
	if divergenceExceeded(f.markPriceCents, f.indexPriceCents, f.divergenceLimitBps) {
		return HealthStaleDivergence
	}
	return HealthHealthy
}

func divergenceExceeded(markCents, indexCents, limitBps int64) bool {
	if markCents <= 0 || indexCents <= 0 {
		return true
	}
	diff := new(big.Int).Sub(big.NewInt(markCents), big.NewInt(indexCents))
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(10_000))
	bound := new(big.Int).Mul(big.NewInt(indexCents), big.NewInt(limitBps))
	return diff.Cmp(bound) > 0
}
