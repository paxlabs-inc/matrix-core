// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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
//	POST   /admin/workforce/reconcile reconcile every eligible Railway runtime
//	POST   /admin/neo/reconcile       reconcile Neo variables on every eligible Railway runtime
//
// Provisioning is synchronous in v1 (the request blocks while we call
// the provider API). v1 wakes < 1 user concurrently per box; if
// that becomes a bottleneck the provision_jobs row is already queued
// so a background worker can take over.
package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"matrix/router/internal/db"
	"matrix/router/internal/provision"
	"matrix/router/internal/workforceauth"
)

// Logf is the optional log sink used by handlers. Cmd/main.go wires
// to os.Stderr.
type Logf func(format string, args ...interface{})

// Handler bundles dependencies the admin routes share.
type Handler struct {
	DB             *db.DB
	Prov           provision.Provisioner
	ShardProviders interface {
		Provider(string) (provision.Provisioner, bool)
	}
	Provider             string // 'fly' | 'railway' — recorded on attached rows; empty defaults to 'fly'
	DefaultRegion        string
	MachineEnv           map[string]string // baseline env for every environment
	Workforce            *workforceauth.Deriver
	WorkforcePostgresURI string
	ProvisionTimeout     time.Duration // budget per provision call
	Log                  Logf

	// inflight dedupes concurrent StartProvision calls per user id so a
	// burst of first requests provisions exactly one environment.
	inflight sync.Map
	// Workforce reconciliation is operator-triggered and provider-mutating.
	// These guards prevent overlapping fleet runs and duplicate writes for the
	// same user when a single-user command races a fleet rollout.
	workforceReconcileMu       sync.Mutex
	workforceReconcileInflight sync.Map
	neoReconcileMu             sync.Mutex
	neoReconcileInflight       sync.Map
}

