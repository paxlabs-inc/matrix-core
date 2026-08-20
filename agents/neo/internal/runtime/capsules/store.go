// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capsules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"centra/agents/neo/internal/runtime/records"
	"centra/packages/vault"
)

type Temperature string

const (
	Hot  Temperature = "hot"
	Warm Temperature = "warm"
	Cold Temperature = "cold"
)

type Claim struct {
	Statement string   `json:"statement"`
	Status    string   `json:"epistemic_status"`
	Evidence  []string `json:"evidence"`
}

type Capsule struct {
	CapsuleID           string                       `json:"capsule_id"`
	LogicalTurnID       string                       `json:"logical_turn_id"`
	CycleStart          uint64                       `json:"cycle_start"`
	CycleEnd            uint64                       `json:"cycle_end"`
	Objective           string                       `json:"objective"`
	Subgoal             string                       `json:"subgoal,omitempty"`
	OperationsAttempted []string                     `json:"operations_attempted"`
	Observations        []string                     `json:"observations"`
	EvidenceRefs        []string                     `json:"evidence_refs"`
	Claims              []Claim                      `json:"claims"`
	Decisions           []string                     `json:"decisions"`
	UnresolvedQuestions []string                     `json:"unresolved_questions"`
	BlockedSubgoals     []string                     `json:"blocked_subgoals"`
	CompletedSubgoals   []string                     `json:"completed_subgoals"`
	FailureFingerprints []records.FailureFingerprint `json:"failure_fingerprints"`
	NextIntendedAction  string                       `json:"next_intended_action"`
	SourceIdentities    []string                     `json:"source_identities"`
	ContentHash         string                       `json:"content_hash"`
	Supersedes          []string                     `json:"supersedes"`
	Temperature         Temperature                  `json:"temperature"`
	CreatedAt           time.Time                    `json:"created_at"`
}

type Store struct {
	dir   string
	vault *vault.UserVault
	user  string
}

func Open(dir string, session *vault.Session) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("capsules: directory is required")
	}
	if session == nil || !session.Encrypting() || session.UserVault() == nil {
		return nil, vault.ErrVaultRequired
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, vault: session.UserVault(), user: session.UserVault().User()}, nil
}

func (s *Store) ad(id string) vault.AD {
	return vault.AD{User: s.user, Store: "neo.capsules", Stream: id, Schema: "causal-capsule.v1"}
}

func capsuleHash(capsule Capsule) string {
	copy := capsule
	copy.ContentHash = ""
	copy.CreatedAt = time.Time{}
	encoded, _ := json.Marshal(copy)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func ValidateFence(turn records.TurnRecord, sourceIdentities []string) error {
	if !turn.SynthesisDebt.Owed {
		return nil
	}
	unconsumed := make(map[string]struct{}, len(turn.SynthesisDebt.UnconsumedEvidence))
	for _, identity := range turn.SynthesisDebt.UnconsumedEvidence {
		unconsumed[identity] = struct{}{}
	}
	for _, identity := range sourceIdentities {
		if _, blocked := unconsumed[identity]; blocked {
			return fmt.Errorf("capsules: synthesis consumption fence blocks %s", identity)
		}
	}
	return nil
}

func (s *Store) Append(_ context.Context, turn records.TurnRecord, capsule Capsule) (Capsule, error) {
	if strings.TrimSpace(capsule.CapsuleID) == "" || strings.TrimSpace(capsule.LogicalTurnID) == "" || capsule.LogicalTurnID != turn.LogicalTurnID || strings.TrimSpace(capsule.Objective) == "" || capsule.CycleEnd < capsule.CycleStart {
		return Capsule{}, fmt.Errorf("capsules: invalid causal capsule")
	}
	if capsule.Temperature == "" {
		capsule.Temperature = Warm
	}
	if capsule.Temperature != Hot && capsule.Temperature != Warm && capsule.Temperature != Cold {
		return Capsule{}, fmt.Errorf("capsules: invalid temperature")
	}
	if err := ValidateFence(turn, capsule.SourceIdentities); err != nil {
		return Capsule{}, err
	}
	path := filepath.Join(s.dir, capsule.CapsuleID+".vault")
	if _, err := os.Stat(path); err == nil {
		return Capsule{}, fmt.Errorf("capsules: append-only identity already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Capsule{}, err
	}
	capsule.CreatedAt = time.Now().UTC()
	capsule.ContentHash = capsuleHash(capsule)
	encoded, err := json.Marshal(capsule)
	if err != nil {
		return Capsule{}, err
	}
	sealed, err := s.vault.SealFile(s.ad(capsule.CapsuleID), encoded)
	if err != nil {
		return Capsule{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Capsule{}, err
	}
	if _, err = file.Write(sealed); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return Capsule{}, err
	}
	return capsule, nil
}

func (s *Store) Load(_ context.Context, id string) (Capsule, error) {
	sealed, err := os.ReadFile(filepath.Join(s.dir, id+".vault"))
	if err != nil {
		return Capsule{}, err
	}
	plain, err := s.vault.OpenFile(s.ad(id), sealed)
	if err != nil {
		return Capsule{}, err
	}
	var capsule Capsule
	if err := json.Unmarshal(plain, &capsule); err != nil {
		return Capsule{}, err
	}
	if capsule.ContentHash != capsuleHash(capsule) {
		return Capsule{}, fmt.Errorf("capsules: content hash mismatch")
	}
	return capsule, nil
}

func (s *Store) List(ctx context.Context) ([]Capsule, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var result []Capsule
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".vault") {
			continue
		}
		capsule, err := s.Load(ctx, strings.TrimSuffix(entry.Name(), ".vault"))
		if err != nil {
			return nil, err
		}
		result = append(result, capsule)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CycleStart != result[j].CycleStart {
			return result[i].CycleStart < result[j].CycleStart
		}
		return result[i].CapsuleID < result[j].CapsuleID
	})
	return result, nil
}
