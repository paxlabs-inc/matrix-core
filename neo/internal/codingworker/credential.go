package codingworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const actorHeader = "X-Matrix-Actor-DID"

type GatewayCredentialSourceConfig struct {
	GatewayURL   string
	LegacyBearer []byte
	Timeout      time.Duration
	MinValidity  time.Duration
	Transport    http.RoundTripper
	Now          func() time.Time
}

type GatewayCredentialSource struct {
	endpoint    string
	client      *http.Client
	now         func() time.Time
	minValidity time.Duration
	mu          sync.Mutex
	bearer      []byte
	closed      bool
}

func NewGatewayCredentialSource(cfg GatewayCredentialSourceConfig) (*GatewayCredentialSource, error) {
	endpoint, err := credentialEndpoint(cfg.GatewayURL)
	if err != nil {
		return nil, err
	}
	if len(cfg.LegacyBearer) == 0 || bytes.IndexFunc(cfg.LegacyBearer, func(r rune) bool { return r <= ' ' }) >= 0 {
		return nil, errors.New("legacy Matrix gateway bearer is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Timeout > 30*time.Second {
		return nil, errors.New("gateway credential timeout cannot exceed 30 seconds")
	}
	if cfg.MinValidity <= 0 {
		cfg.MinValidity = 30 * time.Second
	}
	if cfg.MinValidity >= time.Hour {
		return nil, errors.New("minimum credential validity must be less than one hour")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &GatewayCredentialSource{
		endpoint:    endpoint,
		client:      client,
		now:         now,
		minValidity: cfg.MinValidity,
		bearer:      append([]byte(nil), cfg.LegacyBearer...),
	}, nil
}

func credentialEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse Matrix gateway URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Matrix gateway URL must be an HTTP(S) origin or /v1 base URL")
	}
	path := strings.TrimRight(u.Path, "/")
	if path != "" && path != "/v1" {
		return "", errors.New("Matrix gateway URL path must be empty or /v1")
	}
	u.Path = "/internal/agentcore/token"
	u.RawPath = ""
	return u.String(), nil
}

func (s *GatewayCredentialSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		eraseBytes(s.bearer)
		s.bearer = nil
	}
	return nil
}

func (s *GatewayCredentialSource) IssueCodingCredential(ctx context.Context, user UserKey, actorDID string) (ScopedCredential, error) {
	if strings.TrimSpace(string(user)) == "" {
		return ScopedCredential{}, errors.New("user key is required")
	}
	actorDID = strings.TrimSpace(actorDID)
	if !looksLikeDID(actorDID) {
		return ScopedCredential{}, errors.New("actor DID is malformed")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ScopedCredential{}, errors.New("gateway credential source is closed")
	}
	bearer := append([]byte(nil), s.bearer...)
	s.mu.Unlock()
	defer eraseBytes(bearer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, http.NoBody)
	if err != nil {
		return ScopedCredential{}, fmt.Errorf("create scoped credential request: %w", err)
	}
	authorization := make([]byte, 0, len("Bearer ")+len(bearer))
	authorization = append(authorization, "Bearer "...)
	authorization = append(authorization, bearer...)
	req.Header.Set("Authorization", string(authorization))
	eraseBytes(authorization)
	req.Header.Set(actorHeader, actorDID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := s.client.Do(req)
	if err != nil {
		return ScopedCredential{}, fmt.Errorf("mint scoped coding credential: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return ScopedCredential{}, fmt.Errorf("mint scoped coding credential returned %s", resp.Status)
	}
	if !headerHasToken(resp.Header.Get("Cache-Control"), "no-store") {
		return ScopedCredential{}, errors.New("scoped credential response is missing Cache-Control: no-store")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		return ScopedCredential{}, fmt.Errorf("read scoped credential response: %w", err)
	}
	if len(body) > 64<<10 {
		return ScopedCredential{}, errors.New("scoped credential response exceeds 64 KiB")
	}
	defer eraseBytes(body)
	var response struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Actor     string `json:"actor"`
		Slot      string `json:"slot"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return ScopedCredential{}, fmt.Errorf("decode scoped credential response: %w", err)
	}
	if response.Actor != actorDID || response.Slot != "cody" || response.Provider != "xiaomi" || response.Model != "mimo-v2.5-pro" {
		return ScopedCredential{}, errors.New("scoped credential response authority does not match the coding runtime")
	}
	now := s.now().UTC()
	expires := time.Unix(response.ExpiresAt, 0).UTC()
	if expires.Before(now.Add(s.minValidity)) || expires.After(now.Add(time.Hour+30*time.Second)) {
		return ScopedCredential{}, errors.New("scoped credential response has an invalid expiry")
	}
	if len(response.Token) < 32 || len(response.Token) > 8192 || !strings.HasPrefix(response.Token, "mxg1.") || strings.IndexFunc(response.Token, func(r rune) bool { return r <= ' ' }) >= 0 {
		return ScopedCredential{}, errors.New("scoped credential response contains an invalid token")
	}
	token := []byte(response.Token)
	response.Token = ""
	return ScopedCredential{Model: token}, nil
}

func looksLikeDID(value string) bool {
	if !strings.HasPrefix(value, "did:") || strings.IndexFunc(value, func(r rune) bool { return r <= ' ' }) >= 0 {
		return false
	}
	parts := strings.SplitN(value, ":", 3)
	return len(parts) == 3 && parts[1] != "" && parts[2] != ""
}