// Mount registers the admin routes onto mux under "/admin/".
//
// Handler bodies are JSON; on success they return 200 + a JSON
// snapshot of the user row. On error they return text (operator
// debugging) with the appropriate status.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/admin/users", h.handleUsersCollection)
	mux.HandleFunc("/admin/users/", h.handleUserItem)
	mux.HandleFunc("/admin/workforce/reconcile", h.handleWorkforceFleetReconcile)
	mux.HandleFunc("/admin/neo/reconcile", h.handleNeoFleetReconcile)
	mux.HandleFunc("/admin/shards", h.handleShards)
	mux.HandleFunc("/admin/shards/", h.handleShard)
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

	// 2. Return the existing attached environment without mutating provider
	//    configuration from a request path.
	user, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("db lookup: %w", err)
	}
	if user.EnvID != "" {
		return user, false, nil
	}

	// 3. Queue a provision_jobs row for the paper trail.
	prov := h.Prov
	if h.Provider == "railway" && h.ShardProviders != nil {
		allocation, reserveErr := h.DB.ReserveRailwayShard(ctx, userID)
		if reserveErr != nil {
			return nil, false, reserveErr
		}
		var ok bool
		prov, ok = h.ShardProviders.Provider(allocation.ShardID)
		if !ok {
			return nil, false, fmt.Errorf("assigned shard %q is not configured", allocation.ShardID)
		}
		_ = h.DB.SetAllocationState(ctx, userID, "provisioning", false)
	}
	jobID, err := h.DB.QueueProvisionJob(ctx, userID, "create")
	if err != nil {
		return nil, false, fmt.Errorf("db queue: %w", err)
	}

	// 4. Provision volume + instance via the provider API. The env block
	//    bakes in MATRIX_USER_ID + MATRIX_S3_* so the daemon's BootPull
	//    (executor/internal/snapshot) hits the right snapshot prefix on
	//    first boot.
	instanceEnv, err := h.instanceEnv(userID)
	if err != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", err.Error(), nil)
		_ = h.DB.SetUserState(ctx, userID, db.StateFailed)
		return nil, false, err
	}
	createReq := provision.CreateRequest{
		UserID: userID,
		Region: region,
		Env:    instanceEnv,
	}
	var env *provision.Env
	var provErr error
	var railwayOp *db.RailwayOperation
	if h.Provider == "railway" && h.ShardProviders != nil {
		railwayOp, provErr = h.DB.BeginRailwayOperation(ctx, userID, "ensure")
		if provErr == nil {
			recoverable, ok := prov.(provision.Recoverable)
			if !ok {
				provErr = errors.New("assigned Railway provisioner has no recovery surface")
			} else {
				env, provErr = h.ensureRailway(ctx, railwayOp, recoverable, createReq)
			}
		}
	} else {
		env, provErr = prov.Ensure(ctx, createReq)
	}
	if provErr != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", provErr.Error(), nil)
		_ = h.DB.SetUserState(ctx, userID, db.StateFailed)
		if h.Provider == "railway" && h.ShardProviders != nil {
			_ = h.DB.SetAllocationState(ctx, userID, "cleanup_pending", false)
		}
		return nil, false, fmt.Errorf("provision: %w", provErr)
	}

	// 5. Bind environment + volume to the row and flip to active.
	if err := h.DB.AttachMachine(ctx, userID, h.Provider, env.ID, env.VolumeID, region); err != nil {
		_ = h.DB.FinishProvisionJob(ctx, jobID, "failed", "attach: "+err.Error(), nil)
		return nil, false, fmt.Errorf("attach: %w", err)
	}
	if h.Provider == "railway" && h.ShardProviders != nil {
		if err := h.DB.SetAllocationState(ctx, userID, "active", false); err != nil {
			return nil, false, fmt.Errorf("activate allocation: %w", err)
		}
		if err := h.DB.FinishRailwayOperation(ctx, railwayOp.ID, "succeeded", "ready_and_attached", ""); err != nil {
			return nil, false, err
		}
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

func (h *Handler) ensureRailway(ctx context.Context, op *db.RailwayOperation, p provision.Recoverable, req provision.CreateRequest) (*provision.Env, error) {
	if err := h.DB.MarkRailwayOperationRunning(ctx, op.ID, "discover_service"); err != nil {
		return nil, err
	}
	service, err := p.FindService(ctx, p.ServiceName(req.UserID))
	if errors.Is(err, provision.ErrNotFound) {
		if err := h.DB.ValidateRailwayOperation(ctx, op.ID); err != nil {
			return nil, err
		}
		service, err = p.CreateService(ctx, req)
		if err == nil {
			if recErr := h.DB.RecordRailwayService(ctx, op.ID, service.ID); recErr != nil {
				return nil, recErr
			}
		} else if errors.Is(err, provision.ErrUncertain) {
			service, err = h.discoverService(ctx, p, req.UserID)
		}
	}
	if err != nil {
		state := "cleanup_pending"
		if errors.Is(err, provision.ErrUncertain) {
			state = "unknown"
		}
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, state, "service_unresolved", err.Error())
		return nil, err
	}
	if op.ServiceID != "" && op.ServiceID != service.ID {
		err = fmt.Errorf("deterministic service identity changed from %s to %s", op.ServiceID, service.ID)
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "duplicate_service_detected", err.Error())
		return nil, err
	}
	if err := h.DB.RecordRailwayService(ctx, op.ID, service.ID); err != nil {
		return nil, err
	}

	if err := h.DB.MarkRailwayOperationRunning(ctx, op.ID, "discover_volume"); err != nil {
		return nil, err
	}
	volumeID, err := p.FindVolume(ctx, service.ID, "/data")
	if errors.Is(err, provision.ErrNotFound) {
		if err := h.DB.ValidateRailwayOperation(ctx, op.ID); err != nil {
			return nil, err
		}
		volumeID, err = p.CreateVolume(ctx, service.ID, "/data")
		if err == nil {
			if recErr := h.DB.RecordRailwayVolume(ctx, op.ID, volumeID); recErr != nil {
				return nil, recErr
			}
		} else if errors.Is(err, provision.ErrUncertain) {
			volumeID, err = h.discoverVolume(ctx, p, service.ID)
		}
	}
	if err != nil {
		state := "cleanup_pending"
		if errors.Is(err, provision.ErrUncertain) {
			state = "unknown"
		}
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, state, "volume_unresolved", err.Error())
		return nil, err
	}
	if op.VolumeID != "" && op.VolumeID != volumeID {
		err = fmt.Errorf("deterministic volume identity changed from %s to %s", op.VolumeID, volumeID)
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "duplicate_volume_detected", err.Error())
		return nil, err
	}
	if err := h.DB.RecordRailwayVolume(ctx, op.ID, volumeID); err != nil {
		return nil, err
	}
	if err := h.DB.MarkRailwayOperationRunning(ctx, op.ID, "await_readiness"); err != nil {
		return nil, err
	}
	if err := p.WaitReady(ctx, service.ID); err != nil {
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "readiness_unproven", err.Error())
		return nil, err
	}
	service.VolumeID = volumeID
	service.Ready = true
	service.State = "deployed"
	return service, nil
}

