package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const workforcedPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

func TestIntegration_RunContextMountsOwnerAndChronosBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("real PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	container, databaseURL, err := startWorkforcedPostgres(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", container).Run()
	}()
	if err := waitWorkforcedPostgres(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	listen := reserveWorkforcedAddress(t)
	ownerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, runtimePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"WORKFORCE_POSTGRES_URI":         databaseURL,
		"WORKFORCE_TENANT_ID":            "integration-user",
		"WORKFORCE_ORGANIZATION_ID":      "organization-integration-user",
		"WORKFORCE_OWNER_ID":             "owner-integration-user",
		"WORKFORCE_OWNER_TOKEN":          "owner-integration-token",
		"WORKFORCE_WAKE_TOKEN":           "wake-integration-token",
		"WORKFORCE_OWNER_KEY_ID":         "bootstrap-owner-v1",
		"WORKFORCE_OWNER_PUBLIC_KEY":     base64.RawURLEncoding.EncodeToString(ownerPublic),
		"WORKFORCE_RUNTIME_KEY_ID":       "runtime-v1",
		"WORKFORCE_RUNTIME_PRIVATE_KEY":  base64.RawURLEncoding.EncodeToString(runtimePrivate),
		"WORKFORCE_BUBBLEWRAP":           "/bin/true",
		"WORKFORCE_SEAT_BINARY":          testExecutable,
		"WORKFORCE_AUDITOR_BINARY":       testExecutable,
		"WORKFORCE_DEVELOPER_REPOSITORY": t.TempDir(),
		"WORKFORCE_CODEGRAPH_EXECUTABLE": "/bin/true",
		"WORKFORCE_AUDITOR_SEAT_ID":      "seat-developer-auditor",
		"WORKFORCE_DATA_DIR":             t.TempDir(),
		"WORKFORCE_POLL_INTERVAL":        "20ms",
		"MATRIX_GATEWAY_URL":             "http://127.0.0.1:1",
		"MATRIX_GATEWAY_TOKEN":           "gateway-integration-token",
		"VAULT_REQUIRED":                 "true",
		"VAULT_KEK":                      strings.Repeat("01", 32),
		"VAULT_KEK_ID":                   "integration-kek-v1",
	} {
		t.Setenv(name, value)
	}
	runCtx, stop := context.WithCancel(ctx)
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runContext(runCtx, []string{"-serve", "-listen", listen}, &stdout, &stderr)
	}()
	waitWorkforcedSession(t, ctx, "http://"+listen)

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, "http://"+listen+"/internal/workforce/wake",
		strings.NewReader("{"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer wake-integration-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("authenticated wake route status = %d", response.StatusCode)
	}

	request, err = http.NewRequestWithContext(
		ctx, http.MethodPost, "http://"+listen+"/internal/workforce/wake",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer owner-integration-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("owner credential crossed wake boundary: status=%d", response.StatusCode)
	}

	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("workforced exit=%d stderr=%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("workforced did not shut down")
	}
}

func reserveWorkforcedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitWorkforcedSession(t *testing.T, ctx context.Context, baseURL string) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/workforce/session", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer owner-integration-token")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func startWorkforcedPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforced-main-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		workforcedPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", "", fmt.Errorf("start PostgreSQL returned no container id")
	}
	container := fields[len(fields)-1]
	var address string
	for address == "" {
		portOutput, portErr := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
		if portErr == nil {
			address = strings.TrimSpace(string(portOutput))
			if address != "" {
				break
			}
		}
		select {
		case <-ctx.Done():
			_ = exec.Command("docker", "rm", "-f", container).Run()
			return container, "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf("invalid PostgreSQL port %q", address)
	}
	return container,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			address[index+1:] + "/workforce?sslmode=disable", nil
}

func waitWorkforcedPostgres(ctx context.Context, databaseURL string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if err := pool.Ping(ctx); err == nil {
				pool.Close()
				return nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
