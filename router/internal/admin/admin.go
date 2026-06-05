// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package admin implements the matrix-router /admin/* endpoints used
// by operators (and eventually the signup webhook) to provision,
// suspend, restore, and destroy user Machines.
//
// Endpoints (all bearer-token auth via mw.Admin upstream):
//
//	POST   /admin/users              create-or-touch user + ensure Machine + volume
//	POST   /admin/users/{id}/suspend set state=suspended
//	POST   /admin/users/{id}/restore set state=active
//	DELETE /admin/users/{id}         destroy Machine + state=deleted
//	GET    /admin/users/{id}         lookup row (debug aid)
//
// Provisioning is synchronous in v1 (the request blocks while we POST
// to api.machines.dev). v1 wakes < 1 user concurrently per box; if
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
	"matrix/router/internal/fly"
)

// Logf is the optional log sink used by handlers. Cmd/main.go wires
// to os.Stderr.
type Logf func(format string, args ...interface{})

// Handler bundles dependencies the admin routes share.
type Handler struct {
	DB               *db.DB
	Fly              *fly.Client
	DefaultRegion    string
	DaemonImage      string            // e.g. "registry.fly.io/matrix-daemon:dev"
	VolumeSizeGB     int               // e.g. 5
	MachineEnv       map[string]string // baseline env for every Machine
	ProvisionTimeout time.Duration     // budget per provision call
	Log              Logf

	// inflight dedupes concurrent StartProvision calls per user id so a
	// burst of first requests provisions exactly one Machine.
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
}

// CreateUserRequest is the POST /admin/users body.
type CreateUserRequest struct {
	SupabaseUserID string `json:"supabase_user_id"`
	Email          string `json:"email,omitempty"`
	Handle         string `json:"handle,omitempty"`
	Region         string `json:"region,omitempty"` // override DefaultRegion
}

// CreateUserResponse mirrors enough of the user row for the operator
// to confirm provisioning landed.
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
		FlyMachineID: user.FlyMachineID,
		FlyVolumeID:  user.FlyVolumeID,
		Region:       user.FlyRegion,
	})
}

// EnsureMachine idempotently makes sure userID has a row plus an
// attached Fly Machine, provisioning a Volume + Machine when absent.
// Returns the resulting user row and whether a Machine was provisioned
// in this call. Shared by the admin POST /admin/users handler and the
// proxy's first-request auto-provisioning path.
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

	// 2. Idempotent: a Machine already attached -> return it.
	user, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("db lookup: %w", err)
	}
	if user.FlyMachineID != "" {
		return user, false, nil
	}

	// 3. Queue a provision_jobs row for the paper trail.
	jobID, err := h.DB.QueueProvisionJob(ctx, userID, "create")
	if err != nil {
		return nil, false, fmt.Errorf("db queue: %w", err)
	}

	// 4. Provision Volume + Machine via the Fly Machines API.
	vol, mach, provErr := h.provisionMachine(ctx, userID, region)
	if provErr != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", provErr.Error(), nil)
		_ = h.DB.SetUserState(ctx, userID, db.StateFailed)
		return nil, false, fmt.Errorf("provision: %w", provErr)
	}

	// 5. Bind Machine + Volume to the row and flip to active.
	if err := h.DB.AttachMachine(ctx, userID, mach.ID, vol.ID, region); err != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", "attach: "+err.Error(), nil)
		return nil, false, fmt.Errorf("attach: %w", err)
	}

	// 6. Mark the job done with the Fly response captured for forensics.
	machJSON, _ := json.Marshal(map[string]any{"machine": mach, "volume": vol})
	if err := h.DB.FinishProvisionJob(ctx, jobID, "done", "", machJSON); err != nil {
		h.logf("finish job %d: %v (non-fatal)", jobID, err)
	}

	user, err = h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		return nil, true, fmt.Errorf("post-attach lookup: %w", err)
	}
	return user, true, nil
}

