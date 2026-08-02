package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	AgentCoreAudience = "agentcore"
	AgentCoreScope    = "chat:completions"
	AgentCoreSlot     = "cody"
	AgentCoreProvider = "xiaomi"
	AgentCoreModel    = "mimo-v2.5-pro"
	// A coding turn can include local installs and browser verification between
	// model calls. Thirty minutes remains short-lived while avoiding expiry in
	// the middle of the worker's bounded twenty-minute job window.
	AgentCoreTokenTTL    = 30 * time.Minute
	AgentCoreMaxTokenTTL = time.Hour
	agentCoreTokenPrefix = "mxg1"
	agentCoreClockSkew   = 30 * time.Second
)

type AgentCoreClaims struct {
	KeyID     string `json:"kid"`
	Audience  string `json:"aud"`
	Scope     string `json:"scope"`
	ActorDID  string `json:"actor_did"`
	Slot      string `json:"slot"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	TokenID   string `json:"jti"`
}

type AgentCoreIssuerOptions struct {
	KeyID string
	Key   []byte
	Now   func() time.Time
	Rand  io.Reader
}

type AgentCoreIssuer struct {
	keyID string
	key   []byte
	now   func() time.Time
	rand  io.Reader
}

func NewAgentCoreIssuer(opts AgentCoreIssuerOptions) (*AgentCoreIssuer, error) {
	if !validKeyID(opts.KeyID) {
		return nil, fmt.Errorf("gateway.auth: valid AgentCore key id is required")
	}
	if len(opts.Key) < 32 {
		return nil, fmt.Errorf("gateway.auth: AgentCore signing key must be at least 32 bytes")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	random := opts.Rand
	if random == nil {
		random = rand.Reader
	}
	return &AgentCoreIssuer{
		keyID: opts.KeyID,
		key:   append([]byte(nil), opts.Key...),
		now:   now,
		rand:  random,
	}, nil
}

func (issuer *AgentCoreIssuer) Mint(actorDID string, ttl time.Duration) (string, AgentCoreClaims, error) {
	actorDID = strings.TrimSpace(actorDID)
	if !looksLikeDID(actorDID) {
		return "", AgentCoreClaims{}, ErrMalformedActor
	}
	if ttl == 0 {
		ttl = AgentCoreTokenTTL
	}
	if ttl <= 0 || ttl > AgentCoreMaxTokenTTL {
		return "", AgentCoreClaims{}, fmt.Errorf("gateway.auth: AgentCore token TTL must be greater than zero and at most %s", AgentCoreMaxTokenTTL)
	}
	jtiBytes := make([]byte, 16)
	if _, err := io.ReadFull(issuer.rand, jtiBytes); err != nil {
		return "", AgentCoreClaims{}, fmt.Errorf("gateway.auth: generate AgentCore token id: %w", err)
	}
	now := issuer.now().UTC().Truncate(time.Second)
	claims := AgentCoreClaims{
		KeyID:     issuer.keyID,
		Audience:  AgentCoreAudience,
		Scope:     AgentCoreScope,
		ActorDID:  actorDID,
		Slot:      AgentCoreSlot,
		Provider:  AgentCoreProvider,
		Model:     AgentCoreModel,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		TokenID:   base64.RawURLEncoding.EncodeToString(jtiBytes),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", AgentCoreClaims{}, fmt.Errorf("gateway.auth: encode AgentCore claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signed := agentCoreTokenPrefix + "." + encoded
	signature := signAgentCoreToken(issuer.key, signed)
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), claims, nil
}

func signAgentCoreToken(key []byte, signed string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(signed))
	return mac.Sum(nil)
}

func validKeyID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' ||
			current >= 'A' && current <= 'Z' ||
			current >= '0' && current <= '9' ||
			current == '_' || current == '-' {
			continue
		}
		return false
	}
	return true
}
