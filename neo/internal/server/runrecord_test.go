package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/runrecord"
)

func newRunRecordEngine(t *testing.T) *Engine {
	t.Helper()
	e := &Engine{
		broker:     newBroker(),
		runs:       map[string]*run{},
		conv:       conversation.Open(t.TempDir()),
		runRecords: runrecord.Open(t.TempDir()),
	}
	e.sessions = newSessionRegistry(e)
	return e
}

func pollRun(t *testing.T, s *Server, id string) map[string]interface{} {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/messages/async/"+id, nil)
	w := httptest.NewRecorder()
	s.handleAsyncPoll(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("poll %s: status=%d body=%s", id, w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	return body
}

func TestRunRecordPollAndLiveRunSurviveRegistryAndBrokerLoss(t *testing.T) {
	e := newRunRecordEngine(t)
	rec, created, err := e.runRecords.Begin("neo_durable", "conv_durable", "key_durable", "long task")
	if err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}
	e.conv.AppendUser(rec.ConversationID, rec.IntentID, rec.Request)
	s := &Server{engine: e}
	first := pollRun(t, s, rec.IntentID)
	second := pollRun(t, s, rec.IntentID)
	if first["created_at"] != second["created_at"] {
		t.Fatalf("created_at changed across polls: %v != %v", first["created_at"], second["created_at"])
	}
	if first["status"] != runrecord.StatusRunning {
		t.Fatalf("running poll status: %v", first["status"])
	}
	r := httptest.NewRequest(http.MethodGet, "/conversations/"+rec.ConversationID, nil)
	w := httptest.NewRecorder()
	s.handleConversations(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("conversation: status=%d body=%s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["live_run"] != rec.IntentID {
		t.Fatalf("durable live_run: got %v want %q", detail["live_run"], rec.IntentID)
	}
}

func TestConversationForkRefusesActiveParent(t *testing.T) {
	e := newRunRecordEngine(t)
	e.conv.AppendUser("conv-active-fork", "neo-active-fork", "still running")
	if _, created, err := e.runRecords.Begin(
		"neo-active-fork",
		"conv-active-fork",
		"fork-active-key",
		"still running",
	); err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}

	s := &Server{engine: e}
	req := httptest.NewRequest(
		http.MethodPost,
		"/conversations/conv-active-fork/fork",
		strings.NewReader(`{"up_to_turn":1}`),
	)
	w := httptest.NewRecorder()
	s.handleConversationFork(w, req, "conv-active-fork")
	if w.Code != http.StatusConflict {
		t.Fatalf("active fork status=%d body=%s", w.Code, w.Body.String())
	}
	if len(e.conv.List()) != 1 {
		t.Fatalf("active fork created a partial conversation: %+v", e.conv.List())
	}
}

func TestTerminalCommitIsExactlyOnceAndReplaysAfterBrokerEviction(t *testing.T) {
	e := newRunRecordEngine(t)
	rec, created, err := e.runRecords.Begin("neo_terminal", "conv_terminal", "key_terminal", "finish once")
	if err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}
	e.broker.ensure(rec.IntentID)
	sess := &session{id: rec.ConversationID, engine: e}
	run := &run{id: rec.IntentID, convID: rec.ConversationID, sess: sess}
	if !sess.finishRun(run, runrecord.StatusCompleted, "durable answer", "", nil, true) {
		t.Fatal("first terminal commit was not published")
	}
	if sess.finishRun(run, runrecord.StatusCompleted, "duplicate answer", "", nil, true) {
		t.Fatal("second terminal commit published")
	}
	stored, ok, err := e.runRecords.Get(rec.IntentID)
	if err != nil || !ok {
		t.Fatalf("Get terminal: ok=%v err=%v", ok, err)
	}
	if stored.Status != runrecord.StatusCompleted || stored.Result != "durable answer" || stored.LastEventSeq != 2 {
		t.Fatalf("terminal record: %+v", stored)
	}
	thread := e.conv.Get(rec.ConversationID)
	if thread == nil || len(thread.Turns) != 1 || thread.Turns[0].Text != "durable answer" {
		t.Fatalf("durable final not exactly once: %+v", thread)
	}
	e.broker.drop(rec.IntentID)
	s := &Server{engine: e}
	poll := pollRun(t, s, rec.IntentID)
	if poll["status"] != runrecord.StatusCompleted {
		t.Fatalf("terminal poll status: %v", poll["status"])
	}
	replayReq := httptest.NewRequest(http.MethodGet, "/events/replay/"+rec.IntentID, nil)
	replay := httptest.NewRecorder()
	s.handleReplay(replay, replayReq)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	body := replay.Body.String()
	if !strings.Contains(body, `"type":"message.complete"`) || !strings.Contains(body, `"text":"durable answer"`) {
		t.Fatalf("durable replay missing terminal/final: %s", body)
	}
}

func TestInterruptedTerminalCannotBeOverwrittenAfterStop(t *testing.T) {
	e := newRunRecordEngine(t)
	rec, _, err := e.runRecords.Begin("neo_stop", "conv_stop", "key_stop", "keep mutating")
	if err != nil {
		t.Fatal(err)
	}
	e.broker.ensure(rec.IntentID)
	sess := &session{id: rec.ConversationID, engine: e}
	run := &run{id: rec.IntentID, convID: rec.ConversationID, sess: sess}
	sess.interrupt(run)
	if !run.stopped.Load() {
		t.Fatal("stop did not latch on the run")
	}
	if !sess.finishRun(run, runrecord.StatusInterrupted, "Stopped.", "", nil, true) {
		t.Fatal("interrupted terminal was not committed")
	}
	if sess.finishRun(run, runrecord.StatusCompleted, "late mutation", "", nil, true) {
		t.Fatal("late completion overwrote interrupted terminal")
	}
	stored, ok, err := e.runRecords.Get(rec.IntentID)
	if err != nil || !ok || stored.Status != runrecord.StatusInterrupted || stored.Result != "Stopped." {
		t.Fatalf("interrupted record changed: ok=%v err=%v rec=%+v", ok, err, stored)
	}
	thread := e.conv.Get(rec.ConversationID)
	if thread == nil || len(thread.Turns) != 1 || thread.Turns[0].Text != "Stopped." {
		t.Fatalf("late mutation reached conversation: %+v", thread)
	}
}

func TestChatRetryWithSameIdempotencyKeyReusesIntentWithoutDuplicateTurn(t *testing.T) {
	e := newRunRecordEngine(t)
	gateway, err := channelgateway.Open(context.Background(), t.TempDir(), nil, "did:matrix:test")
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	defer gateway.Close()
	e.channelGateway = gateway
	e.vaultUser = "did:matrix:test"
	const (
		intentID = "neo_idempotent"
		convID   = "conv_idempotent"
		key      = "chat_idempotent"
		message  = "change state once"
	)
	if _, created, err := e.runRecords.Begin(intentID, convID, key, message); err != nil || !created {
		t.Fatalf("Begin: created=%v err=%v", created, err)
	}
	e.conv.AppendUser(convID, intentID, message)
	s := &Server{engine: e}
	body, _ := json.Marshal(map[string]string{"message": message, "conversation_id": convID, "idempotency_key": key})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleChat(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("retry %d: status=%d body=%s", i+1, w.Code, w.Body.String())
		}
		var response map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["intent_id"] != intentID {
			t.Fatalf("retry %d minted intent %v, want %q", i+1, response["intent_id"], intentID)
		}
	}
	thread := e.conv.Get(convID)
	if thread == nil || len(thread.Turns) != 1 {
		t.Fatalf("idempotent retry duplicated the user turn: %+v", thread)
	}
}
