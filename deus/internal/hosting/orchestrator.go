// Package hosting orchestrates Paxeer Cloud deploy lifecycle (docs/06-execution-hosting.md §6.3).
package hosting

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/paxlabs-inc/deus/internal/store"
)

// BlobStore uploads and fetches service artifacts.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	URL(key string) string
}

// Orchestrator coordinates artifact storage, budget checks, and backend deploy.
type Orchestrator struct {
	store   *store.Store
	blobs   BlobStore
	backend Backend
	limits  Limits
	budget  *Budget
}

// NewOrchestrator wires hosting dependencies.
func NewOrchestrator(st *store.Store, blobs BlobStore, backend Backend, limits Limits) *Orchestrator {
	return &Orchestrator{
		store:   st,
		blobs:   blobs,
		backend: backend,
		limits:  limits,
		budget:  NewBudget(st, limits),
	}
}

// UploadArtifact stores a source bundle for a hosted service.
func (o *Orchestrator) UploadArtifact(ctx context.Context, serviceID, filename string, r io.Reader, size int64) (string, error) {
	if size > o.limits.MaxArtifactBytes {
		return "", fmt.Errorf("hosting: artifact exceeds max size (%d bytes)", o.limits.MaxArtifactBytes)
	}
	svc, err := o.store.GetServiceByID(ctx, serviceID)
	if err != nil {
		return "", err
	}
	if svc.Mode != "hosted" {
		return "", fmt.Errorf("hosting: service is not hosted mode")
	}
	key := path.Join("artifacts", serviceID, sanitizeName(filename))
	if err := o.blobs.Put(ctx, key, r, size, "application/gzip"); err != nil {
		return "", err
	}
	return key, nil
}

// DeployInput is the orchestrator deploy request.
type DeployRequest struct {
	ServiceID   string
	ArtifactKey string
	Runtime     string
	AlwaysWarm  bool
	Region      string
}

// DeployOutput is returned to API callers.
type DeployOutput struct {
	DeploymentID string `json:"deployment_id"`
	Status       string `json:"status"`
	ExecEndpoint string `json:"exec_endpoint,omitempty"`
	Runtime      string `json:"runtime"`
}

// Deploy provisions hosted execution for a service.
func (o *Orchestrator) Deploy(ctx context.Context, req DeployRequest) (DeployOutput, error) {
	svc, err := o.store.GetServiceByID(ctx, req.ServiceID)
	if err != nil {
		return DeployOutput{}, err
	}
	if svc.Mode != "hosted" {
		return DeployOutput{}, fmt.Errorf("hosting: service is not hosted mode")
	}
	if req.ArtifactKey == "" {
		return DeployOutput{}, fmt.Errorf("hosting: artifact_key required")
	}
	runtime := strings.TrimSpace(req.Runtime)
	if runtime == "" {
		runtime = "node20"
	}
	if err := o.budget.AllowNewDeployment(ctx, req.AlwaysWarm); err != nil {
		return DeployOutput{}, err
	}

	depID, err := o.store.InsertDeployment(ctx, store.DeploymentRow{
		ServiceID:  req.ServiceID,
		Runtime:    runtime,
		Status:     "pending",
		Region:     strPtr(req.Region),
		AlwaysWarm: req.AlwaysWarm,
	})
	if err != nil {
		return DeployOutput{}, err
	}

	res, err := o.backend.Deploy(ctx, DeployInput{
		ServiceID:    req.ServiceID,
		ArtifactKey:  req.ArtifactKey,
		Runtime:      runtime,
		AlwaysWarm:   req.AlwaysWarm,
		Region:       req.Region,
		FunctionName: "deus-" + svc.Slug,
	})
	if err != nil {
		_ = o.store.UpdateDeploymentStatus(ctx, depID, "failed", nil)
		return DeployOutput{}, err
	}

	_ = o.store.DeactivateDeploymentsForService(ctx, req.ServiceID)
	fnID := res.FunctionID
	exec := res.ExecEndpoint
	if err := o.store.UpdateDeploymentStatus(ctx, depID, "active", &exec); err != nil {
		return DeployOutput{}, err
	}
	if err := o.store.SetDeploymentBackendIDs(ctx, depID, fnID, res.DeploymentID); err != nil {
		return DeployOutput{}, err
	}

	return DeployOutput{
		DeploymentID: depID,
		Status:       "active",
		ExecEndpoint: exec,
		Runtime:      runtime,
	}, nil
}

// ActiveEndpoint returns the invoke URL for a hosted service.
func (o *Orchestrator) ActiveEndpoint(ctx context.Context, serviceID string) (string, error) {
	dep, err := o.store.ActiveDeploymentForService(ctx, serviceID)
	if err != nil {
		return "", err
	}
	if dep.ExecEndpoint == nil || *dep.ExecEndpoint == "" {
		return "", fmt.Errorf("hosting: deployment missing exec endpoint")
	}
	return *dep.ExecEndpoint, nil
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "bundle.tar.gz"
	}
	name = strings.ReplaceAll(name, "..", "")
	return path.Base(name)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
