// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package cloudchannels

import (
	"errors"
	"net/url"
	"strings"
)

type LarkConfig struct {
	Enabled           bool   `json:"enabled"`
	Region            string `json:"region"`
	Mode              string `json:"mode"`
	AppID             string `json:"app_id"`
	AppSecret         string `json:"app_secret"`
	VerificationToken string `json:"verification_token"`
	EncryptKey        string `json:"encrypt_key,omitempty"`
	BotOpenID         string `json:"bot_open_id,omitempty"`
	GroupTrigger      string `json:"group_trigger"`
}

func (c LarkConfig) Configured() bool {
	return strings.TrimSpace(c.AppID) != "" && strings.TrimSpace(c.AppSecret) != "" && len(strings.TrimSpace(c.VerificationToken)) >= 8 && (c.Mode == "webhook" || c.Mode == "websocket")
}

type DingTalkConfig struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RobotCode      string `json:"robot_code"`
	CallbackSecret string `json:"callback_secret,omitempty"`
	GroupTrigger   string `json:"group_trigger"`
}

func (c DingTalkConfig) Configured() bool {
	return strings.TrimSpace(c.ClientID) != "" && strings.TrimSpace(c.ClientSecret) != "" && strings.TrimSpace(c.RobotCode) != "" && c.Mode == "stream"
}

type WeComBotConfig struct {
	Enabled        bool   `json:"enabled"`
	Mode           string `json:"mode"`
	BotID          string `json:"bot_id"`
	Secret         string `json:"secret,omitempty"`
	Token          string `json:"token,omitempty"`
	EncodingAESKey string `json:"encoding_aes_key,omitempty"`
	ReceiveID      string `json:"receive_id,omitempty"`
	GroupTrigger   string `json:"group_trigger"`
}

func (c WeComBotConfig) Configured() bool {
	if strings.TrimSpace(c.BotID) == "" {
		return false
	}
	if c.Mode == "websocket" {
		return strings.TrimSpace(c.Secret) != ""
	}
	return c.Mode == "callback" && len(strings.TrimSpace(c.Token)) >= 8 && len(strings.TrimSpace(c.EncodingAESKey)) == 43
}

type WeComAppConfig struct {
	Enabled        bool   `json:"enabled"`
	CorpID         string `json:"corp_id"`
	AgentID        int64  `json:"agent_id"`
	Secret         string `json:"secret"`
	Token          string `json:"token"`
	EncodingAESKey string `json:"encoding_aes_key"`
	GroupTrigger   string `json:"group_trigger"`
}

type QQConfig struct {
	Enabled      bool   `json:"enabled"`
	AppID        string `json:"app_id"`
	ClientSecret string `json:"client_secret"`
	BotID        string `json:"bot_id,omitempty"`
	GroupTrigger string `json:"group_trigger"`
	SessionID    string `json:"session_id,omitempty"`
	ResumeURL    string `json:"resume_url,omitempty"`
	Sequence     int64  `json:"sequence,omitempty"`
}

func (c QQConfig) Configured() bool {
	return strings.TrimSpace(c.AppID) != "" && strings.TrimSpace(c.ClientSecret) != ""
}

type WeixinContext struct {
	Token     string `json:"token"`
	UpdatedAt int64  `json:"updated_at"`
}

type WeixinConfig struct {
	Enabled       bool                     `json:"enabled"`
	Token         string                   `json:"token"`
	BotID         string                   `json:"bot_id"`
	BaseURL       string                   `json:"base_url,omitempty"`
	CDNBaseURL    string                   `json:"cdn_base_url,omitempty"`
	UpdatesCursor string                   `json:"updates_cursor,omitempty"`
	Contexts      map[string]WeixinContext `json:"contexts,omitempty"`
}

func (c WeixinConfig) Configured() bool {
	return strings.TrimSpace(c.Token) != "" && strings.TrimSpace(c.BotID) != "" && validWeixinEndpoint(c.BaseURL) && validWeixinEndpoint(c.CDNBaseURL)
}

func validWeixinEndpoint(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "weixin.qq.com" || strings.HasSuffix(host, ".weixin.qq.com")
}

type WeChatMPConfig struct {
	Enabled        bool   `json:"enabled"`
	AppID          string `json:"app_id"`
	AppSecret      string `json:"app_secret"`
	Token          string `json:"token"`
	EncodingAESKey string `json:"encoding_aes_key,omitempty"`
}