func (h *Handler) discoverService(ctx context.Context, p provision.Recoverable, userID string) (*provision.Env, error) {
	var env *provision.Env
	err := boundedReconcile(ctx, func() error {
		var err error
		env, err = p.FindService(ctx, p.ServiceName(userID))
		return err
	})
	return env, err
}

func (h *Handler) discoverVolume(ctx context.Context, p provision.Recoverable, serviceID string) (string, error) {
	var id string
	err := boundedReconcile(ctx, func() error {
		var err error
		id, err = p.FindVolume(ctx, serviceID, "/data")
		return err
	})
	return id, err
}

func boundedReconcile(ctx context.Context, probe func() error) error {
	delay := 100 * time.Millisecond
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if last = probe(); last == nil {
			return nil
		}
		if !errors.Is(last, provision.ErrNotFound) && !errors.Is(last, provision.ErrUncertain) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			delay *= 2
		}
	}
	return fmt.Errorf("%w: reconciliation exhausted: %v", provision.ErrUncertain, last)
}

var routerOnlyWorkforceVariables = [...]string{
	"ROUTER_WORKFORCE_ENABLED",
	"ROUTER_WORKFORCE_PORT",
	"ROUTER_WORKFORCE_POSTGRES_URI",
	"ROUTER_WORKFORCE_ROOT_SECRET",
	"ROUTER_WORKFORCE_WAKE_TOKEN",
}

// instanceEnv builds the per-instance environment-variable set: the
// user identity plus the operator baseline (MachineEnv).
func (h *Handler) instanceEnv(userID string) (map[string]string, error) {
	env := map[string]string{
		"MATRIX_USER_ID":  userID,
		"MATRIX_DATA_DIR": "/data",
		// codyd's preview manager registers its sandbox target under this id;
		// it MUST equal the JWT subject (== MATRIX_USER_ID) the router's
		// /preview/{user} proxy authorizes against, or previews 403.
		"CODY_USER_ID": userID,
	}
	for k, v := range h.MachineEnv {
		env[k] = v
	}
	// Router Workforce configuration is control-plane authority, never a
	// machine baseline. Scrub it defensively even if an operator accidentally
	// placed one of these names in MachineEnv; per-user runtimes receive only
	// the derived WORKFORCE_* credentials below.
	for _, name := range routerOnlyWorkforceVariables {
		delete(env, name)
	}
	if h.Workforce == nil {
		return env, nil
	}
	ownerToken, err := h.Workforce.OwnerToken(userID)
	if err != nil {
		return nil, err
	}
	wakeToken, err := h.Workforce.WakeToken(userID)
	if err != nil {
		return nil, err
	}
	runtimeKey, err := h.Workforce.RuntimePrivateKey(userID)
	if err != nil {
		return nil, err
	}
	companyIssuerKey, err := h.Workforce.CompanyIssuerPrivateKey(userID)
	if err != nil {
		return nil, err
	}
	ownerPublicKey, err := h.Workforce.BootstrapOwnerPublicKey(userID)
	if err != nil {
		return nil, err
	}
	for key, value := range map[string]string{
		"WORKFORCE_ENABLED":                    "true",
		"WORKFORCE_POSTGRES_URI":               h.WorkforcePostgresURI,
		"WORKFORCE_TENANT_ID":                  userID,
		"WORKFORCE_ORGANIZATION_ID":            "organization-" + userID,
		"WORKFORCE_OWNER_ID":                   "owner-" + userID,
		"WORKFORCE_OWNER_TOKEN":                ownerToken,
		"WORKFORCE_WAKE_TOKEN":                 wakeToken,
		"WORKFORCE_OWNER_KEY_ID":               "bootstrap-owner-v1",
		"WORKFORCE_OWNER_PUBLIC_KEY":           ownerPublicKey,
		"WORKFORCE_RUNTIME_KEY_ID":             "runtime-v1",
		"WORKFORCE_RUNTIME_PRIVATE_KEY":        runtimeKey,
		"WORKFORCE_COMPANY_ISSUER_KEY_ID":      "company-issuer-v1",
		"WORKFORCE_COMPANY_ISSUER_PRIVATE_KEY": companyIssuerKey,
		"WORKFORCE_AUDITOR_SEAT_ID":            "seat-developer-auditor",
	} {
		env[key] = value
	}
	return env, nil
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
		// Defense-in-depth public-launch gate: the out-of-band provisioning
		// path requires the same disclosure acknowledgement and explicit
		// training-data choice enforced by the public proxy.
		// The operator override (admin POST /admin/users) calls
		// EnsureMachine directly and is intentionally not gated here.
		approved, err := h.DB.HasCompletedFirstRunApprovals(
			ctx,
			userID,
			db.PublicLaunchDisclosureVersion,
		)
		if err != nil {
			h.logf("auto-provision %s: first-run approval check: %v", userID, err)
			return
		}
		if !approved {
			h.logf("auto-provision %s: first-run approvals incomplete; refusing", userID)
			return
		}
		if _, _, err := h.EnsureMachine(ctx, userID, email, "", h.DefaultRegion); err != nil {
			h.logf("auto-provision %s: %v", userID, err)
		}
	}()
}

