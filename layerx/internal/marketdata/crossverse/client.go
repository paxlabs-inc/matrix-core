package crossverse

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paxlabs-inc/layerx/internal/perps/market"
)

const maxRestBodyBytes = 1 << 20

type Config struct {
	BaseURL          string
	SymbolRESTBase   string
	SymbolWSBase     string
	MarketsRESTBase  string
	MarketsWSBase    string
	Symbols          []string
	HTTPClient       *http.Client
	NetDialContext   func(ctx context.Context, network, addr string) (net.Conn, error)
	Now              func() time.Time
	DisableAggregate bool
	Logf             func(format string, args ...any)
}

type Manager struct {
	cfg   Config
	feeds map[string]*feed

	mu      sync.Mutex
	cancel  context.CancelFunc
	started bool
	wg      sync.WaitGroup

	markets             marketsState
	aggregateReceivedMs atomic.Int64
}

func expandSymbol(template, symbol string) string {
	out := strings.ReplaceAll(template, "{SYMBOL}", strings.ToUpper(symbol))
	return strings.ReplaceAll(out, "{symbol}", strings.ToLower(symbol))
}

func (m *Manager) symbolRESTBase(symbol string) string {
	if m.cfg.SymbolRESTBase != "" {
		return expandSymbol(m.cfg.SymbolRESTBase, symbol)
	}
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/api/" + strings.ToLower(symbol)
}

func (m *Manager) symbolWSBase(symbol string) string {
	if m.cfg.SymbolWSBase != "" {
		return expandSymbol(m.cfg.SymbolWSBase, symbol)
	}
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/ws/" + strings.ToLower(symbol)
}

func (m *Manager) marketsRESTBase() string {
	if m.cfg.MarketsRESTBase != "" {
		return strings.TrimRight(m.cfg.MarketsRESTBase, "/")
	}
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/api/markets"
}

func (m *Manager) marketsWSBase() string {
	if m.cfg.MarketsWSBase != "" {
		return strings.TrimRight(m.cfg.MarketsWSBase, "/")
	}
	return strings.TrimRight(m.cfg.BaseURL, "/") + "/ws/markets"
}

func New(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" &&
		(cfg.SymbolRESTBase == "" || cfg.SymbolWSBase == "" ||
			cfg.MarketsRESTBase == "" || cfg.MarketsWSBase == "") {
		return nil, fmt.Errorf("crossverse base url is required unless every service base is configured")
	}
	if cfg.BaseURL != "" {
		if _, err := url.Parse(cfg.BaseURL); err != nil {
			return nil, fmt.Errorf("crossverse base url %q is invalid: %w", cfg.BaseURL, err)
		}
	}
	if len(cfg.Symbols) == 0 {
		return nil, fmt.Errorf("crossverse symbols are required")
	}
	feeds := make(map[string]*feed, len(cfg.Symbols))
	for _, raw := range cfg.Symbols {
		mkt, err := market.Lookup(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := feeds[mkt.Symbol]; exists {
			return nil, fmt.Errorf("crossverse symbol %q is duplicated", mkt.Symbol)
		}
		feeds[mkt.Symbol] = newFeed(mkt.Symbol, mkt.DivergenceLimitBps)
	}
	return &Manager{cfg: cfg, feeds: feeds}, nil
}

func (m *Manager) nowMs() int64 {
	if m.cfg.Now != nil {
		return m.cfg.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return fmt.Errorf("crossverse manager is already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.started = true
	for _, f := range m.feeds {
		m.wg.Add(1)
		go m.runSymbolStream(runCtx, f)
	}
	if !m.cfg.DisableAggregate {
		m.wg.Add(1)
		go m.runAggregateStream(runCtx)
	}
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

func (m *Manager) feedFor(symbol string) (*feed, error) {
	f, ok := m.feeds[strings.ToUpper(strings.TrimSpace(symbol))]
	if !ok {
		return nil, fmt.Errorf("crossverse symbol %q is not enabled", symbol)
	}
	return f, nil
}

func (m *Manager) Snapshot(symbol string) (NormalizedSnapshot, error) {
	f, err := m.feedFor(symbol)
	if err != nil {
		return NormalizedSnapshot{}, err
	}
	return f.Snapshot(m.nowMs()), nil
}

func (m *Manager) RecentTrades(symbol string) ([]Trade, error) {
	f, err := m.feedFor(symbol)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Trade, len(f.recentTrades))
	copy(out, f.recentTrades)
	return out, nil
}

func (m *Manager) Ref(symbol string) (SnapshotRef, error) {
	f, err := m.feedFor(symbol)
	if err != nil {
		return SnapshotRef{}, err
	}
	return f.Ref(), nil
}

func (m *Manager) Health(symbol string) (Health, error) {
	f, err := m.feedFor(symbol)
	if err != nil {
		return "", err
	}
	return f.Health(m.nowMs()), nil
}

func (m *Manager) AggregateFresh(nowMs int64) bool {
	received := m.aggregateReceivedMs.Load()
	return received > 0 && nowMs-received <= aggregateFreshMs
}

func (m *Manager) RiskIncreaseAllowed(symbol string) (bool, error) {
	f, err := m.feedFor(symbol)
	if err != nil {
		return false, err
	}
	nowMs := m.nowMs()
	if f.Health(nowMs) != HealthHealthy {
		return false, nil
	}
	if !m.AggregateFresh(nowMs) {
		return false, nil
	}
	return true, nil
}

func (m *Manager) httpClient() *http.Client {
	if m.cfg.HTTPClient != nil {
		return m.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (m *Manager) FetchPerpStats(ctx context.Context, symbol string) error {
	f, err := m.feedFor(symbol)
	if err != nil {
		return err
	}
	target := m.symbolRESTBase(f.symbol) + "/perp_stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("crossverse %s: perp_stats request: %w", f.symbol, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRestBodyBytes))
	if err != nil {
		return fmt.Errorf("crossverse %s: perp_stats body: %w", f.symbol, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("crossverse %s: perp_stats status %d", f.symbol, resp.StatusCode)
	}
	return f.applyRestStats(body, m.nowMs())
}
