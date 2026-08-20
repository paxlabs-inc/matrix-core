package codingruntime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"centra/packages/vault"
)

func testVaultSession(t *testing.T, dataDir, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: dataDir, UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !session.Encrypting() {
		t.Fatal("expected encrypting vault session")
	}
	return session
}

func createInput() CreateRequest {
	return CreateRequest{
		ActorID: "did:matrix:alice", ConversationID: "conversation-1",
		UserServerID: "server-alice", ProjectID: "project-1", CWD: "/workspace/project-1",
		Brief: BuildBrief{
			Intent: " Build a small notes application. ", Constraints: []string{"Use Go", "Use Go", " "},
			AcceptanceCriteria: []string{"The form saves notes"},
			ProjectContext:     map[string]string{" framework ": " standard library "},
		},
		IdempotencyKey: "conversation-1:build-1",
	}
}

func createJob(t *testing.T, store *Store) Job {
	t.Helper()
	job, created, err := store.CreateQueued(createInput())
	if err != nil {
		t.Fatalf("CreateQueued: %v", err)
	}
	if !created {
		t.Fatal("expected a newly created job")
	}
	return job
}

func TestCreateQueuedIsDurableSealedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "jobs")
	user := "did:matrix:alice"
	session := testVaultSession(t, filepath.Join(root, "vault"), user)
	store := Open(jobDir)
	store.SetVault(session, user)

	job := createJob(t, store)
	if job.Lifecycle != LifecycleQueued || job.Revision != 1 || job.ID == "" {
		t.Fatalf("created job is not durably queued: %+v", job)
	}
	if job.Brief.Intent != "Build a small notes application." || len(job.Brief.Constraints) != 1 {
		t.Fatalf("brief was not normalized: %+v", job.Brief)
	}
	raw, err := os.ReadFile(filepath.Join(jobDir, job.ID+".json"))
	if err != nil {
		t.Fatalf("read job file: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("job file is not sealed")
	}
	if strings.Contains(string(raw), "notes application") || strings.Contains(string(raw), job.IdempotencyKey) {
		t.Fatal("sealed job leaked plaintext")
	}

	reopened := Open(jobDir)
	reopened.SetVault(session, user)
	loaded, ok, err := reopened.Get(job.ID)
	if err != nil || !ok {
		t.Fatalf("reopen Get: ok=%v err=%v", ok, err)
	}
	if loaded.ID != job.ID || loaded.IdempotencyHash != job.IdempotencyHash || loaded.Lifecycle != LifecycleQueued {
		t.Fatalf("reloaded job mismatch: %+v", loaded)
	}
	replayed, created, err := reopened.CreateQueued(createInput())
	if err != nil || created || replayed.ID != job.ID {
		t.Fatalf("idempotent replay: created=%v job=%+v err=%v", created, replayed, err)
	}
	changed := createInput()
	changed.Brief.Intent = "Build a different application"
	if _, _, err := reopened.CreateQueued(changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed idempotent request error = %v", err)
	}
}