// ResumeRailwayOperations reconciles durable non-terminal provider work after
// restart. Each operation is retried from provider discovery, never by blindly
// repeating its last mutation.
func (h *Handler) ResumeRailwayOperations(ctx context.Context) error {
	if h.Provider != "railway" || h.ShardProviders == nil {
		return nil
	}
	ops, err := h.DB.NonTerminalRailwayOperations(ctx)
	if err != nil {
		return err
	}
	for index := range ops {
		op := &ops[index]
		prov, ok := h.ShardProviders.Provider(op.ShardID)
		if !ok {
			h.logf("reconcile %s: assigned shard %s unavailable", op.OperationKey, op.ShardID)
			continue
		}
		recoverable, ok := prov.(provision.Recoverable)
		if !ok {
			h.logf("reconcile %s: provider has no recovery surface", op.OperationKey)
			continue
		}
		switch op.Kind {
		case "ensure":
			if user, lookupErr := h.DB.LookupForRoute(ctx, op.UserID); lookupErr == nil && user.EnvID != "" {
				if user.EnvID != op.ServiceID || user.VolumeID != op.VolumeID {
					_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "attached_resource_evidence_mismatch", "user attachment differs from operation")
					continue
				}
				if readyErr := recoverable.WaitReady(ctx, op.ServiceID); readyErr == nil {
					_ = h.DB.SetAllocationState(ctx, op.UserID, "active", false)
					_ = h.DB.FinishRailwayOperation(ctx, op.ID, "succeeded", "restart_ready_and_attached", "")
					continue
				}
			}
			if _, _, err := h.EnsureMachine(ctx, op.UserID, "", "", h.DefaultRegion); err != nil {
				h.logf("reconcile %s: %v", op.OperationKey, err)
			}
		case "destroy":
			if op.ServiceID == "" {
				_ = h.DB.FinishRailwayOperation(ctx, op.ID, "unknown", "destroy_missing_service_evidence", "missing service id")
				continue
			}
			ref := provision.Ref{UserID: op.UserID, EnvID: op.ServiceID, VolumeID: op.VolumeID}
			if err := h.destroyRailway(ctx, op, recoverable, ref); err != nil {
				h.logf("reconcile %s: %v", op.OperationKey, err)
				continue
			}
			if err := h.DB.SetAllocationState(ctx, op.UserID, "released", true); err != nil {
				h.logf("reconcile %s release: %v", op.OperationKey, err)
			}
		default:
			_ = h.DB.FinishRailwayOperation(ctx, op.ID, "failed", "unsupported_recovery_kind", "no recovery transition")
		}
	}
	return nil
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
	case action == "reconcile-workforce" && r.Method == http.MethodPost:
		h.reconcileWorkforceEnvironment(ctx, w, userID)
	default:
		http.Error(w, fmt.Sprintf("unknown action %q (or wrong method %s)", action, r.Method), http.StatusNotFound)
	}
}