// StartProvision triggers EnsureMachine for userID out-of-band and
// returns immediately, so the proxy can auto-provision on a first
// authenticated request without blocking the response. Concurrent calls
// for the same user are deduplicated via inflight, so a burst of first
// requests provisions exactly one Machine.
func (h *Handler) StartProvision(userID, email string) {
	if _, busy := h.inflight.LoadOrStore(userID, struct{}{}); busy {
		return
	}
	go func() {
		defer h.inflight.Delete(userID)
		ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
		defer cancel()
		if _, _, err := h.EnsureMachine(ctx, userID, email, "", h.DefaultRegion); err != nil {
			h.logf("auto-provision %s: %v", userID, err)
		}
	}()
}

// provisionMachine creates a new Volume + Machine in region. The
// Machine config bakes in MATRIX_USER_ID + MATRIX_S3_* env so the
// daemon's BootPull (executor/internal/snapshot) hits the right
// snapshot prefix on first boot.
func (h *Handler) provisionMachine(ctx context.Context, userID, region string) (*fly.Volume, *fly.Machine, error) {
	if h.DaemonImage == "" {
		return nil, nil, errors.New("admin: DaemonImage not configured (set ROUTER_DAEMON_IMAGE env)")
	}
	volSize := h.VolumeSizeGB
	if volSize <= 0 {
		volSize = 5
	}

	// Volume name must be [a-z0-9_] only and ≤30 chars per Fly API.
	// "matrix_" prefix (7) leaves 23 chars for the user id; we map
	// hyphens to underscores so UUIDs round-trip cleanly.
	volName := "matrix_" + volumeSafeName(userID, 23)
	vol, err := h.Fly.CreateVolume(ctx, &fly.CreateVolumeRequest{
		Name:   volName,
		Region: region,
		SizeGB: volSize,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create volume: %w", err)
	}

	env := map[string]string{
		"MATRIX_USER_ID":  userID,
		"MATRIX_DATA_DIR": "/data",
	}
	for k, v := range h.MachineEnv {
		env[k] = v
	}

	mreq := &fly.CreateMachineRequest{
		Name:   "matrix-" + safeName(userID, 26),
		Region: region,
		Config: fly.CreateMachineConfig{
			Image: h.DaemonImage,
			Env:   env,
			Mounts: []fly.CreateMachineMount{
				{Volume: vol.ID, Path: "/data"},
			},
			Guest: &fly.CreateMachineGuest{
				CPUs:     1,
				MemoryMB: 1024,
				CPUKind:  "shared",
			},
			Restart: &fly.CreateMachineRestart{Policy: "on-failure"},
		},
	}
	mach, err := h.Fly.CreateMachine(ctx, mreq)
	if err != nil {
		// Best-effort cleanup: leave the volume — it's billed but
		// reattachable, and an operator can manually reap it via
		// `flyctl volumes destroy`.
		return vol, nil, fmt.Errorf("create machine: %w", err)
	}
	return vol, mach, nil
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
	if u.FlyMachineID != "" {
		if err := h.Fly.DestroyMachine(ctx, u.FlyMachineID, true); err != nil && !errors.Is(err, fly.ErrMachineNotFound) {
			h.logf("destroy machine %s: %v (continuing)", u.FlyMachineID, err)
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

// safeName returns a DNS-safe lowercase prefix of s, max length n.
// Used so machine names don't blow up Fly's validation. Hyphens are
// preserved; Fly machine names accept them.
func safeName(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < len(s) && len(out) < n; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		}
	}
	if len(out) == 0 {
		return "user"
	}
	return string(out)
}

// volumeSafeName returns a [a-z0-9_]-only prefix of s, max length n.
// Stricter than safeName because Fly volume names reject hyphens
// ("name only allows lowercase alphanumeric characters and underscores
// with at most 30 characters" — observed 400 from POST /v1/apps/<app>/volumes).
// Hyphens are mapped to underscores so UUIDs round-trip cleanly.
func volumeSafeName(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < len(s) && len(out) < n; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '_':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == '-':
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "user"
	}
	return string(out)
}

// writeJSON marshals v + writes as application/json with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
