// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"sync"
	"time"
)

type RunRecord struct {
	ID        string
	User      string
	Workflow  string
	Subject   string
	CacheKey  string
	Status    string
	Cost      float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CacheRecord struct {
	Key       string
	User      string
	Payload   []byte
	Cost      float64
	ExpiresAt time.Time
}

type Store interface {
	PutRun(context.Context, RunRecord) error
	GetRun(context.Context, string, string) (RunRecord, bool, error)
	ActiveRuns(context.Context, string) (int, error)
	PutCache(context.Context, CacheRecord) error
	GetCache(context.Context, string, string, time.Time) (CacheRecord, bool, error)
	ReserveSpend(context.Context, string, time.Time, float64, float64) (bool, error)
}

type MemoryStore struct {
	mu    sync.Mutex
	runs  map[string]RunRecord
	cache map[string]CacheRecord
	spend map[string]float64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: map[string]RunRecord{}, cache: map[string]CacheRecord{}, spend: map[string]float64{}}
}

func (s *MemoryStore) PutRun(_ context.Context, record RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.runs[record.ID]; ok && record.CreatedAt.IsZero() {
		record.CreatedAt = existing.CreatedAt
	}
	s.runs[record.ID] = record
	return nil
}

func (s *MemoryStore) GetRun(_ context.Context, user, id string) (RunRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.runs[id]
	return record, ok && record.User == user, nil
}

func (s *MemoryStore) ActiveRuns(_ context.Context, user string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for id := range s.runs {
		record := s.runs[id]
		if record.User == user && (record.Status == "queued" || record.Status == "running") {
			count++
		}
	}
	return count, nil
}

func (s *MemoryStore) PutCache(_ context.Context, record CacheRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[record.User+"\x00"+record.Key] = record
	return nil
}

func (s *MemoryStore) GetCache(_ context.Context, user, key string, now time.Time) (CacheRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.cache[user+"\x00"+key]
	if !ok || !record.ExpiresAt.After(now) {
		return CacheRecord{}, false, nil
	}
	return record, true, nil
}

func (s *MemoryStore) ReserveSpend(_ context.Context, user string, day time.Time, amount, limit float64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := user + "\x00" + day.UTC().Format("2006-01-02")
	if s.spend[key]+amount > limit {
		return false, nil
	}
	s.spend[key] += amount
	return true, nil
}