func (h *Handler) reconcileWorkforceEnvironment(ctx context.Context, w http.ResponseWriter, userID string) {
	if h.Workforce == nil {
		http.Error(w, "workforce is disabled", http.StatusServiceUnavailable)
		return
	}
	user, err := h.DB.LookupForRoute(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.updateWorkforceEnvironment(ctx, user); err != nil {
		h.logf("reconcile Workforce environment %s: %v", userID, err)
		http.Error(w, "workforce environment reconciliation failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"user_id": userID, "status": "workforce_environment_reconciled",
	})
}

const (
	defaultWorkforceReconcileConcurrency = 3
	maxWorkforceReconcileConcurrency     = 8
	workforceFleetReconcileTimeout       = 30 * time.Minute
)

type workforceFleetReconcileRequest struct {
	Concurrency int `json:"concurrency,omitempty"`
}

type workforceFleetReconcileFailure struct {
	UserID string `json:"user_id"`
	Error  string `json:"error"`
}

type workforceFleetReconcileResponse struct {
	Eligible  int                              `json:"eligible"`
	Succeeded int                              `json:"succeeded"`
	Failed    int                              `json:"failed"`
	Failures  []workforceFleetReconcileFailure `json:"failures"`
}

func (h *Handler) handleWorkforceFleetReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Workforce == nil {
		http.Error(w, "workforce is disabled", http.StatusServiceUnavailable)
		return
	}
	if !h.workforceReconcileMu.TryLock() {
		http.Error(w, "workforce fleet reconciliation already in progress", http.StatusConflict)
		return
	}
	defer h.workforceReconcileMu.Unlock()
	var request workforceFleetReconcileRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	concurrency, err := workforceReconcileConcurrency(request.Concurrency)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), workforceFleetReconcileTimeout)
	defer cancel()
	users, err := h.DB.ListWorkforceReconcileUsers(ctx)
	if err != nil {
		h.logf("list Workforce reconciliation users: %v", err)
		http.Error(w, "workforce reconciliation user listing failed", http.StatusInternalServerError)
		return
	}
	if len(users) < concurrency {
		concurrency = len(users)
	}
	response := workforceFleetReconcileResponse{
		Eligible: len(users), Failures: make([]workforceFleetReconcileFailure, 0),
	}
	if len(users) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	type result struct {
		userID string
		err    error
	}
	jobs := make(chan db.User, len(users))
	results := make(chan result, len(users))
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for user := range jobs {
				userCtx, userCancel := context.WithTimeout(ctx, h.timeout())
				err := h.updateWorkforceEnvironment(userCtx, &user)
				userCancel()
				results <- result{userID: user.ID, err: err}
			}
		}()
	}
	for index := range users {
		jobs <- users[index]
	}
	close(jobs)
	workers.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			response.Succeeded++
			continue
		}
		response.Failed++
		response.Failures = append(response.Failures, workforceFleetReconcileFailure{
			UserID: result.userID, Error: result.err.Error(),
		})
	}
	status := http.StatusOK
	if response.Failed > 0 {
		status = http.StatusMultiStatus
	}
	h.logf(
		"Workforce fleet reconciliation eligible=%d succeeded=%d failed=%d",
		response.Eligible, response.Succeeded, response.Failed,
	)
	writeJSON(w, status, response)
}

func workforceReconcileConcurrency(requested int) (int, error) {
	if requested == 0 {
		return defaultWorkforceReconcileConcurrency, nil
	}
	if requested < 1 || requested > maxWorkforceReconcileConcurrency {
		return 0, fmt.Errorf(
			"concurrency must be between 1 and %d", maxWorkforceReconcileConcurrency,
		)
	}
	return requested, nil
}

func (h *Handler) updateWorkforceEnvironment(ctx context.Context, user *db.User) error {
	if _, busy := h.workforceReconcileInflight.LoadOrStore(user.ID, struct{}{}); busy {
		return errors.New("Workforce environment reconciliation already in progress")
	}
	defer h.workforceReconcileInflight.Delete(user.ID)
	provider := h.Prov
	if user.Provider == "railway" && h.ShardProviders != nil {
		var ok bool
		provider, ok = h.ShardProviders.Provider(user.RailwayShardID)
		if !ok {
			return fmt.Errorf("assigned shard %q is unavailable", user.RailwayShardID)
		}
	}
	updater, ok := provider.(provision.VariableUpdater)
	if !ok {
		return errors.New("provider does not support environment reconciliation")
	}
	environment, err := h.workforceReconcileEnvironment(user.ID)
	if err != nil {
		return err
	}
	ref := provision.Ref{UserID: user.ID, EnvID: user.EnvID, VolumeID: user.VolumeID}
	if err := updater.UpdateVariables(ctx, ref, environment); err != nil {
		return fmt.Errorf("update variables: %w", err)
	}
	return nil
}

