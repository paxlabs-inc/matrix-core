package codingworker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProcessSupervisorLifecycleAndCredentialRotation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("unprivileged credential transition requires root test process")
	}
	root, err := os.MkdirTemp("", "codingworker-supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	managed := filepath.Join(root, "managed")
	for _, dir := range []string{workspace, home, managed} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(workspace, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	python, err = filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	const healthServer = `
import http.server, json, os, sys
required = ('AGENTCORE_DASHBOARD_SESSION_TOKEN','XIAOMI_API_KEY','XIAOMI_BASE_URL','MATRIX_ACTOR_DID','AGENTCORE_WRITE_SAFE_ROOT','AGENTCORE_MANAGED_DIR')
if any(not os.environ.get(k) for k in required): sys.exit(91)
port = int(sys.argv[sys.argv.index('--port') + 1])
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  if self.path != '/api/health': self.send_error(404); return
  body = json.dumps({'ok': True, 'version': 'test-runtime', 'auth_required': False}).encode()
  self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(body))); self.end_headers(); self.wfile.write(body)
 def log_message(self, *args): pass
http.server.ThreadingHTTPServer(('127.0.0.1', port), Handler).serve_forever()
`
	var issues atomic.Int32
	supervisor, err := NewProcessSupervisor(SupervisorConfig{
		Binary:          python,
		Arguments:       []string{"-c", healthServer, "serve", "--host", "127.0.0.1", "--port", "{port}", "--isolated"},
		ManagedDir:      managed,
		AllowedExtraEnv: map[string]struct{}{"MATRIX_CODINGWORKER_TEST_CHILD": {}},
		StartupTimeout:  5 * time.Second,
		HealthTimeout:   500 * time.Millisecond,
		StopTimeout:     2 * time.Second,
		IdleTimeout:     20 * time.Millisecond,
		CredentialSource: CredentialSourceFunc(func(context.Context, UserKey, string) (ScopedCredential, error) {
			value := issues.Add(1)
			return ScopedCredential{Model: []byte(fmt.Sprintf("scoped-credential-%d", value))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{
		User:       "user-1",
		ActorDID:   "did:matrix:user-1",
		GatewayURL: "https://gateway.example/v1",
		Workspace:  workspace,
		Home:       home,
		Port:       port,
		UID:        65534,
		GID:        65534,
		ExtraEnv:   map[string]string{"MATRIX_CODINGWORKER_TEST_CHILD": "1"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, err := supervisor.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.PID <= 0 || len(first.Token) < 32 {
		t.Fatalf("invalid first handle: %+v", first)
	}
	if health, err := supervisor.Health(ctx, spec.User); err != nil || !health.OK {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	second, err := supervisor.RotateCredential(ctx, spec.User)
	if err != nil {
		t.Fatal(err)
	}
	if second.PID == first.PID || string(second.Token) == string(first.Token) || issues.Load() != 2 {
		t.Fatalf("rotation did not replace process/token: first=%d second=%d issues=%d", first.PID, second.PID, issues.Load())
	}
	third, err := supervisor.ForceDeath(ctx, spec.User, true)
	if err != nil {
		t.Fatal(err)
	}
	if third.PID == second.PID || string(third.Token) == string(second.Token) || issues.Load() != 3 {
		t.Fatalf("forced-death restart did not replace process/token: second=%d third=%d issues=%d", second.PID, third.PID, issues.Load())
	}
	reaped := supervisor.ReapIdle(ctx, time.Now().Add(time.Second))
	if len(reaped) != 1 || reaped[0] != spec.User {
		t.Fatalf("reaped=%v", reaped)
	}
	if _, ok := supervisor.Handle(spec.User); ok {
		t.Fatal("idle process remains registered")
	}
	eraseBytes(first.Token)
	eraseBytes(second.Token)
	eraseBytes(third.Token)
}

func TestSupervisorRejectsCallerControlledCommandAndEnvironment(t *testing.T) {
	_, err := NewProcessSupervisor(SupervisorConfig{
		Binary:    "/bin/true",
		Arguments: []string{"serve", "--host", "0.0.0.0", "--isolated"},
		CredentialSource: CredentialSourceFunc(func(context.Context, UserKey, string) (ScopedCredential, error) {
			return ScopedCredential{Model: []byte("x")}, nil
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err=%v", err)
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	managed := filepath.Join(root, "managed")
	for _, dir := range []string{workspace, home, managed} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	supervisor, err := NewProcessSupervisor(SupervisorConfig{
		Binary:     "/bin/true",
		ManagedDir: managed,
		CredentialSource: CredentialSourceFunc(func(context.Context, UserKey, string) (ScopedCredential, error) {
			return ScopedCredential{Model: []byte("x")}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi("9119")
	err = supervisor.validateSpec(LaunchSpec{
		User: "user", ActorDID: "did:matrix:user", GatewayURL: "https://gateway.example/v1",
		Workspace: workspace, Home: home, Port: port, UID: uint32(maxInt(os.Geteuid(), 1)), GID: uint32(maxInt(os.Getegid(), 1)),
		ExtraEnv: map[string]string{"LD_PRELOAD": "/tmp/injected.so"},
	})
	if err == nil || !strings.Contains(err.Error(), "LD_PRELOAD") {
		t.Fatalf("err=%v", err)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
