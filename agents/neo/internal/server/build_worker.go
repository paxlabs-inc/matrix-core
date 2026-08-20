package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"centra/agents/neo/internal/codingruntime"
	"centra/agents/neo/internal/codingworker"
	"centra/agents/neo/internal/preview"
)

type BuildWorkerConfig struct {
	Supervisor   *codingworker.ProcessSupervisor
	ActorDID     string
	GatewayURL   string
	HomeRoot     string
	Port         int
	UID          uint32
	GID          uint32
	Model        string
	Provider     string
	EventTimeout time.Duration
	Preview      *preview.RouterRegistrar
}

type buildWorkerService struct {
	engine *Engine
	cfg    BuildWorkerConfig
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan string

	mu         sync.Mutex
	queued     map[string]bool
	active     map[string]activeBuildWorker
	retained   map[string]bool
	previewJob string
	workerWG   sync.WaitGroup
}

type activeBuildWorker struct {
	client    *codingworker.Client
	runtimeID codingworker.SessionID
}

func (e *Engine) StartBuildWorker(parent context.Context, cfg BuildWorkerConfig) error {
	if e == nil || e.buildJobs == nil || !e.buildJobs.Enabled() {
		return errors.New("durable Build jobs are disabled")
	}
	if cfg.Supervisor == nil {
		return errors.New("AgentCore supervisor is required")
	}
	cfg.ActorDID = strings.TrimSpace(cfg.ActorDID)
	cfg.GatewayURL = strings.TrimRight(strings.TrimSpace(cfg.GatewayURL), "/")
	cfg.HomeRoot = filepath.Clean(strings.TrimSpace(cfg.HomeRoot))
	if cfg.ActorDID == "" || cfg.GatewayURL == "" || !filepath.IsAbs(cfg.HomeRoot) {
		return errors.New("Build worker actor, gateway URL, and absolute home root are required")
	}
	if cfg.Port == 0 {
		cfg.Port = 9119
	}
	if cfg.Model == "" {
		cfg.Model = "mimo-v2.5-pro"
	}
	if cfg.Provider == "" {
		cfg.Provider = "xiaomi"
	}
	if cfg.EventTimeout <= 0 {
		cfg.EventTimeout = 20 * time.Minute
	}
	if err := os.MkdirAll(cfg.HomeRoot, 0o700); err != nil {
		return fmt.Errorf("create AgentCore home root: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(cfg.HomeRoot, int(cfg.UID), int(cfg.GID)); err != nil {
			return fmt.Errorf("own AgentCore home root: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	service := &buildWorkerService{
		engine: e, cfg: cfg, ctx: ctx, cancel: cancel, queue: make(chan string, 128),
		queued: make(map[string]bool), active: make(map[string]activeBuildWorker), retained: make(map[string]bool),
	}
	e.buildWorker = service
	e.buildDispatch = service.Enqueue
	e.buildInterrupt = service.Interrupt
	if e.tools != nil {
		e.tools.SetBuildProject(e.agentBuild)
	}
	service.workerWG.Add(3)
	go service.run()
	go service.runWakeOutbox()
	go func() {
		defer service.workerWG.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				for _, user := range cfg.Supervisor.ReapIdle(ctx, now) {
					service.stopPreview(string(user))
				}
			}
		}
	}()
	jobs, err := e.buildJobs.List()
	if err != nil {
		service.Stop()
		return fmt.Errorf("load durable Build jobs: %w", err)
	}
	for _, job := range jobs {
		if !job.Terminal() {
			service.Enqueue(job)
		}
	}
	return nil
}

func (s *buildWorkerService) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.mu.Lock()
	active := make(map[string]activeBuildWorker, len(s.active))
	for id, worker := range s.active {
		active[id] = worker
	}
	s.mu.Unlock()
	for id, worker := range active {
		_ = worker.client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.cfg.Supervisor.Stop(ctx, codingworker.UserKey(id))
		cancel()
	}
	s.mu.Lock()
	retained := make([]string, 0, len(s.retained))
	for id := range s.retained {
		retained = append(retained, id)
	}
	s.mu.Unlock()
	for _, id := range retained {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.cfg.Supervisor.Stop(ctx, codingworker.UserKey(id))
		cancel()
	}
	s.stopPreview("")
	waitBounded(&s.workerWG, 10*time.Second)
}

