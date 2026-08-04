package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/neo/internal/codingruntime"
)

func (s *buildWorkerService) runWakeOutbox() {
	defer s.workerWG.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.deliverPendingBuildWakes()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *buildWorkerService) deliverPendingBuildWakes() {
	pending, err := s.engine.buildJobs.PendingWakes(time.Now().UTC(), 16)
	if err != nil {
		return
	}
	for _, item := range pending {
		job, ok, err := s.engine.buildJobs.Get(item.JobID)
		if err != nil || !ok {
			continue
		}
		prompt := buildWakePrompt(job, item.Wake)
		_, _, duplicate, busy, dispatchErr := s.engine.sessions.get(job.ConversationID).submitSystemWake(prompt, item.Wake.DedupKey)
		if busy {
			_, _ = s.engine.buildJobs.RecordWakeAttempt(job.ID, item.Wake.ID, "conversation is active", time.Now().UTC().Add(3*time.Second))
			continue
		}
		if dispatchErr != nil && !errors.Is(dispatchErr, codingruntime.ErrDeliveryConflict) {
			_, _ = s.engine.buildJobs.RecordWakeAttempt(job.ID, item.Wake.ID, dispatchErr.Error(), buildWakeRetry(item.Wake.Attempts))
			continue
		}
		// A fresh dispatch is recorded durably before this returns. A duplicate
		// means that exact durable Neo run already exists. In either case the
		// task reaper owns completion across restart, so the outbox can ack.
		if duplicate || dispatchErr == nil {
			_, _, _ = s.engine.buildJobs.AckWake(job.ID, item.Wake.ID)
		}
	}
}

func buildWakeRetry(attempts int) time.Time {
	delay := time.Duration(1<<min(attempts, 6)) * time.Second
	return time.Now().UTC().Add(delay)
}

func buildWakePrompt(job codingruntime.Job, wake codingruntime.WakeEnvelope) string {
	public := job.Public()
	type wakeEvidence struct {
		Kind     codingruntime.EvidenceKind `json:"kind"`
		Summary  string                     `json:"summary,omitempty"`
		Path     string                     `json:"path,omitempty"`
		Check    string                     `json:"check,omitempty"`
		Passed   *bool                      `json:"passed,omitempty"`
		Question string                     `json:"question,omitempty"`
		Error    string                     `json:"error,omitempty"`
	}
	start := 0
	if len(public.Evidence) > 12 {
		start = len(public.Evidence) - 12
	}
	evidence := make([]wakeEvidence, 0, len(public.Evidence)-start)
	for _, item := range public.Evidence[start:] {
		evidence = append(evidence, wakeEvidence{
			Kind: item.Kind, Summary: truncateBuildText(item.Summary, 1000), Path: item.Path,
			Check: truncateBuildText(item.Check, 1000), Passed: item.Passed,
			Question: truncateBuildText(item.Question, 2000), Error: truncateBuildText(item.Error, 2000),
		})
	}
	payload, _ := json.Marshal(struct {
		JobID     string         `json:"job_id"`
		ProjectID string         `json:"project_id"`
		Intent    string         `json:"intent"`
		Status    string         `json:"status"`
		Outcome   string         `json:"outcome,omitempty"`
		Reason    string         `json:"wake_reason"`
		Evidence  []wakeEvidence `json:"evidence"`
	}{
		JobID: public.ID, ProjectID: public.ProjectID, Intent: truncateBuildText(public.Brief.Intent, 2000),
		Status: string(public.Lifecycle), Outcome: string(public.Outcome),
		Reason: truncateBuildText(wake.Reason, 2000), Evidence: evidence,
	})
	return strings.TrimSpace(fmt.Sprintf(`Your durable Build run for this conversation has produced an update. You are resuming your own coding work; there is no separate builder.

Continue under Neo's normal system rules. If input or approval is needed, ask the exact question clearly. If the work completed, perform every applicable completion step before presenting it, including the frontend Paxeer Cloud post-build delivery rule, then summarize what you built and the recorded verification honestly. If blocked or failed, explain the actionable reason. Do not call build_project, do not start another coding job, do not invent files or checks, and do not expose internal execution architecture or identifiers beyond the public Build job id.

Build update:
%s`, payload))
}
