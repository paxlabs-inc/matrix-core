// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package cloudchannels

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"centra/packages/vault"
)

var ErrEncryptionRequired = errors.New("encrypted cloud channel storage is required")

const (
	stateFile   = "cloud-channels.vault"
	stateStore  = "neo.cloud.channels"
	stateSchema = "cloud.channels.v1"
)

type SlackConfig struct {
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode"`
	BotToken      string `json:"bot_token"`
	AppToken      string `json:"app_token,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
	TeamID        string `json:"team_id,omitempty"`
	BotUserID     string `json:"bot_user_id,omitempty"`
	GroupTrigger  string `json:"group_trigger"`
}

func (c SlackConfig) Configured() bool {
	if !strings.HasPrefix(c.BotToken, "xoxb-") {
		return false
	}
	switch c.Mode {
	case "events":
		return len(c.SigningSecret) >= 16
	case "socket":
		return strings.HasPrefix(c.AppToken, "xapp-")
	default:
		return false
	}
}

type DiscordConfig struct {
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"bot_token"`
	ApplicationID string `json:"application_id"`
	PublicKey     string `json:"public_key"`
	BotUserID     string `json:"bot_user_id,omitempty"`
	Gateway       bool   `json:"gateway"`
	GroupTrigger  string `json:"group_trigger"`
	SessionID     string `json:"session_id,omitempty"`
	ResumeURL     string `json:"resume_url,omitempty"`
	Sequence      int64  `json:"sequence,omitempty"`
}

func (c DiscordConfig) Configured() bool {
	return strings.TrimSpace(c.BotToken) != "" && strings.TrimSpace(c.ApplicationID) != "" && len(strings.TrimSpace(c.PublicKey)) == 64
}

type State struct {
	Slack    SlackConfig    `json:"slack"`
	Discord  DiscordConfig  `json:"discord"`
	Lark     LarkConfig     `json:"lark"`
	DingTalk DingTalkConfig `json:"dingtalk"`
	WeComBot WeComBotConfig `json:"wecom_bot"`
	WeComApp WeComAppConfig `json:"wecom_app"`
	QQ       QQConfig       `json:"qq"`
	Weixin   WeixinConfig   `json:"weixin"`
	WeChatMP WeChatMPConfig `json:"wechat_official"`
	WeChatKF WeChatKFConfig `json:"wechat_customer_service"`
}

type Store struct {
	mu    sync.Mutex
	root  string
	vault *vault.Session
	user  string
	state State
}

func Open(root string, session *vault.Session, user string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("cloud channel root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, err
	}
	store := &Store{root: abs, vault: session, user: strings.TrimSpace(user)}
	if err := store.loadLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) View() State {
	if s == nil {
		return State{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func cloneState(state State) State {
	if state.Weixin.Contexts != nil {
		contexts := make(map[string]WeixinContext, len(state.Weixin.Contexts))
		for key, value := range state.Weixin.Contexts {
			contexts[key] = value
		}
		state.Weixin.Contexts = contexts
	}
	if state.WeChatKF.Cursors != nil {
		cursors := make(map[string]string, len(state.WeChatKF.Cursors))
		for key, value := range state.WeChatKF.Cursors {
			cursors[key] = value
		}
		state.WeChatKF.Cursors = cursors
	}
	return state
}

func (s *Store) ReplaceSlack(config SlackConfig) error {
	if s == nil {
		return errors.New("cloud channel store is unavailable")
	}
	normalizeSlack(&config)
	if !config.Configured() {
		return errors.New("Slack configuration is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.Slack
	s.state.Slack = config
	if err := s.persistLocked(); err != nil {
		s.state.Slack = previous
		return err
	}
	return nil
}

func (s *Store) ReplaceDiscord(config DiscordConfig) error {
	if s == nil {
		return errors.New("cloud channel store is unavailable")
	}
	normalizeDiscord(&config)
	if !config.Configured() {
		return errors.New("Discord configuration is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.Discord
	s.state.Discord = config
	if err := s.persistLocked(); err != nil {
		s.state.Discord = previous
		return err
	}
	return nil
}

func (s *Store) UpdateDiscordSession(sessionID, resumeURL string, sequence int64) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.Discord
	s.state.Discord.SessionID = strings.TrimSpace(sessionID)
	s.state.Discord.ResumeURL = strings.TrimSpace(resumeURL)
	s.state.Discord.Sequence = sequence
	if err := s.persistLocked(); err != nil {
		s.state.Discord = previous
		return err
	}
	return nil
}

func (s *Store) ClearSlack() error {
	return s.clear(func(state *State) { state.Slack = SlackConfig{} })
}

func (s *Store) ClearDiscord() error {
	return s.clear(func(state *State) { state.Discord = DiscordConfig{} })
}

func (s *Store) clear(apply func(*State)) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	apply(&s.state)
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *Store) loadLocked() error {
	data, err := os.ReadFile(filepath.Join(s.root, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.vault == nil || !s.vault.Encrypting() || s.vault.UserVault() == nil {
		return ErrEncryptionRequired
	}
	plain, err := s.vault.UserVault().OpenFile(s.ad(), data)
	if err != nil {
		return fmt.Errorf("cloud channel state decrypt: %w", err)
	}
	if err := json.Unmarshal(plain, &s.state); err != nil {
		return fmt.Errorf("cloud channel state decode: %w", err)
	}
	return nil
}

func (s *Store) persistLocked() error {
	if s.vault == nil || !s.vault.Encrypting() {
		return ErrEncryptionRequired
	}
	plain, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	sealed, err := s.vault.MaybeSealFile(s.ad(), plain)
	for index := range plain {
		plain[index] = 0
	}
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, stateFile)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, sealed, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

func (s *Store) ad() vault.AD {
	return vault.AD{User: s.user, Store: stateStore, Stream: "configuration", Schema: stateSchema}
}

func normalizeSlack(config *SlackConfig) {
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.BotToken = strings.TrimSpace(config.BotToken)
	config.AppToken = strings.TrimSpace(config.AppToken)
	config.SigningSecret = strings.TrimSpace(config.SigningSecret)
	config.TeamID = strings.TrimSpace(config.TeamID)
	config.BotUserID = strings.TrimSpace(config.BotUserID)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeDiscord(config *DiscordConfig) {
	config.BotToken = strings.TrimSpace(config.BotToken)
	config.ApplicationID = strings.TrimSpace(config.ApplicationID)
	config.PublicKey = strings.ToLower(strings.TrimSpace(config.PublicKey))
	config.BotUserID = strings.TrimSpace(config.BotUserID)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeTrigger(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "all", "mention_only", "mention_or_reply":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "mention_or_reply"
	}
}