func (s *buildWorkerService) Enqueue(job codingruntime.Job) {
	if s == nil || job.ID == "" || job.Terminal() {
		return
	}
	s.mu.Lock()
	active, running := s.active[job.ID]
	if running {
		s.mu.Unlock()
		go s.dispatchActiveInput(job.ID, active)
		return
	}
	if s.queued[job.ID] {
		s.mu.Unlock()
		return
	}
	s.queued[job.ID] = true
	s.mu.Unlock()
	select {
	case s.queue <- job.ID:
	case <-s.ctx.Done():
		s.mu.Lock()
		delete(s.queued, job.ID)
		s.mu.Unlock()
	default:
		go func() {
			select {
			case s.queue <- job.ID:
			case <-s.ctx.Done():
				s.mu.Lock()
				delete(s.queued, job.ID)
				s.mu.Unlock()
			}
		}()
	}
}

func (s *buildWorkerService) dispatchActiveInput(jobID string, active activeBuildWorker) {
	for attempt := 0; attempt < 8; attempt++ {
		job, ok, err := s.engine.buildJobs.Get(jobID)
		if err != nil || !ok || job.Terminal() {
			return
		}
		_, action, err := s.claimNextInput(job)
		if errors.Is(err, codingruntime.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			s.recordWorkerFailure(jobID, err)
			return
		}
		if action == nil {
			return
		}
		if err := action(active.client, active.runtimeID); err != nil {
			s.recordWorkerFailure(jobID, err)
		}
		return
	}
	s.recordWorkerFailure(jobID, codingruntime.ErrRevisionConflict)
}

func (s *buildWorkerService) run() {
	defer s.workerWG.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case id := <-s.queue:
			s.mu.Lock()
			delete(s.queued, id)
			s.mu.Unlock()
			job, ok, err := s.engine.buildJobs.Get(id)
			if err != nil || !ok || job.Terminal() {
				continue
			}
			if err := s.process(job); err != nil && !errors.Is(err, context.Canceled) {
				s.recordWorkerFailure(id, err)
			}
		}
	}
}

