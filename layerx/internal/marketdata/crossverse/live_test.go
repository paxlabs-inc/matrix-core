package crossverse

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

type tcpProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	conns    []net.Conn
	closed   bool
}

func startProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{listener: listener, target: target}
	go p.acceptLoop()
	t.Cleanup(p.close)
	return p
}

func (p *tcpProxy) acceptLoop() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		upstream, err := net.DialTimeout("tcp", p.target, 15*time.Second)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			client.Close()
			upstream.Close()
			return
		}
		p.conns = append(p.conns, client, upstream)
		p.mu.Unlock()
		go pipe(client, upstream)
		go pipe(upstream, client)
	}
}

func pipe(dst, src net.Conn) {
	io.Copy(dst, src)
	dst.Close()
	src.Close()
}

func (p *tcpProxy) kill() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (p *tcpProxy) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.listener.Close()
	p.kill()
}

func (p *tcpProxy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, p.listener.Addr().String())
}

func waitFor(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLiveCrossverseFeed(t *testing.T) {
	baseURL := os.Getenv("CROSSVERSE_TEST_URL")
	if baseURL == "" {
		t.Skip("CROSSVERSE_TEST_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https", "wss":
			port = "443"
		default:
			port = "80"
		}
	}
	proxy := startProxy(t, net.JoinHostPort(parsed.Hostname(), port))

	m, err := New(Config{
		BaseURL:        baseURL,
		Symbols:        []string{"BTC", "ETH"},
		NetDialContext: proxy.dialContext,
		Logf:           t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	healthy := func(symbol string) bool {
		h, err := m.Health(symbol)
		if err != nil {
			t.Fatal(err)
		}
		return h == HealthHealthy
	}
	waitFor(t, 60*time.Second, "BTC feed HEALTHY", func() bool { return healthy("BTC") })
	waitFor(t, 60*time.Second, "ETH feed HEALTHY", func() bool { return healthy("ETH") })
	waitFor(t, 30*time.Second, "aggregate freshness", func() bool { return m.AggregateFresh(m.nowMs()) })

	for _, symbol := range []string{"BTC", "ETH"} {
		s, err := m.Snapshot(symbol)
		if err != nil {
			t.Fatal(err)
		}
		if s.SnapshotID == "" || s.OrderbookSeq == 0 {
			t.Fatalf("%s snapshot ref is empty: %+v", symbol, s)
		}
		if s.MarkPriceCents <= 0 || s.IndexPriceCents <= 0 {
			t.Fatalf("%s refs = mark %d index %d", symbol, s.MarkPriceCents, s.IndexPriceCents)
		}
		if len(s.Bids) == 0 || len(s.Asks) == 0 || s.BestBidCents <= 0 || s.BestAskCents <= 0 {
			t.Fatalf("%s book is empty: %d bids %d asks", symbol, len(s.Bids), len(s.Asks))
		}
		rec, ok := m.MarketsRecord(symbol)
		if !ok {
			t.Fatalf("%s has no markets record", symbol)
		}
		if rec.PerpNextFundingAtMs <= 1_000_000_000_000 {
			t.Fatalf("%s next funding = %d", symbol, rec.PerpNextFundingAtMs)
		}
		if rec.PerpMarkPriceCents <= 0 || rec.PriceCents <= 0 {
			t.Fatalf("%s markets record prices = %d %d", symbol, rec.PerpMarkPriceCents, rec.PriceCents)
		}
		ref, err := m.Ref(symbol)
		if err != nil {
			t.Fatal(err)
		}
		if ref.SnapshotID != s.SnapshotID || ref.SourceTimestampMs == 0 {
			t.Fatalf("%s ref mismatch: %+v vs snapshot %q", symbol, ref, s.SnapshotID)
		}
	}

	allowed, err := m.RiskIncreaseAllowed("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("risk increase must be allowed while live feed is healthy")
	}

	statsCtx, statsCancel := context.WithTimeout(ctx, 15*time.Second)
	defer statsCancel()
	if err := m.FetchPerpStats(statsCtx, "BTC"); err != nil {
		t.Fatalf("REST stats recovery: %v", err)
	}
	if err := m.FetchMarkets(statsCtx); err != nil {
		t.Fatalf("REST markets fetch: %v", err)
	}

	proxy.kill()
	waitFor(t, 10*time.Second, "feed to leave HEALTHY and fail closed after disconnect", func() bool {
		h, err := m.Health("BTC")
		if err != nil {
			t.Fatal(err)
		}
		if h == HealthHealthy {
			return false
		}
		allowed, err := m.RiskIncreaseAllowed("BTC")
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatalf("risk increase allowed while BTC health is %s", h)
		}
		return true
	})

	waitFor(t, 90*time.Second, "BTC feed HEALTHY after reconnect", func() bool { return healthy("BTC") })
	waitFor(t, 90*time.Second, "ETH feed HEALTHY after reconnect", func() bool { return healthy("ETH") })
	waitFor(t, 30*time.Second, "risk increase allowed after reconnect", func() bool {
		allowed, err := m.RiskIncreaseAllowed("BTC")
		if err != nil {
			t.Fatal(err)
		}
		return allowed
	})
}
