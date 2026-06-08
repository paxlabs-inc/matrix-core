package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientSendReturnsTxHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/send" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("missing/wrong bearer: %q", got)
		}
		var body struct {
			Tx struct {
				To    string `json:"to"`
				Value string `json:"value"`
			} `json:"tx"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Tx.To != "0xdev" || body.Tx.Value != "1000" {
			t.Errorf("unexpected tx %+v", body.Tx)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tx_hash": "0xabc", "address": "0xwallet"})
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL}
	tx, err := c.Send(context.Background(), "caller-token", "0xdev", "1000")
	if err != nil {
		t.Fatal(err)
	}
	if tx != "0xabc" {
		t.Fatalf("tx = %q, want 0xabc", tx)
	}
}

func TestHTTPClientSendPolicyDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "SPEND_CAP_EXCEEDED",
			"message": "over per-call cap",
			"cap_wei": "500",
		})
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL}
	_, err := c.Send(context.Background(), "caller-token", "0xdev", "1000")
	var pd *PolicyDenied
	if !errors.As(err, &pd) {
		t.Fatalf("expected *PolicyDenied, got %v", err)
	}
	if pd.CapWei != "500" {
		t.Fatalf("cap = %q, want 500", pd.CapWei)
	}
}

func TestHTTPClientSendRequiresConfig(t *testing.T) {
	c := &HTTPClient{}
	if _, err := c.Send(context.Background(), "t", "0x", "1"); err == nil {
		t.Fatal("expected error when BaseURL unset")
	}
}