func (s *buildWorkerService) process(job codingruntime.Job) error {
	if strings.TrimSpace(job.ActorID) != s.cfg.ActorDID {
		return errors.New("Build job actor does not match this user server")
	}
	project, err := s.engine.resolveProjectRecord(job.ProjectID)
	if err != nil || filepath.Clean(project.Root) != filepath.Clean(job.CWD) {
		return errors.New("Build job project binding is no longer valid")
	}
	info, err := os.Lstat(project.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Build job project root is unavailable or unsafe")
	}
	realWorkspace, workspaceErr := filepath.EvalSymlinks(s.engine.workspaceRoot)
	realProject, projectErr := filepath.EvalSymlinks(project.Root)
	relative, relativeErr := filepath.Rel(realWorkspace, realProject)
	if workspaceErr != nil || projectErr != nil || relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Build job project root leaves this user workspace")
	}
	s.stopRetainedExcept(job.ID)
	home := filepath.Join(s.cfg.HomeRoot, job.ID)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create job runtime home: %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(home, int(s.cfg.UID), int(s.cfg.GID)); err != nil {
			return fmt.Errorf("own job runtime home: %w", err)
		}
	}
	user := codingworker.UserKey(job.ID)
	launch := codingworker.LaunchSpec{
		User: user, ActorDID: s.cfg.ActorDID, GatewayURL: s.cfg.GatewayURL,
		Workspace: job.CWD, Home: home, Port: s.cfg.Port, UID: s.cfg.UID, GID: s.cfg.GID,
	}
	handle, ok := s.cfg.Supervisor.Handle(user)
	if ok {
		var healthErr error
		if time.Since(handle.Started) >= 25*time.Minute {
			healthErr = errors.New("coding credential rotation is due")
		} else {
			healthCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			_, healthErr = s.cfg.Supervisor.Health(healthCtx, user)
			cancel()
		}
		if healthErr != nil {
			_ = s.cfg.Supervisor.Stop(context.Background(), user)
			handle, healthErr = s.cfg.Supervisor.Start(s.ctx, launch)
			if healthErr != nil {
				return fmt.Errorf("restart unhealthy coding worker: %w", healthErr)
			}
		}
	} else {
		var err error
		handle, err = s.cfg.Supervisor.Start(s.ctx, launch)
		if err != nil {
			return err
		}
	}
	keepProcess := false
	defer func() {
		if keepProcess {
			s.mu.Lock()
			s.retained[job.ID] = true
			s.mu.Unlock()
			return
		}
		s.stopPreview(job.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.cfg.Supervisor.Stop(ctx, user)
		cancel()
	}()
	client, err := codingworker.Dial(s.ctx, handle.ClientConfig())
	for index := range handle.Token {
		handle.Token[index] = 0
	}
	if err != nil {
		return err
	}
	defer client.Close()

	var session codingworker.Session
	if job.Runtime.SessionID == "" {
		session, err = client.Create(s.ctx, codingworker.CreateOptions{
			CWD: job.CWD, Title: "Neo Build: " + truncateBuildText(job.Brief.Intent, 120),
			Source: "centra-neo-build", Model: s.cfg.Model, Provider: s.cfg.Provider,
			CloseOnDisconnect: false,
		})
		if err != nil {
			return fmt.Errorf("create AgentCore session: %w", err)
		}
		job, err = s.engine.buildJobs.BindRuntime(job.ID, job.Revision, codingruntime.RuntimeBinding{
			SessionID: string(session.DurableID()), ServiceInstanceID: buildServiceInstanceID(),
		})
		if err != nil {
			return fmt.Errorf("persist AgentCore session binding: %w", err)
		}
	} else {
		session, err = client.Resume(s.ctx, codingworker.StoredSessionID(job.Runtime.SessionID), codingworker.ResumeOptions{
			CWD: job.CWD, Source: "centra-neo-build", CloseOnDisconnect: false,
		})
		if err != nil {
			return fmt.Errorf("resume bound AgentCore session: %w", err)
		}
	}

	s.mu.Lock()
	s.active[job.ID] = activeBuildWorker{client: client, runtimeID: session.RuntimeID}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, job.ID)
		s.mu.Unlock()
	}()
	s.cfg.Supervisor.Touch(user)

	job, action, err := s.claimNextInput(job)
	if err != nil {
		return err
	}
	if action != nil {
		if err := action(client, session.RuntimeID); err != nil {
			return fmt.Errorf("submit claimed Build input: %w", err)
		}
	} else if !session.Running && job.Lifecycle != codingruntime.LifecycleRunning {
		return errors.New("the recovered coding session is idle and has no new durable input; send a correction to resume it")
	}

	timer := time.NewTimer(s.cfg.EventTimeout)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-timer.C:
			return errors.New("the coding session stopped producing lifecycle events")
		case <-client.Done():
			return fmt.Errorf("AgentCore connection closed: %w", client.Err())
		case event, open := <-client.Events():
			if !open {
				return fmt.Errorf("AgentCore event stream closed: %w", client.Err())
			}
			if event.SessionID.Valid() && event.SessionID != session.RuntimeID {
				continue
			}
			if event.Type == "message.complete" {
				verifiedEvent, verificationErr := s.attestCompletion(client, session.DurableID(), job.CWD, event)
				if verificationErr != nil {
					return verificationErr
				}
				event = verifiedEvent
			}
			batch, selected, reduceErr := codingruntime.ReduceEvent(event)
			if reduceErr != nil {
				return reduceErr
			}
			if !selected {
				continue
			}
			job, batch, reduceErr = s.preparePreview(job, batch)
			if reduceErr != nil {
				return reduceErr
			}
			updated, applyErr := s.applyDelivery(job.ID, batch)
			if applyErr != nil {
				return applyErr
			}
			job = updated
			s.cfg.Supervisor.Touch(user)
			if job.Terminal() || job.Lifecycle == codingruntime.LifecycleNeedsInput || job.Lifecycle == codingruntime.LifecycleNeedsApproval || job.Lifecycle == codingruntime.LifecycleBlocked {
				keepProcess = job.Lifecycle == codingruntime.LifecycleVerified && job.Runtime.PreviewTarget != ""
				return nil
			}
		}
	}
}

func (s *buildWorkerService) attestCompletion(client *codingworker.Client, sessionID codingworker.StoredSessionID, cwd string, event codingworker.Event) (codingworker.Event, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return event, fmt.Errorf("decode AgentCore completion for verification: %w", err)
	}
	status, _ := payload["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "interrupted", "cancelled", "canceled", "aborted", "error", "failed", "failure":
		return event, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	verification, err := client.VerificationStatus(ctx, sessionID, cwd)
	cancel()
	if err != nil {
		return event, fmt.Errorf("read AgentCore verification status: %w", err)
	}
	payload["verification_status"] = verification.Status
	if verification.Status == "passed" {
		payload["status"] = "verified"
	} else {
		payload["status"] = "unverified"
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return event, err
	}
	event.Payload = encoded
	return event, nil
}

