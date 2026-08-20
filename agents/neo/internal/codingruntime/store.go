package codingruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"centra/packages/vault"
)

const (
	storeName  = "neo.coding-runtime"
	schemaName = "build-job.v1"
)

var (
	ErrNotFound            = errors.New("coding build job not found")
	ErrDisabled            = errors.New("coding build job store is disabled")
	ErrInvalid             = errors.New("invalid coding build job value")
	ErrRevisionConflict    = errors.New("coding build job revision conflict")
	ErrIdempotencyConflict = errors.New("coding build idempotency conflict")
	ErrInvalidTransition   = errors.New("invalid coding build job transition")
	ErrTerminal            = errors.New("coding build job is terminal")
	ErrDeliveryConflict    = errors.New("coding build delivery conflict")
	ErrSourceEventConflict = errors.New("coding build source event conflict")
)

type Lifecycle string

const (
	LifecycleAccepted      Lifecycle = "accepted"
	LifecycleQueued        Lifecycle = "queued"
	LifecycleRunning       Lifecycle = "running"
	LifecycleNeedsInput    Lifecycle = "needs_input"
	LifecycleNeedsApproval Lifecycle = "needs_approval"
	LifecycleBlocked       Lifecycle = "blocked"
	LifecycleInterrupted   Lifecycle = "interrupted"
	LifecycleFailed        Lifecycle = "failed"
	LifecycleVerified      Lifecycle = "verified"
)

func (l Lifecycle) Terminal() bool {
	return l == LifecycleInterrupted || l == LifecycleFailed || l == LifecycleVerified
}

type Outcome string

const (
	OutcomeNone        Outcome = ""
	OutcomeVerified    Outcome = "verified"
	OutcomeFailed      Outcome = "failed"
	OutcomeInterrupted Outcome = "interrupted"
)

type BuildBrief struct {
	Intent             string            `json:"intent"`
	Constraints        []string          `json:"constraints,omitempty"`
	AcceptanceCriteria []string          `json:"acceptance_criteria,omitempty"`
	ProjectContext     map[string]string `json:"project_context,omitempty"`
}

type RuntimeBinding struct {
	SessionID           string     `json:"session_id,omitempty"`
	ServiceInstanceID   string     `json:"service_instance_id,omitempty"`
	LastObservedEventID string     `json:"last_observed_event_id,omitempty"`
	InitialPromptTried  bool       `json:"initial_prompt_tried,omitempty"`
	LastCorrectionID    string     `json:"last_correction_id,omitempty"`
	LastAnswerID        string     `json:"last_answer_id,omitempty"`
	PreviewTarget       string     `json:"preview_target,omitempty"`
	BoundAt             *time.Time `json:"bound_at,omitempty"`
	ObservedAt          *time.Time `json:"observed_at,omitempty"`
}

type EvidenceKind string

const (
	EvidenceStatus       EvidenceKind = "status"
	EvidenceCommand      EvidenceKind = "command"
	EvidenceFileChange   EvidenceKind = "file_change"
	EvidenceDiff         EvidenceKind = "diff"
	EvidenceVerification EvidenceKind = "verification"
	EvidencePreview      EvidenceKind = "preview"
	EvidenceQuestion     EvidenceKind = "question"
	EvidenceApproval     EvidenceKind = "approval"
	EvidenceBlocker      EvidenceKind = "blocker"
	EvidenceError        EvidenceKind = "error"
)

