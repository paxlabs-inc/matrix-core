package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/ratelimit"
)

// newLimitedServer builds a public-mode server with a tiny read limiter so the
// middleware's 429 path is exercised deterministically.
func newLimitedServer() *Server {
	return New(Deps{
		Challenges: auth.NewChallenges(time.Minute),
		Tokens:     auth.NewTokens("agent-secret", time.Hour),
		ChainID:    125,
		RateLimit: RateLimit{
			Enabled:    true,
			TrustProxy: true,
			Read:       ratelimit.New(1, 2), // burst 2
			Write:      ratelimit.New(1, 2),
			Auth:       ratelimit.New(1, 2),
		},
	})
}

func getFromIP(h http.Handler, path, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Real-IP", ip)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRateLimit429AfterBurst(t *testing.T) {
	h := newLimitedServer().Handler()
	// burst = 2 allowed, 3rd within the same instant -> 429.
	if rr := getFromIP(h, "/v1/info", "10.0.0.1"); rr.Code != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200", rr.Code)
	}
	if rr := getFromIP(h, "/v1/info", "10.0.0.1"); rr.Code != http.StatusOK {
		t.Fatalf("req2 status = %d, want 200", rr.Code)
	}
	rr := getFromIP(h, "/v1/info", "10.0.0.1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("req3 status = %d, want 429", rr.Code)
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 response must carry a Retry-After header")
	}
}

func TestRateLimitPerIPIsolation(t *testing.T) {
	h := newLimitedServer().Handler()
	// Drain IP A's burst.
	getFromIP(h, "/v1/info", "10.0.0.2")
	getFromIP(h, "/v1/info", "10.0.0.2")
	if rr := getFromIP(h, "/v1/info", "10.0.0.2"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("IP A 3rd status = %d, want 429", rr.Code)
	}
	// A different IP must be unaffected.
	if rr := getFromIP(h, "/v1/info", "10.0.0.3"); rr.Code != http.StatusOK {
		t.Fatalf("IP B status = %d, want 200 (per-IP isolation)", rr.Code)
	}
}

func TestRateLimitRootExempt(t *testing.T) {
	h := newLimitedServer().Handler()
	// "/" (and /healthz) are exempt so liveness probes are never throttled.
	for i := 0; i < 10; i++ {
		if rr := getFromIP(h, "/", "10.0.0.9"); rr.Code != http.StatusOK {
			t.Fatalf("root probe %d status = %d, want 200 (never 429)", i+1, rr.Code)
		}
	}
}

func TestRateLimitDisabledByDefault(t *testing.T) {
	srv, _ := newTestServer() // no RateLimit configured -> disabled
	h := srv.Handler()
	for i := 0; i < 20; i++ {
		if rr := getFromIP(h, "/v1/info", "10.0.0.10"); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d got 429 but limiter should be disabled by default", i+1)
		}
	}
}
