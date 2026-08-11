// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/cloudchannels"
)

type slackConfigureRequest struct {
	Mode          string `json:"mode"`
	BotToken      string `json:"bot_token"`
	AppToken      string `json:"app_token,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
	GroupTrigger  string `json:"group_trigger,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

type larkConfigureRequest struct {
	Region            string `json:"region"`
	Mode              string `json:"mode"`
	AppID             string `json:"app_id"`
	AppSecret         string `json:"app_secret"`
	VerificationToken string `json:"verification_token"`
	EncryptKey        string `json:"encrypt_key,omitempty"`
	GroupTrigger      string `json:"group_trigger,omitempty"`
	Enabled           *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleLark(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lark is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.LarkStatus())
	case http.MethodPut:
		var request larkConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureLark(r.Context(), cloudchannels.LarkConfig{Enabled: enabled, Region: request.Region, Mode: request.Mode, AppID: request.AppID, AppSecret: request.AppSecret, VerificationToken: request.VerificationToken, EncryptKey: request.EncryptKey, GroupTrigger: request.GroupTrigger})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.AppSecret, request.VerificationToken, request.EncryptKey)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectLark(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.LarkStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleLarkEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lark is unavailable"})
		return
	}
	config := bridge.store.View().Lark
	if !config.Enabled || config.Mode != "webhook" || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lark webhook mode is not enabled"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Lark event body is invalid"})
		return
	}
	payload, err := cloudchannels.DecodeLarkPayload(body, config.EncryptKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Lark event encryption is invalid"})
		return
	}
	if payload.Challenge != "" {
		if payload.Token != config.VerificationToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Lark verification token is invalid"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"challenge": payload.Challenge})
		return
	}
	if payload.Header.AppID != config.AppID || payload.Header.Token != config.VerificationToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Lark event identity is invalid"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go func() { _ = bridge.handleLarkPayload(bridge.context(), payload) }()
}

