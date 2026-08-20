package companyrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

const recoveryBackendSchemaVersion = "workforce.recovery-backend.v1"

type CommandBackend struct {
	executable string
	arguments  []string
	timeout    time.Duration
}

func NewCommandBackend(
	executable string,
	arguments []string,
	timeout time.Duration,
) (*CommandBackend, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" || !filepath.IsAbs(executable) || timeout <= 0 || timeout > 24*time.Hour {
		return nil, fmt.Errorf("company recovery: backend command configuration is invalid")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("company recovery: backend executable is unavailable")
	}
	for _, argument := range arguments {
		if strings.ContainsRune(argument, '\x00') {
			return nil, fmt.Errorf("company recovery: backend argument is invalid")
		}
	}
	return &CommandBackend{
		executable: executable, arguments: append([]string(nil), arguments...), timeout: timeout,
	}, nil
}

type backendRequest struct {
	SchemaVersion  string                   `json:"schema_version"`
	Operation      string                   `json:"operation"`
	TenantID       string                   `json:"tenant_id,omitempty"`
	OrganizationID contracts.OrganizationID `json:"organization_id,omitempty"`
	BackupID       string                   `json:"backup_id,omitempty"`
	PITRPoint      string                   `json:"pitr_point,omitempty"`
	CapturedAt     time.Time                `json:"captured_at,omitempty"`
	TargetAt       time.Time                `json:"target_at,omitempty"`
	Erasure        *ErasureDirectiveBody    `json:"erasure,omitempty"`
}

type backendResponse struct {
	SchemaVersion string `json:"schema_version"`
	Operation     string `json:"operation"`
	PITRPoint     string `json:"pitr_point,omitempty"`
	ErasedKeys    uint32 `json:"erased_keys,omitempty"`
	ErasedBytes   uint64 `json:"erased_bytes,omitempty"`
}

func (backend *CommandBackend) Capture(
	ctx context.Context,
	tenantID string,
	organizationID contracts.OrganizationID,
	backupID string,
	capturedAt time.Time,
) (string, error) {
	if validateToken("tenant_id", tenantID) != nil ||
		validateToken("organization_id", string(organizationID)) != nil ||
		validateToken("backup_id", backupID) != nil || !validUTC(capturedAt) {
		return "", ErrUnauthorized
	}
	response, err := backend.run(ctx, backendRequest{
		SchemaVersion: recoveryBackendSchemaVersion, Operation: "capture",
		TenantID: tenantID, OrganizationID: organizationID,
		BackupID: backupID, CapturedAt: capturedAt,
	})
	if err != nil {
		return "", err
	}
	if validateBounded("pitr_point", response.PITRPoint, 1024) != nil {
		return "", fmt.Errorf("company recovery: backend returned an invalid PITR point")
	}
	return response.PITRPoint, nil
}

func (backend *CommandBackend) Restore(
	ctx context.Context,
	tenantID string,
	organizationID contracts.OrganizationID,
	pitrPoint string,
	targetAt time.Time,
) error {
	if validateToken("tenant_id", tenantID) != nil ||
		validateToken("organization_id", string(organizationID)) != nil ||
		validateBounded("pitr_point", pitrPoint, 1024) != nil || !validUTC(targetAt) {
		return ErrUnauthorized
	}
	_, err := backend.run(ctx, backendRequest{
		SchemaVersion: recoveryBackendSchemaVersion, Operation: "restore",
		TenantID: tenantID, OrganizationID: organizationID,
		PITRPoint: pitrPoint, TargetAt: targetAt,
	})
	return err
}

func (backend *CommandBackend) Erase(
	ctx context.Context,
	directive ErasureDirectiveBody,
) (uint32, uint64, error) {
	if directive.Validate() != nil || directive.TargetKind != ErasureScope {
		return 0, 0, ErrUnauthorized
	}
	response, err := backend.run(ctx, backendRequest{
		SchemaVersion: recoveryBackendSchemaVersion,
		Operation:     "erase", OrganizationID: directive.OrganizationID, Erasure: &directive,
	})
	if err != nil {
		return 0, 0, err
	}
	if response.ErasedKeys == 0 {
		return 0, 0, fmt.Errorf("company recovery: backend erased no scoped keys")
	}
	return response.ErasedKeys, response.ErasedBytes, nil
}

func (backend *CommandBackend) run(
	ctx context.Context,
	request backendRequest,
) (backendResponse, error) {
	if backend == nil {
		return backendResponse{}, fmt.Errorf("company recovery: backend is unavailable")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return backendResponse{}, err
	}
	commandCtx, cancel := context.WithTimeout(ctx, backend.timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, backend.executable, backend.arguments...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.Stdin = bytes.NewReader(payload)
	stdout := &boundedCommandOutput{remaining: 1 << 20}
	stderr := &boundedCommandOutput{remaining: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if commandCtx.Err() != nil {
			return backendResponse{}, commandCtx.Err()
		}
		return backendResponse{}, fmt.Errorf("company recovery: backend command failed: %w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.buffer.Bytes()))
	decoder.DisallowUnknownFields()
	var response backendResponse
	if err := decoder.Decode(&response); err != nil {
		return backendResponse{}, fmt.Errorf("company recovery: backend response is invalid: %w", err)
	}
	if trailingErr := decoder.Decode(&struct{}{}); !errors.Is(trailingErr, io.EOF) ||
		response.SchemaVersion != recoveryBackendSchemaVersion ||
		response.Operation != request.Operation {
		return backendResponse{}, fmt.Errorf("company recovery: backend response contract mismatch")
	}
	return response, nil
}

type boundedCommandOutput struct {
	buffer    bytes.Buffer
	remaining int
}

func (output *boundedCommandOutput) Write(value []byte) (int, error) {
	if len(value) > output.remaining {
		return 0, fmt.Errorf("company recovery: backend output exceeded its limit")
	}
	written, err := output.buffer.Write(value)
	output.remaining -= written
	return written, err
}

var _ PITRBackend = (*CommandBackend)(nil)
var _ ErasureBackend = (*CommandBackend)(nil)
