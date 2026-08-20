package codingworker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type UserKey string

type ScopedCredential struct {
	Model []byte
}

func (c *ScopedCredential) erase() {
	for i := range c.Model {
		c.Model[i] = 0
	}
	c.Model = nil
}

type CredentialSource interface {
	IssueCodingCredential(context.Context, UserKey, string) (ScopedCredential, error)
}

type CredentialSourceFunc func(context.Context, UserKey, string) (ScopedCredential, error)

func (f CredentialSourceFunc) IssueCodingCredential(ctx context.Context, user UserKey, actorDID string) (ScopedCredential, error) {
	return f(ctx, user, actorDID)
}

type SupervisorConfig struct {
	Binary           string
	Arguments        []string
	ManagedDir       string
	PATH             string
	AllowedExtraEnv  map[string]struct{}
	CredentialSource CredentialSource
	StartupTimeout   time.Duration
	HealthTimeout    time.Duration
	StopTimeout      time.Duration
	IdleTimeout      time.Duration
	Output           io.Writer
}

type LaunchSpec struct {
	User       UserKey
	ActorDID   string
	GatewayURL string
	Workspace  string
	Home       string
	Port       int
	UID        uint32
	GID        uint32
	ExtraEnv   map[string]string
}

type ProcessHandle struct {
	User     UserKey
	Endpoint string
	Token    []byte
	PID      int
	Started  time.Time
}

func (h ProcessHandle) ClientConfig() ClientConfig {
	return ClientConfig{Endpoint: h.Endpoint, Token: append([]byte(nil), h.Token...)}
}

func (h *ProcessHandle) erase() {
	for i := range h.Token {
		h.Token[i] = 0
	}
	h.Token = nil
}

type managedProcess struct {
	spec       LaunchSpec
	cmd        *exec.Cmd
	handle     ProcessHandle
	exit       chan struct{}
	exitErr    error
	lastActive time.Time
}

type ProcessSupervisor struct {
	cfg       SupervisorConfig
	mu        sync.Mutex
	processes map[UserKey]*managedProcess
}

func NewProcessSupervisor(cfg SupervisorConfig) (*ProcessSupervisor, error) {
	if strings.TrimSpace(cfg.Binary) == "" || !filepath.IsAbs(cfg.Binary) {
		return nil, errors.New("agentcore binary must be an absolute path")
	}
	if cfg.CredentialSource == nil {
		return nil, errors.New("coding credential source is required")
	}
	if len(cfg.Arguments) == 0 {
		cfg.Arguments = []string{"serve", "--host", "127.0.0.1", "--port", "{port}", "--isolated"}
	}
	if !hasExactLoopbackHost(cfg.Arguments) || !containsArg(cfg.Arguments, "--isolated") {
		return nil, errors.New("agentcore arguments must bind 127.0.0.1 and enable --isolated")
	}
	if !containsArg(cfg.Arguments, "{port}") {
		return nil, errors.New("agentcore arguments must contain the {port} placeholder")
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 45 * time.Second
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 3 * time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if cfg.PATH == "" {
		cfg.PATH = "/opt/matrix/coding-bin:/opt/agentcore/.venv/bin:/opt/agentcore/node_modules/.bin:/usr/local/bin:/usr/bin:/bin"
	}
	if cfg.Output == nil {
		cfg.Output = io.Discard
	}
	return &ProcessSupervisor{cfg: cfg, processes: make(map[UserKey]*managedProcess)}, nil
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func hasExactLoopbackHost(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--host" && args[i+1] == "127.0.0.1" {
			return true
		}
	}
	return false
}

