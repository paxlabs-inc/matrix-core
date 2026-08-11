// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package telegramsettings

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

const settingsFile = "telegram.settings.json"

const (
	storeSettings   = "neo.telegram.settings"
	schemaSettings1 = "telegram.settings.v1"
)

var ErrEncryptionRequired = errors.New("the encrypted user vault is not available")

type State struct {
	Token            string `json:"token"`
	BotID            int64  `json:"bot_id"`
	BotUsername      string `json:"bot_username"`
	PairSecret       string `json:"pair_secret,omitempty"`
	ChatID           int64  `json:"chat_id,omitempty"`
	TelegramUserID   int64  `json:"telegram_user_id,omitempty"`
	TelegramUsername string `json:"telegram_username,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	LastUpdateID     int64  `json:"last_update_id,omitempty"`
}

func (s State) Configured() bool { return s.Token != "" && s.BotID != 0 && s.BotUsername != "" }
func (s State) Paired() bool     { return s.Configured() && s.ChatID != 0 && s.TelegramUserID != 0 }

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

func (s *Store) Replace(next State) error {
	if s == nil {
		return errors.New("telegram settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	s.state = next
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *Store) Pair(secret string, chatID, userID int64, username, conversationID string) (bool, error) {
	if s == nil {
		return false, errors.New("telegram settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if secret == "" || s.state.PairSecret == "" || secret != s.state.PairSecret || s.state.Paired() {
		return false, nil
	}
	previous := s.state
	s.state.PairSecret = ""
	s.state.ChatID = chatID
	s.state.TelegramUserID = userID
	s.state.TelegramUsername = strings.TrimPrefix(strings.TrimSpace(username), "@")
	s.state.ConversationID = strings.TrimSpace(conversationID)
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return false, err
	}
	return true, nil
}

func (s *Store) SetLastUpdateID(updateID int64) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if updateID <= s.state.LastUpdateID {
		return nil
	}
	previous := s.state.LastUpdateID
	s.state.LastUpdateID = updateID
	if err := s.persistLocked(); err != nil {
		s.state.LastUpdateID = previous
		return err
	}
	return nil
}

func (s *Store) RotateConversation(conversationID string) error {
	if s == nil {
		return errors.New("telegram settings store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.ConversationID
	s.state.ConversationID = strings.TrimSpace(conversationID)
	if err := s.persistLocked(); err != nil {
		s.state.ConversationID = previous
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
			return fmt.Errorf("telegram settings: remove: %w", err)
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
		return fmt.Errorf("telegram settings: create directory: %w", err)
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return fmt.Errorf("telegram settings: encode: %w", err)
	}
	b, err = s.vault.MaybeSealFile(s.ad(), b)
	if err != nil {
		return fmt.Errorf("telegram settings: encrypt: %w", err)
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("telegram settings: write: %w", err)
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("telegram settings: replace: %w", err)
	}
	return nil
}

func (s *Store) path() string { return filepath.Join(s.dir, settingsFile) }

func Dir(override, cortexRoot string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	if root := strings.TrimSpace(cortexRoot); root != "" {
		return filepath.Join(filepath.Dir(root), "telegram")
	}
	return ""
}
