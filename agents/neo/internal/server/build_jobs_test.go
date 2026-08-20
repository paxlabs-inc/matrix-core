package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/agents/neo/internal/codingruntime"
	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/tools"
)

func TestAgentBuildCreatesSelectsAndAcceptsProjectInOneCall(t *testing.T) {
	_, engine := buildJobServer(t)
	run := &run{id: "neo-build-admission", convID: "conv-new-build"}
	request := tools.BuildRequest{
		Project: "Webhook Dispatcher", Request: "Build the webhook dispatcher",
		AcceptanceCriteria: []string{"tests pass"},
	}
	first, err := engine.agentBuild(withRun(context.Background(), run), request)
	if err != nil || !strings.Contains(first, "webhook-dispatcher") {
		t.Fatalf("first Build admission = %q, %v", first, err)
	}
	if got := engine.conv.Project(run.convID); got != "webhook-dispatcher" {
		t.Fatalf("conversation project = %q", got)
	}
	project, err := engine.resolveProjectRecord("webhook-dispatcher")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(project.Root); err != nil || !info.IsDir() {
		t.Fatalf("project root = %+v, %v", info, err)
	}
	second, err := engine.agentBuild(withRun(context.Background(), run), request)
	if err != nil || second != first {
		t.Fatalf("replayed Build admission = %q, %v; first %q", second, err, first)
	}
	jobs, err := engine.buildJobs.List()
	if err != nil || len(jobs) != 1 || jobs[0].ProjectID != "webhook-dispatcher" {
		t.Fatalf("durable jobs = %+v, %v", jobs, err)
	}
}

func buildJobServer(t *testing.T) (*Server, *Engine) {
	t.Helper()
	workspace := t.TempDir()
	projectRoot := filepath.Join(workspace, "site")
	if err := os.Mkdir(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(EngineOptions{
		Config:          config.Config{ActorDID: "did:matrix:test-builder"},
		WorkspaceDir:    workspace,
		BuildDir:        t.TempDir(),
		ConversationDir: t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	engine.buildDispatch = func(codingruntime.Job) {}
	if err := engine.projectsRegistry().create(project{ID: "site", Name: "Site", Root: projectRoot}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	server, err := New(engine, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	return server, engine
}

func buildJobRequest(t *testing.T, server *Server, method, target string, body interface{}, idempotency string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestBuildJobsPersistBeforeReturnAndNeverExposeRuntimeBinding(t *testing.T) {
	server, engine := buildJobServer(t)
	body := map[string]interface{}{
		"conversation_id": "conv-build", "project": "site",
		"brief": map[string]interface{}{
			"intent":              "Build a responsive product site",
			"acceptance_criteria": []string{"mobile navigation works", "tests pass"},
			"constraints":         []string{"do not deploy"},
		},
	}
	response := buildJobRequest(t, server, http.MethodPost, "/build-jobs", body, "build-key-1")
	if response.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" || created["status"] != string(codingruntime.LifecycleQueued) {
		t.Fatalf("created = %v", created)
	}
	for _, private := range []string{"runtime", "cwd", "user_server_id", "idempotency_key", "wake_outbox", "session_id"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("public response exposed %q: %s", private, response.Body.String())
		}
	}
	stored, ok, err := engine.buildJobs.Get(id)
	if err != nil || !ok || stored.CWD != filepath.Join(engine.workspaceRoot, "site") {
		t.Fatalf("durable private job = (%v, %v, %v)", stored, ok, err)
	}

	duplicate := buildJobRequest(t, server, http.MethodPost, "/build-jobs", body, "build-key-1")
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), id) {
		t.Fatalf("duplicate = %d: %s", duplicate.Code, duplicate.Body.String())
	}
	conflictBody := body
	conflictBody["brief"] = map[string]interface{}{"intent": "Different build"}
	conflict := buildJobRequest(t, server, http.MethodPost, "/build-jobs", conflictBody, "build-key-1")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict = %d: %s", conflict.Code, conflict.Body.String())
	}

	list := buildJobRequest(t, server, http.MethodGet, "/build-jobs?project=site&conversation_id=conv-build", nil, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), id) {
		t.Fatalf("list = %d: %s", list.Code, list.Body.String())
	}
	correction := buildJobRequest(t, server, http.MethodPost, "/build-jobs/"+id+"/corrections", map[string]string{"message": "Use a compact mobile header"}, "")
	if correction.Code != http.StatusAccepted {
		t.Fatalf("correction = %d: %s", correction.Code, correction.Body.String())
	}
	corrected, ok, err := engine.buildJobs.Get(id)
	if err != nil || !ok || len(corrected.Corrections) != 1 || corrected.Corrections[0].Text != "Use a compact mobile header" {
		t.Fatalf("durable correction = (%v, %v, %v)", corrected, ok, err)
	}
	interrupted := buildJobRequest(t, server, http.MethodPost, "/build-jobs/"+id+"/interrupt", nil, "")
	if interrupted.Code != http.StatusOK || !strings.Contains(interrupted.Body.String(), `"status":"interrupted"`) {
		t.Fatalf("interrupt = %d: %s", interrupted.Code, interrupted.Body.String())
	}
}

func TestBuildJobAnswerResumesSameDurableJob(t *testing.T) {
	server, engine := buildJobServer(t)
	job, _, err := engine.acceptBuild("conv-answer", "site", codingruntime.BuildBrief{Intent: "Build a form"}, "answer-key")
	if err != nil {
		t.Fatal(err)
	}
	needsInput, _, err := engine.buildJobs.ApplyDelivery(job.ID, job.Revision, codingruntime.DeliveryBatch{
		DeliveryID: "delivery-question", SourceEventIDs: []string{"source-question"},
		NextLifecycle: codingruntime.LifecycleNeedsInput,
		Evidence: []codingruntime.EvidenceRecord{{
			ID: "question-1", Kind: codingruntime.EvidenceQuestion,
			Question: "Which database should I use?", SourceEventID: "source-question",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := buildJobRequest(t, server, http.MethodPost, "/build-jobs/"+job.ID+"/answers", map[string]interface{}{
		"kind": "question", "target_id": "question-1", "answer": "SQLite",
	}, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("answer = %d: %s", response.Code, response.Body.String())
	}
	stored, ok, err := engine.buildJobs.Get(job.ID)
	if err != nil || !ok || stored.ID != needsInput.ID || stored.Lifecycle != codingruntime.LifecycleQueued || len(stored.Answers) != 1 || stored.Answers[0].Text != "SQLite" {
		t.Fatalf("resumed job = (%v, %v, %v)", stored, ok, err)
	}
}
