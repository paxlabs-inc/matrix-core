// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// wake.go — POST /internal/wake: the scheduler's wake door.
//
// chronosd (the centralized agent scheduler) calls this when a durable alarm
// fires. The router is the ONLY component that knows how to reach a user's Fly
// Machine, so Chronos delegates the hard part here and reuses the exact wake
// path the public proxy already trusts:
//
//	resolve user_id → DB lookup (state-checked) → fly.EnsureStarted →
//	waitDaemonReady → POST the daemon's /chat over Fly 6PN
//
// Auth is a shared wake token (constant-time bearer), enforced by the caller
// (main wraps this handler). The endpoint lives on the router's INTERNAL
// listener (:8088) alongside /admin, never the public one.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"matrix/router/internal/db"
	"matrix/router/internal/fly"
)

// WakeRequest is the POST /internal/wake body sent by chronosd.
type WakeRequest struct {
	UserID         string          `json:"user_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Message        string          `json:"message"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	AlarmID        string          `json:"alarm_id,omitempty"`
	Origin         string          `json:"origin,omitempty"`
}

// WakeHandler returns the HTTP handler for POST /internal/wake. It resolves the
// target user's Machine, wakes it, waits for the daemon to bind, and forwards a
// chat turn to the daemon's /chat endpoint. The caller is responsible for
// authenticating the request (wake token).
func (h *Handler) WakeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req WakeRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.UserID = strings.TrimSpace(req.UserID)
		req.Message = strings.TrimSpace(req.Message)
		if req.UserID == "" {
			http.Error(w, "user_id is required", http.StatusBadRequest)
			return
		}
		if req.Message == "" {
			http.Error(w, "message is required", http.StatusBadRequest)
			return
		}

		user, err := h.DB.LookupForRoute(r.Context(), req.UserID)
		if err != nil {
			if errors.Is(err, db.ErrUserNotFound) {
				http.Error(w, "user not provisioned", http.StatusNotFound)
				return
			}
			h.Logf("wake: db lookup %s: %v", req.UserID, err)
			http.Error(w, "db lookup error", http.StatusInternalServerError)
			return
		}
		switch user.State {
		case db.StateActive:
			// continue
		case db.StateProvisioning:
			http.Error(w, "user provisioning; retry shortly", http.StatusServiceUnavailable)
			return
		case db.StateSuspended:
			http.Error(w, "user suspended", http.StatusUnavailableForLegalReasons)
			return
		case db.StateDeleted:
			http.Error(w, "user deleted", http.StatusGone)
			return
		default:
			http.Error(w, "user in unexpected state: "+user.State, http.StatusInternalServerError)
			return
		}
		if user.FlyMachineID == "" {
			http.Error(w, "user has no machine attached", http.StatusServiceUnavailable)
			return
		}

		// Wake the Machine (idempotent if already started).
		wakeCtx, cancel := context.WithTimeout(r.Context(), h.WakeTimeout)
		machine, err := h.Fly.EnsureStarted(wakeCtx, user.FlyMachineID, h.ProbeInterval)
		cancel()
		if err != nil {
			switch {
			case errors.Is(err, fly.ErrMachineNotFound):
				http.Error(w, "machine vanished from fly", http.StatusGone)
			case errors.Is(err, context.DeadlineExceeded):
				http.Error(w, "machine wake timed out", http.StatusGatewayTimeout)
			default:
				h.Logf("wake: ensure started %s: %v", user.FlyMachineID, err)
				http.Error(w, "machine not ready", http.StatusBadGateway)
			}
			return
		}

		// Wait for the daemon HTTP server to accept connections post-wake.
		if err := h.waitDaemonReady(r.Context(), machine); err != nil {
			h.Logf("wake: daemon readiness %s: %v", user.FlyMachineID, err)
			w.Header().Set("Retry-After", "3")
			http.Error(w, "daemon waking; retry shortly", http.StatusServiceUnavailable)
			return
		}

		status, body, err := h.deliverChat(r.Context(), machine, req)
		if err != nil {
			h.Logf("wake: deliver chat %s: %v", user.FlyMachineID, err)
			http.Error(w, "wake delivery failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Relay the daemon's response so chronosd can record an honest outcome.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// deliverChat POSTs the wake turn to the daemon's /chat endpoint over 6PN,
// mirroring the public proxy's forwarding hygiene (X-Matrix-User, no inbound
// Authorization). Returns the daemon's status + body.
func (h *Handler) deliverChat(ctx context.Context, machine *fly.Machine, req WakeRequest) (int, []byte, error) {
	chatURL, err := chatURL(machine, h.DaemonPort)
	if err != nil {
		return 0, nil, err
	}
	payload, err := json.Marshal(chatBody{
		Message:        req.Message,
		ConversationID: req.ConversationID,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal chat body: %w", err)
	}

	// The daemon's pipeline can take a while to accept + dispatch; give the
	// delivery its own generous deadline independent of the wake budget.
	postCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(postCtx, http.MethodPost, chatURL, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Matrix-User", req.UserID)
	httpReq.Header.Set("X-Matrix-Wake-Origin", originOr(req.Origin))
	if req.AlarmID != "" {
		httpReq.Header.Set("X-Matrix-Wake-Alarm", req.AlarmID)
	}

	client := &http.Client{Timeout: 50 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, body, fmt.Errorf("daemon /chat returned %d", resp.StatusCode)
	}
	return resp.StatusCode, body, nil
}

// chatBody is the subset of the daemon's POST /chat request the wake path sends.
type chatBody struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func originOr(o string) string {
	if o == "" {
		return "chronos"
	}
	return o
}

// chatURL composes the daemon's /chat URL using the same host-resolution rules
// (bracketed private IPv6 or fly internal DNS fallback) as buildUpstreamURL.
func chatURL(m *fly.Machine, port string) (string, error) {
	probe := &http.Request{URL: &url.URL{Path: "/chat"}}
	u, err := buildUpstreamURL(m, port, probe)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
