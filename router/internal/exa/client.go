// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	APIKeyEnv       = "EXA_API_KEY"
	defaultBaseURL  = "https://api.exa.ai"
	defaultTimeout  = 45 * time.Second
	maxResponseSize = 16 << 20
)

type ClientConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(cfg ClientConfig) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{apiKey: strings.TrimSpace(cfg.APIKey), baseURL: base, http: httpClient}
}

func (c *Client) Configured() bool { return c != nil && c.apiKey != "" }

func (c *Client) Search(ctx context.Context, input SearchRequest) (*SearchResponse, error) {
	if strings.TrimSpace(input.Query) == "" {
		return nil, badRequest("search", "A search query is required.")
	}
	if input.Contents.Highlights == nil && input.Contents.Text == nil && input.Contents.Summary == nil {
		input.Contents.Highlights = true
	}
	var out SearchResponse
	if err := c.do(ctx, http.MethodPost, "/search", input, &out); err != nil {
		return nil, err
	}
	if out.Output != nil && len(out.Output.Grounding) == 0 {
		return &out, &Failure{Kind: FailureUngrounded, Endpoint: "search", Message: "The generated search synthesis did not include grounding."}
	}
	return &out, nil
}

func (c *Client) Contents(ctx context.Context, input ContentsRequest) (*ContentsResponse, error) {
	if len(input.URLs) == 0 {
		return nil, badRequest("contents", "At least one URL is required.")
	}
	if input.Highlights == nil && input.Text == nil && input.Summary == nil {
		input.Highlights = true
	}
	var out ContentsResponse
	if err := c.do(ctx, http.MethodPost, "/contents", input, &out); err != nil {
		return nil, err
	}
	failed := 0
	for _, status := range out.Statuses {
		if status.Status != "success" {
			failed++
		}
	}
	if failed > 0 {
		return &out, &Failure{Kind: FailurePartial, Endpoint: "contents", Message: "Some source pages could not be retrieved.", Detail: fmt.Sprintf("%d of %d URLs failed", failed, len(out.Statuses))}
	}
	return &out, nil
}

func (c *Client) CreateRun(ctx context.Context, input AgentRequest) (*AgentRun, error) {
	if strings.TrimSpace(input.Query) == "" {
		return nil, badRequest("agent/runs", "A research query is required.")
	}
	var out AgentRun
	if err := c.do(ctx, http.MethodPost, "/agent/runs", input, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, &Failure{Kind: FailureUpstream, Endpoint: "agent/runs", Message: "The research provider returned no run identifier."}
	}
	return &out, nil
}

func (c *Client) GetRun(ctx context.Context, id string) (*AgentRun, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, badRequest("agent/runs", "A research run identifier is required.")
	}
	var out AgentRun
	if err := c.do(ctx, http.MethodGet, "/agent/runs/"+id, nil, &out); err != nil {
		return nil, err
	}
	if out.Status == "completed" && (out.Output == nil || len(out.Output.Grounding) == 0) {
		return &out, &Failure{Kind: FailureUngrounded, Endpoint: "agent/runs", Message: "The completed research did not include grounding."}
	}
	return &out, nil
}

func (c *Client) CancelRun(ctx context.Context, id string) (*AgentRun, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, badRequest("agent/runs/cancel", "A research run identifier is required.")
	}
	var out AgentRun
	if err := c.do(ctx, http.MethodPost, "/agent/runs/"+id+"/cancel", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
	if !c.Configured() {
		return &Failure{Kind: FailureNotConfigured, Endpoint: strings.Trim(path, "/"), Message: "Grounded web research is not configured for this deployment.", Detail: "missing " + APIKeyEnv}
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return badRequest(strings.Trim(path, "/"), "That research request could not be encoded.")
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return badRequest(strings.Trim(path, "/"), "That research request could not be built.")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "matrix-router-exa/1")

	res, err := c.http.Do(req)
	if err != nil {
		kind := FailureUpstream
		message := "The research provider could not be reached."
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
			kind = FailureTimeout
			message = "The research provider did not answer in time."
		}
		return &Failure{Kind: kind, Endpoint: strings.Trim(path, "/"), Message: message, Detail: redact(err.Error())}
	}
	defer res.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseSize))
	if readErr != nil {
		return &Failure{Kind: FailureUpstream, Endpoint: strings.Trim(path, "/"), Status: res.StatusCode, Message: "The research provider response could not be read.", Detail: redact(readErr.Error())}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return statusFailure(strings.Trim(path, "/"), res, data)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return &Failure{Kind: FailureUpstream, Endpoint: strings.Trim(path, "/"), Status: res.StatusCode, Message: "The research provider returned an unreadable response.", Detail: redact(err.Error())}
	}
	return nil
}

func statusFailure(endpoint string, res *http.Response, body []byte) *Failure {
	kind := FailureUpstream
	message := "The research provider returned an error."
	switch res.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		kind, message = FailureBadRequest, "The research provider rejected that request."
	case http.StatusUnauthorized, http.StatusForbidden:
		kind, message = FailureNotConfigured, "The research provider rejected this deployment's credentials."
	case http.StatusNotFound:
		kind, message = FailureNotFound, "That research run or source does not exist."
	case http.StatusTooManyRequests:
		kind, message = FailureRateLimited, "Grounded web research is rate limited right now."
	}
	return &Failure{Kind: kind, Endpoint: endpoint, Status: res.StatusCode, Message: message, Detail: snippet(body), RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
}

func badRequest(endpoint, message string) *Failure {
	return &Failure{Kind: FailureBadRequest, Endpoint: endpoint, Message: message}
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func snippet(data []byte) string {
	value := strings.TrimSpace(string(data))
	if len(value) > 400 {
		value = value[:400]
	}
	return redact(value)
}

func redact(value string) string {
	for _, marker := range []string{"Bearer ", "EXA_API_KEY=", "apiKey=", "api_key="} {
		for {
			index := strings.Index(value, marker)
			if index < 0 {
				break
			}
			end := index + len(marker)
			for end < len(value) && !strings.ContainsRune(" &\"'\n\t,)}", rune(value[end])) {
				end++
			}
			value = value[:index+len(marker)] + "REDACTED" + value[end:]
		}
	}
	return value
}