type EvidenceRecord struct {
	ID            string                 `json:"id"`
	Kind          EvidenceKind           `json:"kind"`
	Summary       string                 `json:"summary,omitempty"`
	Command       string                 `json:"command,omitempty"`
	Path          string                 `json:"path,omitempty"`
	Diff          string                 `json:"diff,omitempty"`
	Check         string                 `json:"check,omitempty"`
	Passed        *bool                  `json:"passed,omitempty"`
	PreviewURL    string                 `json:"preview_url,omitempty"`
	Question      string                 `json:"question,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Details       map[string]interface{} `json:"details,omitempty"`
	RecordedAt    time.Time              `json:"recorded_at"`
	SourceEventID string                 `json:"source_event_id"`
}

type Correction struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type AnswerKind string

const (
	AnswerQuestion AnswerKind = "question"
	AnswerApproval AnswerKind = "approval"
)

type Answer struct {
	ID        string     `json:"id"`
	TargetID  string     `json:"target_id"`
	Kind      AnswerKind `json:"kind"`
	Text      string     `json:"text"`
	CreatedAt time.Time  `json:"created_at"`
}

type WakeKind string

const (
	WakeInputRequired    WakeKind = "input_required"
	WakeApprovalRequired WakeKind = "approval_required"
	WakeBlocked          WakeKind = "blocked"
	WakeFailed           WakeKind = "failed"
	WakeInterrupted      WakeKind = "interrupted"
	WakeVerified         WakeKind = "verified"
)

type WakeState string

const (
	WakePending WakeState = "pending"
	WakeAcked   WakeState = "acked"
)

type WakeEnvelope struct {
	ID            string     `json:"id"`
	DedupKey      string     `json:"dedup_key"`
	Kind          WakeKind   `json:"kind"`
	Reason        string     `json:"reason"`
	SourceEventID string     `json:"source_event_id,omitempty"`
	State         WakeState  `json:"state"`
	Attempts      int        `json:"attempts"`
	CreatedAt     time.Time  `json:"created_at"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	AckedAt       *time.Time `json:"acked_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type DeliveryReceipt struct {
	DeliveryID     string    `json:"delivery_id"`
	Digest         string    `json:"digest"`
	SourceEventIDs []string  `json:"source_event_ids"`
	AppliedAt      time.Time `json:"applied_at"`
}

type Job struct {
	ID                 string            `json:"id"`
	ActorID            string            `json:"actor_id"`
	ConversationID     string            `json:"conversation_id"`
	UserServerID       string            `json:"user_server_id"`
	ProjectID          string            `json:"project_id"`
	CWD                string            `json:"cwd"`
	Brief              BuildBrief        `json:"brief"`
	Lifecycle          Lifecycle         `json:"lifecycle"`
	Outcome            Outcome           `json:"outcome,omitempty"`
	OutcomeSummary     string            `json:"outcome_summary,omitempty"`
	Evidence           []EvidenceRecord  `json:"evidence"`
	Corrections        []Correction      `json:"corrections"`
	Answers            []Answer          `json:"answers"`
	Runtime            RuntimeBinding    `json:"runtime"`
	IdempotencyKey     string            `json:"idempotency_key"`
	IdempotencyHash    string            `json:"idempotency_hash"`
	Deliveries         []DeliveryReceipt `json:"deliveries"`
	ProcessedSourceIDs []string          `json:"processed_source_event_ids"`
	WakeOutbox         []WakeEnvelope    `json:"wake_outbox"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	StartedAt          *time.Time        `json:"started_at,omitempty"`
	EndedAt            *time.Time        `json:"ended_at,omitempty"`
	Revision           uint64            `json:"revision"`
}

func (j Job) Terminal() bool { return j.Lifecycle.Terminal() }

type CreateRequest struct {
	ID             string
	ActorID        string
	ConversationID string
	UserServerID   string
	ProjectID      string
	CWD            string
	Brief          BuildBrief
	IdempotencyKey string
}

type TerminalResult struct {
	Lifecycle Lifecycle `json:"lifecycle"`
	Outcome   Outcome   `json:"outcome"`
	Summary   string    `json:"summary,omitempty"`
}

type WakeRequest struct {
	DedupKey      string   `json:"dedup_key"`
	Kind          WakeKind `json:"kind"`
	Reason        string   `json:"reason"`
	SourceEventID string   `json:"source_event_id,omitempty"`
}

type DeliveryBatch struct {
	DeliveryID     string           `json:"delivery_id"`
	SourceEventIDs []string         `json:"source_event_ids"`
	Evidence       []EvidenceRecord `json:"evidence,omitempty"`
	NextLifecycle  Lifecycle        `json:"next_lifecycle,omitempty"`
	Terminal       *TerminalResult  `json:"terminal,omitempty"`
	Wake           *WakeRequest     `json:"wake,omitempty"`
}

type PendingWake struct {
	JobID string
	Wake  WakeEnvelope
}

type PublicEvidence struct {
	ID         string       `json:"id"`
	Kind       EvidenceKind `json:"kind"`
	Summary    string       `json:"summary,omitempty"`
	Command    string       `json:"command,omitempty"`
	Path       string       `json:"path,omitempty"`
	Diff       string       `json:"diff,omitempty"`
	Check      string       `json:"check,omitempty"`
	Passed     *bool        `json:"passed,omitempty"`
	PreviewURL string       `json:"preview_url,omitempty"`
	Question   string       `json:"question,omitempty"`
	Error      string       `json:"error,omitempty"`
	RecordedAt time.Time    `json:"recorded_at"`
}