func (h *Handler) workforceReconcileEnvironment(userID string) (map[string]string, error) {
	environment, err := h.instanceEnv(userID)
	if err != nil {
		return nil, err
	}
	// Legacy Railway services referenced the shared Router variables directly.
	// Neutralize those references in the same collection upsert that installs
	// the derived per-user authority, so reconciliation causes one deployment
	// and no Router root secret reaches the next user-runtime process.
	for _, name := range routerOnlyWorkforceVariables {
		environment[name] = ""
	}
	return environment, nil
}

type neoFleetReconcileRequest struct {
	Concurrency    int    `json:"concurrency,omitempty"`
	NeocortexToken string `json:"neocortex_token"`
}

func (h *Handler) handleNeoFleetReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.neoReconcileMu.TryLock() {
		http.Error(w, "Neo fleet reconciliation already in progress", http.StatusConflict)
		return
	}
	defer h.neoReconcileMu.Unlock()
	var request neoFleetReconcileRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	concurrency, err := workforceReconcileConcurrency(request.Concurrency)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	environment, err := neoReconcileEnvironment(request.NeocortexToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), workforceFleetReconcileTimeout)
	defer cancel()
	users, err := h.DB.ListRailwayReconcileUsers(ctx)
	if err != nil {
		h.logf("list Neo reconciliation users: %v", err)
		http.Error(w, "Neo reconciliation user listing failed", http.StatusInternalServerError)
		return
	}
	if len(users) < concurrency {
		concurrency = len(users)
	}
	response := workforceFleetReconcileResponse{
		Eligible: len(users), Failures: make([]workforceFleetReconcileFailure, 0),
	}
	if len(users) == 0 {
		writeJSON(w, http.StatusOK, response)
		return
	}
	type result struct {
		userID string
		err    error
	}
	jobs := make(chan db.User, len(users))
	results := make(chan result, len(users))
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for user := range jobs {
				userCtx, userCancel := context.WithTimeout(ctx, h.timeout())
				err := h.updateNeoEnvironment(userCtx, &user, environment)
				userCancel()
				results <- result{userID: user.ID, err: err}
			}
		}()
	}
	for index := range users {
		jobs <- users[index]
	}
	close(jobs)
	workers.Wait()
	close(results)
	for result := range results {
		if result.err == nil {
			response.Succeeded++
			continue
		}
		response.Failed++
		response.Failures = append(response.Failures, workforceFleetReconcileFailure{
			UserID: result.userID, Error: result.err.Error(),
		})
	}
	status := http.StatusOK
	if response.Failed > 0 {
		status = http.StatusMultiStatus
	}
	h.logf(
		"Neo fleet reconciliation eligible=%d succeeded=%d failed=%d",
		response.Eligible, response.Succeeded, response.Failed,
	)
	writeJSON(w, status, response)
}

func neoReconcileEnvironment(token string) (map[string]string, error) {
	token = strings.TrimSpace(token)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("neocortex_token must be exactly 64 hexadecimal characters")
	}
	return map[string]string{
		"NEO_MEMORY_SUBSTRATE": "neocortex",
		"NEO_NEOCORTEX_TOKEN":  token,
		"NEO_RUNTIME":          "LEGACY",
	}, nil
}