func (s *buildWorkerService) stopRetainedExcept(jobID string) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.retained))
	for id := range s.retained {
		if id != jobID {
			ids = append(ids, id)
			delete(s.retained, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.cfg.Supervisor.Stop(ctx, codingworker.UserKey(id))
		cancel()
		s.stopPreview(id)
	}
}

func (s *buildWorkerService) preparePreview(job codingruntime.Job, batch codingruntime.DeliveryBatch) (codingruntime.Job, codingruntime.DeliveryBatch, error) {
	for index := range batch.Evidence {
		if batch.Evidence[index].Kind != codingruntime.EvidencePreview || batch.Evidence[index].PreviewURL == "" {
			continue
		}
		target, err := privatePreviewTarget(batch.Evidence[index].PreviewURL)
		if err != nil {
			batch.Evidence[index].PreviewURL = ""
			continue
		}
		if job.Runtime.PreviewTarget != target {
			binding := job.Runtime
			binding.PreviewTarget = target
			job, err = s.engine.buildJobs.BindRuntime(job.ID, job.Revision, binding)
			if err != nil {
				return job, batch, err
			}
		}
		publicURL := "/build-jobs/" + job.ID + "/preview/"
		if s.cfg.Preview != nil && s.cfg.Preview.Enabled() {
			parsed, _ := url.Parse(target)
			port, portErr := strconv.Atoi(parsed.Port())
			if portErr != nil {
				return job, batch, portErr
			}
			publicURL, err = s.cfg.Preview.Register(s.ctx, port)
			if err != nil {
				return job, batch, err
			}
			s.mu.Lock()
			s.previewJob = job.ID
			s.mu.Unlock()
		}
		batch.Evidence[index].PreviewURL = publicURL
	}
	return job, batch, nil
}

func (s *buildWorkerService) stopPreview(jobID string) {
	if s.cfg.Preview == nil || !s.cfg.Preview.Enabled() {
		return
	}
	s.mu.Lock()
	if jobID != "" && s.previewJob != jobID {
		s.mu.Unlock()
		return
	}
	s.previewJob = ""
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = s.cfg.Preview.Deregister(ctx)
	cancel()
}

func privatePreviewTarget(raw string) (string, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Scheme != "http" || target.User != nil || target.Host == "" {
		return "", errors.New("preview target must be an unauthenticated HTTP loopback URL")
	}
	host := strings.ToLower(target.Hostname())
	parsed := net.ParseIP(host)
	if host != "localhost" && (parsed == nil || !parsed.IsLoopback()) {
		return "", errors.New("preview target must be loopback")
	}
	if target.Port() == "" {
		return "", errors.New("preview target must declare a port")
	}
	target.Host = net.JoinHostPort("127.0.0.1", target.Port())
	target.RawQuery = ""
	target.Fragment = ""
	return target.String(), nil
}

type buildInputAction func(*codingworker.Client, codingworker.SessionID) error

func (s *buildWorkerService) claimNextInput(job codingruntime.Job) (codingruntime.Job, buildInputAction, error) {
	if !job.Runtime.InitialPromptTried {
		binding := job.Runtime
		binding.InitialPromptTried = true
		if len(job.Corrections) > 0 {
			binding.LastCorrectionID = job.Corrections[len(job.Corrections)-1].ID
		}
		updated, err := s.engine.buildJobs.BindRuntime(job.ID, job.Revision, binding)
		if err != nil {
			return job, nil, err
		}
		return updated, promptBuildAction(s.ctx, initialBuildPrompt(updated)), nil
	}
	if len(job.Answers) > 0 {
		answer := job.Answers[len(job.Answers)-1]
		if answer.ID != job.Runtime.LastAnswerID {
			binding := job.Runtime
			binding.LastAnswerID = answer.ID
			updated, err := s.engine.buildJobs.BindRuntime(job.ID, job.Revision, binding)
			if err != nil {
				return job, nil, err
			}
			if answer.Kind == codingruntime.AnswerApproval {
				choice := codingworker.DenyApproval
				if strings.EqualFold(answer.Text, "approved") {
					choice = codingworker.ApproveOnce
				}
				return updated, func(client *codingworker.Client, runtime codingworker.SessionID) error {
					_, err := client.Approve(s.ctx, runtime, choice, false)
					return err
				}, nil
			}
			prompt := "User answer to the pending question: " + answer.Text
			return updated, promptBuildAction(s.ctx, prompt), nil
		}
	}
	if len(job.Corrections) > 0 {
		correction := job.Corrections[len(job.Corrections)-1]
		if correction.ID != job.Runtime.LastCorrectionID {
			binding := job.Runtime
			binding.LastCorrectionID = correction.ID
			updated, err := s.engine.buildJobs.BindRuntime(job.ID, job.Revision, binding)
			if err != nil {
				return job, nil, err
			}
			return updated, promptBuildAction(s.ctx, "User correction for this same Build job: "+correction.Text), nil
		}
	}
	return job, nil, nil
}

func promptBuildAction(ctx context.Context, prompt string) buildInputAction {
	return func(client *codingworker.Client, runtime codingworker.SessionID) error {
		_, err := client.Prompt(ctx, runtime, prompt)
		return err
	}
}

func initialBuildPrompt(job codingruntime.Job) string {
	request := struct {
		Brief       codingruntime.BuildBrief   `json:"brief"`
		Corrections []codingruntime.Correction `json:"corrections,omitempty"`
	}{Brief: job.Brief, Corrections: job.Corrections}
	brief, _ := json.Marshal(request)
	return "Implement this bounded Centra Build job in the current working directory. Work only inside that directory. Do not deploy, access production credentials, change git history, or claim a check passed unless you ran it. Preserve the existing project and implement complete runnable code. Use the clarification or approval protocol only when user input is genuinely required. Run relevant local verification before completion. For a frontend preview, bind the dev server on an unprivileged port to 0.0.0.0 on this managed private server; Centra AI will expose it through its existing authenticated preview path.\n\nBuild brief:\n" + string(brief)
}

func (s *buildWorkerService) applyDelivery(jobID string, batch codingruntime.DeliveryBatch) (codingruntime.Job, error) {
	for attempts := 0; attempts < 8; attempts++ {
		job, ok, err := s.engine.buildJobs.Get(jobID)
		if err != nil || !ok {
			if err == nil {
				err = codingruntime.ErrNotFound
			}
			return job, err
		}
		updated, _, err := s.engine.buildJobs.ApplyDelivery(jobID, job.Revision, batch)
		if errors.Is(err, codingruntime.ErrRevisionConflict) {
			continue
		}
		return updated, err
	}
	return codingruntime.Job{}, codingruntime.ErrRevisionConflict
}

func (s *buildWorkerService) recordWorkerFailure(jobID string, cause error) {
	internal := truncateBuildText(cause.Error(), 2000)
	reason := "The coding worker stopped before it could confirm an outcome. Send a correction to resume this same Build job from its current files."
	sum := sha256.Sum256([]byte("worker-failure\x00" + jobID + "\x00" + internal))
	source := "worker_" + hex.EncodeToString(sum[:16])
	_, _ = s.applyDelivery(jobID, codingruntime.DeliveryBatch{
		DeliveryID: "delivery_" + hex.EncodeToString(sum[16:]), SourceEventIDs: []string{source},
		Evidence:      []codingruntime.EvidenceRecord{{Kind: codingruntime.EvidenceBlocker, Summary: reason, SourceEventID: source}},
		NextLifecycle: codingruntime.LifecycleBlocked,
		Wake:          &codingruntime.WakeRequest{DedupKey: "build-worker-blocked:" + source, Kind: codingruntime.WakeBlocked, Reason: reason, SourceEventID: source},
	})
}

func (s *buildWorkerService) Interrupt(ctx context.Context, job codingruntime.Job) error {
	s.mu.Lock()
	active, ok := s.active[job.ID]
	s.mu.Unlock()
	if ok {
		_, err := active.client.Interrupt(ctx, active.runtimeID)
		return err
	}
	// A queued, paused, or disconnected job has no in-flight AgentCore effect
	// to interrupt. The caller's durable terminal CAS is authoritative and the
	// worker re-reads it before doing any further work.
	return nil
}

func buildServiceInstanceID() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

func truncateBuildText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit]) + " [truncated]"
}
