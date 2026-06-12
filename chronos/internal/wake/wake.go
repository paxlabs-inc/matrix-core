// Package wake delivers a due alarm to its agent by asking the router to wake
// the machine + inject a chat turn. Chronos NEVER talks to Fly or the daemon
// directly (chronos.frozen.kvx [wake]); it reuses the router's battle-tested
// EnsureStarted + waitDaemonReady + 6PN reverse-proxy path via ONE new router
// surface: POST /internal/wake (wake-token auth).
package wake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Request is the body POSTed to the router /internal/wake endpoint. The router
// resolves user_id -> machine, wakes it, and POSTs {message, conversation_id}
// to the daemon /chat over Fly 6PN. payload + alarm_id ride along so the
// delivery is dedup-able downstream (invariant i3) and tagged as a timer wake.
type Request struct {
	UserID         string          `json:"user_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Message        string          `json:"message"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	AlarmID        string          `json:"alarm_id"`
	Origin         string          `json:"origin"` // always "chronos"
}

// Waker delivers a wake. Implementations must return a non-nil error on any
// non-2xx so the dispatch retry ladder can act (honest failure, invariant i6).
type Waker interface {
	Wake(ctx context.Context, req Request) error
}

// HTTPWaker posts wakes to the router internal endpoint with the shared token.
type HTTPWaker struct {
	URL    string
	Token  string
	Client *http.Client
}

// New constructs an HTTPWaker with a sane default client.
func New(url, token string) *HTTPWaker {
	return &HTTPWaker{
		URL:    url,
		Token:  token,
		Client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Wake POSTs the request, returning an error for any transport failure or
// non-2xx response (the body is surfaced for honest failure recording).
func (w *HTTPWaker) Wake(ctx context.Context, req Request) error {
	req.Origin = "chronos"
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("wake: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("wake: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if w.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+w.Token)
	}
	resp, err := w.Client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("wake: post %s: %w", w.URL, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wake: router returned %d: %s", resp.StatusCode, truncate(respBody, 300))
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
