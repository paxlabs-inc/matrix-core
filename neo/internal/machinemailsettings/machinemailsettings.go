package machinemailsettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"matrix/vault"
)

const settingsFile = "machinemail.settings.json"

const (
	storeSettings   = "neo.machinemail.settings"
	schemaSettings1 = "machinemail.settings.v1"
)

var ErrEncryptionRequired = errors.New("the encrypted user vault is not available")

type State struct {
	APIKey string `json:"api_key"`
}

func (s State) Configured() bool { return strings.TrimSpace(s.APIKey) != "" }

type Store struct {
	dir   string
	mu    sync.Mutex
	state State
	vault *vault.Session
	user  string
}

func Open(dir string) *Store { return &Store{dir: strings.TrimSpace(dir)} }

func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vault = sess
	s.user = user
	s.loadLocked()
}

func (s *Store) Persistent() bool { return s != nil && s.dir != "" }

func (s *Store) Secure() bool {
	if s == nil {
		return false
	}
	if !s.Persistent() {
		return true
	}
	return s.vault != nil && s.vault.Encrypting()
}

func (s *Store) View() State {
	if s == nil {
		return State{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Store) Replace(apiKey string) error {
	if s == nil {
		return errors.New("MachineMail settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = State{APIKey: strings.TrimSpace(apiKey)}
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *Store) Clear() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("MachineMail settings: remove: %w", err)
		}
	}
	s.state = State{}
	return nil
}

func (s *Store) ad() vault.AD {
	return vault.AD{User: s.user, Store: storeSettings, Schema: schemaSettings1}
}

func (s *Store) loadLocked() {
	s.state = State{}
	if s.dir == "" || s.vault == nil || !s.vault.Encrypting() {
		return
	}
	b, err := os.ReadFile(s.path())
	if err != nil || !vault.IsVault(b) {
		return
	}
	uv := s.vault.UserVault()
	if uv == nil {
		return
	}
	plain, err := uv.OpenFile(s.ad(), b)
	if err != nil {
		return
	}
	var state State
	if json.Unmarshal(plain, &state) == nil {
		s.state = state
	}
}

func (s *Store) persistLocked() error {
	if s.dir == "" {
		return nil
	}
	if s.vault == nil || !s.vault.Encrypting() {
		return ErrEncryptionRequired
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("MachineMail settings: create directory: %w", err)
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return fmt.Errorf("MachineMail settings: encode: %w", err)
	}
	b, err = s.vault.MaybeSealFile(s.ad(), b)
	if err != nil {
		return fmt.Errorf("MachineMail settings: encrypt: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("MachineMail settings: write: %w", err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("MachineMail settings: replace: %w", err)
	}
	return nil
}

func (s *Store) path() string { return filepath.Join(s.dir, settingsFile) }

func Dir(override, cortexRoot string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	if root := strings.TrimSpace(cortexRoot); root != "" {
		return filepath.Join(filepath.Dir(root), "machinemail")
	}
	return ""
}
