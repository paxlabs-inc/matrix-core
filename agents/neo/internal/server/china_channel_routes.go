// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"

	"centra/agents/neo/internal/cloudchannels"
)

type qqConfigureRequest struct {
	AppID        string `json:"app_id"`
	ClientSecret string `json:"client_secret"`
	GroupTrigger string `json:"group_trigger,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleQQ(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "QQ is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.QQStatus())
	case http.MethodPut:
		var request qqConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureQQ(r.Context(), cloudchannels.QQConfig{Enabled: enabled, AppID: request.AppID, ClientSecret: request.ClientSecret, GroupTrigger: request.GroupTrigger})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.ClientSecret)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectQQ(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.QQStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

type weixinConfigureRequest struct {
	Token      string `json:"token"`
	BotID      string `json:"bot_id"`
	BaseURL    string `json:"base_url,omitempty"`
	CDNBaseURL string `json:"cdn_base_url,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleWeixin(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Weixin is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.WeixinStatus())
	case http.MethodPut:
		var request weixinConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureWeixin(r.Context(), cloudchannels.WeixinConfig{Enabled: enabled, Token: request.Token, BotID: request.BotID, BaseURL: request.BaseURL, CDNBaseURL: request.CDNBaseURL})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.Token)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectWeixin(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.WeixinStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}
func (s *Server) handleWeixinQR(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Weixin is unavailable"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		result, err := bridge.weixinAPI.FetchQRCode(r.Context(), r.URL.Query().Get("base_url"))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		var request struct {
			QRCode  string `json:"qrcode"`
			BaseURL string `json:"base_url,omitempty"`
			Enabled *bool  `json:"enabled,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil || strings.TrimSpace(request.QRCode) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "qrcode is required"})
			return
		}
		result, err := bridge.weixinAPI.PollQRCode(r.Context(), request.BaseURL, request.QRCode)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if strings.EqualFold(result.Status, "confirmed") && result.BotToken != "" && result.BotID != "" {
			enabled := true
			if request.Enabled != nil {
				enabled = *request.Enabled
			}
			base := result.BaseURL
			if base == "" {
				base = request.BaseURL
			}
			if _, err = bridge.ConfigureWeixin(r.Context(), cloudchannels.WeixinConfig{Enabled: enabled, Token: result.BotToken, BotID: result.BotID, BaseURL: base}); err != nil {
				writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), result.BotToken)})
				return
			}
			result.BotToken = ""
		}
		writeJSON(w, http.StatusOK, result)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

type weChatMPConfigureRequest struct {
	AppID          string `json:"app_id"`
	AppSecret      string `json:"app_secret"`
	Token          string `json:"token"`
	EncodingAESKey string `json:"encoding_aes_key,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleWeChatMP(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Official Account is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.WeChatMPStatus())
	case http.MethodPut:
		var request weChatMPConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureWeChatMP(r.Context(), cloudchannels.WeChatMPConfig{Enabled: enabled, AppID: request.AppID, AppSecret: request.AppSecret, Token: request.Token, EncodingAESKey: request.EncodingAESKey})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.AppSecret, request.Token, request.EncodingAESKey)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectWeChatMP(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.WeChatMPStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleWeChatMPCallback(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Official Account is unavailable"})
		return
	}
	config := bridge.store.View().WeChatMP
	if !config.Enabled || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Official Account is not enabled"})
		return
	}
	timestamp, nonce := r.URL.Query().Get("timestamp"), r.URL.Query().Get("nonce")
	encrypted := r.URL.Query().Get("encrypt_type") == "aes"
	if r.Method == http.MethodGet {
		echo := r.URL.Query().Get("echostr")
		if encrypted {
			crypt, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.AppID)
			if err != nil || crypt.Verify(r.URL.Query().Get("msg_signature"), timestamp, nonce, echo) != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeChat Official Account callback signature is invalid"})
				return
			}
			plain, err := crypt.Decrypt(echo)
			if err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
				return
			}
			echo = string(plain)
		} else if cloudchannels.VerifyCallbackSignature(config.Token, r.URL.Query().Get("signature"), timestamp, nonce) != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeChat Official Account callback signature is invalid"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(echo))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "callback body is invalid"})
		return
	}
	if encrypted {
		var wrapper cloudchannels.WeComEncryptedXML
		if xml.Unmarshal(body, &wrapper) != nil || wrapper.Encrypt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "encrypted callback envelope is invalid"})
			return
		}
		crypt, cryptoErr := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.AppID)
		if cryptoErr != nil || crypt.Verify(r.URL.Query().Get("msg_signature"), timestamp, nonce, wrapper.Encrypt) != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeChat Official Account callback signature is invalid"})
			return
		}
		body, err = crypt.Decrypt(wrapper.Encrypt)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
	} else if cloudchannels.VerifyCallbackSignature(config.Token, r.URL.Query().Get("signature"), timestamp, nonce) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "WeChat Official Account callback signature is invalid"})
		return
	}
	var message cloudchannels.WeChatMPMessage
	if xml.Unmarshal(body, &message) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "WeChat Official Account message is invalid"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))
	go func() { _ = bridge.handleWeChatMPMessage(bridge.context(), message) }()
}