func (s *Server) handleLarkCards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lark is unavailable"})
		return
	}
	config := bridge.store.View().Lark
	if !config.Enabled || config.Mode != "webhook" || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lark webhook mode is not enabled"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Lark card body is invalid"})
		return
	}
	payload, err := cloudchannels.DecodeLarkCardAction(body, config.EncryptKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Lark card encryption is invalid"})
		return
	}
	response, err := bridge.handleLarkCardAction(r.Context(), payload)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type dingTalkConfigureRequest struct {
	Mode           string `json:"mode"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	RobotCode      string `json:"robot_code"`
	CallbackSecret string `json:"callback_secret,omitempty"`
	GroupTrigger   string `json:"group_trigger,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleDingTalk(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "DingTalk is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.DingTalkStatus())
	case http.MethodPut:
		var request dingTalkConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureDingTalk(r.Context(), cloudchannels.DingTalkConfig{Enabled: enabled, Mode: request.Mode, ClientID: request.ClientID, ClientSecret: request.ClientSecret, RobotCode: request.RobotCode, CallbackSecret: request.CallbackSecret, GroupTrigger: request.GroupTrigger})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.ClientSecret, request.CallbackSecret)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectDingTalk(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.DingTalkStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

type weComBotConfigureRequest struct {
	Mode           string `json:"mode"`
	BotID          string `json:"bot_id"`
	Secret         string `json:"secret,omitempty"`
	Token          string `json:"token,omitempty"`
	EncodingAESKey string `json:"encoding_aes_key,omitempty"`
	ReceiveID      string `json:"receive_id,omitempty"`
	GroupTrigger   string `json:"group_trigger,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleWeComBot(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom Bot is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.WeComBotStatus())
	case http.MethodPut:
		var request weComBotConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureWeComBot(r.Context(), cloudchannels.WeComBotConfig{Enabled: enabled, Mode: request.Mode, BotID: request.BotID, Secret: request.Secret, Token: request.Token, EncodingAESKey: request.EncodingAESKey, ReceiveID: request.ReceiveID, GroupTrigger: request.GroupTrigger})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.Secret, request.Token, request.EncodingAESKey)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectWeComBot(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.WeComBotStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleWeComBotCallback(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom Bot is unavailable"})
		return
	}
	config := bridge.store.View().WeComBot
	if !config.Enabled || config.Mode != "callback" || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom Bot callback mode is not enabled"})
		return
	}
	crypt, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.ReceiveID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom Bot callback crypto is unavailable"})
		return
	}
	signature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	if r.Method == http.MethodGet {
		echo := r.URL.Query().Get("echostr")
		if err := crypt.Verify(signature, timestamp, nonce, echo); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		plain, err := crypt.Decrypt(echo)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(plain)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom Bot callback body is invalid"})
		return
	}
	var wrapper cloudchannels.WeComEncryptedJSON
	if json.Unmarshal(body, &wrapper) != nil || wrapper.Encrypt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom Bot callback envelope is invalid"})
		return
	}
	if err := crypt.Verify(signature, timestamp, nonce, wrapper.Encrypt); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	plain, err := crypt.Decrypt(wrapper.Encrypt)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var message cloudchannels.WeComBotMessage
	if json.Unmarshal(plain, &message) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom Bot message is invalid"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))
	go func() { _ = bridge.handleWeComBotMessage(bridge.context(), message) }()
}

type weComAppConfigureRequest struct {
	CorpID         string `json:"corp_id"`
	AgentID        int64  `json:"agent_id"`
	Secret         string `json:"secret"`
	Token          string `json:"token"`
	EncodingAESKey string `json:"encoding_aes_key"`
	GroupTrigger   string `json:"group_trigger,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleWeComApp(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom App is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.WeComAppStatus())
	case http.MethodPut:
		var request weComAppConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureWeComApp(r.Context(), cloudchannels.WeComAppConfig{Enabled: enabled, CorpID: request.CorpID, AgentID: request.AgentID, Secret: request.Secret, Token: request.Token, EncodingAESKey: request.EncodingAESKey, GroupTrigger: request.GroupTrigger})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.Secret, request.Token, request.EncodingAESKey)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectWeComApp(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.WeComAppStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleWeComAppCallback(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom App is unavailable"})
		return
	}
	config := bridge.store.View().WeComApp
	if !config.Enabled || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom App is not enabled"})
		return
	}
	crypt, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.CorpID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeCom App callback crypto is unavailable"})
		return
	}
	signature := r.URL.Query().Get("msg_signature")
	timestamp := r.URL.Query().Get("timestamp")
	nonce := r.URL.Query().Get("nonce")
	if r.Method == http.MethodGet {
		echo := r.URL.Query().Get("echostr")
		if crypt.Verify(signature, timestamp, nonce, echo) != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeCom App callback signature is invalid"})
			return
		}
		plain, err := crypt.Decrypt(echo)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(plain)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom App callback body is invalid"})
		return
	}
	var wrapper cloudchannels.WeComEncryptedXML
	if xmlErr := xml.Unmarshal(body, &wrapper); xmlErr != nil || wrapper.Encrypt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom App callback envelope is invalid"})
		return
	}
	if crypt.Verify(signature, timestamp, nonce, wrapper.Encrypt) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeCom App callback signature is invalid"})
		return
	}
	plain, err := crypt.Decrypt(wrapper.Encrypt)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var message cloudchannels.WeComAppMessage
	if xml.Unmarshal(plain, &message) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeCom App message is invalid"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))
	go func() { _ = bridge.handleWeComAppMessage(bridge.context(), message) }()
}

