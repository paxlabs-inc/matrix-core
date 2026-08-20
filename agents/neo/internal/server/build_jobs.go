package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"centra/agents/neo/internal/codingruntime"
	"centra/agents/neo/internal/codingworker"
	"centra/agents/neo/internal/tools"
)

const maxBuildRequestBytes = 128 << 10

type buildJobView struct {
	ID             string                  `json:"id"`
	ConversationID string                  `json:"conversation_id"`
	Project        string                  `json:"project"`
	Status         codingruntime.Lifecycle `json:"status"`
	Outcome        codingruntime.Outcome   `json:"outcome,omitempty"`
	Summary        string                  `json:"summary"`
	StatusText     string                  `json:"status_text,omitempty"`
	Evidence       []buildEvidenceView     `json:"evidence"`
	Question       *buildQuestionView      `json:"question,omitempty"`
	Approval       *buildApprovalView      `json:"approval,omitempty"`
	Preview        *buildPreviewView       `json:"preview,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
	Revision       uint64                  `json:"revision"`
}

type buildEvidenceView struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Detail  string `json:"detail,omitempty"`
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	Diff    string `json:"diff,omitempty"`
	OK      *bool  `json:"ok,omitempty"`
	URL     string `json:"url,omitempty"`
	TS      string `json:"ts,omitempty"`
}

type buildQuestionView struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

type buildApprovalView struct {
	ID      string `json:"id"`
	Prompt  string `json:"prompt"`
	Details string `json:"details,omitempty"`
}

type buildPreviewView struct {
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

func viewBuildJob(job codingruntime.Job) buildJobView {
	public := job.Public()
	view := buildJobView{
		ID: public.ID, ConversationID: public.ConversationID, Project: public.ProjectID,
		Status: public.Lifecycle, Outcome: public.Outcome, Summary: public.Brief.Intent,
		StatusText: public.OutcomeSummary, Evidence: make([]buildEvidenceView, 0, len(public.Evidence)),
		CreatedAt: public.CreatedAt, UpdatedAt: public.UpdatedAt, Revision: public.Revision,
	}
	for _, item := range public.Evidence {
		switch item.Kind {
		case codingruntime.EvidenceQuestion:
			view.Question = &buildQuestionView{ID: item.ID, Prompt: item.Question}
		case codingruntime.EvidenceApproval:
			view.Approval = &buildApprovalView{ID: item.ID, Prompt: item.Question, Details: item.Summary}
		case codingruntime.EvidencePreview:
			state := "pending"
			if item.PreviewURL != "" {
				state = "ready"
			} else if item.Error != "" {
				state = "failed"
			}
			view.Preview = &buildPreviewView{State: state, URL: item.PreviewURL, Error: item.Error}
			view.Evidence = append(view.Evidence, projectBuildEvidence(item))
		default:
			view.Evidence = append(view.Evidence, projectBuildEvidence(item))
		}
	}
	return view
}

func projectBuildEvidence(item codingruntime.PublicEvidence) buildEvidenceView {
	kind := string(item.Kind)
	switch item.Kind {
	case codingruntime.EvidenceFileChange:
		kind = "file"
	case codingruntime.EvidenceVerification:
		kind = "check"
	case codingruntime.EvidenceBlocker:
		kind = "error"
	}
	label := strings.TrimSpace(item.Summary)
	if label == "" {
		for _, candidate := range []string{item.Check, item.Path, item.Command, kind} {
			if strings.TrimSpace(candidate) != "" {
				label = candidate
				break
			}
		}
	}
	detail := item.Error
	if detail == "" && item.Check != "" && item.Check != label {
		detail = item.Check
	}
	return buildEvidenceView{
		ID: item.ID, Kind: kind, Label: label, Detail: detail, Path: item.Path,
		Command: item.Command, Diff: item.Diff, OK: item.Passed, URL: item.PreviewURL,
		TS: item.RecordedAt.Format(time.RFC3339Nano),
	}
}

func (s *Server) handleBuildJobs(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.buildJobs == nil || !s.engine.buildJobs.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Build jobs are unavailable on this machine."})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/build-jobs"), "/")
	if path == "" {
		switch r.Method {
		case http.MethodGet:
			s.listBuildJobs(w, r)
		case http.MethodPost:
			if s.engine.buildDispatch == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "The private Build worker is unavailable on this machine."})
				return
			}
			s.createBuildJob(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
		}
		return
	}
	parts := strings.Split(path, "/")
	if parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
		return
	}
	if len(parts) >= 2 && parts[1] == "preview" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
			return
		}
		s.proxyBuildPreview(w, r, parts[0])
		return
	}
	if len(parts) > 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
			return
		}
		job, ok, err := s.engine.buildJobs.Get(parts[0])
		if err != nil || !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
			return
		}
		writeJSON(w, http.StatusOK, viewBuildJob(job))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
		return
	}
	switch parts[1] {
	case "answers":
		s.answerBuildJob(w, r, parts[0])
	case "corrections":
		s.correctBuildJob(w, r, parts[0])
	case "interrupt":
		s.interruptBuildJob(w, r, parts[0])
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job action not found."})
	}
}

func (s *Server) proxyBuildPreview(w http.ResponseWriter, r *http.Request, id string) {
	job, ok, err := s.engine.buildJobs.Get(id)
	if err != nil || !ok || job.Runtime.PreviewTarget == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build preview is not available."})
		return
	}
	if s.engine.buildWorker != nil {
		s.engine.buildWorker.cfg.Supervisor.Touch(codingworker.UserKey(job.ID))
	}
	target, err := url.Parse(job.Runtime.PreviewTarget)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Build preview target is invalid."})
		return
	}
	prefix := "/build-jobs/" + id + "/preview"
	if token := strings.TrimSpace(r.URL.Query().Get("access_token")); token != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "mx_pv", Value: token, Path: prefix, HttpOnly: true,
			Secure: true, SameSite: http.SameSiteNoneMode, MaxAge: 3600,
		})
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	baseDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		baseDirector(request)
		suffix := strings.TrimPrefix(request.URL.Path, prefix)
		if suffix == "" {
			suffix = "/"
		}
		request.URL.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
		query := request.URL.Query()
		query.Del("access_token")
		request.URL.RawQuery = query.Encode()
		request.Host = target.Host
		request.Header.Del("Authorization")
		request.Header.Del("Cookie")
		request.Header.Del("X-Matrix-Actor-DID")
		request.Header.Set("X-Forwarded-Prefix", prefix)
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
		writeJSON(writer, http.StatusBadGateway, map[string]string{"error": "Build preview is no longer running."})
	}
	w.Header().Set("Cache-Control", "no-store")
	proxy.ServeHTTP(w, r)
}

func (s *Server) listBuildJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.engine.buildJobs.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not read Build jobs."})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	conversation := strings.TrimSpace(r.URL.Query().Get("conversation_id"))
	items := make([]buildJobView, 0, len(jobs))
	for _, job := range jobs {
		if project != "" && job.ProjectID != project || conversation != "" && job.ConversationID != conversation {
			continue
		}
		items = append(items, viewBuildJob(job))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": items})
}

type buildBriefBody struct {
	RequestedBehavior string   `json:"requested_behavior"`
	Intent            string   `json:"intent"`
	Constraints       []string `json:"constraints"`
	Acceptance        []string `json:"acceptance_criteria"`
	ProjectContext    string   `json:"project_context"`
}

func (brief buildBriefBody) runtimeBrief() codingruntime.BuildBrief {
	intent := strings.TrimSpace(brief.Intent)
	if intent == "" {
		intent = strings.TrimSpace(brief.RequestedBehavior)
	}
	context := map[string]string(nil)
	if value := strings.TrimSpace(brief.ProjectContext); value != "" {
		context = map[string]string{"summary": value}
	}
	return codingruntime.BuildBrief{
		Intent: intent, Constraints: brief.Constraints,
		AcceptanceCriteria: brief.Acceptance, ProjectContext: context,
	}
}

type buildCreateBody struct {
	ConversationID string         `json:"conversation_id"`
	Project        string         `json:"project"`
	Brief          buildBriefBody `json:"brief"`
}

func (s *Server) createBuildJob(w http.ResponseWriter, r *http.Request) {
	var request buildCreateBody
	if err := decodeBuildJSON(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Matrix-Idempotency-Key"))
	}
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required."})
		return
	}
	job, created, err := s.engine.acceptBuild(request.ConversationID, request.Project, request.Brief.runtimeBrief(), key)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, codingruntime.ErrIdempotencyConflict) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, viewBuildJob(job))
}

func (e *Engine) acceptBuild(conversationID, projectID string, brief codingruntime.BuildBrief, idempotencyKey string) (codingruntime.Job, bool, error) {
	conversationID = strings.TrimSpace(conversationID)
	projectID = strings.TrimSpace(projectID)
	brief.Intent = strings.TrimSpace(brief.Intent)
	if conversationID == "" || projectID == "" || brief.Intent == "" {
		return codingruntime.Job{}, false, errors.New("conversation_id, project, and brief.intent are required")
	}
	project, err := e.resolveProjectRecord(projectID)
	if err != nil {
		return codingruntime.Job{}, false, err
	}
	actorID := strings.TrimSpace(e.cfg.ActorDID)
	if actorID == "" {
		actorID = strings.TrimSpace(os.Getenv("MATRIX_USER_ID"))
	}
	serverID, _ := os.Hostname()
	job, created, err := e.buildJobs.CreateQueued(codingruntime.CreateRequest{
		ActorID: actorID, ConversationID: conversationID, UserServerID: serverID,
		ProjectID: project.ID, CWD: project.Root, Brief: brief, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return job, false, err
	}
	if created && e.buildDispatch != nil {
		e.buildDispatch(job)
	}
	return job, created, nil
}

func (e *Engine) agentBuild(ctx context.Context, request tools.BuildRequest) (string, error) {
	run := runFromContext(ctx)
	if run == nil {
		return "", errors.New("Build dispatch has no active Neo conversation")
	}
	projectHash := sha256.Sum256([]byte(run.id + "\x00" + request.Request))
	projectID := projectDirSlug(request.Project)
	if projectID == "" {
		projectID = strings.TrimSpace(e.conv.Project(run.convID))
	}
	if projectID == "" {
		request.Project = "neo-build-" + hex.EncodeToString(projectHash[:6])
		projectID = projectDirSlug(request.Project)
	}
	if _, err := e.resolveProjectRecord(projectID); err != nil {
		project, _, createErr := e.ensureBuildProject(request.Project)
		if createErr != nil {
			return "", createErr
		}
		projectID = project.ID
	}
	hash := sha256.Sum256([]byte(run.id + "\x00" + projectID + "\x00" + request.Request))
	job, _, err := e.acceptBuild(run.convID, projectID, codingruntime.BuildBrief{
		Intent: request.Request, AcceptanceCriteria: request.AcceptanceCriteria,
		Constraints: append(request.Constraints, "Do not deploy or use production credentials."),
	}, "neo-build-"+hex.EncodeToString(hash[:]))
	if err != nil {
		return "", err
	}
	e.conv.SetProject(run.convID, projectID)
	return fmt.Sprintf("Build job %s was accepted for project %s; project selection or creation is atomic with admission. End this turn; you will be woken only if it needs input or reaches an outcome.", job.ID, projectID), nil
}

func (s *Server) correctBuildJob(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		Message string `json:"message"`
	}
	if err := decodeBuildJSON(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job, ok, err := s.engine.buildJobs.Get(id)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
		return
	}
	updated, err := s.engine.buildJobs.AddCorrection(id, job.Revision, request.Message)
	if err != nil {
		writeBuildMutationError(w, err)
		return
	}
	if s.engine.buildDispatch != nil {
		s.engine.buildDispatch(updated)
	}
	writeJSON(w, http.StatusAccepted, viewBuildJob(updated))
}

func (s *Server) answerBuildJob(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		Kind       string `json:"kind"`
		TargetID   string `json:"target_id"`
		QuestionID string `json:"question_id"`
		ApprovalID string `json:"approval_id"`
		Answer     string `json:"answer"`
		Approved   *bool  `json:"approved"`
	}
	if err := decodeBuildJSON(w, r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	job, ok, err := s.engine.buildJobs.Get(id)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
		return
	}
	kind := codingruntime.AnswerKind(strings.TrimSpace(request.Kind))
	text := strings.TrimSpace(request.Answer)
	if kind == codingruntime.AnswerApproval && request.Approved != nil {
		if *request.Approved {
			text = "approved"
		} else {
			text = "denied"
		}
	}
	targetID := strings.TrimSpace(request.TargetID)
	if targetID == "" && kind == codingruntime.AnswerQuestion {
		targetID = strings.TrimSpace(request.QuestionID)
	}
	if targetID == "" && kind == codingruntime.AnswerApproval {
		targetID = strings.TrimSpace(request.ApprovalID)
	}
	if targetID == "" {
		for index := len(job.Evidence) - 1; index >= 0; index-- {
			if kind == codingruntime.AnswerQuestion && job.Evidence[index].Kind == codingruntime.EvidenceQuestion || kind == codingruntime.AnswerApproval && job.Evidence[index].Kind == codingruntime.EvidenceApproval {
				targetID = job.Evidence[index].ID
				break
			}
		}
	}
	updated, err := s.engine.buildJobs.AddAnswer(id, job.Revision, targetID, kind, text)
	if err != nil {
		writeBuildMutationError(w, err)
		return
	}
	if s.engine.buildDispatch != nil {
		s.engine.buildDispatch(updated)
	}
	writeJSON(w, http.StatusAccepted, viewBuildJob(updated))
}

func (s *Server) interruptBuildJob(w http.ResponseWriter, r *http.Request, id string) {
	job, ok, err := s.engine.buildJobs.Get(id)
	if err != nil || !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
		return
	}
	if job.Terminal() {
		writeJSON(w, http.StatusOK, viewBuildJob(job))
		return
	}
	if s.engine.buildInterrupt != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err = s.engine.buildInterrupt(ctx, job)
		cancel()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "The coding worker could not confirm the interruption."})
			return
		}
	}
	result := codingruntime.TerminalResult{
		Lifecycle: codingruntime.LifecycleInterrupted, Outcome: codingruntime.OutcomeInterrupted,
		Summary: "The user interrupted this Build job.",
	}
	var updated codingruntime.Job
	for attempt := 0; attempt < 8; attempt++ {
		current, exists, readErr := s.engine.buildJobs.Get(id)
		if readErr != nil || !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Build job not found."})
			return
		}
		if current.Terminal() {
			if current.Lifecycle == codingruntime.LifecycleInterrupted {
				writeJSON(w, http.StatusOK, viewBuildJob(current))
				return
			}
			writeBuildMutationError(w, codingruntime.ErrTerminal)
			return
		}
		updated, _, err = s.engine.buildJobs.CompareAndSetTerminal(id, current.Revision, result)
		if errors.Is(err, codingruntime.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			writeBuildMutationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, viewBuildJob(updated))
		return
	}
	writeBuildMutationError(w, codingruntime.ErrRevisionConflict)
}

func decodeBuildJSON(w http.ResponseWriter, r *http.Request, destination interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBuildRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("The Build request is invalid or too large.")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("The Build request must contain one object.")
	}
	return nil
}

func writeBuildMutationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, codingruntime.ErrRevisionConflict) || errors.Is(err, codingruntime.ErrTerminal) {
		status = http.StatusConflict
	} else if errors.Is(err, codingruntime.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