type weChatKFConfigureRequest struct {
	CorpID         string `json:"corp_id"`
	Secret         string `json:"secret"`
	Token          string `json:"token"`
	EncodingAESKey string `json:"encoding_aes_key"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

func (s *Server) handleWeChatKF(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Customer Service is disabled on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, bridge.WeChatKFStatus())
	case http.MethodPut:
		var request weChatKFConfigureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		status, err := bridge.ConfigureWeChatKF(r.Context(), cloudchannels.WeChatKFConfig{Enabled: enabled, CorpID: request.CorpID, Secret: request.Secret, Token: request.Token, EncodingAESKey: request.EncodingAESKey})
		if err != nil {
			writeJSON(w, cloudConfigErrorStatus(err), map[string]string{"error": safeCloudError(err.Error(), request.Secret, request.Token, request.EncodingAESKey)})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := bridge.DisconnectWeChatKF(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bridge.WeChatKFStatus())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleWeChatKFCallback(w http.ResponseWriter, r *http.Request) {
	bridge := s.engine.cloudChannels
	if bridge == nil || bridge.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Customer Service is unavailable"})
		return
	}
	config := bridge.store.View().WeChatKF
	if !config.Enabled || !config.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "WeChat Customer Service is not enabled"})
		return
	}
	crypt, err := cloudchannels.NewWeComCrypto(config.Token, config.EncodingAESKey, config.CorpID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "callback crypto is unavailable"})
		return
	}
	signature, timestamp, nonce := r.URL.Query().Get("msg_signature"), r.URL.Query().Get("timestamp"), r.URL.Query().Get("nonce")
	if r.Method == http.MethodGet {
		echo := r.URL.Query().Get("echostr")
		if crypt.Verify(signature, timestamp, nonce, echo) != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "callback signature is invalid"})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "callback body is invalid"})
		return
	}
	var wrapper cloudchannels.WeComEncryptedXML
	if xml.Unmarshal(body, &wrapper) != nil || wrapper.Encrypt == "" || crypt.Verify(signature, timestamp, nonce, wrapper.Encrypt) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "callback envelope is invalid"})
		return
	}
	plain, err := crypt.Decrypt(wrapper.Encrypt)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	var callback cloudchannels.WeChatKFCallback
	if xml.Unmarshal(plain, &callback) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "callback message is invalid"})
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))
	if callback.MsgType == "event" && callback.Event == "kf_msg_or_event" && callback.Token != "" && callback.OpenKFID != "" {
		go func() { _ = bridge.consumeWeChatKF(bridge.context(), callback.Token, callback.OpenKFID) }()
	}
}

func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	for _, key := range []string{"access_token", "secret", "token"} {
		if query.Has(key) {
			query.Set(key, "[redacted]")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