func cloudConfigErrorStatus(err error) int {
	if errors.Is(err, cloudchannels.ErrEncryptionRequired) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func (s *Server) handleSlack(w http.ResponseWriter, r *http.Request) {
	if s.engine.cloudChannels == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Slack is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.cloudChannels.SlackStatus())
	case http.MethodPut:
		var request slackConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := s.engine.cloudChannels.ConfigureSlack(r.Context(), cloudchannels.SlackConfig{
			Enabled: enabled, Mode: request.Mode, BotToken: request.BotToken, AppToken: request.AppToken,
			SigningSecret: request.SigningSecret, GroupTrigger: request.GroupTrigger,
		})
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, cloudchannels.ErrEncryptionRequired) {
				code = http.StatusServiceUnavailable
			}
			writeJSON(w, code, map[string]string{"error": safeCloudError(err.Error(), request.BotToken, request.AppToken, request.SigningSecret)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := s.engine.cloudChannels.DisconnectSlack(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.engine.cloudChannels.SlackStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Slack is unavailable"})
		return
	}
	config := bridge.store.View().Slack
	if !config.Enabled || config.Mode != "events" || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Slack Events API is not enabled"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Slack event body is invalid"})
		return
	}
	if err := cloudchannels.VerifySlackRequest(r.Header, body, config.SigningSecret, time.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var payload cloudchannels.SlackEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Slack event JSON is invalid"})
		return
	}
	if payload.Type == "url_verification" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": payload.Challenge})
		return
	}
	if payload.Type != "event_callback" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported Slack event type"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go func() { _ = bridge.handleSlackPayload(bridge.context(), payload) }()
}

type slackActionPayload = cloudchannels.SlackActionPayload

func (s *Server) handleSlackActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Slack is unavailable"})
		return
	}
	config := bridge.store.View().Slack
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Slack action body is invalid"})
		return
	}
	if err := cloudchannels.VerifySlackRequest(r.Header, body, config.SigningSecret, time.Now()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Slack action form is invalid"})
		return
	}
	var payload slackActionPayload
	if err := json.Unmarshal([]byte(values.Get("payload")), &payload); err != nil || payload.Team.ID != config.TeamID || len(payload.Actions) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Slack action identity is invalid"})
		return
	}
	action := payload.Actions[0].ActionID
	answerErr := bridge.handleSlackAction(r.Context(), payload)
	message := "That approval is no longer active."
	if answerErr == nil && action == "neo_gate_approve" {
		message = "Approved. Neo is continuing."
	} else if answerErr == nil {
		message = "Denied. Neo is continuing."
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": message})
}

type discordConfigureRequest struct {
	BotToken      string `json:"bot_token"`
	ApplicationID string `json:"application_id"`
	PublicKey     string `json:"public_key"`
	Gateway       bool   `json:"gateway"`
	GroupTrigger  string `json:"group_trigger,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleDiscord(w http.ResponseWriter, r *http.Request) {
	if s.engine.cloudChannels == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Discord is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.cloudChannels.DiscordStatus())
	case http.MethodPut:
		var request discordConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := s.engine.cloudChannels.ConfigureDiscord(r.Context(), cloudchannels.DiscordConfig{
			Enabled: enabled, BotToken: request.BotToken, ApplicationID: request.ApplicationID,
			PublicKey: request.PublicKey, Gateway: request.Gateway, GroupTrigger: request.GroupTrigger,
		})
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, cloudchannels.ErrEncryptionRequired) {
				code = http.StatusServiceUnavailable
			}
			writeJSON(w, code, map[string]string{"error": safeCloudError(err.Error(), request.BotToken)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := s.engine.cloudChannels.DisconnectDiscord(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.engine.cloudChannels.DiscordStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

type discordInteraction struct {
	ID            string                        `json:"id"`
	ApplicationID string                        `json:"application_id"`
	Type          int                           `json:"type"`
	ChannelID     string                        `json:"channel_id"`
	GuildID       string                        `json:"guild_id,omitempty"`
	User          cloudchannels.DiscordIdentity `json:"user"`
	Member        struct {
		User cloudchannels.DiscordIdentity `json:"user"`
	} `json:"member"`
	Data struct {
		Name     string `json:"name"`
		CustomID string `json:"custom_id"`
		Options  []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"options"`
	} `json:"data"`
}