func (h *Handler) updateNeoEnvironment(
	ctx context.Context,
	user *db.User,
	environment map[string]string,
) error {
	if _, busy := h.neoReconcileInflight.LoadOrStore(user.ID, struct{}{}); busy {
		return errors.New("Neo environment reconciliation already in progress")
	}
	defer h.neoReconcileInflight.Delete(user.ID)
	provider := h.Prov
	if user.Provider == "railway" && h.ShardProviders != nil {
		var ok bool
		provider, ok = h.ShardProviders.Provider(user.RailwayShardID)
		if !ok {
			return fmt.Errorf("assigned shard %q is unavailable", user.RailwayShardID)
		}
	}
	updater, ok := provider.(provision.VariableUpdater)
	if !ok {
		return errors.New("provider does not support environment reconciliation")
	}
	ref := provision.Ref{UserID: user.ID, EnvID: user.EnvID, VolumeID: user.VolumeID}
	if err := updater.UpdateVariables(ctx, ref, environment); err != nil {
		return fmt.Errorf("update variables: %w", err)
	}
	return nil
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
	// Restore is an explicit operator mutation, so it is also the safe point to
	// bring a previously suspended runtime onto the current Workforce contract.
	// Ordinary authenticated traffic never performs this provider write.
	if state == db.StateActive && h.Workforce != nil {
		user, err := h.DB.LookupForRoute(ctx, userID)
		if err == nil {
			err = h.updateWorkforceEnvironment(ctx, user)
		}
		if err != nil {
			h.logf("restore Workforce environment %s: %v", userID, err)
			http.Error(w, "restored user but Workforce reconciliation failed", http.StatusBadGateway)
			return
		}
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
		prov := h.Prov
		if u.Provider == "railway" && h.ShardProviders != nil {
			var ok bool
			prov, ok = h.ShardProviders.Provider(u.RailwayShardID)
			if !ok {
				http.Error(w, "assigned shard unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		var destroyErr error
		if u.Provider == "railway" && h.ShardProviders != nil {
			op, err := h.DB.BeginRailwayOperation(ctx, userID, "destroy")
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			recoverable, ok := prov.(provision.Recoverable)
			if !ok {
				destroyErr = errors.New("assigned Railway provisioner has no recovery surface")
			} else {
				destroyErr = h.destroyRailway(ctx, op, recoverable, ref)
			}
		} else {
			destroyErr = prov.Destroy(ctx, ref)
		}
		if destroyErr != nil && !errors.Is(destroyErr, provision.ErrNotFound) {
			h.logf("destroy environment %s: %v (continuing)", u.EnvID, destroyErr)
			if u.Provider == "railway" && h.ShardProviders != nil {
				_ = h.DB.SetAllocationState(ctx, userID, "cleanup_pending", false)
			}
			http.Error(w, "cleanup pending", http.StatusServiceUnavailable)
			return
		}
		if u.Provider == "railway" && h.ShardProviders != nil {
			if err := h.DB.SetAllocationState(ctx, userID, "released", true); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
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

func (h *Handler) destroyRailway(ctx context.Context, op *db.RailwayOperation, p provision.Recoverable, ref provision.Ref) error {
	if err := h.DB.MarkRailwayOperationRunning(ctx, op.ID, "destroy_reconcile"); err != nil {
		return err
	}
	if err := h.DB.RecordRailwayService(ctx, op.ID, ref.EnvID); err != nil {
		return err
	}
	if ref.VolumeID != "" {
		if err := h.DB.RecordRailwayVolume(ctx, op.ID, ref.VolumeID); err != nil {
			return err
		}
		if absent, err := p.VolumeAbsent(ctx, ref.VolumeID); err != nil {
			_ = h.DB.FinishRailwayOperation(ctx, op.ID, "unknown", "volume_absence_unknown", err.Error())
			return err
		} else if !absent {
			if err := h.DB.ValidateRailwayOperation(ctx, op.ID); err != nil {
				return err
			}
			if err := p.DeleteVolume(ctx, ref.VolumeID); err != nil && !errors.Is(err, provision.ErrNotFound) {
				_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "volume_delete_pending", err.Error())
				return err
			}
		}
		if err := boundedReconcile(ctx, func() error {
			absent, err := p.VolumeAbsent(ctx, ref.VolumeID)
			if err != nil {
				return err
			}
			if !absent {
				return provision.ErrNotFound
			}
			return nil
		}); err != nil {
			_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "volume_absence_unproven", err.Error())
			return err
		}
	}
	if absent, err := p.ServiceAbsent(ctx, ref.EnvID); err != nil {
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, "unknown", "service_absence_unknown", err.Error())
		return err
	} else if !absent {
		if err := h.DB.ValidateRailwayOperation(ctx, op.ID); err != nil {
			return err
		}
		if err := p.DeleteService(ctx, ref.EnvID); err != nil && !errors.Is(err, provision.ErrNotFound) {
			_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "service_delete_pending", err.Error())
			return err
		}
	}
	if err := boundedReconcile(ctx, func() error {
		absent, err := p.ServiceAbsent(ctx, ref.EnvID)
		if err != nil {
			return err
		}
		if !absent {
			return provision.ErrNotFound
		}
		return nil
	}); err != nil {
		_ = h.DB.FinishRailwayOperation(ctx, op.ID, "cleanup_pending", "service_absence_unproven", err.Error())
		return err
	}
	return h.DB.FinishRailwayOperation(ctx, op.ID, "succeeded", "service_and_volume_absent", "")
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

// Copyright © 2026 Sidiora Labs. All rights reserved.
