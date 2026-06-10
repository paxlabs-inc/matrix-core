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

// isAppwriteExecutionsURL reports whether execURL is an Appwrite synchronous
// executions endpoint (".../functions/{id}/executions"). Dev/runner endpoints
// use {url}/invoke instead and are routed through CallHosted.
func isAppwriteExecutionsURL(execURL string) bool {
	return strings.HasSuffix(strings.TrimRight(strings.TrimSpace(execURL), "/"), "/executions")
}

// appwriteExecution is the subset of the Appwrite execution response we read.
type appwriteExecution struct {
	ResponseStatusCode int    `json:"responseStatusCode"`
	ResponseBody       string `json:"responseBody"`
	Status             string `json:"status"`
	Errors             string `json:"errors"`
}

// CallAppwriteExecution invokes a hosted function through the Appwrite Server
// executions API. It wraps the runner input (HostedInvokeRequest) as the
// execution body, POSTs it synchronously, then decodes the function's
// responseBody (a JSON HostedInvokeResponse). maxBytes caps the function
// response payload.
func CallAppwriteExecution(ctx context.Context, executionsURL, project, apiKey string, req HostedInvokeRequest, timeoutMS, maxBytes int) (HostedResult, error) {
	url := strings.TrimRight(strings.TrimSpace(executionsURL), "/")
	inner, err := json.Marshal(req)
	if err != nil {
		return HostedResult{}, err
	}
	// The runner receives this as req.bodyText and dispatches POST /invoke.
	execReq := map[string]any{
		"body":   string(inner),
		"method": "POST",
		"path":   "/invoke",
		"async":  false,
	}
	payload, err := json.Marshal(execReq)
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return HostedResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Appwrite-Project", project)
	httpReq.Header.Set("X-Appwrite-Key", apiKey)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return HostedResult{}, fmt.Errorf("gateway: appwrite execution failed: %w", err)
	}
	defer resp.Body.Close()
	// The Appwrite envelope is larger than the function payload; bound it but
	// allow headroom over maxBytes so we can still surface the body-too-large
	// error from the embedded responseBody rather than truncating blindly.
	envelopeCap := int64(maxBytes)*2 + 8192
	raw, err := io.ReadAll(io.LimitReader(resp.Body, envelopeCap+1))
	if err != nil {
		return HostedResult{}, err
	}
	latency := int(time.Since(start).Milliseconds())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HostedResult{LatencyMS: latency},
			fmt.Errorf("gateway: appwrite execution status %d: %s", resp.StatusCode, string(raw))
	}
	var exec appwriteExecution
	if err := json.Unmarshal(raw, &exec); err != nil {
		return HostedResult{LatencyMS: latency}, fmt.Errorf("gateway: invalid appwrite execution json: %w", err)
	}
	if exec.ResponseStatusCode != 0 && (exec.ResponseStatusCode < 200 || exec.ResponseStatusCode >= 300) {
		return HostedResult{LatencyMS: latency},
			fmt.Errorf("gateway: hosted function status %d: %s", exec.ResponseStatusCode, exec.ResponseBody)
	}
	if len(exec.ResponseBody) > maxBytes {
		return HostedResult{LatencyMS: latency}, fmt.Errorf("gateway: hosted response exceeds max bytes")
	}
	if exec.ResponseBody == "" {
		return HostedResult{LatencyMS: latency}, fmt.Errorf("gateway: empty hosted response (status=%s errors=%s)", exec.Status, exec.Errors)
	}
	var out HostedInvokeResponse
	if err := json.Unmarshal([]byte(exec.ResponseBody), &out); err != nil {
		return HostedResult{LatencyMS: latency}, fmt.Errorf("gateway: invalid hosted json: %w", err)
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