func (s *Server) handleDiscordInteractions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Discord is unavailable"})
		return
	}
	config := bridge.store.View().Discord
	if !config.Enabled || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Discord is not enabled"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Discord interaction body is invalid"})
		return
	}
	if err := cloudchannels.VerifyDiscordRequest(r.Header, body, config.PublicKey); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var interaction discordInteraction
	if err := json.Unmarshal(body, &interaction); err != nil || interaction.ApplicationID != config.ApplicationID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Discord interaction identity is invalid"})
		return
	}
	if interaction.Type == 1 {
		writeJSON(w, http.StatusOK, map[string]int{"type": 1})
		return
	}
	if interaction.Type == 3 {
		user := interaction.User
		if interaction.Member.User.ID != "" {
			user = interaction.Member.User
		}
		scope := channelgateway.ScopeDirect
		if interaction.GuildID != "" {
			scope = channelgateway.ScopeGroup
		}
		address := channelgateway.Address{Channel: channelgateway.ChannelDiscord, AccountID: config.ApplicationID, ConversationID: interaction.ChannelID, ParticipantID: user.ID, Scope: scope}
		answered := bridge.answerApproval(r.Context(), address, interaction.Data.CustomID)
		envelope := channelgateway.Envelope{
			Direction: channelgateway.Inbound, Kind: channelgateway.KindApproval, Address: address,
			ExternalEventID: interaction.ID, ExternalMessageID: interaction.ID, IdempotencyKey: "discord:action:" + interaction.ID,
			Approval: &channelgateway.Approval{ID: interaction.ID, Prompt: "Neo approval", Decision: interaction.Data.CustomID}, OccurredAt: time.Now().UTC(),
		}
		if s.engine.channelGateway != nil {
			claim, claimErr := s.engine.channelGateway.ClaimInbound(r.Context(), envelope)
			if claimErr == nil && claim.State != channelgateway.ClaimDuplicate {
				_ = s.engine.channelGateway.CompleteInbound(r.Context(), envelope, "", "")
			}
		}
		message := "That approval is no longer active."
		if answered && interaction.Data.CustomID == "neo_gate_approve" {
			message = "Approved. Neo is continuing."
		} else if answered {
			message = "Denied. Neo is continuing."
		}
		writeJSON(w, http.StatusOK, cloudchannels.DiscordInteractionResponse(4, message))
		return
	}
	if interaction.Type != 2 || interaction.Data.Name != "neo" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported Discord interaction"})
		return
	}
	message := ""
	for _, option := range interaction.Data.Options {
		if option.Name == "message" {
			message, _ = option.Value.(string)
		}
	}
	message = strings.TrimSpace(message)
	if message == "" {
		writeJSON(w, http.StatusOK, cloudchannels.DiscordInteractionResponse(4, "A message is required."))
		return
	}
	user := interaction.User
	if interaction.Member.User.ID != "" {
		user = interaction.Member.User
	}
	scope := channelgateway.ScopeDirect
	if interaction.GuildID != "" {
		scope = channelgateway.ScopeGroup
	}
	envelope := channelgateway.Envelope{
		Direction: channelgateway.Inbound, Kind: channelgateway.KindMessage,
		Address:         channelgateway.Address{Channel: channelgateway.ChannelDiscord, AccountID: config.ApplicationID, ConversationID: interaction.ChannelID, ParticipantID: user.ID, Scope: scope},
		ExternalEventID: interaction.ID, ExternalMessageID: interaction.ID, IdempotencyKey: "discord:interaction:" + interaction.ID,
		Text: message, OccurredAt: time.Now().UTC(), Metadata: map[string]string{"channel": interaction.ChannelID},
	}
	writeJSON(w, http.StatusOK, cloudchannels.DiscordInteractionResponse(4, "Neo received your request."))
	go func() { _ = bridge.accept(bridge.context(), envelope, interaction.ChannelID, "") }()
}
