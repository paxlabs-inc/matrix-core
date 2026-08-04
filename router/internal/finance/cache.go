// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Class groups results by how fast they go stale. The tier a call belongs to
// decides its TTL, and nothing else in the lane needs to know about time.
type Class string

const (
	ClassQuote        Class = "quote"
	ClassSeriesLive   Class = "series_intraday"
	ClassSeriesDaily  Class = "series_daily"
	ClassMovers       Class = "movers"
	ClassNews         Class = "news"
	ClassProfile      Class = "profile"
	ClassFundamentals Class = "fundamentals"
	ClassStatus       Class = "status"
	ClassMacro        Class = "macro"
	ClassSearch       Class = "search"
)

// tiers is the freshness policy. A market page opens many panels against the
// same symbol in the same instant; without these a single page view would be a
// dozen upstream calls, and the vendor quota is the scarce resource.
var tiers = map[Class]time.Duration{
	ClassQuote:        10 * time.Second,
	ClassSeriesLive:   60 * time.Second,
	ClassSeriesDaily:  15 * time.Minute,
	ClassMovers:       60 * time.Second,
	ClassNews:         3 * time.Minute,
	ClassProfile:      12 * time.Hour,
	ClassFundamentals: 6 * time.Hour,
	ClassStatus:       2 * time.Minute,
	ClassMacro:        6 * time.Hour,
	ClassSearch:       30 * time.Minute,
}

// TTLFor reports a class's freshness window. An unknown class gets the shortest
// tier rather than an unbounded one: a cache that never expires is worse than no
// cache on a market surface.
func TTLFor(class Class) time.Duration {
	if d, ok := tiers[class]; ok {
		return d
	}
	return 10 * time.Second
}

// staleGrace is how long an expired entry is still allowed to answer AFTER the
// upstream has refused a refresh. Serving a labeled stale price beats blanking
// a panel because the vendor is throttling — but only for a bounded while.
const staleGrace = 30 * time.Minute

type entry struct {
	value    any
	storedAt time.Time
	expires  time.Time
}

type call struct {
	done  chan struct{}
	value any
	err   error
}

// Cache is the lane's TTL store with single-flight collapse and a bounded stale
// fallback. It is the reason one page view is one upstream call.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	calls   map[string]*call
	now     func() time.Time
	// max bounds the entry count so a symbol-spraying client cannot grow the
	// process without limit; the oldest entries are dropped first.
	max int
}

// NewCache builds a cache. now is the clock seam — tests drive expiry without
// sleeping.
func NewCache(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{
		entries: map[string]*entry{},
		calls:   map[string]*call{},
		now:     now,
		max:     4096,
	}
}

// Outcome describes how a Do call was answered, for the metering record.
type Outcome struct {
	CacheHit bool
	Stale    bool
	Latency  time.Duration
}

// Do returns the cached value for key when it is fresh, and otherwise calls fn
// exactly once across all concurrent callers for that key.
//
// When fn fails and a stale entry is still within the grace window, the stale
// value answers instead of the error — labeled stale so the surface above can
// say so. That is the difference between a market page that degrades and one
// that goes blank the moment a vendor hiccups.
func (c *Cache) Do(ctx context.Context, key string, ttl time.Duration, fn func(context.Context) (any, error)) (any, Outcome, error) {
	start := c.now()

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
		value := e.value
		c.mu.Unlock()
		return value, Outcome{CacheHit: true}, nil
	}
	// A refresh for this key may already be in flight; join it rather than
	// spending a second upstream call on the same question.
	if inflight, ok := c.calls[key]; ok {
		c.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, Outcome{}, ctx.Err()
		}
		return inflight.value, Outcome{CacheHit: true, Latency: c.now().Sub(start)}, inflight.err
	}
	leader := &call{done: make(chan struct{})}
	c.calls[key] = leader
	c.mu.Unlock()

	value, err := fn(ctx)

	c.mu.Lock()
	if err == nil {
		c.entries[key] = &entry{value: value, storedAt: c.now(), expires: c.now().Add(ttl)}
		c.evictLocked()
	} else if stale, ok := c.entries[key]; ok && c.now().Sub(stale.storedAt) <= staleGrace {
		// The upstream refused; the last good answer stands in, marked stale.
		value = stale.value
		leader.value, leader.err = value, nil
		delete(c.calls, key)
		close(leader.done)
		c.mu.Unlock()
		return value, Outcome{Stale: true, Latency: c.now().Sub(start)}, nil
	}
	leader.value, leader.err = value, err
	delete(c.calls, key)
	close(leader.done)
	c.mu.Unlock()

	return value, Outcome{Latency: c.now().Sub(start)}, err
}

// evictLocked drops the oldest entries once the cache is over its bound.
func (c *Cache) evictLocked() {
	if len(c.entries) <= c.max {
		return
	}
	type aged struct {
		key string
		at  time.Time
	}
	all := make([]aged, 0, len(c.entries))
	for k, e := range c.entries {
		all = append(all, aged{k, e.storedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)-c.max; i++ {
		delete(c.entries, all[i].key)
	}
}

// Size reports the number of live entries — read by the diagnostics surface.
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Purge drops every entry. Used by tests and by an operator forcing a refresh.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]*entry{}
}