func TestLifecycleCorrectionsAnswersAndTerminalCompareAndSet(t *testing.T) {
	store := Open(t.TempDir())
	job := createJob(t, store)

	bound, err := store.BindRuntime(job.ID, job.Revision, RuntimeBinding{SessionID: "private-session", ServiceInstanceID: "service-1"})
	if err != nil {
		t.Fatalf("BindRuntime: %v", err)
	}
	if bound.Runtime.BoundAt == nil || bound.Revision != 2 {
		t.Fatalf("runtime binding not recorded: %+v", bound)
	}
	if _, err := store.BindRuntime(job.ID, bound.Revision, RuntimeBinding{SessionID: "replacement-session"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("session replacement error = %v", err)
	}
	running, err := store.Transition(job.ID, bound.Revision, LifecycleRunning)
	if err != nil || running.StartedAt == nil {
		t.Fatalf("Transition running: job=%+v err=%v", running, err)
	}
	needsInput, changed, err := store.ApplyDelivery(job.ID, running.Revision, DeliveryBatch{
		DeliveryID: "delivery-question", SourceEventIDs: []string{"event-question"},
		NextLifecycle: LifecycleNeedsInput,
		Evidence:      []EvidenceRecord{{Kind: EvidenceQuestion, Question: "Which database?", SourceEventID: "event-question"}},
		Wake:          &WakeRequest{DedupKey: "question:event-question", Kind: WakeInputRequired, Reason: "A database choice is needed.", SourceEventID: "event-question"},
	})
	if err != nil || !changed || needsInput.Lifecycle != LifecycleNeedsInput || len(needsInput.Evidence) != 1 || len(needsInput.WakeOutbox) != 1 {
		t.Fatalf("question delivery: changed=%v job=%+v err=%v", changed, needsInput, err)
	}
	answered, err := store.AddAnswer(job.ID, needsInput.Revision, needsInput.Evidence[0].ID, AnswerQuestion, "SQLite")
	if err != nil || answered.Lifecycle != LifecycleQueued || len(answered.Answers) != 1 {
		t.Fatalf("AddAnswer: job=%+v err=%v", answered, err)
	}
	corrected, err := store.AddCorrection(job.ID, answered.Revision, "Also add a search box")
	if err != nil || len(corrected.Corrections) != 1 {
		t.Fatalf("AddCorrection: job=%+v err=%v", corrected, err)
	}
	running, err = store.Transition(job.ID, corrected.Revision, LifecycleRunning)
	if err != nil {
		t.Fatalf("resume running: %v", err)
	}
	terminal := TerminalResult{Lifecycle: LifecycleVerified, Outcome: OutcomeVerified, Summary: "All requested checks passed."}
	finished, won, err := store.CompareAndSetTerminal(job.ID, running.Revision, terminal)
	if err != nil || !won || !finished.Terminal() || finished.EndedAt == nil {
		t.Fatalf("terminal CAS: won=%v job=%+v err=%v", won, finished, err)
	}
	replayed, won, err := store.CompareAndSetTerminal(job.ID, running.Revision, terminal)
	if err != nil || won || replayed.Revision != finished.Revision {
		t.Fatalf("terminal replay: won=%v job=%+v err=%v", won, replayed, err)
	}
	_, won, err = store.CompareAndSetTerminal(job.ID, finished.Revision, TerminalResult{Lifecycle: LifecycleFailed, Outcome: OutcomeFailed})
	if won || !errors.Is(err, ErrTerminal) {
		t.Fatalf("second terminal writer: won=%v err=%v", won, err)
	}
	if _, err := store.AddCorrection(job.ID, finished.Revision, "change it"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal correction error = %v", err)
	}
}

func TestDeliveryDeduplicatesDeliveryAndSourceEventIDs(t *testing.T) {
	store := Open(t.TempDir())
	job := createJob(t, store)
	running, err := store.Transition(job.ID, job.Revision, LifecycleRunning)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	passed := true
	batch := DeliveryBatch{
		DeliveryID: "delivery-1", SourceEventIDs: []string{"event-1", "event-2"},
		Evidence: []EvidenceRecord{
			{Kind: EvidenceFileChange, Path: "main.go", Summary: "Updated handler", SourceEventID: "event-1"},
			{Kind: EvidenceVerification, Check: "go test ./...", Passed: &passed, SourceEventID: "event-2"},
		},
	}
	applied, changed, err := store.ApplyDelivery(job.ID, running.Revision, batch)
	if err != nil || !changed || len(applied.Evidence) != 2 || len(applied.Deliveries) != 1 {
		t.Fatalf("first delivery: changed=%v job=%+v err=%v", changed, applied, err)
	}
	replayed, changed, err := store.ApplyDelivery(job.ID, running.Revision, batch)
	if err != nil || changed || replayed.Revision != applied.Revision || len(replayed.Evidence) != 2 {
		t.Fatalf("delivery replay: changed=%v job=%+v err=%v", changed, replayed, err)
	}
	conflict := batch
	conflict.Evidence = append([]EvidenceRecord(nil), batch.Evidence...)
	conflict.Evidence[0].Summary = "different payload"
	if _, _, err := store.ApplyDelivery(job.ID, applied.Revision, conflict); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("delivery conflict error = %v", err)
	}
	sameSources := batch
	sameSources.DeliveryID = "delivery-2"
	if duplicate, changed, err := store.ApplyDelivery(job.ID, applied.Revision, sameSources); err != nil || changed || duplicate.Revision != applied.Revision {
		t.Fatalf("source replay: changed=%v job=%+v err=%v", changed, duplicate, err)
	}
	partial := DeliveryBatch{DeliveryID: "delivery-3", SourceEventIDs: []string{"event-2", "event-3"}}
	if _, _, err := store.ApplyDelivery(job.ID, applied.Revision, partial); !errors.Is(err, ErrSourceEventConflict) {
		t.Fatalf("partial source overlap error = %v", err)
	}
}

