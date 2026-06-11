// Package vault stores per-user provider credentials. The Store interface deals
// in plaintext Records; persistent backends (Postgres) encrypt the token fields
// at rest via internal/cryptox. The in-memory Memory store is the dev backend.
//
// Keying is (UserID, Provider) — the UserID is the owner's Supabase user id,
// resolved from the agent DID label, which is ALSO the id that consented the
// app via GoTrue, so the binding closes itself.
package vault

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when the owner has not connected the provider.
var ErrNotFound = errors.New("vault: provider not connected")

// Record is one vaulted provider grant.
type Record struct {
	UserID       string    `json:"user_id"`
	Provider     string    `json:"provider"`
	ConnectorID  string    `json:"connector_id"`
	AccessToken  string    `json:"access_token,omitempty"`  // short-lived cache
	RefreshToken string    `json:"refresh_token,omitempty"` // durable credential
	Scopes       []string  `json:"scopes,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Status       string    `json:"status"` // "active" | "revoked"
	UpdatedAt    time.Time `json:"updated_at"`
}

// HasScope reports whether the grant covers want.
func (r *Record) HasScope(want string) bool {
	for _, s := range r.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// Store persists provider credentials keyed by (userID, provider).
type Store interface {
	Get(ctx context.Context, userID, provider string) (*Record, error)
	Put(ctx context.Context, rec *Record) error
	Delete(ctx context.Context, userID, provider string) error
	List(ctx context.Context, userID string) ([]*Record, error)
}

// Memory is an in-process Store for dev/tests. Not durable.
type Memory struct {
	mu sync.RWMutex
	m  map[string]Record
}

// NewMemory constructs an empty in-memory store.
func NewMemory() *Memory { return &Memory{m: map[string]Record{}} }

func key(userID, provider string) string { return userID + "\x00" + provider }

// Get returns a copy of the record, or ErrNotFound.
func (s *Memory) Get(_ context.Context, userID, provider string) (*Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.m[key(userID, provider)]
	if !ok || rec.Status == "revoked" {
		return nil, ErrNotFound
	}
	cp := rec
	cp.Scopes = append([]string(nil), rec.Scopes...)
	return &cp, nil
}

// Put upserts a record.
func (s *Memory) Put(_ context.Context, rec *Record) error {
	if rec == nil || rec.UserID == "" || rec.Provider == "" {
		return errors.New("vault: record requires user_id and provider")
	}
	cp := *rec
	cp.Scopes = append([]string(nil), rec.Scopes...)
	if cp.Status == "" {
		cp.Status = "active"
	}
	cp.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.m[key(rec.UserID, rec.Provider)] = cp
	s.mu.Unlock()
	return nil
}

// Delete removes a record (hard delete for the dev store).
func (s *Memory) Delete(_ context.Context, userID, provider string) error {
	s.mu.Lock()
	delete(s.m, key(userID, provider))
	s.mu.Unlock()
	return nil
}

// List returns all active records for a user.
func (s *Memory) List(_ context.Context, userID string) ([]*Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Record
	for _, rec := range s.m {
		if rec.UserID == userID && rec.Status != "revoked" {
			cp := rec
			cp.Scopes = append([]string(nil), rec.Scopes...)
			out = append(out, &cp)
		}
	}
	return out, nil
}

// compile-time check.
var _ Store = (*Memory)(nil)
