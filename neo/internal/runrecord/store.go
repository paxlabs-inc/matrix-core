package runrecord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/vault"
)

const (
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
	storeRun          = "neo.run"
	schemaRunV1       = "run.v1"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency key already belongs to another request")
	ErrTerminal            = errors.New("run is already terminal")
)

type Record struct {
	IntentID       string     `json:"intent_id"`
	ConversationID string     `json:"conversation_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	Request        string     `json:"request"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	Result         string     `json:"result,omitempty"`
	Error          string     `json:"error,omitempty"`
	LastEventSeq   int        `json:"last_event_seq"`
}

func (r Record) Terminal() bool {
	return r.Status == StatusCompleted || r.Status == StatusFailed || r.Status == StatusInterrupted
}

type Store struct {
	mu    sync.Mutex
	dir   string
	vault *vault.Session
	user  string
}

func Open(dir string) *Store { return &Store{dir: strings.TrimSpace(dir)} }

func Dir(override, cortexRoot string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(cortexRoot); v != "" {
		return filepath.Join(filepath.Dir(v), "runs")
	}
	return ""
}

func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.vault = sess
	s.user = user
	s.mu.Unlock()
}

func sanitize(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "\\", "_")
	return strings.TrimSpace(id)
}

func (s *Store) pathLocked(intentID string) string {
	if !s.Enabled() || strings.TrimSpace(intentID) == "" {
		return ""
	}
	return filepath.Join(s.dir, sanitize(intentID)+".json")
}

func (s *Store) ad(intentID string) vault.AD {
	return vault.AD{User: s.user, Store: storeRun, Stream: sanitize(intentID), Schema: schemaRunV1}
}

func (s *Store) loadLocked(intentID string) (Record, bool, error) {
	path := s.pathLocked(intentID)
	if path == "" {
		return Record{}, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if vault.IsVault(b) {
		uv := s.vault.UserVault()
		if uv == nil {
			return Record{}, false, errors.New("run record vault unavailable")
		}
		b, err = uv.OpenFile(s.ad(intentID), b)
		if err != nil {
			return Record{}, false, err
		}
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

func (s *Store) writeLocked(rec Record) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b, err = s.vault.MaybeSealFile(s.ad(rec.IntentID), b)
	if err != nil {
		return err
	}
	path := s.pathLocked(rec.IntentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) findByKeyLocked(key string) (Record, bool, error) {
	key = strings.TrimSpace(key)
	if !s.Enabled() || key == "" {
		return Record{}, false, nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		rec, ok, err := s.loadLocked(id)
		if err != nil {
			return Record{}, false, err
		}
		if ok && rec.IdempotencyKey == key {
			return rec, true, nil
		}
	}
	return Record{}, false, nil
}

func (s *Store) Begin(intentID, conversationID, key, request string) (Record, bool, error) {
	if !s.Enabled() {
		now := time.Now().UTC()
		return Record{IntentID: intentID, ConversationID: conversationID, IdempotencyKey: key, Request: strings.TrimSpace(request), Status: StatusRunning, CreatedAt: now, StartedAt: &now}, true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok, err := s.findByKeyLocked(key); err != nil {
		return Record{}, false, err
	} else if ok {
		if rec.ConversationID != conversationID || rec.Request != strings.TrimSpace(request) {
			return rec, false, ErrIdempotencyConflict
		}
		return rec, false, nil
	}
	if rec, ok, err := s.loadLocked(intentID); err != nil {
		return Record{}, false, err
	} else if ok {
		return rec, false, nil
	}
	now := time.Now().UTC()
	rec := Record{IntentID: intentID, ConversationID: conversationID, IdempotencyKey: strings.TrimSpace(key), Request: strings.TrimSpace(request), Status: StatusRunning, CreatedAt: now, StartedAt: &now}
	if err := s.writeLocked(rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

func (s *Store) Get(intentID string) (Record, bool, error) {
	if !s.Enabled() {
		return Record{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(intentID)
}

func (s *Store) FindByIdempotency(key string) (Record, bool, error) {
	if !s.Enabled() {
		return Record{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findByKeyLocked(key)
}

func (s *Store) Finish(intentID, status, result, failure string, lastEventSeq int) (Record, bool, error) {
	if !s.Enabled() {
		return Record{}, true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok, err := s.loadLocked(intentID)
	if err != nil || !ok {
		return rec, false, err
	}
	if rec.Terminal() {
		if rec.Status == status {
			return rec, false, nil
		}
		return rec, false, ErrTerminal
	}
	if status != StatusCompleted && status != StatusFailed && status != StatusInterrupted {
		return rec, false, errors.New("invalid terminal run status")
	}
	now := time.Now().UTC()
	rec.Status = status
	rec.EndedAt = &now
	rec.Result = strings.TrimSpace(result)
	rec.Error = strings.TrimSpace(failure)
	if lastEventSeq > rec.LastEventSeq {
		rec.LastEventSeq = lastEventSeq
	}
	if err := s.writeLocked(rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

func (s *Store) ActiveForConversation(conversationID string) (Record, bool) {
	if !s.Enabled() || strings.TrimSpace(conversationID) == "" {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return Record{}, false
	}
	var found []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rec, ok, err := s.loadLocked(strings.TrimSuffix(entry.Name(), ".json"))
		if err == nil && ok && rec.ConversationID == conversationID && !rec.Terminal() {
			found = append(found, rec)
		}
	}
	if len(found) == 0 {
		return Record{}, false
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.After(found[j].CreatedAt) })
	return found[0], true
}
