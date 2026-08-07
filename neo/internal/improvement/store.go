// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package improvement

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

	"matrix/neo/internal/memory"
	"matrix/vault"
)

const (
	stateFile   = "improvement.state.json"
	storeName   = "neo.improvement"
	storeSchema = "improvement.v1"
)

type Kind string

const (
	KindMemory         Kind = "memory"
	KindKnowledge      Kind = "knowledge"
	KindCapability     Kind = "capability"
	KindUnfinishedWork Kind = "unfinished_work"
	KindSkill          Kind = "skill"
	KindRule           Kind = "rule"
	KindConfiguration  Kind = "configuration"
	KindAuthority      Kind = "authority"
)

type ProposalStatus string

const (
	StatusPending    ProposalStatus = "pending"
	StatusApproved   ProposalStatus = "approved"
	StatusApplied    ProposalStatus = "applied"
	StatusRejected   ProposalStatus = "rejected"
	StatusRolledBack ProposalStatus = "rolled_back"
)

type ObservationStatus string

const (
	ObservationScheduled ObservationStatus = "scheduled"
	ObservationRunning   ObservationStatus = "running"
	ObservationDone      ObservationStatus = "done"
	ObservationNoop      ObservationStatus = "noop"
	ObservationFailed    ObservationStatus = "failed"
)

type Evidence struct {
	ConversationID string    `json:"conversation_id"`
	RunID          string    `json:"run_id"`
	Role           string    `json:"role"`
	Quote          string    `json:"quote"`
	OccurredAt     time.Time `json:"occurred_at,omitempty"`
}

type CapabilityPayload struct {
	Manifest   string `json:"manifest"`
	Prose      string `json:"prose"`
	Provenance string `json:"provenance"`
}

type Payload struct {
	Memory      *memory.MutationItem           `json:"memory,omitempty"`
	Knowledge   *memory.KnowledgeImportRequest `json:"knowledge,omitempty"`
	Capability  *CapabilityPayload             `json:"capability,omitempty"`
	Opportunity *memory.OpportunitySpec        `json:"opportunity,omitempty"`
}

type Draft struct {
	Kind       Kind       `json:"kind"`
	Summary    string     `json:"summary"`
	Rationale  string     `json:"rationale"`
	Confidence float32    `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
	Payload    Payload    `json:"payload"`
}

type Transition struct {
	Version int            `json:"version"`
	Status  ProposalStatus `json:"status"`
	At      time.Time      `json:"at"`
	By      string         `json:"by"`
	Detail  string         `json:"detail,omitempty"`
}

type Proposal struct {
	ID             string         `json:"id"`
	ObservationKey string         `json:"observation_key"`
	Draft          Draft          `json:"draft"`
	Status         ProposalStatus `json:"status"`
	Version        int            `json:"version"`
	AppliedRef     string         `json:"applied_ref,omitempty"`
	RollbackRef    string         `json:"rollback_ref,omitempty"`
	LastError      string         `json:"last_error,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	History        []Transition   `json:"history"`
}