func (s *ProcessSupervisor) Start(ctx context.Context, spec LaunchSpec) (ProcessHandle, error) {
	if err := s.validateSpec(spec); err != nil {
		return ProcessHandle{}, err
	}
	s.mu.Lock()
	if process := s.processes[spec.User]; process != nil && processRunning(process) {
		s.mu.Unlock()
		return ProcessHandle{}, fmt.Errorf("agentcore process already running for %q", spec.User)
	}
	s.mu.Unlock()
	credential, err := s.cfg.CredentialSource.IssueCodingCredential(ctx, spec.User, spec.ActorDID)
	if err != nil {
		return ProcessHandle{}, fmt.Errorf("issue scoped coding credential: %w", err)
	}
	defer credential.erase()
	if len(credential.Model) == 0 {
		return ProcessHandle{}, errors.New("credential source returned an empty model credential")
	}
	token, err := randomToken()
	if err != nil {
		return ProcessHandle{}, err
	}
	endpoint := "ws://127.0.0.1:" + strconv.Itoa(spec.Port) + "/api/ws"
	args := make([]string, len(s.cfg.Arguments))
	for i, arg := range s.cfg.Arguments {
		args[i] = strings.ReplaceAll(arg, "{port}", strconv.Itoa(spec.Port))
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), s.cfg.Binary, args...)
	env, err := s.buildEnvironment(spec, credential.Model, token)
	if err != nil {
		eraseBytes(token)
		return ProcessHandle{}, err
	}
	cmd.Env = env
	cmd.Dir = spec.Workspace
	cmd.Stdout = s.cfg.Output
	cmd.Stderr = s.cfg.Output
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: spec.UID, Gid: spec.GID}
	}
	if err := cmd.Start(); err != nil {
		eraseStrings(env)
		eraseBytes(token)
		return ProcessHandle{}, fmt.Errorf("start agentcore: %w", err)
	}
	eraseStrings(env)
	cmd.Env = nil
	now := time.Now().UTC()
	process := &managedProcess{
		spec:       spec,
		cmd:        cmd,
		handle:     ProcessHandle{User: spec.User, Endpoint: endpoint, Token: token, PID: cmd.Process.Pid, Started: now},
		exit:       make(chan struct{}),
		lastActive: now,
	}
	s.mu.Lock()
	if existing := s.processes[spec.User]; existing != nil && processRunning(existing) {
		s.mu.Unlock()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		process.handle.erase()
		return ProcessHandle{}, fmt.Errorf("agentcore process already running for %q", spec.User)
	}
	s.processes[spec.User] = process
	s.mu.Unlock()
	go s.waitProcess(spec.User, process)
	if err := s.waitHealthy(ctx, process); err != nil {
		_ = s.stopProcess(context.Background(), process, true)
		s.mu.Lock()
		if s.processes[spec.User] == process {
			delete(s.processes, spec.User)
		}
		s.mu.Unlock()
		return ProcessHandle{}, err
	}
	return cloneHandle(process.handle), nil
}