type PublicJob struct {
	ID             string           `json:"id"`
	ConversationID string           `json:"conversation_id"`
	ProjectID      string           `json:"project_id"`
	Brief          BuildBrief       `json:"brief"`
	Lifecycle      Lifecycle        `json:"lifecycle"`
	Outcome        Outcome          `json:"outcome,omitempty"`
	OutcomeSummary string           `json:"outcome_summary,omitempty"`
	Evidence       []PublicEvidence `json:"evidence"`
	Corrections    []Correction     `json:"corrections"`
	Answers        []Answer         `json:"answers"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	StartedAt      *time.Time       `json:"started_at,omitempty"`
	EndedAt        *time.Time       `json:"ended_at,omitempty"`
	Revision       uint64           `json:"revision"`
}

func (j Job) Public() PublicJob {
	evidence := make([]PublicEvidence, 0, len(j.Evidence))
	for _, item := range j.Evidence {
		evidence = append(evidence, PublicEvidence{
			ID: item.ID, Kind: item.Kind, Summary: item.Summary, Command: item.Command,
			Path: item.Path, Diff: item.Diff, Check: item.Check, Passed: cloneBool(item.Passed),
			PreviewURL: item.PreviewURL, Question: item.Question, Error: item.Error,
			RecordedAt: item.RecordedAt,
		})
	}
	return PublicJob{
		ID: j.ID, ConversationID: j.ConversationID, ProjectID: j.ProjectID,
		Brief: cloneBrief(j.Brief), Lifecycle: j.Lifecycle, Outcome: j.Outcome,
		OutcomeSummary: j.OutcomeSummary, Evidence: evidence,
		Corrections: append([]Correction(nil), j.Corrections...), Answers: append([]Answer(nil), j.Answers...),
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, StartedAt: cloneTime(j.StartedAt),
		EndedAt: cloneTime(j.EndedAt), Revision: j.Revision,
	}
}

type Store struct {
	mu    sync.Mutex
	dir   string
	vault *vault.Session
	user  string
}

func Open(dir string) *Store { return &Store{dir: strings.TrimSpace(dir)} }

func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

func (s *Store) SetVault(session *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = session
	s.user = strings.TrimSpace(user)
	s.mu.Unlock()
}

func (s *Store) CreateQueued(input CreateRequest) (Job, bool, error) {
	if !s.Enabled() {
		return Job{}, false, ErrDisabled
	}
	input = normalizeCreate(input)
	if err := validateCreate(input); err != nil {
		return Job{}, false, err
	}
	hash, err := createHash(input)
	if err != nil {
		return Job{}, false, err
	}
	if input.ID == "" {
		input.ID = uuid.NewString()
	}
	if !safeID(input.ID) {
		return Job{}, false, fmt.Errorf("%w: unsafe job id", ErrInvalid)
	}
	now := time.Now().UTC()
	job := Job{
		ID: input.ID, ActorID: input.ActorID, ConversationID: input.ConversationID,
		UserServerID: input.UserServerID, ProjectID: input.ProjectID, CWD: input.CWD,
		Brief: input.Brief, Lifecycle: LifecycleQueued, Outcome: OutcomeNone,
		Evidence: []EvidenceRecord{}, Corrections: []Correction{}, Answers: []Answer{},
		IdempotencyKey: input.IdempotencyKey, IdempotencyHash: hash,
		Deliveries: []DeliveryReceipt{}, ProcessedSourceIDs: []string{}, WakeOutbox: []WakeEnvelope{},
		CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok, err := s.findByIdempotencyLocked(input.IdempotencyKey)
	if err != nil {
		return Job{}, false, err
	}
	if ok {
		if prior.IdempotencyHash != hash {
			return cloneJob(prior), false, ErrIdempotencyConflict
		}
		return cloneJob(prior), false, nil
	}
	if prior, ok, err = s.loadLocked(input.ID); err != nil {
		return Job{}, false, err
	} else if ok {
		return cloneJob(prior), false, ErrIdempotencyConflict
	}
	if err := s.writeLocked(job); err != nil {
		return Job{}, false, err
	}
	return cloneJob(job), true, nil
}

func (s *Store) Get(jobID string) (Job, bool, error) {
	if !s.Enabled() {
		return Job{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	return cloneJob(job), ok, err
}

func (s *Store) FindByIdempotency(key string) (Job, bool, error) {
	if !s.Enabled() || strings.TrimSpace(key) == "" {
		return Job{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.findByIdempotencyLocked(strings.TrimSpace(key))
	return cloneJob(job), ok, err
}

func (s *Store) List() ([]Job, error) {
	if !s.Enabled() {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) Transition(jobID string, expectedRevision uint64, next Lifecycle) (Job, error) {
	if next.Terminal() || !knownLifecycle(next) {
		return Job{}, ErrInvalidTransition
	}
	return s.update(jobID, expectedRevision, func(job *Job, now time.Time) error {
		if job.Terminal() {
			return ErrTerminal
		}
		if job.Lifecycle == next {
			return nil
		}
		if !allowedTransition(job.Lifecycle, next) {
			return ErrInvalidTransition
		}
		job.Lifecycle = next
		if next == LifecycleRunning && job.StartedAt == nil {
			job.StartedAt = cloneTime(&now)
		}
		return nil
	})
}

func (s *Store) BindRuntime(jobID string, expectedRevision uint64, binding RuntimeBinding) (Job, error) {
	binding.SessionID = strings.TrimSpace(binding.SessionID)
	binding.ServiceInstanceID = strings.TrimSpace(binding.ServiceInstanceID)
	if binding.SessionID == "" {
		return Job{}, fmt.Errorf("%w: runtime session is required", ErrInvalid)
	}
	return s.update(jobID, expectedRevision, func(job *Job, now time.Time) error {
		if job.Terminal() {
			return ErrTerminal
		}
		if job.Runtime.SessionID != "" && job.Runtime.SessionID != binding.SessionID {
			return fmt.Errorf("%w: runtime session cannot be replaced", ErrInvalid)
		}
		if binding.BoundAt == nil {
			if job.Runtime.BoundAt != nil {
				binding.BoundAt = cloneTime(job.Runtime.BoundAt)
			} else {
				binding.BoundAt = cloneTime(&now)
			}
		}
		job.Runtime = binding
		return nil
	})
}

func (s *Store) AddCorrection(jobID string, expectedRevision uint64, text string) (Job, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Job{}, fmt.Errorf("%w: correction is required", ErrInvalid)
	}
	return s.update(jobID, expectedRevision, func(job *Job, now time.Time) error {
		if job.Terminal() {
			return ErrTerminal
		}
		job.Corrections = append(job.Corrections, Correction{ID: uuid.NewString(), Text: text, CreatedAt: now})
		if job.Lifecycle == LifecycleNeedsInput || job.Lifecycle == LifecycleNeedsApproval || job.Lifecycle == LifecycleBlocked {
			job.Lifecycle = LifecycleQueued
		}
		return nil
	})
}

func (s *Store) AddAnswer(jobID string, expectedRevision uint64, targetID string, kind AnswerKind, text string) (Job, error) {
	targetID, text = strings.TrimSpace(targetID), strings.TrimSpace(text)
	if targetID == "" || text == "" || (kind != AnswerQuestion && kind != AnswerApproval) {
		return Job{}, fmt.Errorf("%w: answer target, kind, and text are required", ErrInvalid)
	}
	return s.update(jobID, expectedRevision, func(job *Job, now time.Time) error {
		if job.Terminal() {
			return ErrTerminal
		}
		if kind == AnswerQuestion && job.Lifecycle != LifecycleNeedsInput {
			return ErrInvalidTransition
		}
		if kind == AnswerApproval && job.Lifecycle != LifecycleNeedsApproval {
			return ErrInvalidTransition
		}
		job.Answers = append(job.Answers, Answer{ID: uuid.NewString(), TargetID: targetID, Kind: kind, Text: text, CreatedAt: now})
		job.Lifecycle = LifecycleQueued
		return nil
	})
}

func (s *Store) CompareAndSetTerminal(jobID string, expectedRevision uint64, result TerminalResult) (Job, bool, error) {
	if !validTerminal(result) {
		return Job{}, false, fmt.Errorf("%w: invalid terminal result", ErrInvalid)
	}
	if !s.Enabled() {
		return Job{}, false, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil {
		return Job{}, false, err
	}
	if !ok {
		return Job{}, false, ErrNotFound
	}
	if job.Terminal() {
		if job.Lifecycle == result.Lifecycle && job.Outcome == result.Outcome && job.OutcomeSummary == strings.TrimSpace(result.Summary) {
			return cloneJob(job), false, nil
		}
		return cloneJob(job), false, ErrTerminal
	}
	if job.Revision != expectedRevision {
		return cloneJob(job), false, ErrRevisionConflict
	}
	now := time.Now().UTC()
	applyTerminal(&job, result, now)
	job.Revision++
	job.UpdatedAt = now
	if err := s.writeLocked(job); err != nil {
		return Job{}, false, err
	}
	return cloneJob(job), true, nil
}

func (s *Store) ApplyDelivery(jobID string, expectedRevision uint64, batch DeliveryBatch) (Job, bool, error) {
	batch = normalizeDelivery(batch)
	if err := validateDelivery(batch); err != nil {
		return Job{}, false, err
	}
	digest, err := digestJSON(batch)
	if err != nil {
		return Job{}, false, err
	}
	if !s.Enabled() {
		return Job{}, false, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil {
		return Job{}, false, err
	}
	if !ok {
		return Job{}, false, ErrNotFound
	}
	for _, receipt := range job.Deliveries {
		if receipt.DeliveryID != batch.DeliveryID {
			continue
		}
		if receipt.Digest != digest {
			return cloneJob(job), false, ErrDeliveryConflict
		}
		return cloneJob(job), false, nil
	}
	already := 0
	for _, sourceID := range batch.SourceEventIDs {
		if contains(job.ProcessedSourceIDs, sourceID) {
			already++
		}
	}
	if already == len(batch.SourceEventIDs) {
		return cloneJob(job), false, nil
	}
	if already != 0 {
		return cloneJob(job), false, ErrSourceEventConflict
	}
	if job.Revision != expectedRevision {
		return cloneJob(job), false, ErrRevisionConflict
	}
	if job.Terminal() {
		return cloneJob(job), false, ErrTerminal
	}
	now := time.Now().UTC()
	if batch.Terminal != nil {
		applyTerminal(&job, *batch.Terminal, now)
	} else if batch.NextLifecycle != "" && batch.NextLifecycle != job.Lifecycle {
		if batch.NextLifecycle.Terminal() || !allowedTransition(job.Lifecycle, batch.NextLifecycle) {
			return cloneJob(job), false, ErrInvalidTransition
		}
		job.Lifecycle = batch.NextLifecycle
		if batch.NextLifecycle == LifecycleRunning && job.StartedAt == nil {
			job.StartedAt = cloneTime(&now)
		}
	}
	for _, evidence := range batch.Evidence {
		evidence.RecordedAt = now
		job.Evidence = append(job.Evidence, evidence)
	}
	if batch.Wake != nil {
		enqueueWake(&job, *batch.Wake, now)
	}
	job.Deliveries = append(job.Deliveries, DeliveryReceipt{DeliveryID: batch.DeliveryID, Digest: digest, SourceEventIDs: append([]string(nil), batch.SourceEventIDs...), AppliedAt: now})
	job.ProcessedSourceIDs = append(job.ProcessedSourceIDs, batch.SourceEventIDs...)
	job.Revision++
	job.UpdatedAt = now
	if err := s.writeLocked(job); err != nil {
		return Job{}, false, err
	}
	return cloneJob(job), true, nil
}

func (s *Store) EnqueueWake(jobID string, expectedRevision uint64, request WakeRequest) (Job, bool, error) {
	request = normalizeWake(request)
	if err := validateWake(request); err != nil {
		return Job{}, false, err
	}
	if !s.Enabled() {
		return Job{}, false, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil || !ok {
		if err != nil {
			return Job{}, false, err
		}
		return Job{}, false, ErrNotFound
	}
	for _, wake := range job.WakeOutbox {
		if wake.DedupKey == request.DedupKey {
			return cloneJob(job), false, nil
		}
	}
	if job.Revision != expectedRevision {
		return cloneJob(job), false, ErrRevisionConflict
	}
	now := time.Now().UTC()
	enqueueWake(&job, request, now)
	job.Revision++
	job.UpdatedAt = now
	if err := s.writeLocked(job); err != nil {
		return Job{}, false, err
	}
	return cloneJob(job), true, nil
}

func (s *Store) PendingWakes(now time.Time, limit int) ([]PendingWake, error) {
	if !s.Enabled() || limit == 0 {
		return nil, nil
	}
	if limit < 0 {
		limit = int(^uint(0) >> 1)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	var pending []PendingWake
	for _, job := range jobs {
		for _, wake := range job.WakeOutbox {
			if wake.State == WakePending && !wake.NextAttemptAt.After(now) {
				pending = append(pending, PendingWake{JobID: job.ID, Wake: wake})
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Wake.NextAttemptAt.Equal(pending[j].Wake.NextAttemptAt) {
			return pending[i].Wake.CreatedAt.Before(pending[j].Wake.CreatedAt)
		}
		return pending[i].Wake.NextAttemptAt.Before(pending[j].Wake.NextAttemptAt)
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func (s *Store) RecordWakeAttempt(jobID, wakeID, failure string, retryAt time.Time) (Job, error) {
	if !s.Enabled() {
		return Job{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil || !ok {
		if err != nil {
			return Job{}, err
		}
		return Job{}, ErrNotFound
	}
	now := time.Now().UTC()
	found := false
	for i := range job.WakeOutbox {
		if job.WakeOutbox[i].ID != wakeID {
			continue
		}
		if job.WakeOutbox[i].State == WakeAcked {
			return cloneJob(job), nil
		}
		job.WakeOutbox[i].Attempts++
		job.WakeOutbox[i].LastAttemptAt = cloneTime(&now)
		job.WakeOutbox[i].LastError = strings.TrimSpace(failure)
		if retryAt.IsZero() || retryAt.Before(now) {
			retryAt = now
		}
		job.WakeOutbox[i].NextAttemptAt = retryAt.UTC()
		found = true
		break
	}
	if !found {
		return cloneJob(job), ErrNotFound
	}
	job.Revision++
	job.UpdatedAt = now
	if err := s.writeLocked(job); err != nil {
		return Job{}, err
	}
	return cloneJob(job), nil
}

func (s *Store) AckWake(jobID, wakeID string) (Job, bool, error) {
	if !s.Enabled() {
		return Job{}, false, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil || !ok {
		if err != nil {
			return Job{}, false, err
		}
		return Job{}, false, ErrNotFound
	}
	now := time.Now().UTC()
	for i := range job.WakeOutbox {
		if job.WakeOutbox[i].ID != wakeID {
			continue
		}
		if job.WakeOutbox[i].State == WakeAcked {
			return cloneJob(job), false, nil
		}
		job.WakeOutbox[i].State = WakeAcked
		job.WakeOutbox[i].AckedAt = cloneTime(&now)
		job.WakeOutbox[i].LastError = ""
		job.Revision++
		job.UpdatedAt = now
		if err := s.writeLocked(job); err != nil {
			return Job{}, false, err
		}
		return cloneJob(job), true, nil
	}
	return cloneJob(job), false, ErrNotFound
}

func (s *Store) update(jobID string, expectedRevision uint64, mutate func(*Job, time.Time) error) (Job, error) {
	if !s.Enabled() {
		return Job{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok, err := s.loadLocked(jobID)
	if err != nil {
		return Job{}, err
	}
	if !ok {
		return Job{}, ErrNotFound
	}
	if job.Revision != expectedRevision {
		return cloneJob(job), ErrRevisionConflict
	}
	now := time.Now().UTC()
	before, _ := json.Marshal(job)
	if err := mutate(&job, now); err != nil {
		return cloneJob(job), err
	}
	after, _ := json.Marshal(job)
	if string(before) == string(after) {
		return cloneJob(job), nil
	}
	job.Revision++
	job.UpdatedAt = now
	if err := s.writeLocked(job); err != nil {
		return Job{}, err
	}
	return cloneJob(job), nil
}

func (s *Store) pathLocked(jobID string) string {
	if !s.Enabled() || !safeID(jobID) {
		return ""
	}
	return filepath.Join(s.dir, jobID+".json")
}

func (s *Store) ad(jobID string) vault.AD {
	return vault.AD{User: s.user, Store: storeName, Stream: jobID, Schema: schemaName}
}

func (s *Store) loadLocked(jobID string) (Job, bool, error) {
	jobID = strings.TrimSpace(jobID)
	path := s.pathLocked(jobID)
	if path == "" {
		return Job{}, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if vault.IsVault(b) {
		uv := s.vault.UserVault()
		if uv == nil {
			return Job{}, false, errors.New("coding build job vault unavailable")
		}
		b, err = uv.OpenFile(s.ad(jobID), b)
		if err != nil {
			return Job{}, false, err
		}
	}
	var job Job
	if err := json.Unmarshal(b, &job); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) writeLocked(job Job) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	b, err = s.vault.MaybeSealFile(s.ad(job.ID), b)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".build-job-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	clean := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		clean()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		clean()
		return err
	}
	if err := tmp.Sync(); err != nil {
		clean()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.pathLocked(job.ID)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *Store) listLocked() ([]Job, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		job, ok, err := s.loadLocked(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if ok {
			jobs = append(jobs, cloneJob(job))
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt) })
	return jobs, nil
}

func (s *Store) findByIdempotencyLocked(key string) (Job, bool, error) {
	jobs, err := s.listLocked()
	if err != nil {
		return Job{}, false, err
	}
	for _, job := range jobs {
		if job.IdempotencyKey == key {
			return job, true, nil
		}
	}
	return Job{}, false, nil
}

func normalizeCreate(input CreateRequest) CreateRequest {
	input.ID = strings.TrimSpace(input.ID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.ConversationID = strings.TrimSpace(input.ConversationID)
	input.UserServerID = strings.TrimSpace(input.UserServerID)
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.CWD = filepath.Clean(strings.TrimSpace(input.CWD))
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.Brief.Intent = strings.TrimSpace(input.Brief.Intent)
	input.Brief.Constraints = cleanStrings(input.Brief.Constraints)
	input.Brief.AcceptanceCriteria = cleanStrings(input.Brief.AcceptanceCriteria)
	cleanContext := make(map[string]string, len(input.Brief.ProjectContext))
	for key, value := range input.Brief.ProjectContext {
		if key, value = strings.TrimSpace(key), strings.TrimSpace(value); key != "" && value != "" {
			cleanContext[key] = value
		}
	}
	input.Brief.ProjectContext = cleanContext
	return input
}

func validateCreate(input CreateRequest) error {
	if input.ActorID == "" || input.ConversationID == "" || input.UserServerID == "" || input.ProjectID == "" || input.CWD == "." || input.CWD == "" || input.Brief.Intent == "" {
		return fmt.Errorf("%w: actor, conversation, user server, project, cwd, and intent are required", ErrInvalid)
	}
	if !filepath.IsAbs(input.CWD) {
		return fmt.Errorf("%w: cwd must be absolute", ErrInvalid)
	}
	if len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 200 {
		return fmt.Errorf("%w: idempotency key must be 8 to 200 characters", ErrInvalid)
	}
	return nil
}

func createHash(input CreateRequest) (string, error) {
	input.ID = ""
	input.IdempotencyKey = ""
	return digestJSON(input)
}

func normalizeDelivery(batch DeliveryBatch) DeliveryBatch {
	batch.DeliveryID = strings.TrimSpace(batch.DeliveryID)
	for i := range batch.SourceEventIDs {
		batch.SourceEventIDs[i] = strings.TrimSpace(batch.SourceEventIDs[i])
	}
	sort.Strings(batch.SourceEventIDs)
	for i := range batch.Evidence {
		batch.Evidence[i].ID = stableEvidenceID(batch.DeliveryID, batch.Evidence[i].SourceEventID, i)
		batch.Evidence[i].Summary = strings.TrimSpace(batch.Evidence[i].Summary)
		batch.Evidence[i].SourceEventID = strings.TrimSpace(batch.Evidence[i].SourceEventID)
		batch.Evidence[i].RecordedAt = time.Time{}
	}
	if batch.Wake != nil {
		wake := normalizeWake(*batch.Wake)
		batch.Wake = &wake
	}
	if batch.Terminal != nil {
		batch.Terminal.Summary = strings.TrimSpace(batch.Terminal.Summary)
	}
	return batch
}

func validateDelivery(batch DeliveryBatch) error {
	if batch.DeliveryID == "" || len(batch.SourceEventIDs) == 0 {
		return fmt.Errorf("%w: delivery and source event ids are required", ErrInvalid)
	}
	if duplicates(batch.SourceEventIDs) {
		return fmt.Errorf("%w: duplicate source event id", ErrInvalid)
	}
	if batch.Terminal != nil && batch.NextLifecycle != "" {
		return fmt.Errorf("%w: delivery cannot contain terminal and transition", ErrInvalid)
	}
	if batch.Terminal != nil && !validTerminal(*batch.Terminal) {
		return fmt.Errorf("%w: invalid terminal result", ErrInvalid)
	}
	if batch.NextLifecycle != "" && (!knownLifecycle(batch.NextLifecycle) || batch.NextLifecycle.Terminal()) {
		return ErrInvalidTransition
	}
	sources := make(map[string]struct{}, len(batch.SourceEventIDs))
	for _, id := range batch.SourceEventIDs {
		if id == "" {
			return fmt.Errorf("%w: source event id is required", ErrInvalid)
		}
		sources[id] = struct{}{}
	}
	for _, evidence := range batch.Evidence {
		if !knownEvidenceKind(evidence.Kind) {
			return fmt.Errorf("%w: evidence kind is required", ErrInvalid)
		}
		if evidence.SourceEventID == "" {
			return fmt.Errorf("%w: evidence source event id is required", ErrInvalid)
		}
		if _, ok := sources[evidence.SourceEventID]; !ok {
			return fmt.Errorf("%w: evidence source is outside delivery", ErrInvalid)
		}
	}
	if batch.Wake != nil {
		if err := validateWake(*batch.Wake); err != nil {
			return err
		}
		if batch.Wake.SourceEventID != "" {
			if _, ok := sources[batch.Wake.SourceEventID]; !ok {
				return fmt.Errorf("%w: wake source is outside delivery", ErrInvalid)
			}
		}
	}
	return nil
}

func normalizeWake(wake WakeRequest) WakeRequest {
	wake.DedupKey = strings.TrimSpace(wake.DedupKey)
	wake.Reason = strings.TrimSpace(wake.Reason)
	wake.SourceEventID = strings.TrimSpace(wake.SourceEventID)
	return wake
}

func validateWake(wake WakeRequest) error {
	if wake.DedupKey == "" || wake.Reason == "" || !knownWakeKind(wake.Kind) {
		return fmt.Errorf("%w: wake key, kind, and reason are required", ErrInvalid)
	}
	return nil
}

func enqueueWake(job *Job, request WakeRequest, now time.Time) bool {
	for _, wake := range job.WakeOutbox {
		if wake.DedupKey == request.DedupKey {
			return false
		}
	}
	job.WakeOutbox = append(job.WakeOutbox, WakeEnvelope{
		ID: uuid.NewString(), DedupKey: request.DedupKey, Kind: request.Kind,
		Reason: request.Reason, SourceEventID: request.SourceEventID,
		State: WakePending, CreatedAt: now, NextAttemptAt: now,
	})
	return true
}

func applyTerminal(job *Job, result TerminalResult, now time.Time) {
	job.Lifecycle = result.Lifecycle
	job.Outcome = result.Outcome
	job.OutcomeSummary = strings.TrimSpace(result.Summary)
	job.EndedAt = cloneTime(&now)
}

func validTerminal(result TerminalResult) bool {
	switch result.Lifecycle {
	case LifecycleVerified:
		return result.Outcome == OutcomeVerified
	case LifecycleFailed:
		return result.Outcome == OutcomeFailed
	case LifecycleInterrupted:
		return result.Outcome == OutcomeInterrupted
	default:
		return false
	}
}

func allowedTransition(from, to Lifecycle) bool {
	switch from {
	case LifecycleAccepted:
		return to == LifecycleQueued
	case LifecycleQueued:
		return to == LifecycleRunning || to == LifecycleBlocked || to == LifecycleNeedsInput || to == LifecycleNeedsApproval
	case LifecycleRunning:
		return to == LifecycleQueued || to == LifecycleNeedsInput || to == LifecycleNeedsApproval || to == LifecycleBlocked
	case LifecycleNeedsInput, LifecycleNeedsApproval, LifecycleBlocked:
		return to == LifecycleQueued || to == LifecycleRunning
	default:
		return false
	}
}

func knownLifecycle(value Lifecycle) bool {
	switch value {
	case LifecycleAccepted, LifecycleQueued, LifecycleRunning, LifecycleNeedsInput, LifecycleNeedsApproval, LifecycleBlocked, LifecycleInterrupted, LifecycleFailed, LifecycleVerified:
		return true
	default:
		return false
	}
}

func knownWakeKind(value WakeKind) bool {
	switch value {
	case WakeInputRequired, WakeApprovalRequired, WakeBlocked, WakeFailed, WakeInterrupted, WakeVerified:
		return true
	default:
		return false
	}
}

func knownEvidenceKind(value EvidenceKind) bool {
	switch value {
	case EvidenceStatus, EvidenceCommand, EvidenceFileChange, EvidenceDiff, EvidenceVerification, EvidencePreview, EvidenceQuestion, EvidenceApproval, EvidenceBlocker, EvidenceError:
		return true
	default:
		return false
	}
}

func digestJSON(value interface{}) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func stableEvidenceID(deliveryID, sourceEventID string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", strings.TrimSpace(deliveryID), strings.TrimSpace(sourceEventID), index)))
	return "evidence_" + hex.EncodeToString(sum[:12])
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func safeID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 200 {
		return false
	}
	return !strings.ContainsAny(id, "/\\\x00")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func duplicates(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func cloneJob(job Job) Job {
	b, err := json.Marshal(job)
	if err != nil {
		return job
	}
	var clone Job
	if err := json.Unmarshal(b, &clone); err != nil {
		return job
	}
	return clone
}

func cloneBrief(brief BuildBrief) BuildBrief {
	return BuildBrief{
		Intent: brief.Intent, Constraints: append([]string(nil), brief.Constraints...),
		AcceptanceCriteria: append([]string(nil), brief.AcceptanceCriteria...),
		ProjectContext:     cloneStringMap(brief.ProjectContext),
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func cloneTime(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneBool(input *bool) *bool {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}