func TestWakeOutboxPersistsRetryAndAck(t *testing.T) {
	dir := t.TempDir()
	store := Open(dir)
	job := createJob(t, store)
	withWake, queued, err := store.EnqueueWake(job.ID, job.Revision, WakeRequest{
		DedupKey: "blocked:one", Kind: WakeBlocked, Reason: "The schema needs a user decision.",
	})
	if err != nil || !queued || len(withWake.WakeOutbox) != 1 {
		t.Fatalf("EnqueueWake: queued=%v job=%+v err=%v", queued, withWake, err)
	}
	duplicate, queued, err := store.EnqueueWake(job.ID, job.Revision, WakeRequest{
		DedupKey: "blocked:one", Kind: WakeBlocked, Reason: "The schema needs a user decision.",
	})
	if err != nil || queued || duplicate.Revision != withWake.Revision {
		t.Fatalf("duplicate wake: queued=%v job=%+v err=%v", queued, duplicate, err)
	}
	now := time.Now().UTC()
	pending, err := store.PendingWakes(now.Add(time.Second), 10)
	if err != nil || len(pending) != 1 || pending[0].JobID != job.ID {
		t.Fatalf("PendingWakes: %+v err=%v", pending, err)
	}
	wakeID := pending[0].Wake.ID
	retryAt := now.Add(time.Hour)
	attempted, err := store.RecordWakeAttempt(job.ID, wakeID, "delivery unavailable", retryAt)
	if err != nil || attempted.WakeOutbox[0].Attempts != 1 || attempted.WakeOutbox[0].LastError == "" {
		t.Fatalf("RecordWakeAttempt: job=%+v err=%v", attempted, err)
	}
	if pending, err = store.PendingWakes(now.Add(time.Minute), 10); err != nil || len(pending) != 0 {
		t.Fatalf("wake retried too early: %+v err=%v", pending, err)
	}
	reopened := Open(dir)
	if pending, err = reopened.PendingWakes(retryAt.Add(time.Second), 10); err != nil || len(pending) != 1 || pending[0].Wake.Attempts != 1 {
		t.Fatalf("durable retry missing: %+v err=%v", pending, err)
	}
	acked, changed, err := reopened.AckWake(job.ID, wakeID)
	if err != nil || !changed || acked.WakeOutbox[0].State != WakeAcked || acked.WakeOutbox[0].AckedAt == nil {
		t.Fatalf("AckWake: changed=%v job=%+v err=%v", changed, acked, err)
	}
	if _, changed, err = reopened.AckWake(job.ID, wakeID); err != nil || changed {
		t.Fatalf("AckWake replay: changed=%v err=%v", changed, err)
	}
	if pending, err = reopened.PendingWakes(retryAt.Add(2*time.Hour), 10); err != nil || len(pending) != 0 {
		t.Fatalf("acked wake was redelivered: %+v err=%v", pending, err)
	}
}

