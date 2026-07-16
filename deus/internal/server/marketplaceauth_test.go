package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/registry"
)

func mintTestMarketplaceToken(t *testing.T, secret string, payload marketplaceTokenPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	message := "mkt1." + encoded
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestMarketplaceAuthRoundTrip(t *testing.T) {
	const secret = "marketplace-account-auth-secret-32bytes"
	now := time.Unix(1_800_000_000, 0)
	auth := NewMarketplaceAuth(secret)
	auth.now = func() time.Time { return now }
	token := mintTestMarketplaceToken(t, secret, marketplaceTokenPayload{
		Version: 1, Issuer: "deusmarkets.com", Audience: "deusd",
		Subject: "4ee575d2-741c-4ca1-a932-8f4c78feabf0",
		DID:     "did:matrix:marketplace:0123456789abcdef", DisplayName: "Alpha Dev",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	})
	principal, err := auth.VerifyToken(token)
	if err != nil {
		t.Fatalf("verify marketplace token: %v", err)
	}
	if principal.Kind != DeveloperPrincipalAccount || principal.Subject == "" || principal.Owner != "did:matrix:marketplace:0123456789abcdef" {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestMarketplaceAuthRejectsTamperAndExpiry(t *testing.T) {
	const secret = "marketplace-account-auth-secret-32bytes"
	now := time.Unix(1_800_000_000, 0)
	auth := NewMarketplaceAuth(secret)
	auth.now = func() time.Time { return now }
	payload := marketplaceTokenPayload{
		Version: 1, Issuer: "deusmarkets.com", Audience: "deusd", Subject: "account-1",
		DID:      "did:matrix:marketplace:0123456789abcdef",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
	token := mintTestMarketplaceToken(t, secret, payload)
	if _, err := auth.VerifyToken(token + "x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	payload.ExpiresAt = now.Add(-time.Second).Unix()
	expired := mintTestMarketplaceToken(t, secret, payload)
	if _, err := auth.VerifyToken(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token error = %v", err)
	}
}

func TestMarketplaceAccountCreatesAndOwnsListing(t *testing.T) {
	st := testStore(t)
	reg := registry.NewService(st, nil, nil)
	const secret = "marketplace-account-auth-secret-32bytes"
	srv := httptest.NewServer(New(Deps{
		Store: st, Registry: reg, MarketplaceAuthSecret: secret,
	}).Handler())
	defer srv.Close()

	now := time.Now()
	token := mintTestMarketplaceToken(t, secret, marketplaceTokenPayload{
		Version: 1, Issuer: "deusmarkets.com", Audience: "deusd",
		Subject: "4ee575d2-741c-4ca1-a932-8f4c78feabf0",
		DID:     "did:matrix:marketplace:0123456789abcdef", DisplayName: "Alpha Dev",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	})
	body := `{"manifest":{"schema_version":"1","slug":"account-canary","kind":"agent","display_name":"Account Canary","summary":"Account-owned canary service","owner":"did:matrix:marketplace:0123456789abcdef","mode":"hosted","payee_did":"did:matrix:payee:fedcba9876543210","settlement_mode":"exact","operations":[{"name":"echo","method":"POST","input_schema":{"type":"object"},"output_schema":{"type":"object"}}],"pricing":[{"operation":"echo","model":"per_call","unit":"call","unit_price_usdx":"0.010000","min_charge_usdx":"0.010000"}]}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/services", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Developer-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d body %s", res.StatusCode, raw)
	}

	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/me/services", nil)
	req.Header.Set("X-Developer-Token", token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "account-canary") {
		t.Fatalf("owned services status %d body %s", res.StatusCode, raw)
	}
}
