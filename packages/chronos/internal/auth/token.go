package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Principal tokens are short-lived, stateless HMAC tokens minted after a
// successful agent-DID verify. They bind {did, owner user_id, expiry} and are
// presented on /v1/alarms* (header X-Chronos-Agent). Stateless = no session
// store; works across chronosd instances.
//
// Format: base64url(payload) "." base64url(mac), where
//
//	payload = "<did>|<owner>|<expUnix>"
//
// The DID is carried (not just the owner) so alarm ownership scopes on the full
// owner_did while the wake target resolves from the owner user id.
type Tokens struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewTokens derives an HMAC key from secret (sha256) and binds a token TTL.
func NewTokens(secret string, ttl time.Duration) *Tokens {
	h := sha256.Sum256([]byte(secret))
	return &Tokens{key: h[:], ttl: ttl, now: time.Now}
}

// Claims is the verified principal carried by a token.
type Claims struct {
	DID   string
	Owner string
}

// Mint signs {did, owner, expiry}.
func (t *Tokens) Mint(did, owner string) (token string, expiresIn int) {
	exp := t.now().Add(t.ttl)
	payload := did + "|" + owner + "|" + strconv.FormatInt(exp.Unix(), 10)
	return b64([]byte(payload)) + "." + b64(t.sign(payload)), int(t.ttl / time.Second)
}

// Verify checks a token's signature + expiry and returns its claims.
func (t *Tokens) Verify(tok string) (Claims, error) {
	p, m, ok := strings.Cut(strings.TrimSpace(tok), ".")
	if !ok {
		return Claims{}, errors.New("auth: malformed token")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, errors.New("auth: malformed token payload")
	}
	macBytes, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil {
		return Claims{}, errors.New("auth: malformed token mac")
	}
	payload := string(payloadBytes)
	if !hmac.Equal(macBytes, t.sign(payload)) {
		return Claims{}, errors.New("auth: bad token signature")
	}
	parts := strings.SplitN(payload, "|", 3)
	if len(parts) != 3 {
		return Claims{}, errors.New("auth: malformed token claims")
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Claims{}, errors.New("auth: malformed token expiry")
	}
	if t.now().After(time.Unix(exp, 0)) {
		return Claims{}, errors.New("auth: token expired")
	}
	if parts[0] == "" || parts[1] == "" {
		return Claims{}, errors.New("auth: empty token claims")
	}
	return Claims{DID: parts[0], Owner: parts[1]}, nil
}

func (t *Tokens) sign(payload string) []byte {
	h := hmac.New(sha256.New, t.key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
