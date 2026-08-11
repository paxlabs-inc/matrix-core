// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MountBeta registers the beta-launch admin routes onto mux under "/admin/".
// Called by Mount after the core user routes are registered.
func (h *Handler) MountBeta(mux *http.ServeMux) {
	mux.HandleFunc("/admin/invites", h.handleInvitesCollection)
	mux.HandleFunc("/admin/invites/", h.handleInviteItem)
	mux.HandleFunc("/admin/reports", h.handleReportsCollection)
	mux.HandleFunc("/admin/reports/", h.handleReportItem)
}

// --- Invite management --------------------------------------------------

// CreateInviteRequest is the POST /admin/invites body.
type CreateInviteRequest struct {
	MaxRedemptions int        `json:"max_redemptions,omitempty"` // default 1
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`      // RFC3339
}

// InviteResponse is the JSON shape for a single invite code.
type InviteResponse struct {
	Code            string     `json:"code"`
	MaxRedemptions  int        `json:"max_redemptions"`
	RedemptionsUsed int        `json:"redemptions_used"`
	Remaining       int        `json:"remaining"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedBy       string     `json:"created_by,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

func (h *Handler) handleInvitesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.createInvite(w, r)
	case http.MethodGet:
		h.listInvites(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	var req CreateInviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.MaxRedemptions <= 0 {
		req.MaxRedemptions = 1
	}

	code, err := generateInviteCode()
	if err != nil {
		h.logf("generate invite entropy: %v", err)
		http.Error(w, "could not generate code", http.StatusInternalServerError)
		return
	}
	createdBy := r.Header.Get("X-Admin-User")
	if createdBy == "" {
		createdBy = "admin"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := h.DB.GenerateInviteCode(ctx, code, req.MaxRedemptions, req.ExpiresAt, createdBy); err != nil {
		h.logf("generate invite: %v", err)
		http.Error(w, "generate invite: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"code":            code,
		"max_redemptions": req.MaxRedemptions,
		"expires_at":      req.ExpiresAt,
		"created_by":      createdBy,
	})
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	codes, err := h.DB.ListInviteCodes(ctx)
	if err != nil {
		h.logf("list invites: %v", err)
		http.Error(w, "list invites: "+err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]InviteResponse, 0, len(codes))
	for _, c := range codes {
		out = append(out, InviteResponse{
			Code:            c.Code,
			MaxRedemptions:  c.MaxRedemptions,
			RedemptionsUsed: c.RedemptionsUsed,
			Remaining:       c.MaxRedemptions - c.RedemptionsUsed,
			ExpiresAt:       c.ExpiresAt,
			CreatedBy:       c.CreatedBy,
			CreatedAt:       c.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (h *Handler) handleInviteItem(w http.ResponseWriter, r *http.Request) {
	// /admin/invites/{code} — GET returns the invite with redemptions.
	code := strings.TrimPrefix(r.URL.Path, "/admin/invites/")
	if code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	codes, err := h.DB.ListInviteCodes(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, c := range codes {
		if c.Code == code {
			writeJSON(w, http.StatusOK, InviteResponse{
				Code:            c.Code,
				MaxRedemptions:  c.MaxRedemptions,
				RedemptionsUsed: c.RedemptionsUsed,
				Remaining:       c.MaxRedemptions - c.RedemptionsUsed,
				ExpiresAt:       c.ExpiresAt,
				CreatedBy:       c.CreatedBy,
				CreatedAt:       c.CreatedAt,
			})
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// --- Bug report triage --------------------------------------------------

func (h *Handler) handleReportsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	reports, err := h.DB.ListBugReports(ctx)
	if err != nil {
		h.logf("list reports: %v", err)
		http.Error(w, "list reports: "+err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(reports))
	for _, br := range reports {
		out = append(out, map[string]any{
			"id":             br.ID,
			"user_id":        br.UserID,
			"message":        br.Message,
			"context":        json.RawMessage(br.Context),
			"attachment_ref": br.AttachmentRef,
			"status":         br.Status,
			"created_at":     br.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

func (h *Handler) handleReportItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/admin/reports/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	br, err := h.DB.GetBugReport(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if br == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":             br.ID,
		"user_id":        br.UserID,
		"message":        br.Message,
		"context":        json.RawMessage(br.Context),
		"attachment_ref": br.AttachmentRef,
		"status":         br.Status,
		"created_at":     br.CreatedAt,
	})
}

// generateInviteCode produces a 24-character hex string (12 bytes of
// crypto-strong entropy). 2^96 possibilities — unguessable. Returns an
// error if the system CSPRNG fails, so a degraded-entropy (e.g. all-zero)
// code can never be issued.
func generateInviteCode() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
