package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HostedInvokeRequest is POST {exec_endpoint}/invoke from the gateway.
type HostedInvokeRequest struct {
	InvocationID  string         `json:"invocation_id"`
	Operation     string         `json:"operation"`
	Args          map[string]any `json:"args"`
	CallerDID     string         `json:"caller_did"`
	DeadlineMS    int            `json:"deadline_ms"`
	ReceiptDigest string         `json:"receipt_digest,omitempty"`
}

// HostedInvokeResponse is the runner harness response envelope.
type HostedInvokeResponse struct {
	Outcome   string         `json:"outcome"`
	Result    map[string]any `json:"result"`
	Units     string         `json:"units"`
	RunnerSig *string        `json:"runner_sig"`
}

// HostedResult is the parsed runner call outcome.
type HostedResult struct {
	Outcome   string
	Result    map[string]any
	Units     string
	RunnerSig *string
	LatencyMS int
}

// CallHosted invokes a Paxeer Cloud function execution endpoint.
func CallHosted(ctx context.Context, execURL string, req HostedInvokeRequest, timeoutMS, maxBytes int) (HostedResult, error) {
	base := strings.TrimRight(strings.TrimSpace(execURL), "/")
	url := base + "/invoke"
	body, err := json.Marshal(req)
	if err != nil {
		return HostedResult{}, err
	}
	timeout := defaultProxyTimeout
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	if maxBytes <= 0 {
		maxBytes = 262144
	}
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return HostedResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return HostedResult{}, fmt.Errorf("gateway: hosted call failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return HostedResult{}, err
	}
	if len(raw) > maxBytes {
		return HostedResult{}, fmt.Errorf("gateway: hosted response exceeds max bytes")
	}
	latency := int(time.Since(start).Milliseconds())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HostedResult{LatencyMS: latency},
			fmt.Errorf("gateway: hosted status %d: %s", resp.StatusCode, string(raw))
	}
	var out HostedInvokeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return HostedResult{}, fmt.Errorf("gateway: invalid hosted json: %w", err)
	}
	if out.Outcome == "" {
		out.Outcome = "ok"
	}
	if out.Units == "" {
		out.Units = "1"
	}
	return HostedResult{
		Outcome:   out.Outcome,
		Result:    out.Result,
		Units:     out.Units,
		RunnerSig: out.RunnerSig,
		LatencyMS: latency,
	}, nil
}