func (c WeChatMPConfig) Configured() bool {
	return strings.TrimSpace(c.AppID) != "" && strings.TrimSpace(c.AppSecret) != "" && len(strings.TrimSpace(c.Token)) >= 8 && (strings.TrimSpace(c.EncodingAESKey) == "" || len(strings.TrimSpace(c.EncodingAESKey)) == 43)
}

type WeChatKFConfig struct {
	Enabled        bool              `json:"enabled"`
	CorpID         string            `json:"corp_id"`
	Secret         string            `json:"secret"`
	Token          string            `json:"token"`
	EncodingAESKey string            `json:"encoding_aes_key"`
	Cursors        map[string]string `json:"cursors,omitempty"`
}

func (c WeChatKFConfig) Configured() bool {
	return strings.TrimSpace(c.CorpID) != "" && strings.TrimSpace(c.Secret) != "" && len(strings.TrimSpace(c.Token)) >= 8 && len(strings.TrimSpace(c.EncodingAESKey)) == 43
}

func (c WeComAppConfig) Configured() bool {
	return strings.TrimSpace(c.CorpID) != "" && c.AgentID > 0 && strings.TrimSpace(c.Secret) != "" && len(strings.TrimSpace(c.Token)) >= 8 && len(strings.TrimSpace(c.EncodingAESKey)) == 43
}

