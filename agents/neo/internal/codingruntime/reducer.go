package codingruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"centra/agents/neo/internal/codingworker"
)

const (
	maxSummaryRunes = 4_000
	maxStatusRunes  = 2_000
	maxCommandRunes = 4_000
	maxDiffRunes    = 32_000
	maxPathRunes    = 1_024
	maxChoiceCount  = 20
	maxChoiceRunes  = 256
	maxTodoCount    = 50
)

var (
	ErrInvalidWorkerEvent = errors.New("invalid coding worker event")
	credentialAssignment  = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password)\s*[:=]\s*["']?[^\s,;"']+`)
	bearerCredential      = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`)
	providerCredential    = regexp.MustCompile(`\b(?:sk|xai|ghp|github_pat)-[A-Za-z0-9_-]{8,}`)
	verificationCommand   = regexp.MustCompile(`(?i)(?:^|[;&|]\s*)(?:(?:env|command|time)\s+)?(?:[A-Z_][A-Z0-9_]*=\S+\s+)*(?:(?:pnpm|npm|yarn|bun)\s+(?:run\s+)?(?:test|build|lint|typecheck|check)|go\s+(?:test|vet|build)|pytest|python3?\s+-m\s+pytest|cargo\s+(?:test|check|build|clippy)|(?:npx\s+)?(?:vitest|jest|eslint|tsc|next\s+build)|ruff\s+check|make\s+(?:test|check|build))(?:\s|$)`)
)

// ReduceEvent converts one private AgentCore gateway event into a bounded,
// product-owned delivery. The boolean is false for protocol chatter that must
// never be persisted or shown (token deltas, reasoning, runtime metadata, and
// other non-product events).
func ReduceEvent(event codingworker.Event) (DeliveryBatch, bool, error) {
	event.Type = strings.TrimSpace(event.Type)
	if event.Type == "" {
		return DeliveryBatch{}, false, fmt.Errorf("%w: type is required", ErrInvalidWorkerEvent)
	}
	if ignoredWorkerEvent(event.Type) {
		return DeliveryBatch{}, false, nil
	}

	payload, err := decodeEventPayload(event.Payload)
	if err != nil {
		return DeliveryBatch{}, false, fmt.Errorf("%w: %s payload: %v", ErrInvalidWorkerEvent, event.Type, err)
	}
	sessionID := strings.TrimSpace(string(event.SessionID))
	batch := DeliveryBatch{}

	switch event.Type {
	case "message.start":
		batch.NextLifecycle = LifecycleRunning

	case "message.interim", "review.summary", "background.complete":
		text := safeText(firstString(payload, "text", "summary"), sessionID, maxSummaryRunes)
		if text == "" {
			return DeliveryBatch{}, false, nil
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceStatus, Summary: text})

	case "status.update":
		text := safeText(firstString(payload, "text", "message"), sessionID, maxStatusRunes)
		if text == "" {
			return DeliveryBatch{}, false, nil
		}
		kind := safeToken(stringValue(payload["kind"]), 64)
		if isBlockedStatus(kind, text) {
			batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceBlocker, Summary: text})
			batch.NextLifecycle = LifecycleBlocked
			batch.Wake = &WakeRequest{Kind: WakeBlocked, Reason: text}
		} else {
			batch.Evidence = append(batch.Evidence, EvidenceRecord{
				Kind: EvidenceStatus, Summary: text,
				Details: nonEmptyDetails(map[string]interface{}{"kind": kind}),
			})
		}

	case "tool.start":
		name := safeToken(stringValue(payload["name"]), 64)
		context := safeText(stringValue(payload["context"]), sessionID, maxStatusRunes)
		if context == "" || !productTool(name) {
			return DeliveryBatch{}, false, nil
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{
			Kind: EvidenceStatus, Summary: toolProgressSummary(name, context),
		})

	case "tool.complete":
		batch.Evidence = append(batch.Evidence, reduceToolComplete(payload, sessionID)...)
		if len(batch.Evidence) == 0 {
			return DeliveryBatch{}, false, nil
		}

	case "clarify.request":
		question := safeText(stringValue(payload["question"]), sessionID, maxSummaryRunes)
		if question == "" {
			return DeliveryBatch{}, false, fmt.Errorf("%w: clarify.request question is required", ErrInvalidWorkerEvent)
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{
			Kind: EvidenceQuestion, Question: question, Summary: question,
			Details: nonEmptyDetails(map[string]interface{}{"choices": safeStrings(payload["choices"], sessionID, maxChoiceCount, maxChoiceRunes)}),
		})
		batch.NextLifecycle = LifecycleNeedsInput
		batch.Wake = &WakeRequest{Kind: WakeInputRequired, Reason: question}

	case "approval.request":
		command := safeText(stringValue(payload["command"]), sessionID, maxCommandRunes)
		description := safeText(stringValue(payload["description"]), sessionID, maxSummaryRunes)
		if description == "" {
			description = "This build action needs approval."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{
			Kind: EvidenceApproval, Summary: description, Command: command,
			Details: nonEmptyDetails(map[string]interface{}{
				"choices":         safeStrings(payload["choices"], sessionID, maxChoiceCount, maxChoiceRunes),
				"allow_permanent": boolValue(payload["allow_permanent"]),
				"smart_denied":    boolValue(payload["smart_denied"]),
			}),
		})
		batch.NextLifecycle = LifecycleNeedsApproval
		batch.Wake = &WakeRequest{Kind: WakeApprovalRequired, Reason: description}

	case "preview.open":
		previewURL := safePreviewURL(stringValue(payload["url"]), sessionID)
		if previewURL == "" {
			return DeliveryBatch{}, false, nil
		}
		label := safeText(stringValue(payload["label"]), sessionID, maxChoiceRunes)
		summary := label
		if summary == "" {
			summary = "Preview is ready."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidencePreview, Summary: summary, PreviewURL: previewURL})

	case "secret.request", "sudo.request":
		reason := "The build requires unavailable privileged access."
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceBlocker, Summary: reason})
		batch.NextLifecycle = LifecycleBlocked
		batch.Wake = &WakeRequest{Kind: WakeBlocked, Reason: reason}

	case "error":
		message := safeText(firstString(payload, "message", "error"), sessionID, maxSummaryRunes)
		if message == "" {
			message = "The build worker failed."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceError, Error: message, Summary: message})
		batch.Terminal = &TerminalResult{Lifecycle: LifecycleFailed, Outcome: OutcomeFailed, Summary: message}
		batch.Wake = &WakeRequest{Kind: WakeFailed, Reason: message}

	case "message.complete":
		reduceMessageComplete(&batch, payload, sessionID)

	default:
		return DeliveryBatch{}, false, nil
	}

	if len(batch.Evidence) == 0 && batch.NextLifecycle == "" && batch.Terminal == nil && batch.Wake == nil {
		return DeliveryBatch{}, false, nil
	}
	sourceID, deliveryID, err := stableEventIDs(event.Type, payload)
	if err != nil {
		return DeliveryBatch{}, false, fmt.Errorf("%w: fingerprint: %v", ErrInvalidWorkerEvent, err)
	}
	batch.SourceEventIDs = []string{sourceID}
	batch.DeliveryID = deliveryID
	for i := range batch.Evidence {
		batch.Evidence[i].SourceEventID = sourceID
	}
	if batch.Wake != nil {
		batch.Wake.SourceEventID = sourceID
		batch.Wake.DedupKey = wakeDedupKey(batch.Wake.Kind, sourceID)
	}
	return batch, true, nil
}

func decodeEventPayload(raw json.RawMessage) (map[string]interface{}, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return map[string]interface{}{}, nil
	}
	var payload map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	if payload == nil {
		return nil, errors.New("expected an object")
	}
	return payload, nil
}

func reduceMessageComplete(batch *DeliveryBatch, payload map[string]interface{}, sessionID string) {
	status := strings.ToLower(strings.TrimSpace(stringValue(payload["status"])))
	text := safeText(stringValue(payload["text"]), sessionID, maxSummaryRunes)
	errorText := safeText(stringValue(payload["error"]), sessionID, maxSummaryRunes)
	switch status {
	case "verified":
		if text == "" {
			text = "Build completed with current passing verification evidence."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceStatus, Summary: text})
		batch.Terminal = &TerminalResult{Lifecycle: LifecycleVerified, Outcome: OutcomeVerified, Summary: text}
		batch.Wake = &WakeRequest{Kind: WakeVerified, Reason: text}
	case "interrupted", "cancelled", "canceled", "aborted":
		if text == "" {
			text = "Build interrupted."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceStatus, Summary: text})
		batch.Terminal = &TerminalResult{Lifecycle: LifecycleInterrupted, Outcome: OutcomeInterrupted, Summary: text}
		batch.Wake = &WakeRequest{Kind: WakeInterrupted, Reason: text}
	case "error", "failed", "failure":
		if errorText == "" {
			errorText = text
		}
		if errorText == "" {
			errorText = "The build failed."
		}
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceError, Error: errorText, Summary: errorText})
		batch.Terminal = &TerminalResult{Lifecycle: LifecycleFailed, Outcome: OutcomeFailed, Summary: errorText}
		batch.Wake = &WakeRequest{Kind: WakeFailed, Reason: errorText}
	default:
		reason := "The coding pass finished without current passing project verification. Send a correction to run the required checks and resume this same Build job."
		batch.Evidence = append(batch.Evidence, EvidenceRecord{Kind: EvidenceBlocker, Summary: reason})
		batch.NextLifecycle = LifecycleBlocked
		batch.Wake = &WakeRequest{Kind: WakeBlocked, Reason: reason}
	}
}

func reduceToolComplete(payload map[string]interface{}, sessionID string) []EvidenceRecord {
	name := safeToken(stringValue(payload["name"]), 64)
	if !productTool(name) {
		return nil
	}
	args := objectValue(payload["args"])
	result := objectValue(payload["result"])
	summary := safeText(stringValue(payload["summary"]), sessionID, maxStatusRunes)
	errorText := safeText(firstString(payload, "error"), sessionID, maxSummaryRunes)
	if errorText == "" {
		errorText = safeText(firstString(result, "error"), sessionID, maxSummaryRunes)
	}
	var evidence []EvidenceRecord

	switch name {
	case "terminal", "process":
		command := safeText(firstString(args, "command", "cmd"), sessionID, maxCommandRunes)
		if command == "" {
			command = safeText(stringValue(payload["context"]), sessionID, maxCommandRunes)
		}
		if command != "" {
			commandSummary := summary
			if commandSummary == "" {
				commandSummary = "Command completed."
			}
			evidence = append(evidence, EvidenceRecord{Kind: EvidenceCommand, Command: command, Summary: commandSummary})
			if exitCode, ok := intValue(result["exit_code"]); ok && verificationCommand.MatchString(command) {
				passed := exitCode == 0
				evidence = append(evidence, EvidenceRecord{
					Kind: EvidenceVerification, Check: command, Passed: &passed,
					Summary: verificationSummary(command, passed),
				})
			}
		}

	case "write_file", "patch":
		paths := changedPaths(args, result, sessionID)
		for _, path := range paths {
			changeSummary := summary
			if changeSummary == "" {
				changeSummary = "Updated " + path
			}
			evidence = append(evidence, EvidenceRecord{Kind: EvidenceFileChange, Path: path, Summary: changeSummary})
		}
		diff := safeText(stringValue(payload["inline_diff"]), sessionID, maxDiffRunes)
		if diff == "" {
			diff = safeText(stringValue(result["diff"]), sessionID, maxDiffRunes)
		}
		if diff != "" {
			path := ""
			if len(paths) == 1 {
				path = paths[0]
			}
			evidence = append(evidence, EvidenceRecord{Kind: EvidenceDiff, Path: path, Diff: diff, Summary: summary})
		}

	case "todo":
		todos := safeTodos(payload["todos"], sessionID)
		if len(todos) == 0 {
			todos = safeTodos(result["todos"], sessionID)
		}
		if len(todos) != 0 {
			evidence = append(evidence, EvidenceRecord{
				Kind: EvidenceStatus, Summary: todoSummary(todos),
				Details: map[string]interface{}{"todos": todos},
			})
		}

	case "browser_navigate", "browser_click", "browser_type", "browser_scroll", "browser_back", "browser_press", "browser_snapshot", "browser_get_images", "browser_vision", "browser_console", "browser_cdp", "browser_dialog":
		if summary != "" {
			evidence = append(evidence, EvidenceRecord{Kind: EvidenceStatus, Summary: summary})
		}
	}

	if errorText != "" {
		evidence = append(evidence, EvidenceRecord{Kind: EvidenceError, Error: errorText, Summary: errorText})
	}
	return evidence
}

func ignoredWorkerEvent(eventType string) bool {
	switch eventType {
	case "gateway.ready", "session.info", "message.delta", "thinking.delta", "reasoning.delta", "reasoning.available", "moa.reference", "moa.aggregating", "moa.progress", "moa.phase", "tool.progress", "tool.generating", "tool.output_risk", "notification.show", "notification.clear", "reaction", "voice.status", "voice.transcript", "wake.detected", "gateway.stderr", "browser.progress", "agent.terminal.output", "terminal.close", "clarify.expire", "secret.expire", "sudo.expire":
		return true
	default:
		return false
	}
}

func productTool(name string) bool {
	switch name {
	case "terminal", "process", "write_file", "patch", "todo",
		"browser_navigate", "browser_click", "browser_type", "browser_scroll", "browser_back", "browser_press",
		"browser_snapshot", "browser_get_images", "browser_vision", "browser_console", "browser_cdp", "browser_dialog":
		return true
	default:
		return false
	}
}

func stableEventIDs(eventType string, payload map[string]interface{}) (string, string, error) {
	clean := scrubFingerprint(payload)
	encoded, err := json.Marshal(struct {
		Type    string                 `json:"type"`
		Payload map[string]interface{} `json:"payload"`
	}{Type: eventType, Payload: clean})
	if err != nil {
		return "", "", err
	}
	sourceSum := sha256.Sum256(append([]byte("matrix-build-source-v1\x00"), encoded...))
	sourceID := "source_" + hex.EncodeToString(sourceSum[:16])
	deliverySum := sha256.Sum256([]byte("matrix-build-delivery-v1\x00" + sourceID))
	return sourceID, "delivery_" + hex.EncodeToString(deliverySum[:16]), nil
}

func scrubFingerprint(value interface{}) map[string]interface{} {
	object := objectValue(value)
	clean := make(map[string]interface{}, len(object))
	for key, raw := range object {
		lower := strings.ToLower(key)
		switch lower {
		case "session_id", "runtime_id", "service_instance_id", "child_session_id",
			"provider", "model", "base_url", "api_key", "token", "usage", "reasoning", "reasoning_content", "reasoning_details",
			"rendered", "billing", "failure_reason", "profile", "metadata":
			continue
		}
		switch typed := raw.(type) {
		case map[string]interface{}:
			clean[key] = scrubFingerprint(typed)
		case []interface{}:
			items := make([]interface{}, 0, min(len(typed), maxTodoCount))
			for _, item := range typed[:min(len(typed), maxTodoCount)] {
				if nested, ok := item.(map[string]interface{}); ok {
					items = append(items, scrubFingerprint(nested))
				} else {
					items = append(items, item)
				}
			}
			clean[key] = items
		default:
			clean[key] = raw
		}
	}
	return clean
}

func wakeDedupKey(kind WakeKind, sourceID string) string {
	return "build-wake:" + string(kind) + ":" + sourceID
}

func safeText(value, privateID string, maxRunes int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if privateID != "" {
		value = strings.ReplaceAll(value, privateID, "[private id]")
	}
	value = bearerCredential.ReplaceAllString(value, "Bearer [redacted]")
	value = providerCredential.ReplaceAllString(value, "[redacted]")
	value = credentialAssignment.ReplaceAllStringFunc(value, func(match string) string {
		if index := strings.IndexAny(match, ":="); index >= 0 {
			return strings.TrimSpace(match[:index]) + "=[redacted]"
		}
		return "[redacted]"
	})
	return capRunes(value, maxRunes)
}

func capRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	const marker = "\n[truncated]"
	markerRunes := []rune(marker)
	if limit <= len(markerRunes) {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-len(markerRunes)])) + marker
}

func safePath(value, privateID string) string {
	value = safeText(value, privateID, maxPathRunes)
	if value == "" {
		return ""
	}
	value = filepath.ToSlash(filepath.Clean(value))
	if strings.HasPrefix(value, "/") {
		parts := strings.FieldsFunc(value, func(r rune) bool { return r == '/' })
		if len(parts) > 1 && parts[0] == "workspace" {
			parts = parts[1:]
		}
		if len(parts) > 4 {
			parts = parts[len(parts)-4:]
		}
		value = strings.Join(parts, "/")
	}
	if value == "." {
		return ""
	}
	return value
}

func safePreviewURL(value, privateID string) string {
	value = safeText(value, privateID, maxPathRunes)
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func safeStrings(value interface{}, privateID string, count, runeLimit int) []string {
	items, ok := value.([]interface{})
	if !ok || count <= 0 {
		return nil
	}
	if len(items) > count {
		items = items[:count]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text := safeText(stringValue(item), privateID, runeLimit)
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func safeTodos(value interface{}, privateID string) []map[string]interface{} {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	if len(items) > maxTodoCount {
		items = items[:maxTodoCount]
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		todo := objectValue(item)
		content := safeText(firstString(todo, "content", "text", "title"), privateID, maxChoiceRunes)
		status := safeToken(stringValue(todo["status"]), 32)
		if content == "" && status == "" {
			continue
		}
		row := map[string]interface{}{}
		if content != "" {
			row["content"] = content
		}
		if status != "" {
			row["status"] = status
		}
		out = append(out, row)
	}
	return out
}

func changedPaths(args, result map[string]interface{}, privateID string) []string {
	seen := map[string]struct{}{}
	paths := make([]string, 0, 4)
	add := func(value string) {
		path := safePath(value, privateID)
		if path == "" {
			return
		}
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	add(firstString(args, "path", "file_path", "target"))
	add(firstString(result, "resolved_path", "path"))
	for _, key := range []string{"files_modified", "files_created", "files_deleted"} {
		items, _ := result[key].([]interface{})
		for _, item := range items {
			if len(paths) >= maxTodoCount {
				return paths
			}
			add(stringValue(item))
		}
	}
	return paths
}

func todoSummary(todos []map[string]interface{}) string {
	counts := map[string]int{}
	for _, todo := range todos {
		status := stringValue(todo["status"])
		if status == "" {
			status = "unspecified"
		}
		counts[status]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[key], strings.ReplaceAll(key, "_", " ")))
	}
	return "Plan: " + strings.Join(parts, ", ")
}

func toolProgressSummary(name, context string) string {
	switch name {
	case "terminal", "process":
		return "Running " + context
	case "write_file", "patch":
		return "Updating " + context
	case "todo":
		return "Updating the build plan."
	default:
		return context
	}
}

func verificationSummary(command string, passed bool) string {
	if passed {
		return "Check passed: " + capRunes(command, maxChoiceRunes)
	}
	return "Check failed: " + capRunes(command, maxChoiceRunes)
}

func isBlockedStatus(kind, text string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	lower := strings.ToLower(strings.TrimSpace(text))
	return kind == "blocked" || kind == "blocker" || strings.HasPrefix(lower, "blocked:") || strings.HasPrefix(lower, "blocked ")
}

func nonEmptyDetails(details map[string]interface{}) map[string]interface{} {
	for key, value := range details {
		switch typed := value.(type) {
		case string:
			if typed == "" {
				delete(details, key)
			}
		case []string:
			if len(typed) == 0 {
				delete(details, key)
			}
		case nil:
			delete(details, key)
		}
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func objectValue(value interface{}) map[string]interface{} {
	object, _ := value.(map[string]interface{})
	if object == nil {
		return map[string]interface{}{}
	}
	return object
}

func stringValue(value interface{}) string {
	valueString, _ := value.(string)
	return valueString
}

func firstString(object map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object[key]); strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolValue(value interface{}) interface{} {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return nil
}

func intValue(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		integer, err := typed.Int64()
		return int(integer), err == nil
	case float64:
		return int(typed), typed == float64(int(typed))
	case int:
		return typed, true
	default:
		return 0, false
	}
}

func safeToken(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, char := range value {
		if !(char == '_' || char == '-' || char == '.' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9') {
			return ""
		}
	}
	return capRunes(value, limit)
}