func (s *ProcessSupervisor) validateSpec(spec LaunchSpec) error {
	if strings.TrimSpace(string(spec.User)) == "" || strings.TrimSpace(spec.ActorDID) == "" {
		return errors.New("user key and actor DID are required")
	}
	if spec.Port < 1024 || spec.Port > 65535 {
		return errors.New("agentcore port must be between 1024 and 65535")
	}
	if spec.UID == 0 || spec.GID == 0 {
		return errors.New("agentcore must run with an unprivileged uid and gid")
	}
	if os.Geteuid() != 0 && (uint32(os.Geteuid()) != spec.UID || uint32(os.Getegid()) != spec.GID) {
		return errors.New("non-root supervisor may only launch with its own uid and gid")
	}
	for name, path := range map[string]string{"workspace": spec.Workspace, "home": spec.Home, "managed directory": s.cfg.ManagedDir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an existing absolute directory", name)
		}
	}
	home, _ := filepath.EvalSymlinks(spec.Home)
	workspace, _ := filepath.EvalSymlinks(spec.Workspace)
	if home == workspace || pathWithin(workspace, home) {
		return errors.New("agentcore home must be outside the workspace")
	}
	gateway, err := url.Parse(strings.TrimSpace(spec.GatewayURL))
	if err != nil || (gateway.Scheme != "http" && gateway.Scheme != "https") || gateway.Host == "" || !strings.HasSuffix(strings.TrimRight(gateway.Path, "/"), "/v1") {
		return errors.New("gateway URL must be an HTTP(S) URL ending in /v1")
	}
	for key := range spec.ExtraEnv {
		if _, ok := s.cfg.AllowedExtraEnv[key]; !ok {
			return fmt.Errorf("environment variable %q is not allowed", key)
		}
	}
	return nil
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *ProcessSupervisor) buildEnvironment(spec LaunchSpec, credential, token []byte) ([]string, error) {
	required := map[string]string{
		"HOME":                              spec.Home,
		"PATH":                              s.cfg.PATH,
		"LANG":                              "C.UTF-8",
		"LD_LIBRARY_PATH":                   "/opt/agentcore/lib",
		"AGENTCORE_HOME":                    spec.Home,
		"AGENTCORE_MANAGED_DIR":             s.cfg.ManagedDir,
		"AGENTCORE_WRITE_SAFE_ROOT":         spec.Workspace,
		"AGENTCORE_SAFE_MODE":               "1",
		"AGENTCORE_TUI_TOOLSETS":            "matrix-coding-v1",
		"AGENTCORE_TUI_TOOL_PROGRESS":       "all",
		"AGENTCORE_VERIFY_ON_STOP":          "1",
		"AGENTCORE_DISABLE_LAZY_INSTALLS":   "1",
		"AGENTCORE_DASHBOARD_SESSION_TOKEN": string(token),
		"TERMINAL_ENV":                      "local",
		"XIAOMI_API_KEY":                    string(credential),
		"XIAOMI_BASE_URL":                   strings.TrimRight(spec.GatewayURL, "/"),
		"MATRIX_ACTOR_DID":                  spec.ActorDID,
	}
	for key, value := range spec.ExtraEnv {
		if _, ok := s.cfg.AllowedExtraEnv[key]; !ok {
			return nil, fmt.Errorf("environment variable %q is not allowed", key)
		}
		if _, reserved := required[key]; reserved {
			return nil, fmt.Errorf("environment variable %q is runtime-controlled", key)
		}
		if strings.ContainsRune(key, '=') || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("environment variable %q is malformed", key)
		}
	}
	keys := []string{
		"HOME", "PATH", "LANG", "LD_LIBRARY_PATH", "AGENTCORE_HOME", "AGENTCORE_MANAGED_DIR",
		"AGENTCORE_WRITE_SAFE_ROOT", "AGENTCORE_SAFE_MODE", "AGENTCORE_TUI_TOOLSETS",
		"AGENTCORE_TUI_TOOL_PROGRESS", "AGENTCORE_VERIFY_ON_STOP", "AGENTCORE_DISABLE_LAZY_INSTALLS",
		"AGENTCORE_DASHBOARD_SESSION_TOKEN", "TERMINAL_ENV", "XIAOMI_API_KEY", "XIAOMI_BASE_URL",
		"MATRIX_ACTOR_DID",
	}
	env := make([]string, 0, len(required)+len(spec.ExtraEnv))
	for _, key := range keys {
		if value, ok := required[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	extraKeys := make([]string, 0, len(spec.ExtraEnv))
	for key := range spec.ExtraEnv {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		env = append(env, key+"="+spec.ExtraEnv[key])
	}
	return env, nil
}

func randomToken() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("create loopback token: %w", err)
	}
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(raw)))
	base64.RawURLEncoding.Encode(encoded, raw)
	eraseBytes(raw)
	return encoded, nil
}

func eraseBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func eraseStrings(values []string) {
	for i := range values {
		values[i] = ""
	}
}

func cloneHandle(handle ProcessHandle) ProcessHandle {
	copy := handle
	copy.Token = append([]byte(nil), handle.Token...)
	return copy
}

func processRunning(process *managedProcess) bool {
	select {
	case <-process.exit:
		return false
	default:
		return true
	}
}

func (s *ProcessSupervisor) waitProcess(user UserKey, process *managedProcess) {
	err := process.cmd.Wait()
	s.mu.Lock()
	process.exitErr = err
	close(process.exit)
	s.mu.Unlock()
}

