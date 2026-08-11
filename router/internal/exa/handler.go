// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const SubjectHeader = "X-Matrix-Subject"

type ErrorBody struct {
	Kind       FailureKind `json:"kind"`
	Message    string      `json:"message"`
	RetryAfter int         `json:"retry_after_seconds,omitempty"`
}

type Handler struct {
	Service            *Service
	Finance            bool
	TrustSubjectHeader bool
	Subject            func(context.Context) string
	Log                func(string, ...any)
}

func NewHandler(service *Service, subject func(context.Context) string, log func(string, ...any)) *Handler {
	return &Handler{Service: service, Subject: subject, Log: log}
}
func NewInternalHandler(service *Service, log func(string, ...any)) *Handler {
	return &Handler{Service: service, TrustSubjectHeader: true, Log: log}
}
func NewFinanceHandler(service *Service, internal bool, subject func(context.Context) string, log func(string, ...any)) *Handler {
	return &Handler{Service: service, Finance: true, TrustSubjectHeader: internal, Subject: subject, Log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		h.writeError(w, &Failure{Kind: FailureNotConfigured, Endpoint: "service", Message: "Grounded web research is not configured for this deployment."})
		return
	}
	user := ""
	if h.Subject != nil {
		user = strings.TrimSpace(h.Subject(r.Context()))
	}
	if user == "" && h.TrustSubjectHeader {
		user = strings.TrimSpace(r.Header.Get(SubjectHeader))
	}
	if user == "" {
		h.writeError(w, &Failure{Kind: FailureBadRequest, Endpoint: "auth", Message: "A verified research subject is required."})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal")
	if h.Finance {
		path = strings.Trim(strings.TrimPrefix(path, "/finance/research"), "/")
	} else {
		path = strings.Trim(strings.TrimPrefix(path, "/exa"), "/")
	}
	if h.Finance {
		h.serveFinance(w, r, user, path)
		return
	}
	h.serveGeneral(w, r, user, path)
}

func (h *Handler) serveGeneral(w http.ResponseWriter, r *http.Request, user, path string) {
	switch {
	case r.Method == http.MethodPost && path == "search":
		var input SearchRequest
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.Search(r.Context(), user, input)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && path == "contents":
		var input ContentsRequest
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.Contents(r.Context(), user, input)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && path == "research/start":
		var input struct {
			Workflow string       `json:"workflow"`
			Subject  string       `json:"subject"`
			Request  AgentRequest `json:"request"`
		}
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.StartRun(r.Context(), user, strings.TrimSpace(input.Workflow), strings.TrimSpace(input.Subject), input.Request)
		h.respond(w, out, err)
	case strings.HasPrefix(path, "research/"):
		h.serveRun(w, r, user, strings.TrimPrefix(path, "research/"))
	case r.Method == http.MethodGet && path == "diag" && h.TrustSubjectHeader:
		h.writeJSON(w, http.StatusOK, h.Service.Stats())
	default:
		h.writeError(w, &Failure{Kind: FailureNotFound, Endpoint: path, Message: "That grounded research endpoint does not exist."})
	}
}

func (h *Handler) serveFinance(w http.ResponseWriter, r *http.Request, user, path string) {
	switch {
	case r.Method == http.MethodPost && path == "start":
		var input FinanceRequest
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.StartFinance(r.Context(), user, input)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && path == "verify":
		var input VerifyRequest
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.VerifyFinance(r.Context(), user, input)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && path == "news":
		var input FinanceNewsRequest
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.ExtractFinanceNews(r.Context(), user, input)
		h.respond(w, out, err)
	case path != "":
		h.serveRun(w, r, user, path)
	default:
		h.writeError(w, &Failure{Kind: FailureNotFound, Endpoint: path, Message: "That finance research endpoint does not exist."})
	}
}

func (h *Handler) serveRun(w http.ResponseWriter, r *http.Request, user, suffix string) {
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	id := parts[0]
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		out, err := h.Service.GetRun(r.Context(), user, id)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "continue":
		var input struct {
			Query  string `json:"query"`
			Effort string `json:"effort,omitempty"`
		}
		if !h.decode(w, r, &input) {
			return
		}
		out, err := h.Service.ContinueRun(r.Context(), user, id, input.Query, input.Effort)
		h.respond(w, out, err)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "cancel":
		out, err := h.Service.CancelRun(r.Context(), user, id)
		h.respond(w, out, err)
	default:
		h.writeError(w, &Failure{Kind: FailureNotFound, Endpoint: "agent/runs", Message: "That research run operation does not exist."})
	}
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, output any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		h.writeError(w, &Failure{Kind: FailureBadRequest, Endpoint: "request", Message: "That research request is not valid JSON.", Detail: err.Error()})
		return false
	}
	return true
}

func (h *Handler) respond(w http.ResponseWriter, output any, err error) {
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, output)
}

func errorBody(err error) *ErrorBody {
	failure := FailureOf(err)
	if failure == nil {
		return &ErrorBody{Kind: FailureUpstream, Message: "Grounded research could not be loaded."}
	}
	body := &ErrorBody{Kind: failure.Kind, Message: failure.Message}
	if failure.RetryAfter > 0 {
		body.RetryAfter = int(failure.RetryAfter.Seconds())
	}
	return body
}

func (h *Handler) writeError(w http.ResponseWriter, err error) {
	failure := FailureOf(serviceError(err, "request"))
	if failure.Detail != "" && h.Log != nil {
		h.Log("exa: %s refused: %s (%s)", failure.Endpoint, failure.Message, failure.Detail)
	}
	body := errorBody(failure)
	if body.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfter))
	}
	status := http.StatusBadGateway
	switch failure.Kind {
	case FailureBadRequest:
		status = http.StatusBadRequest
	case FailureNotFound:
		status = http.StatusNotFound
	case FailureRateLimited:
		status = http.StatusTooManyRequests
	case FailureNotConfigured:
		status = http.StatusServiceUnavailable
	case FailureTimeout:
		status = http.StatusGatewayTimeout
	case FailureUngrounded, FailurePartial, FailureConflict:
		status = http.StatusUnprocessableEntity
	}
	h.writeJSON(w, status, map[string]any{"error": body})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
