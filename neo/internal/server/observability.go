// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/llm"
)

var structuredLogMu sync.Mutex

func (e *Engine) logLifecycle(event, intentID, conversationID, status string, duration time.Duration, eventErr error) {
	fields := map[string]interface{}{
		"event":              event,
		"severity":           "info",
		"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
		"intent_id":          strings.TrimSpace(intentID),
		"conversation_id":    strings.TrimSpace(conversationID),
		"correlation_id":     strings.TrimSpace(intentID),
		"tenant_correlation": tenantCorrelation(e.cfg.ActorDID),
		"status":             status,
	}
	if duration > 0 {
		fields["duration_ms"] = duration.Milliseconds()
	}
	writeStructuredLog(fields, eventErr)
}

func (s *Server) observeHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = newHTTPObservabilityID()
		}
		w.Header().Set("X-Request-ID", requestID)
		rec := &observedResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		intentID := requestIntentID(r)
		conversationID := requestConversationID(r)
		fields := map[string]interface{}{
			"event":              "http.request",
			"severity":           "info",
			"ts":                 time.Now().UTC().Format(time.RFC3339Nano),
			"request_id":         requestID,
			"correlation_id":     firstNonEmpty(intentID, requestID),
			"intent_id":          intentID,
			"conversation_id":    conversationID,
			"tenant_correlation": tenantCorrelation(s.engine.cfg.ActorDID),
			"method":             r.Method,
			"path":               r.URL.Path,
			"status":             rec.status,
			"duration_ms":        time.Since(start).Milliseconds(),
			"outcome":            observedHTTPOutcome(rec.status),
		}
		var eventErr error
		if rec.status >= 500 {
			eventErr = fmt.Errorf("server response status %d", rec.status)
		}
		writeStructuredLog(fields, eventErr)
	})
}

func writeStructuredLog(fields map[string]interface{}, eventErr error) {
	w := os.Stdout
	if eventErr != nil {
		fields["severity"] = "error"
		fields["error_class"] = observableErrorClass(eventErr)
		fields["error_fingerprint"] = observableErrorFingerprint(eventErr)
		w = os.Stderr
	}
	b, err := json.Marshal(fields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/observability: marshal: %v\n", err)
		return
	}
	structuredLogMu.Lock()
	_, _ = fmt.Fprintln(w, string(b))
	structuredLogMu.Unlock()
}

func observableErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, llm.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, llm.ErrProviderUnavailable):
		return "provider_unavailable"
	default:
		var networkError net.Error
		if errors.As(err, &networkError) {
			return "network"
		}
		return "internal"
	}
}

func observableErrorFingerprint(err error) string {
	if err == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(err.Error()))
	return "err_" + hex.EncodeToString(sum[:8])
}

func tenantCorrelation(actor string) string {
	if strings.TrimSpace(actor) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(actor))
	return "tenant_" + hex.EncodeToString(sum[:8])
}

func newHTTPObservabilityID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "req_" + hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("req_%d", time.Now().UnixNano())
}

func requestIntentID(r *http.Request) string {
	if id := strings.TrimSpace(r.URL.Query().Get("intent_id")); id != "" {
		return id
	}
	for _, prefix := range []string{"/messages/async/", "/intents/"} {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")[0]
		}
	}
	return ""
}

func requestConversationID(r *http.Request) string {
	if id := strings.TrimSpace(r.URL.Query().Get("conversation_id")); id != "" {
		return id
	}
	if strings.HasPrefix(r.URL.Path, "/events/replay/") {
		return strings.Split(strings.TrimPrefix(r.URL.Path, "/events/replay/"), "/")[0]
	}
	return ""
}

func conversationForRun(e *Engine, intentID string) string {
	if run := e.lookupRun(intentID); run != nil {
		return run.convID
	}
	if rec, ok, err := e.getRunRecord(intentID); err == nil && ok {
		return rec.ConversationID
	}
	return ""
}

func observedHTTPOutcome(status int) string {
	if status >= 500 {
		return "failure"
	}
	if status >= 400 {
		return "rejected"
	}
	return "success"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type observedResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijacking unsupported")
	}
	return hijacker.Hijack()
}