func (s *ProcessSupervisor) waitHealthy(ctx context.Context, process *managedProcess) error {
	startupCtx, cancel := context.WithTimeout(ctx, s.cfg.StartupTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	clientCfg := process.handle.ClientConfig()
	defer eraseBytes(clientCfg.Token)
	clientCfg.HealthTimeout = s.cfg.HealthTimeout
	for {
		if _, err := HealthCheck(startupCtx, clientCfg); err == nil {
			return nil
		}
		select {
		case <-process.exit:
			return fmt.Errorf("agentcore exited during startup: %v", process.exitErr)
		case <-startupCtx.Done():
			return fmt.Errorf("agentcore did not become healthy: %w", startupCtx.Err())
		case <-ticker.C:
		}
	}
}

func (s *ProcessSupervisor) Handle(user UserKey) (ProcessHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	process := s.processes[user]
	if process == nil || !processRunning(process) {
		return ProcessHandle{}, false
	}
	return cloneHandle(process.handle), true
}

func (s *ProcessSupervisor) Touch(user UserKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	process := s.processes[user]
	if process == nil || !processRunning(process) {
		return false
	}
	process.lastActive = time.Now().UTC()
	return true
}

func (s *ProcessSupervisor) Health(ctx context.Context, user UserKey) (Health, error) {
	handle, ok := s.Handle(user)
	if !ok {
		return Health{}, errors.New("agentcore process is not running")
	}
	defer handle.erase()
	cfg := handle.ClientConfig()
	defer eraseBytes(cfg.Token)
	cfg.HealthTimeout = s.cfg.HealthTimeout
	return HealthCheck(ctx, cfg)
}

func (s *ProcessSupervisor) Stop(ctx context.Context, user UserKey) error {
	s.mu.Lock()
	process := s.processes[user]
	if process != nil {
		delete(s.processes, user)
	}
	s.mu.Unlock()
	if process == nil {
		return nil
	}
	return s.stopProcess(ctx, process, false)
}

func (s *ProcessSupervisor) stopProcess(ctx context.Context, process *managedProcess, force bool) error {
	defer process.handle.erase()
	if !processRunning(process) {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-process.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	stopCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		stopCtx, cancel = context.WithTimeout(ctx, s.cfg.StopTimeout)
	}
	defer cancel()
	select {
	case <-process.exit:
		return nil
	case <-stopCtx.Done():
		_ = syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-process.exit:
			return nil
		case <-time.After(s.cfg.StopTimeout):
			return fmt.Errorf("agentcore forced stop timed out: %w", stopCtx.Err())
		}
	}
}

func (s *ProcessSupervisor) Restart(ctx context.Context, user UserKey) (ProcessHandle, error) {
	s.mu.Lock()
	process := s.processes[user]
	if process == nil {
		s.mu.Unlock()
		return ProcessHandle{}, errors.New("agentcore launch specification is unavailable")
	}
	spec := process.spec
	delete(s.processes, user)
	s.mu.Unlock()
	if err := s.stopProcess(ctx, process, false); err != nil {
		return ProcessHandle{}, err
	}
	return s.Start(ctx, spec)
}

func (s *ProcessSupervisor) RotateCredential(ctx context.Context, user UserKey) (ProcessHandle, error) {
	return s.Restart(ctx, user)
}

func (s *ProcessSupervisor) ForceDeath(ctx context.Context, user UserKey, restart bool) (ProcessHandle, error) {
	s.mu.Lock()
	process := s.processes[user]
	if process == nil {
		s.mu.Unlock()
		return ProcessHandle{}, errors.New("agentcore process is not running")
	}
	spec := process.spec
	delete(s.processes, user)
	s.mu.Unlock()
	if err := s.stopProcess(ctx, process, true); err != nil {
		return ProcessHandle{}, err
	}
	if !restart {
		return ProcessHandle{}, nil
	}
	return s.Start(ctx, spec)
}

func (s *ProcessSupervisor) ReapIdle(ctx context.Context, now time.Time) []UserKey {
	s.mu.Lock()
	var victims []*managedProcess
	var users []UserKey
	for user, process := range s.processes {
		if processRunning(process) && now.Sub(process.lastActive) >= s.cfg.IdleTimeout {
			delete(s.processes, user)
			victims = append(victims, process)
			users = append(users, user)
		}
	}
	s.mu.Unlock()
	for _, process := range victims {
		_ = s.stopProcess(ctx, process, false)
	}
	return users
}

func (s *ProcessSupervisor) RunIdleReaper(ctx context.Context) {
	interval := s.cfg.IdleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.ReapIdle(ctx, now)
		}
	}
}