func (s *Store) ReplaceLark(config LarkConfig) error {
	normalizeLark(&config)
	if !config.Configured() {
		return errors.New("Lark configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.Lark = config })
}

func (s *Store) ReplaceDingTalk(config DingTalkConfig) error {
	normalizeDingTalk(&config)
	if !config.Configured() {
		return errors.New("DingTalk configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.DingTalk = config })
}

func (s *Store) ReplaceWeComBot(config WeComBotConfig) error {
	normalizeWeComBot(&config)
	if !config.Configured() {
		return errors.New("WeCom Bot configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.WeComBot = config })
}

func (s *Store) ReplaceWeComApp(config WeComAppConfig) error {
	normalizeWeComApp(&config)
	if !config.Configured() {
		return errors.New("WeCom App configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.WeComApp = config })
}

func (s *Store) ReplaceQQ(config QQConfig) error {
	normalizeQQ(&config)
	if !config.Configured() {
		return errors.New("QQ configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.QQ = config })
}

func (s *Store) ReplaceWeixin(config WeixinConfig) error {
	normalizeWeixin(&config)
	if !config.Configured() {
		return errors.New("Weixin configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.Weixin = config })
}

func (s *Store) ReplaceWeChatMP(config WeChatMPConfig) error {
	normalizeWeChatMP(&config)
	if !config.Configured() {
		return errors.New("WeChat Official Account configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.WeChatMP = config })
}

func (s *Store) ReplaceWeChatKF(config WeChatKFConfig) error {
	normalizeWeChatKF(&config)
	if !config.Configured() {
		return errors.New("WeChat Customer Service configuration is incomplete")
	}
	return s.replaceEnterprise(func(state *State) { state.WeChatKF = config })
}

func (s *Store) ClearLark() error { return s.clear(func(state *State) { state.Lark = LarkConfig{} }) }
func (s *Store) ClearDingTalk() error {
	return s.clear(func(state *State) { state.DingTalk = DingTalkConfig{} })
}
func (s *Store) ClearWeComBot() error {
	return s.clear(func(state *State) { state.WeComBot = WeComBotConfig{} })
}
func (s *Store) ClearWeComApp() error {
	return s.clear(func(state *State) { state.WeComApp = WeComAppConfig{} })
}
func (s *Store) ClearQQ() error { return s.clear(func(state *State) { state.QQ = QQConfig{} }) }
func (s *Store) ClearWeixin() error {
	return s.clear(func(state *State) { state.Weixin = WeixinConfig{} })
}
func (s *Store) ClearWeChatMP() error {
	return s.clear(func(state *State) { state.WeChatMP = WeChatMPConfig{} })
}
func (s *Store) ClearWeChatKF() error {
	return s.clear(func(state *State) { state.WeChatKF = WeChatKFConfig{} })
}

func (s *Store) UpdateQQSession(sessionID, resumeURL string, sequence int64) error {
	return s.replaceEnterprise(func(state *State) {
		state.QQ.SessionID, state.QQ.ResumeURL, state.QQ.Sequence = strings.TrimSpace(sessionID), strings.TrimSpace(resumeURL), sequence
	})
}

func (s *Store) UpdateWeixinProgress(cursor, user, token string, updatedAt int64) error {
	return s.replaceEnterprise(func(state *State) {
		if cursor != "" {
			state.Weixin.UpdatesCursor = cursor
		}
		if user != "" && token != "" {
			if state.Weixin.Contexts == nil {
				state.Weixin.Contexts = map[string]WeixinContext{}
			}
			state.Weixin.Contexts[user] = WeixinContext{Token: token, UpdatedAt: updatedAt}
		}
	})
}

func (s *Store) InvalidateWeixinContext(user string) error {
	return s.replaceEnterprise(func(state *State) { delete(state.Weixin.Contexts, strings.TrimSpace(user)) })
}

func (s *Store) UpdateWeChatKFCursor(openKFID, cursor string) error {
	return s.replaceEnterprise(func(state *State) {
		if state.WeChatKF.Cursors == nil {
			state.WeChatKF.Cursors = map[string]string{}
		}
		state.WeChatKF.Cursors[strings.TrimSpace(openKFID)] = strings.TrimSpace(cursor)
	})
}

func (s *Store) replaceEnterprise(apply func(*State)) error {
	if s == nil {
		return errors.New("cloud channel store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	candidate := cloneState(s.state)
	apply(&candidate)
	s.state = candidate
	if err := s.persistLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func normalizeLark(config *LarkConfig) {
	config.Region = strings.ToLower(strings.TrimSpace(config.Region))
	if config.Region != "feishu" && config.Region != "lark" {
		config.Region = "lark"
	}
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.AppID = strings.TrimSpace(config.AppID)
	config.AppSecret = strings.TrimSpace(config.AppSecret)
	config.VerificationToken = strings.TrimSpace(config.VerificationToken)
	config.EncryptKey = strings.TrimSpace(config.EncryptKey)
	config.BotOpenID = strings.TrimSpace(config.BotOpenID)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeDingTalk(config *DingTalkConfig) {
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.ClientID = strings.TrimSpace(config.ClientID)
	config.ClientSecret = strings.TrimSpace(config.ClientSecret)
	config.RobotCode = strings.TrimSpace(config.RobotCode)
	config.CallbackSecret = strings.TrimSpace(config.CallbackSecret)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeWeComBot(config *WeComBotConfig) {
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	config.BotID = strings.TrimSpace(config.BotID)
	config.Secret = strings.TrimSpace(config.Secret)
	config.Token = strings.TrimSpace(config.Token)
	config.EncodingAESKey = strings.TrimSpace(config.EncodingAESKey)
	config.ReceiveID = strings.TrimSpace(config.ReceiveID)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeWeComApp(config *WeComAppConfig) {
	config.CorpID = strings.TrimSpace(config.CorpID)
	config.Secret = strings.TrimSpace(config.Secret)
	config.Token = strings.TrimSpace(config.Token)
	config.EncodingAESKey = strings.TrimSpace(config.EncodingAESKey)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}

func normalizeQQ(config *QQConfig) {
	config.AppID = strings.TrimSpace(config.AppID)
	config.ClientSecret = strings.TrimSpace(config.ClientSecret)
	config.BotID = strings.TrimSpace(config.BotID)
	config.GroupTrigger = normalizeTrigger(config.GroupTrigger)
}
func normalizeWeixin(config *WeixinConfig) {
	config.Token = strings.TrimSpace(config.Token)
	config.BotID = strings.TrimSpace(config.BotID)
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.CDNBaseURL = strings.TrimRight(strings.TrimSpace(config.CDNBaseURL), "/")
	if config.Contexts == nil {
		config.Contexts = map[string]WeixinContext{}
	} else {
		contexts := make(map[string]WeixinContext, len(config.Contexts))
		for key, value := range config.Contexts {
			contexts[key] = value
		}
		config.Contexts = contexts
	}
}
func normalizeWeChatMP(config *WeChatMPConfig) {
	config.AppID = strings.TrimSpace(config.AppID)
	config.AppSecret = strings.TrimSpace(config.AppSecret)
	config.Token = strings.TrimSpace(config.Token)
	config.EncodingAESKey = strings.TrimSpace(config.EncodingAESKey)
}
func normalizeWeChatKF(config *WeChatKFConfig) {
	config.CorpID = strings.TrimSpace(config.CorpID)
	config.Secret = strings.TrimSpace(config.Secret)
	config.Token = strings.TrimSpace(config.Token)
	config.EncodingAESKey = strings.TrimSpace(config.EncodingAESKey)
	if config.Cursors == nil {
		config.Cursors = map[string]string{}
	} else {
		cursors := make(map[string]string, len(config.Cursors))
		for key, value := range config.Cursors {
			cursors[key] = value
		}
		config.Cursors = cursors
	}
}
