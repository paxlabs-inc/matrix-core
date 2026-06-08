package hosting

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// DevBackend simulates Paxeer Cloud deploy in local dev (docs/06-execution-hosting.md §6.3).
type DevBackend struct {
	ExecURL string
}

// Deploy records a dev function id and routes invoke to ExecURL.
func (d *DevBackend) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	_ = ctx
	url := strings.TrimRight(strings.TrimSpace(d.ExecURL), "/")
	if url == "" {
		return DeployResult{}, fmt.Errorf("hosting: DEUS_HOSTING_DEV_EXEC_URL required for dev deploy")
	}
	if in.Runtime != "node20" {
		return DeployResult{}, fmt.Errorf("hosting: dev backend supports node20 only")
	}
	fnID := "dev-fn-" + uuid.NewString()
	return DeployResult{
		FunctionID:   fnID,
		DeploymentID: "dev-dep-" + uuid.NewString(),
		ExecEndpoint: url,
	}, nil
}

// Delete is a no-op in dev.
func (d *DevBackend) Delete(ctx context.Context, functionID string) error {
	_ = ctx
	_ = functionID
	return nil
}
