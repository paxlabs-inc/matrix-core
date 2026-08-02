package codingworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayCredentialSourceMintsBoundScopedToken(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	legacy := []byte("legacy-matrix-gateway-bearer")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/internal/agentcore/token" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+string(legacy) || r.Header.Get(actorHeader) != "did:matrix:user-1:cody" {
			t.Error("request authority was not bound")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"mxg1.payload.signature-value-123456789","expires_at":` + strconv.FormatInt(now.Add(15*time.Minute).Unix(), 10) + `,"actor":"did:matrix:user-1:cody","slot":"cody","provider":"xiaomi","model":"mimo-v2.5-pro"}`))
	}))
	defer server.Close()

	for _, base := range []string{server.URL, server.URL + "/v1"} {
		source, err := NewGatewayCredentialSource(GatewayCredentialSourceConfig{
			GatewayURL: base, LegacyBearer: legacy, Now: func() time.Time { return now }, Timeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		credential, err := source.IssueCodingCredential(context.Background(), "user-1", "did:matrix:user-1:cody")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(credential.Model), "mxg1.") {
			t.Fatalf("unexpected token shape")
		}
		credential.erase()
		retainedBearer := source.bearer
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		for _, value := range retainedBearer {
			if value != 0 {
				t.Fatal("legacy bearer was not erased")
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestGatewayCredentialSourceRejectsRedirectAndAuthorityDrift(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	var redirected atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	source, err := NewGatewayCredentialSource(GatewayCredentialSourceConfig{
		GatewayURL: redirect.URL, LegacyBearer: []byte("legacy"), Now: func() time.Time { return now }, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	credential, err := source.IssueCodingCredential(context.Background(), "user-1", "did:matrix:user-1:cody")
	if err == nil || len(credential.Model) != 0 || redirected.Load() {
		t.Fatalf("credential=%d err=%v redirected=%v", len(credential.Model), err, redirected.Load())
	}

	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"token":"mxg1.payload.signature-value-123456789","expires_at":` + strconv.FormatInt(now.Add(15*time.Minute).Unix(), 10) + `,"actor":"did:matrix:other:cody","slot":"cody","provider":"xiaomi","model":"mimo-v2.5-pro"}`))
	}))
	defer wrong.Close()
	other, err := NewGatewayCredentialSource(GatewayCredentialSourceConfig{
		GatewayURL: wrong.URL, LegacyBearer: []byte("legacy"), Now: func() time.Time { return now }, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	credential, err = other.IssueCodingCredential(context.Background(), "user-1", "did:matrix:user-1:cody")
	if err == nil || len(credential.Model) != 0 {
		t.Fatalf("credential=%d err=%v", len(credential.Model), err)
	}
}
