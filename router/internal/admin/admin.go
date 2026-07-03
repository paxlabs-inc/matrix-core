// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package admin implements the matrix-router /admin/* endpoints used
// by operators (and eventually the signup webhook) to provision,
// suspend, restore, and destroy user environments.
//
// Endpoints (all bearer-token auth via mw.Admin upstream):
//
//	POST   /admin/users              create-or-touch user + ensure environment + volume
//	POST   /admin/users/{id}/suspend set state=suspended
//	POST   /admin/users/{id}/restore set state=active
//	DELETE /admin/users/{id}         destroy environment + state=deleted
//	GET    /admin/users/{id}         lookup row (debug aid)
//
// Provisioning is synchronous in v1 (the request blocks while we call
// the provider API). v1 wakes < 1 user concurrently per box; if
// that becomes a bottleneck the provision_jobs row is already queued
// so a background worker can take over.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"matrix/router/internal/db"
	"matrix/router/internal/provision"
)

// Logf is the optional log sink used by handlers. Cmd/main.go wires
// to os.Stderr.
type Logf func(format string, args ...interface{})

// Handler bundles dependencies the admin routes share.
type Handler struct {
	DB               *db.DB
	Prov             provision.Provisioner
	Provider         string // 'fly' | 'railway' — recorded on attached rows; empty defaults to 'fly'
	DefaultRegion    string
	MachineEnv       map[string]string // baseline env for every environment
	ProvisionTimeout time.Duration     // budget per provision call
	Log              Logf

	// inflight dedupes concurrent StartProvision calls per user id so a
	// burst of first requests provisions exactly one environment.
	inflight sync.Map
}

// Mount registers the admin routes onto mux under "/admin/".
//
// Handler bodies are JSON; on success they return 200 + a JSON
// snapshot of the user row. On error they return text (operator
// debugging) with the appropriate status.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/admin/users", h.handleUsersCollection)
	mux.HandleFunc("/admin/users/", h.handleUserItem)
	h.MountBeta(mux)
}

// CreateUserRequest is the POST /admin/users body.
type CreateUserRequest struct {
	SupabaseUserID string `json:"supabase_user_id"`
	Email          string `json:"email,omitempty"`
	Handle         string `json:"handle,omitempty"`
	Region         string `json:"region,omitempty"` // override DefaultRegion
}

// CreateUserResponse mirrors enough of the user row for the operator
// to confirm provisioning landed. The fly_-prefixed JSON keys are kept
// for operator-tooling compatibility; they carry the active provider's
// environment/volume ids.
type CreateUserResponse struct {
	UserID       string `json:"user_id"`
	State        string `json:"state"`
	FlyMachineID string `json:"fly_machine_id,omitempty"`
	FlyVolumeID  string `json:"fly_volume_id,omitempty"`
	Region       string `json:"region,omitempty"`
	JobID        int64  `json:"job_id,omitempty"`
}

func (h *Handler) handleUsersCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req CreateUserRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.SupabaseUserID) == "" {
		http.Error(w, "supabase_user_id required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout())
	defer cancel()

	user, created, err := h.EnsureMachine(ctx, req.SupabaseUserID, req.Email, req.Handle, req.Region)
	if err != nil {
		h.logf("ensure machine %s: %v", req.SupabaseUserID, err)
		http.Error(w, "provision: "+err.Error(), http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, &CreateUserResponse{
		UserID:       user.ID,
		State:        user.State,
		FlyMachineID: user.EnvID,
		FlyVolumeID:  user.VolumeID,
		Region:       user.Region,
	})
}

// EnsureMachine idempotently makes sure userID has a row plus an
// attached environment, provisioning a volume + instance via the
// active provider when absent. Returns the resulting user row and
// whether an environment was provisioned in this call. Shared by the
// admin POST /admin/users handler and the proxy's first-request
// auto-provisioning path.
func (h *Handler) EnsureMachine(ctx context.Context, userID, email, handle, region string) (*db.User, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, false, errors.New("supabase user id required")
	}
	if region == "" {
		region = h.DefaultRegion
	}

	// 1. Upsert user row (lands in 'provisioning' state when new).
	if _, err := h.DB.CreateOrTouchUser(ctx, userID, email, handle); err != nil {
		return nil, false, fmt.Errorf("db upsert: %w", err)
	}

	// 2. Idempotent: an environment already attached -> return it.
	user, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("db lookup: %w", err)
	}
	if user.EnvID != "" {
		return user, false, nil
	}

	// 3. Queue a provision_jobs row for the paper trail.
	jobID, err := h.DB.QueueProvisionJob(ctx, userID, "create")
	if err != nil {
		return nil, false, fmt.Errorf("db queue: %w", err)
	}

	// 4. Provision volume + instance via the provider API. The env block
	//    bakes in MATRIX_USER_ID + MATRIX_S3_* so the daemon's BootPull
	//    (executor/internal/snapshot) hits the right snapshot prefix on
	//    first boot.
	env, provErr := h.Prov.Ensure(ctx, provision.CreateRequest{
		UserID: userID,
		Region: region,
		Env:    h.instanceEnv(userID),
	})
	if provErr != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", provErr.Error(), nil)
		_ = h.DB.SetUserState(ctx, userID, db.StateFailed)
		return nil, false, fmt.Errorf("provision: %w", provErr)
	}

	// 5. Bind environment + volume to the row and flip to active.
	if err := h.DB.AttachMachine(ctx, userID, h.Provider, env.ID, env.VolumeID, region); err != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", "attach: "+err.Error(), nil)
		return nil, false, fmt.Errorf("attach: %w", err)
	}

	// 6. Mark the job done with the provider response captured for forensics.
	envJSON, _ := json.Marshal(map[string]any{"env": env})
	if err := h.DB.FinishProvisionJob(ctx, jobID, "done", "", envJSON); err != nil {
		h.logf("finish job %d: %v (non-fatal)", jobID, err)
	}

	user, err = h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		return nil, true, fmt.Errorf("post-attach lookup: %w", err)
	}
	return user, true, nil
}

