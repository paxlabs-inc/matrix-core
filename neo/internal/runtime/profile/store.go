// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/vault"
)

const schema = "neo.profile.v1"

var ErrAbsent = errors.New("profile: absent")

type ConsentState string

const (
	ConsentUnset   ConsentState = "unset"
	ConsentGranted ConsentState = "granted"
	ConsentDenied  ConsentState = "denied"
)

type DeletionState string

const (
	DeletionActive  DeletionState = "active"
	DeletionDeleted DeletionState = "deleted"
)

type Profile struct {
	PreferredPersonName string        `json:"preferred_name"`
	AgentName           string        `json:"agent_name"`
	ExpertiseDomains    []string      `json:"expertise_domains"`
	ProfileVersion      uint64        `json:"profile_version"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
	Consent             ConsentState  `json:"consent_state"`
	Deletion            DeletionState `json:"deletion_state"`
}

type state struct {
	Profile           *Profile `json:"profile,omitempty"`
	MigrationComplete bool     `json:"migration_complete"`
}

type LegacyReader func(context.Context) (Profile, bool, error)

type Store struct {
	path  string
	vault *vault.UserVault
	user  string
	now   func() time.Time
	mu    sync.Mutex
}

func Open(path string, session *vault.Session) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("profile: path is required")
	}
	if session == nil || !session.Encrypting() || session.UserVault() == nil {
		return nil, vault.ErrVaultRequired
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("profile: create directory: %w", err)
	}
	return &Store{path: path, vault: session.UserVault(), user: session.UserVault().User(), now: time.Now}, nil
}

func (s *Store) ad() vault.AD {
	return vault.AD{User: s.user, Store: "neo.profile", Stream: "current", Schema: schema}
}

func (s *Store) read() (state, error) {
	sealed, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("profile: read: %w", err)
	}
	plain, err := s.vault.OpenFile(s.ad(), sealed)
	if err != nil {
		return state{}, fmt.Errorf("profile: decrypt: %w", err)
	}
	var current state
	if err := json.Unmarshal(plain, &current); err != nil {
		return state{}, fmt.Errorf("profile: decode: %w", err)
	}
	return current, nil
}

func (s *Store) write(current state) error {
	plain, err := json.Marshal(current)
	if err != nil {
		return err
	}
	sealed, err := s.vault.SealFile(s.ad(), plain)
	if err != nil {
		return fmt.Errorf("profile: encrypt: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".profile-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func normalize(input Profile) (Profile, error) {
	input.PreferredPersonName = strings.TrimSpace(input.PreferredPersonName)
	input.AgentName = strings.TrimSpace(input.AgentName)
	if input.AgentName == "" {
		input.AgentName = "Neo"
	}
	domains := input.ExpertiseDomains
	seen := make(map[string]struct{})
	input.ExpertiseDomains = make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.TrimSpace(domain)
		if domain == "" {
			continue
		}
		if len(domain) > 120 {
			return Profile{}, fmt.Errorf("profile: expertise domain exceeds 120 bytes")
		}
		key := strings.ToLower(domain)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		input.ExpertiseDomains = append(input.ExpertiseDomains, domain)
	}
	if len(input.PreferredPersonName) > 120 || len(input.AgentName) > 120 || len(input.ExpertiseDomains) > 32 {
		return Profile{}, fmt.Errorf("profile: field limit exceeded")
	}
	sort.Strings(input.ExpertiseDomains)
	if input.Consent == "" {
		input.Consent = ConsentUnset
	}
	if input.Consent != ConsentUnset && input.Consent != ConsentGranted && input.Consent != ConsentDenied {
		return Profile{}, fmt.Errorf("profile: invalid consent state")
	}
	input.Deletion = DeletionActive
	return input, nil
}

func (s *Store) Put(_ context.Context, input Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	input, err := normalize(input)
	if err != nil {
		return Profile{}, err
	}
	current, err := s.read()
	if err != nil {
		return Profile{}, err
	}
	now := s.now().UTC()
	input.ProfileVersion = 1
	input.CreatedAt = now
	if current.Profile != nil {
		input.ProfileVersion = current.Profile.ProfileVersion + 1
		input.CreatedAt = current.Profile.CreatedAt
	}
	input.UpdatedAt = now
	current.Profile = &input
	current.MigrationComplete = true
	if err := s.write(current); err != nil {
		return Profile{}, err
	}
	return input, nil
}

func (s *Store) Get(ctx context.Context, legacy LegacyReader) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.read()
	if err != nil {
		return Profile{}, err
	}
	if current.Profile != nil && current.Profile.Deletion == DeletionActive {
		return *current.Profile, nil
	}
	if !current.MigrationComplete && legacy != nil {
		candidate, ok, err := legacy(ctx)
		if err != nil {
			return Profile{}, err
		}
		current.MigrationComplete = true
		if ok {
			candidate, err = normalize(candidate)
			if err != nil {
				return Profile{}, err
			}
			now := s.now().UTC()
			candidate.ProfileVersion = 1
			candidate.CreatedAt, candidate.UpdatedAt = now, now
			current.Profile = &candidate
		}
		if err := s.write(current); err != nil {
			return Profile{}, err
		}
		if current.Profile != nil {
			return *current.Profile, nil
		}
	}
	return Profile{}, ErrAbsent
}

func (s *Store) Delete(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.read()
	if err != nil {
		return err
	}
	current.MigrationComplete = true
	if current.Profile != nil {
		tombstone := *current.Profile
		tombstone.ProfileVersion++
		tombstone.UpdatedAt = s.now().UTC()
		tombstone.Deletion = DeletionDeleted
		tombstone.PreferredPersonName = ""
		tombstone.AgentName = ""
		tombstone.ExpertiseDomains = nil
		current.Profile = &tombstone
	}
	return s.write(current)
}
