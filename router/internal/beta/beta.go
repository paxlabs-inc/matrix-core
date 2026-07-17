// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

// Package beta implements the user-facing beta-launch endpoints on the
// matrix-router public listener. All routes are JWT-authed (via mw.JWT
// upstream) and act only on the authenticated caller's own records.
//
// Routes:
//
//	GET  /consent               read training consent
//	PUT  /consent               update training consent
//	POST /disclosure/ack        record beta disclosure acknowledgement
//	POST /onboarding/start      verify approvals and start provisioning
//	GET  /subscription          read current subscription access
//	POST /subscription/claim    claim the one-time launch entitlement
//	POST /reports               submit a bug/feedback report
//	GET  /provision/status      read the caller's provisioning state
package beta

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"matrix/router/internal/db"
	"matrix/router/internal/proxy"
)

// Handler bundles dependencies for the beta user routes.
type Handler struct {
	DB        *db.DB
	Log       func(string, ...interface{})
	Provision proxy.Provisioner // optional: starts provisioning after first-run approvals
}

// Mount registers the beta user routes on mux. The mux MUST be wrapped
// by mw.JWT upstream so that proxy.Subject(ctx) is populated.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/consent", h.handleConsent)
	mux.HandleFunc("/disclosure/ack", h.handleDisclosureAck)
	mux.HandleFunc("/onboarding/start", h.handleOnboardingStart)
	mux.HandleFunc("/subscription", h.handleSubscription)
	mux.HandleFunc("/subscription/claim", h.handleSubscriptionClaim)
	mux.HandleFunc("/reports", h.handleReports)
	mux.HandleFunc("/provision/status", h.handleProvisionStatus)
}

// --- First-run approvals ------------------------------------------------

func (h *Handler) handleOnboardingStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	approved, err := h.DB.HasCompletedFirstRunApprovals(ctx, sub, db.PublicLaunchDisclosureVersion)
	if err != nil {
		h.logf("first-run approvals: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !approved {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "first-run approvals required"})
		return
	}
	if h.Provision != nil {
		h.Provision.StartProvision(sub, proxy.Email(r.Context()))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "provisioning"})
}

// --- Consent ------------------------------------------------------------

type consentRequest struct {
	TrainingOptIn bool   `json:"training_opt_in"`
	PolicyVersion string `json:"policy_version"`
}

func (h *Handler) handleConsent(w http.ResponseWriter, r *http.Request) {
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.getConsent(w, r, sub)
	case http.MethodPut:
		h.putConsent(w, r, sub)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (h *Handler) getConsent(w http.ResponseWriter, r *http.Request, sub string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	c, err := h.DB.GetConsent(ctx, sub)
	if err != nil {
		h.logf("get consent: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if c == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id":         sub,
			"training_opt_in": false,
			"policy_version":  "",
			"decided_at":      nil,
			"has_consented":   false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":         c.UserID,
		"training_opt_in": c.TrainingOptIn,
		"policy_version":  c.PolicyVersion,
		"decided_at":      c.DecidedAt,
		"has_consented":   true,
	})
}

func (h *Handler) putConsent(w http.ResponseWriter, r *http.Request, sub string) {
	var req consentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if req.PolicyVersion == "" {
		req.PolicyVersion = "1"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.DB.UpsertConsent(ctx, sub, req.TrainingOptIn, req.PolicyVersion); err != nil {
		h.logf("upsert consent: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":         sub,
		"training_opt_in": req.TrainingOptIn,
		"policy_version":  req.PolicyVersion,
	})
}

// --- Disclosure ack -----------------------------------------------------

type disclosureAckRequest struct {
	DisclosureVersion string `json:"disclosure_version"`
}

func (h *Handler) handleDisclosureAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	var req disclosureAckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(req.DisclosureVersion) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "disclosure_version required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.DB.InsertDisclosureAck(ctx, sub, req.DisclosureVersion); err != nil {
		h.logf("disclosure ack: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

// --- Subscription -------------------------------------------------------

func (h *Handler) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	entitlement, err := h.DB.GetLaunchEntitlement(ctx, sub)
	if err != nil {
		h.logf("get subscription: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeSubscription(w, sub, entitlement)
}

func (h *Handler) handleSubscriptionClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	approved, err := h.DB.HasCompletedFirstRunApprovals(ctx, sub, db.PublicLaunchDisclosureVersion)
	if err != nil {
		h.logf("claim approvals: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !approved {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "first-run approvals required"})
		return
	}
	entitlement, err := h.DB.ClaimLaunchEntitlement(ctx, sub, 48*time.Hour)
	if err != nil {
		h.logf("claim subscription: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeSubscription(w, sub, entitlement)
}

func writeSubscription(w http.ResponseWriter, userID string, entitlement *db.LaunchEntitlement) {
	if entitlement == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id": userID,
			"plan":    "free",
			"status":  "available",
		})
		return
	}
	status := "active"
	if !entitlement.EndsAt.After(time.Now()) {
		status = "expired"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":    entitlement.UserID,
		"plan":       entitlement.Plan,
		"source":     entitlement.Source,
		"status":     status,
		"starts_at":  entitlement.StartsAt,
		"ends_at":    entitlement.EndsAt,
		"claimed_at": entitlement.ClaimedAt,
	})
}

// --- Bug reports --------------------------------------------------------

type reportRequest struct {
	Message       string          `json:"message"`
	Context       json.RawMessage `json:"context"`
	AttachmentRef string          `json:"attachment_ref,omitempty"`
}

func (h *Handler) handleReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	var req reportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message required"})
		return
	}
	if len(req.Context) == 0 {
		req.Context = json.RawMessage(`{}`)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id, err := h.DB.InsertBugReport(ctx, sub, req.Message, req.Context, req.AttachmentRef)
	if err != nil {
		h.logf("insert report: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      id,
		"status":  "submitted",
		"message": "Report received. Thank you.",
	})
}

// --- Provisioning status ------------------------------------------------

func (h *Handler) handleProvisionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	sub := proxy.Subject(r.Context())
	if sub == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	user, err := h.DB.LookupForRoute(ctx, sub)
	state := "none"
	if err != nil {
		if !errors.Is(err, db.ErrUserNotFound) {
			h.logf("provision status: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	} else {
		state = user.State
		if state == "" {
			state = "none"
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"state":   state,
		"user_id": sub,
	})
}

// --- helpers ------------------------------------------------------------

func (h *Handler) logf(format string, args ...interface{}) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
