// Package httpx is a tiny JSON HTTP client for provider API calls, with bearer
// auth, a bounded timeout, and a typed error that preserves the upstream status
// + body so connector handlers can surface provider failures honestly.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Error carries the upstream HTTP status + (truncated) body.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider http %d: %s", e.Status, e.Body)
}

// Client wraps an *http.Client with a default timeout.
type Client struct {
	HTTP    *http.Client
	Timeout time.Duration
}

// New returns a Client with the given timeout (default 30s).
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{HTTP: &http.Client{Timeout: timeout}, Timeout: timeout}
}

// JSON performs an HTTP request with an optional bearer token and JSON body,
// decoding a JSON response into out (out may be nil). Non-2xx -> *Error.
func (c *Client) JSON(ctx context.Context, method, rawURL, bearer string, body, out any) error {
	var headers map[string]string
	if bearer != "" {
		headers = map[string]string{"Authorization": "Bearer " + bearer}
	}
	return c.JSONWithHeaders(ctx, method, rawURL, headers, body, out)
}

// JSONWithHeaders is JSON with arbitrary request headers (e.g. GoTrue's apikey).
func (c *Client) JSONWithHeaders(ctx context.Context, method, rawURL string, headers map[string]string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req, out)
}

// Form performs an application/x-www-form-urlencoded POST (used for OAuth token
// endpoints), decoding a JSON response into out.
func (c *Client) Form(ctx context.Context, rawURL string, form url.Values, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body := string(raw)
		if len(body) > 600 {
			body = body[:600]
		}
		return &Error{Status: res.StatusCode, Body: body}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