// instanceEnv builds the per-instance environment-variable set: the
// user identity plus the operator baseline (MachineEnv).
func (h *Handler) instanceEnv(userID string) map[string]string {
	env := map[string]string{
		"MATRIX_USER_ID":  userID,
		"MATRIX_DATA_DIR": "/data",
	}
	for k, v := range h.MachineEnv {
		env[k] = v
	}
	return env
}

// StartProvision triggers EnsureMachine for userID out-of-band and
// returns immediately, so the proxy can auto-provision on a first
// authenticated request without blocking the response. Concurrent calls
// for the same user are deduplicated via inflight, so a burst of first
// requests provisions exactly one environment.
func (h *Handler) StartProvision(userID, email string) {
	if _, busy := h.inflight.LoadOrStore(userID, struct{}{}); busy {
		return
	}
	go func() {
		defer h.inflight.Delete(userID)
		ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
		defer cancel()
		// Defense-in-depth invite gate (req 3.5/9.1): the out-of-band
		// provisioning path refuses to create an environment for a user
		// with no redeemed invite, even if a caller forgot to pre-check.
		// The operator override (admin POST /admin/users) calls
		// EnsureMachine directly and is intentionally not gated here.
		redeemed, err := h.DB.HasRedeemedInvite(ctx, userID)
		if err != nil {
			h.logf("auto-provision %s: invite check: %v", userID, err)
			return
		}
		if !redeemed {
			h.logf("auto-provision %s: no redeemed invite; refusing", userID)
			return
		}
		if _, _, err := h.EnsureMachine(ctx, userID, email, "", h.DefaultRegion); err != nil {
			h.logf("auto-provision %s: %v", userID, err)
		}
	}()
}

// handleUserItem dispatches /admin/users/{id}[/{action}].
func (h *Handler) handleUserItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	if rest == "" {
		http.Error(w, "user id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	userID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout())
	defer cancel()

	switch {
	case action == "" && r.Method == http.MethodGet:
		h.getUser(ctx, w, userID)
	case action == "" && r.Method == http.MethodDelete:
		h.deleteUser(ctx, w, userID)
	case action == "suspend" && r.Method == http.MethodPost:
		h.setState(ctx, w, userID, db.StateSuspended)
	case action == "restore" && r.Method == http.MethodPost:
		h.setState(ctx, w, userID, db.StateActive)
	default:
		http.Error(w, fmt.Sprintf("unknown action %q (or wrong method %s)", action, r.Method), http.StatusNotFound)
	}
}

func (h *Handler) getUser(ctx context.Context, w http.ResponseWriter, userID string) {
	u, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *Handler) setState(ctx context.Context, w http.ResponseWriter, userID, state string) {
	if err := h.DB.SetUserState(ctx, userID, state); err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"user_id": userID, "state": state,
	})
}

func (h *Handler) deleteUser(ctx context.Context, w http.ResponseWriter, userID string) {
	u, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u.EnvID != "" {
		ref := provision.Ref{UserID: u.ID, EnvID: u.EnvID, VolumeID: u.VolumeID}
		if err := h.Prov.Destroy(ctx, ref); err != nil && !errors.Is(err, provision.ErrNotFound) {
			h.logf("destroy environment %s: %v (continuing)", u.EnvID, err)
		}
	}
	if err := h.DB.SetUserState(ctx, userID, db.StateDeleted); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"user_id": userID, "state": db.StateDeleted,
	})
}

// timeout returns ProvisionTimeout or a 60s default.
func (h *Handler) timeout() time.Duration {
	if h.ProvisionTimeout > 0 {
		return h.ProvisionTimeout
	}
	return 60 * time.Second
}

func (h *Handler) logf(format string, args ...interface{}) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

// writeJSON marshals v + writes as application/json with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
