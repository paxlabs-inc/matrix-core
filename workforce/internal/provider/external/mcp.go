package external

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"centra/workforce/internal/effect"
)

const mcpProtocolVersion = "2024-11-05"

type mcpFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *mcpError       `json:"error"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
	IsError           bool              `json:"isError"`
	Meta              json.RawMessage   `json:"_meta"`
}

func (adapter *Adapter) callMatrixMCP(
	ctx context.Context,
	authorized authorizedOperation,
	operation effect.Operation,
	probe bool,
) (ProviderResponse, networkFailure, error) {
	connection := authorized.loaded.connection
	policy := authorized.policy
	tool := policy.MCPTool
	if probe {
		tool = policy.MCPProbeTool
		if tool == "" {
			return ProviderResponse{}, networkFailure{
				safeCode: "browser_authoritative_probe_unavailable",
			}, fmt.Errorf("%w: browser operation is drift-blind", ErrAmbiguous)
		}
	}
	var arguments map[string]any
	if err := decodeStrict(authorized.envelope.Request.Body, &arguments); err != nil || arguments == nil {
		return ProviderResponse{}, networkFailure{
			safeCode: "browser_arguments_invalid",
		}, fmt.Errorf("%w: browser arguments are invalid", ErrDenied)
	}
	endpoint, err := url.Parse(connection.EndpointURL)
	if err != nil {
		return ProviderResponse{}, networkFailure{safeCode: "browser_endpoint_invalid"}, err
	}
	client := adapter.httpClient(connection, endpoint)
	initialize := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name": "matrix-workforce-external", "version": "1",
			},
		},
	}
	frame, sessionID, err := adapter.mcpPost(
		ctx, client, endpoint, authorized.loaded.credential, "",
		initialize, 1, connection.Limits.OutputBytes,
		connection.Limits.StreamIdleTimeout,
	)
	if err != nil || frame.Error != nil || strings.TrimSpace(sessionID) == "" ||
		len(sessionID) > 512 || containsLineBreak(sessionID) {
		return ProviderResponse{}, networkFailure{
			started: false, safeCode: "browser_session_initialize_failed", cause: err,
		}, fmt.Errorf("%w: browser session initialization failed", ErrUnavailable)
	}
	if err := adapter.mcpNotify(
		ctx, client, endpoint, authorized.loaded.credential, sessionID,
		map[string]any{
			"jsonrpc": "2.0", "method": "notifications/initialized",
			"params": map[string]any{},
		},
	); err != nil {
		return ProviderResponse{}, networkFailure{
			started: false, safeCode: "browser_session_notification_failed", cause: err,
		}, fmt.Errorf("%w: browser session notification failed", ErrUnavailable)
	}
	listenerContext, stopListener := context.WithCancel(ctx)
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		adapter.mcpListen(
			listenerContext, client, endpoint, authorized.loaded.credential,
			sessionID, connection.Limits.OutputBytes,
			connection.Limits.StreamIdleTimeout,
		)
	}()
	defer func() {
		stopListener()
		select {
		case <-listenerDone:
		case <-time.After(time.Second):
		}
		adapter.mcpDeleteSession(client, endpoint, authorized.loaded.credential, sessionID)
	}()
	call := map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments},
	}
	frame, returnedSession, err := adapter.mcpPost(
		ctx, client, endpoint, authorized.loaded.credential, sessionID,
		call, 2, connection.Limits.OutputBytes,
		connection.Limits.StreamIdleTimeout,
	)
	if returnedSession != "" && returnedSession != sessionID {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_session_confused",
		}, fmt.Errorf("%w: browser session identity changed", ErrAmbiguous)
	}
	if err != nil || frame.Error != nil {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_tool_call_failed", cause: err,
		}, fmt.Errorf("%w: browser tool call failed", ErrUnavailable)
	}
	var result mcpToolResult
	if err := decodeStrict(frame.Result, &result); err != nil || len(result.Content) == 0 {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_tool_result_invalid",
		}, fmt.Errorf("%w: browser tool result is invalid", ErrAmbiguous)
	}
	for _, content := range result.Content {
		var descriptor struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(content, &descriptor) != nil ||
			(descriptor.Type != "text" && descriptor.Type != "image" &&
				descriptor.Type != "audio" && descriptor.Type != "resource" &&
				descriptor.Type != "resource_link") {
			return ProviderResponse{}, networkFailure{
				started: policy.Action.Mutates(), safeCode: "browser_content_type_invalid",
			}, fmt.Errorf("%w: browser returned unsupported content", ErrAmbiguous)
		}
		if len(content) > int(authorized.envelope.Request.OutputBytes) {
			return ProviderResponse{}, networkFailure{
				started: policy.Action.Mutates(), safeCode: "browser_content_limit_exceeded",
			}, fmt.Errorf("%w: browser output exceeded limit", ErrAmbiguous)
		}
	}
	if result.IsError {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_tool_reported_error",
		}, fmt.Errorf("%w: browser tool reported an error", ErrUnavailable)
	}
	output := append(json.RawMessage(nil), frame.Result...)
	finalURL := structuredURL(result.StructuredContent)
	if (policy.Action == ActionNavigate || policy.Action.Mutates()) && finalURL == "" {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_final_origin_unverifiable",
		}, fmt.Errorf("%w: browser final origin is unverifiable", ErrAmbiguous)
	}
	if finalURL == "" {
		finalURL = authorized.envelope.Request.TargetURL
	}
	now, timeErr := adapter.store.currentTime()
	if timeErr != nil {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "browser_observation_time_failed",
		}, timeErr
	}
	digest := sha256.Sum256([]byte(adapter.name + "\x00" + operation.IdempotencyKey))
	return ProviderResponse{
		SchemaVersion:  SchemaVersion,
		ExternalID:     adapter.name + ":" + hex.EncodeToString(digest[:16]),
		IdempotencyKey: operation.IdempotencyKey,
		RequestHash:    authorized.requestHash,
		AccountID:      connection.AccountID,
		IdentityID:     connection.IdentityID,
		State:          ExternalCompleted,
		Authoritative:  false,
		ObservedAt:     now,
		FinalURL:       finalURL,
		Output:         output,
	}, networkFailure{started: true}, nil
}

func (adapter *Adapter) mcpPost(
	ctx context.Context,
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
	payload any,
	expectedID int,
	limit uint64,
	idle time.Duration,
) (mcpFrame, string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return mcpFrame{}, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return mcpFrame{}, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	if err := applyCredential(request, credential); err != nil {
		return mcpFrame{}, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return mcpFrame{}, "", err
	}
	defer response.Body.Close()
	returnedSession := strings.TrimSpace(response.Header.Get("Mcp-Session-Id"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return mcpFrame{}, returnedSession, fmt.Errorf("external adapter: browser MCP HTTP status %d", response.StatusCode)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		frame, err := adapter.readMCPStream(
			ctx, client, endpoint, credential, coalesce(returnedSession, sessionID),
			response.Body, expectedID, limit, idle,
		)
		return frame, returnedSession, err
	}
	data, err := readBoundedIdle(ctx, response.Body, limit, idle)
	if err != nil {
		return mcpFrame{}, returnedSession, err
	}
	var frame mcpFrame
	if err := decodeStrict(data, &frame); err != nil || !frameMatchesID(frame, expectedID) {
		return mcpFrame{}, returnedSession, fmt.Errorf("external adapter: browser MCP response is invalid")
	}
	return frame, returnedSession, nil
}

func (adapter *Adapter) mcpNotify(
	ctx context.Context,
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
	payload any,
) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Session-Id", sessionID)
	if err := applyCredential(request, credential); err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("external adapter: browser MCP notification failed")
	}
	return nil
}

func (adapter *Adapter) mcpListen(
	ctx context.Context,
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
	limit uint64,
	idle time.Duration,
) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Mcp-Session-Id", sessionID)
	if applyCredential(request, credential) != nil {
		return
	}
	response, err := client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return
	}
	_, _ = adapter.readMCPStream(
		ctx, client, endpoint, credential, sessionID,
		response.Body, -1, limit, idle,
	)
}

func (adapter *Adapter) readMCPStream(
	ctx context.Context,
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
	body io.Reader,
	expectedID int,
	limit uint64,
	idle time.Duration,
) (mcpFrame, error) {
	reader := bufio.NewReaderSize(body, 32<<10)
	var total uint64
	var dataLines []string
	for {
		if err := ctx.Err(); err != nil {
			return mcpFrame{}, err
		}
		line, err := readLineWithIdle(ctx, reader, idle)
		total += uint64(len(line))
		if total > limit {
			return mcpFrame{}, fmt.Errorf("external adapter: browser SSE output exceeds limit")
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}
		if trimmed == "" && len(dataLines) > 0 {
			var frame mcpFrame
			if decodeErr := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &frame); decodeErr == nil {
				if frame.Method == "ping" && len(frame.ID) > 0 {
					_ = adapter.respondMCPPing(
						ctx, client, endpoint, credential, sessionID, frame.ID,
					)
				} else if expectedID >= 0 && frameMatchesID(frame, expectedID) {
					return frame, nil
				}
			}
			dataLines = dataLines[:0]
		}
		if err != nil {
			if err == io.EOF && expectedID < 0 {
				return mcpFrame{}, nil
			}
			return mcpFrame{}, err
		}
	}
}

func (adapter *Adapter) respondMCPPing(
	ctx context.Context,
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
	id json.RawMessage,
) error {
	var identifier any
	if err := json.Unmarshal(id, &identifier); err != nil {
		return err
	}
	pingContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return adapter.mcpNotify(
		pingContext, client, endpoint, credential, sessionID,
		map[string]any{"jsonrpc": "2.0", "id": identifier, "result": map[string]any{}},
	)
}

func (adapter *Adapter) mcpDeleteSession(
	client *http.Client,
	endpoint *url.URL,
	credential CredentialMaterial,
	sessionID string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Mcp-Session-Id", sessionID)
	if applyCredential(request, credential) != nil {
		return
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
}

func structuredURL(value json.RawMessage) string {
	if len(value) == 0 || !json.Valid(value) {
		return ""
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return ""
	}
	return findStructuredURL(decoded, 0)
}

func findStructuredURL(value any, depth int) string {
	if depth > 16 {
		return ""
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"final_url", "page_url", "url"} {
			if raw, ok := typed[key].(string); ok {
				parsed, err := url.Parse(raw)
				if err == nil && parsed.IsAbs() {
					return raw
				}
			}
		}
		for _, child := range typed {
			if found := findStructuredURL(child, depth+1); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findStructuredURL(child, depth+1); found != "" {
				return found
			}
		}
	}
	return ""
}

func frameMatchesID(frame mcpFrame, expected int) bool {
	if frame.JSONRPC != "2.0" || len(frame.ID) == 0 {
		return false
	}
	var numeric int
	return json.Unmarshal(frame.ID, &numeric) == nil && numeric == expected
}

func readLineWithIdle(
	ctx context.Context,
	reader *bufio.Reader,
	idle time.Duration,
) (string, error) {
	type result struct {
		line string
		err  error
	}
	completed := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		completed <- result{line: line, err: err}
	}()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("external adapter: browser stream idle timeout")
	case value := <-completed:
		return value.line, value.err
	}
}

func coalesce(left, right string) string {
	if left != "" {
		return left
	}
	return right
}
