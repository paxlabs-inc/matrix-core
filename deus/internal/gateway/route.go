package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultProxyTimeout = 30 * time.Second

// ProxyResult is the outcome of proxy egress.
type ProxyResult struct {
	StatusCode int
	Body       []byte
	LatencyMS  int
}

// CallProxy POSTs args to proxy_url and returns the response body.
func CallProxy(ctx context.Context, proxyURL string, args map[string]any, timeoutMS int) (ProxyResult, error) {
	if proxyURL == "" {
		return ProxyResult{}, fmt.Errorf("gateway: proxy_url not configured")
	}
	body, err := json.Marshal(args)
	if err != nil {
		return ProxyResult{}, err
	}
	timeout := defaultProxyTimeout
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL, bytes.NewReader(body))
	if err != nil {
		return ProxyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return ProxyResult{}, fmt.Errorf("gateway: proxy call failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProxyResult{}, err
	}
	latency := int(time.Since(start).Milliseconds())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ProxyResult{StatusCode: resp.StatusCode, Body: raw, LatencyMS: latency},
			fmt.Errorf("gateway: proxy status %d", resp.StatusCode)
	}
	return ProxyResult{StatusCode: resp.StatusCode, Body: raw, LatencyMS: latency}, nil
}

// DecodeJSONResult parses proxy JSON into a generic object.
func DecodeJSONResult(body []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gateway: invalid proxy json: %w", err)
	}
	return out, nil
}