func TestPublicProjectionExcludesPrivateRuntimeAndDeliveryState(t *testing.T) {
	store := Open(t.TempDir())
	job := createJob(t, store)
	job, err := store.BindRuntime(job.ID, job.Revision, RuntimeBinding{
		SessionID: "private-session-id", ServiceInstanceID: "private-service-id", LastObservedEventID: "private-event-id",
	})
	if err != nil {
		t.Fatalf("BindRuntime: %v", err)
	}
	job, _, err = store.EnqueueWake(job.ID, job.Revision, WakeRequest{DedupKey: "private-wake-key", Kind: WakeBlocked, Reason: "blocked"})
	if err != nil {
		t.Fatalf("EnqueueWake: %v", err)
	}
	job.Evidence = append(job.Evidence, EvidenceRecord{
		ID: "public-evidence", Kind: EvidenceStatus, Summary: "Working",
		Details:       map[string]interface{}{"session_id": "private-detail-session"},
		SourceEventID: "private-detail-event", RecordedAt: time.Now().UTC(),
	})
	b, err := json.Marshal(job.Public())
	if err != nil {
		t.Fatalf("marshal public job: %v", err)
	}
	text := string(b)
	for _, forbidden := range []string{
		"private-session-id", "private-service-id", "private-event-id", "private-wake-key",
		"private-detail-session", "private-detail-event",
		"session_id", "service_instance_id", "idempotency_key", "idempotency_hash",
		"delivery_id", "source_event_id", "wake_outbox", "user_server_id", "actor_id", "\"cwd\"",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("public projection contains private %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, job.ID) || !strings.Contains(text, "notes application") {
		t.Fatalf("public projection omitted product state: %s", text)
	}
}

func TestConcurrentTerminalCompareAndSetHasOneWinner(t *testing.T) {
	store := Open(t.TempDir())
	job := createJob(t, store)
	running, err := store.Transition(job.ID, job.Revision, LifecycleRunning)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	type result struct {
		won bool
		err error
	}
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	write := func(terminal TerminalResult) {
		defer ready.Done()
		<-start
		_, won, err := store.CompareAndSetTerminal(job.ID, running.Revision, terminal)
		results <- result{won: won, err: err}
	}
	go write(TerminalResult{Lifecycle: LifecycleVerified, Outcome: OutcomeVerified, Summary: "done"})
	go write(TerminalResult{Lifecycle: LifecycleFailed, Outcome: OutcomeFailed, Summary: "failed"})
	close(start)
	ready.Wait()
	close(results)
	winners := 0
	losers := 0
	for result := range results {
		if result.won && result.err == nil {
			winners++
		} else if !result.won && errors.Is(result.err, ErrTerminal) {
			losers++
		} else {
			t.Fatalf("unexpected terminal CAS result: %+v", result)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("terminal CAS winners=%d losers=%d", winners, losers)
	}
}

func TestDisabledStoreRefusesUndurableCreation(t *testing.T) {
	if _, _, err := Open("").CreateQueued(createInput()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled CreateQueued error = %v", err)
	}
}

func TestVaultRejectsWrongUser(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "jobs")
	alice := testVaultSession(t, filepath.Join(root, "alice-vault"), "did:matrix:alice")
	store := Open(jobDir)
	store.SetVault(alice, "did:matrix:alice")
	job := createJob(t, store)

	mallory := testVaultSession(t, filepath.Join(root, "mallory-vault"), "did:matrix:mallory")
	wrong := Open(jobDir)
	wrong.SetVault(mallory, "did:matrix:mallory")
	if _, _, err := wrong.Get(job.ID); err == nil {
		t.Fatal("wrong user opened sealed build job")
	}
}
