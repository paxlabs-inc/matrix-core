package hosting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AppwriteConfig holds Paxeer Cloud / Appwrite Server API settings.
type AppwriteConfig struct {
	Endpoint  string
	ProjectID string
	APIKey    string
	Region    string
}

// AppwriteBackend drives function deploy via the Appwrite Server API.
type AppwriteBackend struct {
	cfg    AppwriteConfig
	client *http.Client
	blobs  BlobReader
}

// BlobReader fetches uploaded artifacts for deploy packaging.
type BlobReader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// NewAppwriteBackend constructs a production hosting backend.
func NewAppwriteBackend(cfg AppwriteConfig, blobs BlobReader) *AppwriteBackend {
	return &AppwriteBackend{
		cfg:    cfg,
		blobs:  blobs,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// Deploy creates or updates a node20 function and returns its execution endpoint.
func (a *AppwriteBackend) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	if a.cfg.Endpoint == "" || a.cfg.ProjectID == "" || a.cfg.APIKey == "" {
		return DeployResult{}, fmt.Errorf("hosting: appwrite config incomplete")
	}
	if in.Runtime != "node20" {
		return DeployResult{}, fmt.Errorf("hosting: appwrite backend supports node20 first")
	}
	_ = a.blobs // artifact packaging wired in a follow-up; endpoint registration is the Phase 3 seam.

	name := in.FunctionName
	if name == "" {
		name = "deus-" + in.ServiceID[:8]
	}
	fnID, err := a.ensureFunction(ctx, name, in.AlwaysWarm)
	if err != nil {
		return DeployResult{}, err
	}
	depID, execURL, err := a.createDeployment(ctx, fnID, in.ArtifactKey)
	if err != nil {
		return DeployResult{}, err
	}
	return DeployResult{
		FunctionID:   fnID,
		DeploymentID: depID,
		ExecEndpoint: execURL,
	}, nil
}

func (a *AppwriteBackend) ensureFunction(ctx context.Context, name string, alwaysWarm bool) (string, error) {
	body := map[string]any{
		"functionId": "unique()",
		"name":       name,
		"runtime":    "node-20.0",
		"execute":    []string{"any"},
		"enabled":    true,
		"logging":    true,
		"timeout":    30,
		"scopes":     []string{"users.read"},
		"events":     []string{},
		"schedule":   "",
		"entrypoint": "src/main.js",
		"commands":   "npm install",
	}
	if alwaysWarm {
		body["specification"] = "s-1vcpu-512mb"
	}
	var resp struct {
		ID string `json:"$id"`
	}
	if err := a.post(ctx, "/functions", body, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (a *AppwriteBackend) createDeployment(ctx context.Context, functionID, artifactKey string) (depID string, execURL string, err error) {
	_ = artifactKey
	body := map[string]any{
		"entrypoint": "src/main.js",
		"activate":   true,
	}
	var resp struct {
		ID       string `json:"$id"`
		Provider string `json:"provider"`
	}
	path := fmt.Sprintf("/functions/%s/deployments", functionID)
	if err := a.post(ctx, path, body, &resp); err != nil {
		return "", "", err
	}
	execURL = strings.TrimRight(a.cfg.Endpoint, "/") + "/v1/functions/" + functionID + "/executions"
	return resp.ID, execURL, nil
}

// Delete removes a function from Appwrite.
func (a *AppwriteBackend) Delete(ctx context.Context, functionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		strings.TrimRight(a.cfg.Endpoint, "/")+"/functions/"+functionID, http.NoBody)
	if err != nil {
		return err
	}
	a.setHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hosting: appwrite delete %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

func (a *AppwriteBackend) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(a.cfg.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.setHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hosting: appwrite %s %d: %s", path, resp.StatusCode, string(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("hosting: appwrite decode: %w", err)
		}
	}
	return nil
}

func (a *AppwriteBackend) setHeaders(req *http.Request) {
	req.Header.Set("X-Appwrite-Project", a.cfg.ProjectID)
	req.Header.Set("X-Appwrite-Key", a.cfg.APIKey)
}
