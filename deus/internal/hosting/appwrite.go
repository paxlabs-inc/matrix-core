package hosting

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AppwriteConfig holds Paxeer Cloud / Appwrite Server API settings.
//
// Endpoint MUST already include the API version segment (e.g.
// "https://cloud.paxeer.app/v1"); all request paths are appended verbatim.
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
	limits Limits
}

// BlobReader fetches uploaded artifacts for deploy packaging.
type BlobReader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// NewAppwriteBackend constructs a production hosting backend. limits supplies
// the per-function resource caps (timeout, always-warm specification) and the
// runner-facing response cap pushed as a function variable.
func NewAppwriteBackend(cfg AppwriteConfig, blobs BlobReader, limits Limits) *AppwriteBackend {
	return &AppwriteBackend{
		cfg:    cfg,
		blobs:  blobs,
		limits: limits,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// Deploy creates a node20 function, pushes its config, uploads the developer
// code bundle as a real deployment, and returns its executions endpoint.
//
// Expected artifact format: a gzipped tar (code.tar.gz) of the developer's
// project that includes the runner harness as the Appwrite entrypoint
// (src/main.js), the developer handler (src/handler.js exporting
// handle(operation, args, ctx)), and package.json. The artifact bytes are
// fetched from object storage by ArtifactKey.
func (a *AppwriteBackend) Deploy(ctx context.Context, in DeployInput) (DeployResult, error) {
	if a.cfg.Endpoint == "" || a.cfg.ProjectID == "" || a.cfg.APIKey == "" {
		return DeployResult{}, fmt.Errorf("hosting: appwrite config incomplete")
	}
	if in.Runtime != "node20" {
		return DeployResult{}, fmt.Errorf("hosting: appwrite backend supports node20 first")
	}
	if in.ArtifactKey == "" {
		return DeployResult{}, fmt.Errorf("hosting: artifact_key required")
	}

	artifact, err := a.fetchArtifact(ctx, in.ArtifactKey)
	if err != nil {
		return DeployResult{}, err
	}

	name := in.FunctionName
	if name == "" {
		name = "deus-" + shortID(in.ServiceID)
	}
	fnID, err := a.ensureFunction(ctx, name, in.AlwaysWarm)
	if err != nil {
		return DeployResult{}, err
	}
	if err := a.pushVariables(ctx, fnID, in.Env); err != nil {
		return DeployResult{}, err
	}
	depID, execURL, err := a.createDeployment(ctx, fnID, artifact)
	if err != nil {
		return DeployResult{}, err
	}
	return DeployResult{
		FunctionID:   fnID,
		DeploymentID: depID,
		ExecEndpoint: execURL,
	}, nil
}

// fetchArtifact loads the uploaded code bundle from object storage.
func (a *AppwriteBackend) fetchArtifact(ctx context.Context, key string) ([]byte, error) {
	if a.blobs == nil {
		return nil, fmt.Errorf("hosting: blob reader not configured")
	}
	rc, err := a.blobs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("hosting: fetch artifact: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("hosting: read artifact: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("hosting: artifact %s is empty", key)
	}
	return raw, nil
}

func (a *AppwriteBackend) ensureFunction(ctx context.Context, name string, alwaysWarm bool) (string, error) {
	body := map[string]any{
		"functionId": "unique()",
		"name":       name,
		"runtime":    "node-20.0",
		"execute":    []string{"any"},
		"enabled":    true,
		"logging":    true,
		"timeout":    a.timeoutSeconds(),
		"scopes":     []string{"users.read"},
		"events":     []string{},
		"schedule":   "",
		"entrypoint": "src/main.js",
		"commands":   "npm install",
	}
	if alwaysWarm {
		// Pin a dedicated CPU/memory spec so always-warm functions are not
		// throttled on cold scale-to-zero pools.
		body["specification"] = warmSpecification
	}
	var resp struct {
		ID string `json:"$id"`
	}
	if err := a.post(ctx, "/functions", body, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("hosting: appwrite function create returned no id")
	}
	return resp.ID, nil
}

// pushVariables registers per-service config/secrets plus the runner-facing
// response cap as Appwrite function variables. ensureFunction always creates a
// fresh function, so a plain POST per variable is sufficient (no update path).
func (a *AppwriteBackend) pushVariables(ctx context.Context, functionID string, env map[string]string) error {
	vars := map[string]string{
		"DEUS_MAX_RESPONSE_BYTES": strconv.Itoa(a.maxResponseBytes()),
	}
	for k, v := range env {
		if strings.TrimSpace(k) == "" {
			continue
		}
		vars[k] = v
	}
	// Deterministic order keeps deploys reproducible and tests stable.
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		body := map[string]any{"key": k, "value": vars[k]}
		path := fmt.Sprintf("/functions/%s/variables", functionID)
		if err := a.post(ctx, path, body, nil); err != nil {
			return fmt.Errorf("hosting: set variable %s: %w", k, err)
		}
	}
	return nil
}

// createDeployment uploads the code bundle via multipart/form-data and activates
// it. This is the deployment that actually ships the developer's code; an empty
// JSON deployment would never run.
func (a *AppwriteBackend) createDeployment(ctx context.Context, functionID string, artifact []byte) (depID string, execURL string, err error) {
	code, fileName := packageArtifact(artifact)
	fields := map[string]string{
		"activate":   "true",
		"entrypoint": "src/main.js",
		"commands":   "npm install",
	}
	var resp struct {
		ID string `json:"$id"`
	}
	path := fmt.Sprintf("/functions/%s/deployments", functionID)
	if err := a.postMultipart(ctx, path, fields, "code", fileName, code, &resp); err != nil {
		return "", "", err
	}
	if resp.ID == "" {
		return "", "", fmt.Errorf("hosting: appwrite deployment create returned no id")
	}
	return resp.ID, a.executionsURL(functionID), nil
}

// executionsURL is the synchronous execution endpoint the gateway invokes.
// Endpoint already contains the API version, so we must NOT add another /v1.
func (a *AppwriteBackend) executionsURL(functionID string) string {
	return strings.TrimRight(a.cfg.Endpoint, "/") + "/functions/" + functionID + "/executions"
}

func (a *AppwriteBackend) timeoutSeconds() int {
	ms := a.limits.DefaultTimeoutMS
	if ms <= 0 {
		ms = 30000
	}
	s := ms / 1000
	if s < 1 {
		s = 1
	}
	return s
}

func (a *AppwriteBackend) maxResponseBytes() int {
	if a.limits.DefaultMaxResponseBytes > 0 {
		return a.limits.DefaultMaxResponseBytes
	}
	return 262144
}

// warmSpecification is the Appwrite CPU/memory spec for always-warm functions.
const warmSpecification = "s-1vcpu-512mb"

// packageArtifact normalizes the uploaded bundle into a code.tar.gz the Appwrite
// deployments API can extract.
//
// The expected artifact is already a gzipped tar of the project, so when we
// detect the gzip magic bytes (0x1f 0x8b) we pass the bytes through unchanged.
// As a robustness fallback (e.g. a developer uploaded a plain, uncompressed
// tar) we gzip-wrap the raw bytes so Appwrite still receives a gzip stream.
func packageArtifact(raw []byte) (data []byte, fileName string) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		return raw, "code.tar.gz"
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(raw)
	_ = gz.Close()
	return buf.Bytes(), "code.tar.gz"
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
	return a.do(req, path, out)
}

// postMultipart uploads a file part plus form fields as multipart/form-data,
// used for the code bundle on createDeployment (the JSON post() cannot carry a
// file). It does not replace post(); JSON endpoints keep using post().
func (a *AppwriteBackend) postMultipart(ctx context.Context, path string, fields map[string]string, fileField, fileName string, fileBytes []byte, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, fileName))
	hdr.Set("Content-Type", "application/gzip")
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return err
	}
	if _, err := part.Write(fileBytes); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	url := strings.TrimRight(a.cfg.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	a.setHeaders(req)
	return a.do(req, path, out)
}

// do executes a prepared request and decodes a non-error JSON response.
func (a *AppwriteBackend) do(req *http.Request, path string, out any) error {
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

// shortID returns up to the first 8 chars of a service id for the function name.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
