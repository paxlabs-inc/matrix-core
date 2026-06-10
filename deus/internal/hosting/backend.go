package hosting

import "context"

// DeployInput is passed to a hosting backend on deploy.
type DeployInput struct {
	ServiceID    string
	ArtifactKey  string
	Runtime      string
	AlwaysWarm   bool
	Region       string
	FunctionName string
	// Env carries per-service config/secrets pushed to the function as
	// Appwrite variables. The orchestrator threads it from DeployRequest.Env;
	// the HTTP layer leaves it empty today (clean extension point).
	Env map[string]string
}

// DeployResult is returned after a successful backend deploy.
type DeployResult struct {
	FunctionID   string
	DeploymentID string
	ExecEndpoint string
}

// Backend provisions hosted execution (Appwrite or dev stub).
type Backend interface {
	Deploy(ctx context.Context, in DeployInput) (DeployResult, error)
	Delete(ctx context.Context, functionID string) error
}