type Observation struct {
	Key            string            `json:"key"`
	ConversationID string            `json:"conversation_id"`
	RunID          string            `json:"run_id"`
	AlarmID        string            `json:"alarm_id,omitempty"`
	Status         ObservationStatus `json:"status"`
	Attempts       int               `json:"attempts"`
	FireAt         time.Time         `json:"fire_at"`
	LastError      string            `json:"last_error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type State struct {
	SchemaVersion int                    `json:"schema_version"`
	Observations  map[string]Observation `json:"observations"`
	Proposals     map[string]Proposal    `json:"proposals"`
}

type Store struct {
	dir   string
	mu    sync.Mutex
	state State
	vault *vault.Session
	user  string
	now   func() time.Time
}

func Open(dir string, session *vault.Session, user string) (*Store, error) {
	s := &Store{dir: strings.TrimSpace(dir), vault: session, user: strings.TrimSpace(user), now: time.Now}
	s.state = emptyState()
	if s.dir == "" {
		return s, nil
	}
	raw, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("improvement: read state: %w", err)
	}
	if vault.IsVault(raw) {
		uv := session.UserVault()
		if uv == nil {
			return nil, errors.New("improvement: encrypted state requires vault")
		}
		raw, err = uv.OpenFile(s.ad(), raw)
		if err != nil {
			return nil, fmt.Errorf("improvement: open state: %w", err)
		}
	}
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return nil, fmt.Errorf("improvement: decode state: %w", err)
	}
	normalizeState(&s.state)
	return s, nil
}

func emptyState() State {
	return State{SchemaVersion: 1, Observations: map[string]Observation{}, Proposals: map[string]Proposal{}}
}

func normalizeState(state *State) {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = 1
	}
	if state.Observations == nil {
		state.Observations = map[string]Observation{}
	}
	if state.Proposals == nil {
		state.Proposals = map[string]Proposal{}
	}
}

func (s *Store) ad() vault.AD {
	return vault.AD{User: s.user, Store: storeName, Schema: storeSchema}
}

func (s *Store) path() string { return filepath.Join(s.dir, stateFile) }

func cloneState(state State) (State, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, err
	}
	var cloned State
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return State{}, err
	}
	normalizeState(&cloned)
	return cloned, nil
}

func (s *Store) replace(mutate func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err := mutate(&candidate); err != nil {
		return err
	}
	if err := s.persist(candidate); err != nil {
		return err
	}
	s.state = candidate
	return nil
}

func (s *Store) persist(state State) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("improvement: mkdir: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	raw, err = s.vault.MaybeSealFile(s.ad(), raw)
	if err != nil {
		return fmt.Errorf("improvement: seal state: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ObservationKey(conversationID, runID string) string {
	return digestID("obs", strings.TrimSpace(conversationID)+"\x00"+strings.TrimSpace(runID))
}

func (s *Store) Schedule(conversationID, runID string, fireAt time.Time) (Observation, bool, error) {
	key := ObservationKey(conversationID, runID)
	var result Observation
	fresh := false
	err := s.replace(func(state *State) error {
		if existing, ok := state.Observations[key]; ok {
			result = existing
			return nil
		}
		now := s.now().UTC()
		result = Observation{Key: key, ConversationID: strings.TrimSpace(conversationID), RunID: strings.TrimSpace(runID), Status: ObservationScheduled, FireAt: fireAt.UTC(), CreatedAt: now, UpdatedAt: now}
		state.Observations[key] = result
		fresh = true
		return nil
	})
	return result, fresh, err
}

func (s *Store) SetAlarm(key, alarmID string) error {
	return s.replace(func(state *State) error {
		observation, ok := state.Observations[key]
		if !ok {
			return os.ErrNotExist
		}
		observation.AlarmID = strings.TrimSpace(alarmID)
		observation.UpdatedAt = s.now().UTC()
		state.Observations[key] = observation
		return nil
	})
}

func (s *Store) Begin(key string) (Observation, error) {
	var result Observation
	err := s.replace(func(state *State) error {
		observation, ok := state.Observations[key]
		if !ok {
			return os.ErrNotExist
		}
		if observation.Status == ObservationDone || observation.Status == ObservationNoop {
			result = observation
			return nil
		}
		observation.Status = ObservationRunning
		observation.Attempts++
		observation.LastError = ""
		observation.UpdatedAt = s.now().UTC()
		state.Observations[key] = observation
		result = observation
		return nil
	})
	return result, err
}

func (s *Store) Finish(key string, drafts []Draft) ([]Proposal, error) {
	for index := range drafts {
		if err := ValidateDraft(drafts[index]); err != nil {
			return nil, fmt.Errorf("proposal %d: %w", index+1, err)
		}
	}
	created := []Proposal{}
	err := s.replace(func(state *State) error {
		observation, ok := state.Observations[key]
		if !ok {
			return os.ErrNotExist
		}
		now := s.now().UTC()
		for _, draft := range drafts {
			raw, _ := json.Marshal(draft)
			id := digestID("proposal", key+"\x00"+string(raw))
			if existing, exists := state.Proposals[id]; exists {
				created = append(created, cloneProposal(existing))
				continue
			}
			var storedDraft Draft
			_ = json.Unmarshal(raw, &storedDraft)
			proposal := Proposal{ID: id, ObservationKey: key, Draft: storedDraft, Status: StatusPending, Version: 1, CreatedAt: now, UpdatedAt: now}
			proposal.History = []Transition{{Version: 1, Status: StatusPending, At: now, By: "observer", Detail: "evidenced proposal created"}}
			state.Proposals[id] = proposal
			created = append(created, cloneProposal(proposal))
		}
		if len(drafts) == 0 {
			observation.Status = ObservationNoop
		} else {
			observation.Status = ObservationDone
		}
		observation.LastError = ""
		observation.UpdatedAt = now
		state.Observations[key] = observation
		return nil
	})
	return created, err
}

func (s *Store) Fail(key string, failure string) error {
	return s.replace(func(state *State) error {
		observation, ok := state.Observations[key]
		if !ok {
			return os.ErrNotExist
		}
		observation.Status = ObservationFailed
		observation.LastError = bound(failure, 600)
		observation.UpdatedAt = s.now().UTC()
		state.Observations[key] = observation
		return nil
	})
}

func (s *Store) Transition(id string, from []ProposalStatus, to ProposalStatus, by, detail, appliedRef, rollbackRef string) (Proposal, error) {
	var result Proposal
	err := s.replace(func(state *State) error {
		proposal, ok := state.Proposals[id]
		if !ok {
			return os.ErrNotExist
		}
		allowed := false
		for _, status := range from {
			allowed = allowed || proposal.Status == status
		}
		if !allowed {
			return fmt.Errorf("improvement: invalid transition %s -> %s", proposal.Status, to)
		}
		now := s.now().UTC()
		proposal.Status = to
		proposal.Version++
		proposal.UpdatedAt = now
		proposal.LastError = ""
		if appliedRef != "" {
			proposal.AppliedRef = appliedRef
		}
		if rollbackRef != "" {
			proposal.RollbackRef = rollbackRef
		}
		proposal.History = append(proposal.History, Transition{Version: proposal.Version, Status: to, At: now, By: bound(by, 120), Detail: bound(detail, 600)})
		state.Proposals[id] = proposal
		result = cloneProposal(proposal)
		return nil
	})
	return result, err
}

func (s *Store) SetProposalError(id string, failure string) error {
	return s.replace(func(state *State) error {
		proposal, ok := state.Proposals[id]
		if !ok {
			return os.ErrNotExist
		}
		proposal.LastError = bound(failure, 600)
		proposal.UpdatedAt = s.now().UTC()
		state.Proposals[id] = proposal
		return nil
	})
}

func (s *Store) Get(id string) (Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	proposal, ok := s.state.Proposals[id]
	return cloneProposal(proposal), ok
}

func (s *Store) Observation(key string) (Observation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	observation, ok := s.state.Observations[key]
	return observation, ok
}

func (s *Store) List(status ProposalStatus) []Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Proposal, 0, len(s.state.Proposals))
	for _, proposal := range s.state.Proposals {
		if status == "" || proposal.Status == status {
			out = append(out, cloneProposal(proposal))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func cloneProposal(proposal Proposal) Proposal {
	raw, _ := json.Marshal(proposal)
	var cloned Proposal
	_ = json.Unmarshal(raw, &cloned)
	return cloned
}

func ValidateDraft(draft Draft) error {
	draft.Summary = strings.TrimSpace(draft.Summary)
	draft.Rationale = strings.TrimSpace(draft.Rationale)
	if draft.Summary == "" || len([]rune(draft.Summary)) > 300 {
		return errors.New("summary is required and bounded")
	}
	if draft.Rationale == "" || len([]rune(draft.Rationale)) > 2000 {
		return errors.New("rationale is required and bounded")
	}
	if draft.Confidence < 0 || draft.Confidence > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	if len(draft.Evidence) == 0 || len(draft.Evidence) > 12 {
		return errors.New("one to twelve evidence records are required")
	}
	for _, evidence := range draft.Evidence {
		if strings.TrimSpace(evidence.ConversationID) == "" || strings.TrimSpace(evidence.RunID) == "" || strings.TrimSpace(evidence.Quote) == "" || len([]rune(evidence.Quote)) > 4000 {
			return errors.New("evidence must identify a conversation, run, and bounded exact quote")
		}
	}
	payloads := 0
	if draft.Payload.Memory != nil {
		payloads++
	}
	if draft.Payload.Knowledge != nil {
		payloads++
	}
	if draft.Payload.Capability != nil {
		payloads++
	}
	if draft.Payload.Opportunity != nil {
		payloads++
	}
	if payloads != 1 {
		return errors.New("exactly one typed payload is required")
	}
	switch draft.Kind {
	case KindMemory:
		item := draft.Payload.Memory
		if item == nil || item.Operation != memory.MutationSupersede || item.Target == nil || strings.TrimSpace(item.Target.URI) == "" || item.Value == nil || strings.TrimSpace(item.Value.Content) == "" {
			return errors.New("memory proposals must be an exact typed supersession")
		}
	case KindKnowledge:
		if draft.Payload.Knowledge == nil || strings.TrimSpace(draft.Payload.Knowledge.Title) == "" || strings.TrimSpace(draft.Payload.Knowledge.Content) == "" {
			return errors.New("knowledge proposal requires a document")
		}
	case KindUnfinishedWork:
		if draft.Payload.Opportunity == nil || strings.TrimSpace(draft.Payload.Opportunity.Summary) == "" {
			return errors.New("unfinished-work proposal requires an opportunity")
		}
	case KindCapability, KindSkill, KindRule, KindConfiguration, KindAuthority:
		if draft.Payload.Capability == nil || strings.TrimSpace(draft.Payload.Capability.Manifest) == "" || strings.TrimSpace(draft.Payload.Capability.Prose) == "" {
			return errors.New("governed proposal requires a Capability Hub candidate payload")
		}
	default:
		return fmt.Errorf("unsupported proposal kind %q", draft.Kind)
	}
	return nil
}

func digestID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:10])
}

func bound(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > max {
		runes = runes[:max]
	}
	return strings.TrimSpace(string(runes))
}
