// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"strings"
	"time"
)

// Range is a chart range as the user picks it. Resolution is decided HERE rather
// than in the client, so the browser and the agent asking for "1D" get the same
// series out of the same cache entry.
type Range string

const (
	Range1D  Range = "1D"
	Range5D  Range = "5D"
	Range1M  Range = "1M"
	Range6M  Range = "6M"
	RangeYTD Range = "YTD"
	Range1Y  Range = "1Y"
	Range5Y  Range = "5Y"
	RangeMax Range = "MAX"
)

// Ranges is the chart's range set, in display order.
var Ranges = []Range{Range1D, Range5D, Range1M, Range6M, RangeYTD, Range1Y, Range5Y, RangeMax}

// ParseRange reads a range label, defaulting to 1D.
func ParseRange(s string) Range {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "5D":
		return Range5D
	case "1M":
		return Range1M
	case "6M":
		return Range6M
	case "YTD":
		return RangeYTD
	case "1Y":
		return Range1Y
	case "5Y":
		return Range5Y
	case "MAX":
		return RangeMax
	default:
		return Range1D
	}
}

// plan is the fetch plan for a range: which resolution to ASK the vendor for,
// how far back, and what to aggregate the result into.
type plan struct {
	fetch     Interval
	aggregate Interval
	lookback  time.Duration
	ytd       bool
	// lastSessionOnly trims an intraday fetch to the most recent trading day —
	// a "1D" chart must show one session, and the vendor answers with several.
	lastSessionOnly bool
}

const day = 24 * time.Hour

func planFor(r Range) plan {
	switch r {
	case Range5D:
		return plan{fetch: Interval30Min, lookback: 9 * day}
	case Range1M:
		return plan{fetch: Interval1Hour, lookback: 34 * day}
	case Range6M:
		return plan{fetch: IntervalDay, lookback: 190 * day}
	case RangeYTD:
		return plan{fetch: IntervalDay, ytd: true}
	case Range1Y:
		return plan{fetch: IntervalDay, lookback: 372 * day}
	case Range5Y:
		return plan{fetch: IntervalDay, aggregate: IntervalWeek, lookback: 5 * 372 * day}
	case RangeMax:
		return plan{fetch: IntervalDay, aggregate: IntervalMonth, lookback: 30 * 372 * day}
	default:
		return plan{fetch: Interval5Min, lookback: 6 * day, lastSessionOnly: true}
	}
}

// SeriesForRange reads the series a range needs: the right resolution, the right
// window, and any aggregation the vendor cannot do itself.
func (s *Service) SeriesForRange(ctx context.Context, user, symbol string, r Range) (*Series, error) {
	p := planFor(r)
	now := s.now().UTC()
	from := now.Add(-p.lookback)
	if p.ytd {
		from = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}

	series, err := s.Series(ctx, user, symbol, p.fetch, from.Format("2006-01-02"), now.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	out := *series
	if p.lastSessionOnly {
		out.Candles = lastSession(out.Candles)
	}
	if p.aggregate != "" {
		out.Candles = aggregate(out.Candles, p.aggregate)
		out.Interval = p.aggregate
	}
	if len(out.Candles) == 0 {
		return nil, &Failure{
			Kind: FailureNotFound, Provider: out.Source.Provider, Endpoint: "series",
			Message: "No price history was returned for that range.",
		}
	}
	return &out, nil
}

// lastSession keeps only the bars belonging to the most recent calendar day
// present in the series. A "1D" chart that silently spans three sessions is a
// wrong chart, not a generous one.
func lastSession(candles []Candle) []Candle {
	if len(candles) == 0 {
		return candles
	}
	lastDay := candles[len(candles)-1].Time.UTC().Format("2006-01-02")
	cut := len(candles)
	for i := len(candles) - 1; i >= 0; i-- {
		if candles[i].Time.UTC().Format("2006-01-02") != lastDay {
			break
		}
		cut = i
	}
	return candles[cut:]
}

// aggregate rolls daily bars up into weekly or monthly ones by real arithmetic:
// open is the bucket's first open, close its last close, high/low the extremes,
// volume the sum. Nothing is interpolated and no bucket is invented for a period
// the vendor had no bars in.
func aggregate(candles []Candle, into Interval) []Candle {
	if len(candles) == 0 {
		return candles
	}
	bucketOf := func(t time.Time) string {
		t = t.UTC()
		if into == IntervalMonth {
			return t.Format("2006-01")
		}
		year, week := t.ISOWeek()
		return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-W" + itoa2(week)
	}

	var out []Candle
	var current *Candle
	currentKey := ""
	for _, c := range candles {
		key := bucketOf(c.Time)
		if current == nil || key != currentKey {
			if current != nil {
				out = append(out, *current)
			}
			bar := c
			current = &bar
			currentKey = key
			continue
		}
		if c.High > current.High {
			current.High = c.High
		}
		if c.Low < current.Low {
			current.Low = c.Low
		}
		current.Close = c.Close
		if c.Volume != nil {
			sum := *c.Volume
			if current.Volume != nil {
				sum += *current.Volume
			}
			current.Volume = &sum
		}
	}
	if current != nil {
		out = append(out, *current)
	}
	return out
}

func itoa2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
