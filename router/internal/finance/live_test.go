// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"os"
	"testing"
	"time"
)

// These probes run against the REAL vendor APIs. They are gated on the real keys
// and SKIP without them — never faked green. They are the check that the shapes
// this package parses are the shapes the live services actually send, which the
// published documentation alone cannot guarantee (Alpha Vantage's docs publish
// parameters but no response bodies).
//
// Run with:
//
//	FMP_API_KEY=… ALPHAVANTAGE_API_KEY=… go test ./internal/finance/ -run Live -v

func liveFMP(t *testing.T) *FMP {
	t.Helper()
	key := os.Getenv(FMPKeyEnv)
	if key == "" {
		t.Skipf("live probe skipped: %s is not set", FMPKeyEnv)
	}
	return NewFMP(key, "", nil)
}

func liveAlpha(t *testing.T) *AlphaVantage {
	t.Helper()
	key := os.Getenv(AlphaVantageKeyEnv)
	if key == "" {
		t.Skipf("live probe skipped: %s is not set", AlphaVantageKeyEnv)
	}
	return NewAlphaVantage(key, "", nil)
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveFMPQuoteShape(t *testing.T) {
	c := liveFMP(t)
	q, err := c.Quote(liveCtx(t), "AAPL")
	if err != nil {
		t.Fatalf("live quote: %v", err)
	}
	if q.Price == nil || *q.Price <= 0 {
		t.Fatalf("live quote has no usable price: %+v", q)
	}
	if q.Name == "" || q.Exchange == "" {
		t.Fatalf("live quote is missing identity: %+v", q)
	}
	if q.AsOf == nil {
		t.Fatal("live quote carries no vendor timestamp")
	}
	t.Logf("AAPL %.2f (%v) as of %s via %s", *q.Price, q.ChangePercent, q.AsOf, q.Source.Provider)
}

func TestLiveFMPIndexAndCryptoAndForexShareTheQuoteShape(t *testing.T) {
	c := liveFMP(t)
	ctx := liveCtx(t)
	for symbol, want := range map[string]AssetClass{
		"^GSPC":  ClassIndex,
		"BTCUSD": ClassCrypto,
		"EURUSD": ClassForex,
		"GCUSD":  ClassCommodity,
	} {
		q, err := c.Quote(ctx, symbol)
		if err != nil {
			t.Errorf("live quote %s: %v", symbol, err)
			continue
		}
		if q.Class != want {
			t.Errorf("%s class = %q, want %q (exchange %q)", symbol, q.Class, want, q.Exchange)
		}
		if q.Price == nil {
			t.Errorf("%s has no price", symbol)
		}
	}
}

func TestLiveFMPIntradaySeriesIsAscendingAndRecent(t *testing.T) {
	c := liveFMP(t)
	s, err := c.Series(liveCtx(t), "AAPL", Interval5Min, "", "")
	if err != nil {
		t.Fatalf("live series: %v", err)
	}
	if len(s.Candles) < 2 {
		t.Fatalf("live series too short: %d candles", len(s.Candles))
	}
	for i := 1; i < len(s.Candles); i++ {
		if !s.Candles[i].Time.After(s.Candles[i-1].Time) {
			t.Fatalf("live series not ascending at %d", i)
		}
	}
	last := s.Candles[len(s.Candles)-1].Time
	// The exchange-zone assumption for the vendor's naive stamps is what this
	// asserts: a bar dated in the future would mean the zone is wrong.
	if last.After(time.Now().UTC().Add(2 * time.Hour)) {
		t.Fatalf("last bar %s is in the future — the vendor stamp zone assumption is wrong", last)
	}
	t.Logf("%d candles, last %s", len(s.Candles), last)
}

func TestLiveFMPProfileAndFundamentals(t *testing.T) {
	c := liveFMP(t)
	ctx := liveCtx(t)
	p, err := c.Profile(ctx, "AAPL")
	if err != nil {
		t.Fatalf("live profile: %v", err)
	}
	if p.CEO == "" || p.Sector == "" || p.Employees == nil {
		t.Fatalf("live profile thin: %+v", p)
	}
	sum, err := c.KeyMetrics(ctx, "AAPL", nil)
	if err != nil {
		t.Fatalf("live key metrics: %v", err)
	}
	if _, err := c.Ratios(ctx, "AAPL", sum); err != nil {
		t.Fatalf("live ratios: %v", err)
	}
	if sum.PERatio == nil {
		t.Fatalf("live fundamentals carry no P/E: %+v", sum)
	}
}

func TestLiveFMPMoversAndNews(t *testing.T) {
	c := liveFMP(t)
	ctx := liveCtx(t)
	movers, err := c.Movers(ctx, MoversGainers)
	if err != nil {
		t.Fatalf("live movers: %v", err)
	}
	if len(movers.Movers) == 0 {
		t.Fatal("live movers empty")
	}
	feed, err := c.MarketNews(ctx, 5)
	if err != nil {
		t.Fatalf("live news: %v", err)
	}
	if len(feed.Items) == 0 || feed.Items[0].URL == "" {
		t.Fatalf("live news thin: %+v", feed.Items)
	}
}

func TestLiveAlphaVantageGlobalQuoteShape(t *testing.T) {
	c := liveAlpha(t)
	q, err := c.GlobalQuote(liveCtx(t), "IBM")
	if err != nil {
		// A throttle on a shared key is a real outcome, not a test failure —
		// but it must arrive TYPED rather than as empty data.
		if KindOf(err) == FailureThrottled {
			t.Skipf("live probe skipped: provider throttled (%v)", err)
		}
		t.Fatalf("live global quote: %v", err)
	}
	if q.Price == nil || *q.Price <= 0 {
		t.Fatalf("live global quote has no price: %+v", q)
	}
	if q.ChangePercent == nil {
		t.Fatal("live global quote change percent did not parse (the % suffix path)")
	}
}

func TestLiveAlphaVantageSeriesKeyIsFoundByShape(t *testing.T) {
	c := liveAlpha(t)
	s, err := c.Series(liveCtx(t), "IBM", Interval5Min, false)
	if err != nil {
		if KindOf(err) == FailureThrottled || KindOf(err) == FailureNotConfigured {
			t.Skipf("live probe skipped: %v", err)
		}
		t.Fatalf("live series: %v", err)
	}
	if len(s.Candles) < 2 {
		t.Fatalf("live series too short: %d", len(s.Candles))
	}
	for i := 1; i < len(s.Candles); i++ {
		if !s.Candles[i].Time.After(s.Candles[i-1].Time) {
			t.Fatalf("live series not ascending at %d", i)
		}
	}
}

func TestLiveAlphaVantageNewsSentimentShape(t *testing.T) {
	c := liveAlpha(t)
	feed, err := c.NewsSentiment(liveCtx(t), []string{"AAPL"}, "", 10)
	if err != nil {
		if KindOf(err) == FailureThrottled || KindOf(err) == FailureNotConfigured {
			t.Skipf("live probe skipped: %v", err)
		}
		t.Fatalf("live news sentiment: %v", err)
	}
	if len(feed.Items) == 0 {
		t.Fatal("live sentiment feed empty")
	}
	scored := 0
	for _, item := range feed.Items {
		if item.Sentiment != nil {
			scored++
		}
	}
	if scored == 0 {
		t.Fatal("no story carried a sentiment score — the field path is wrong")
	}
}

func TestLiveAlphaVantageMarketStatusShape(t *testing.T) {
	c := liveAlpha(t)
	status, err := c.MarketStatus(liveCtx(t))
	if err != nil {
		if KindOf(err) == FailureThrottled || KindOf(err) == FailureNotConfigured {
			t.Skipf("live probe skipped: %v", err)
		}
		t.Fatalf("live market status: %v", err)
	}
	if len(status.Sessions) == 0 {
		t.Fatal("live market status empty")
	}
}
