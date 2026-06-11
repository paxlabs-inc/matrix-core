package identity

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
// successful agent-DID verify. They bind an owner user id + expiry and are
// presented on /v1/invoke (header X-UWAC-Agent). Stateless = no session store,
// works across uwacd instances. Format: base64url(payload) "." base64url(mac)
// where payload = "<owner>|<expUnix>".

// MintToken signs an owner + expiry with key (HMAC-SHA256).
func MintToken(key []byte, owner string, ttl time.Duration) string {
	payload := owner + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	mac := sign(key, payload)
	return b64(payload) + "." + b64(mac)
}

// VerifyToken checks a principal token's signature + expiry and returns the
// bound owner user id.
func VerifyToken(key []byte, tok string) (string, error) {
	p, m, ok := strings.Cut(tok, ".")
	if !ok {
		return "", errors.New("identity: malformed token")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return "", errors.New("identity: malformed token payload")
	}
	macBytes, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil {
		return "", errors.New("identity: malformed token mac")
	}
	payload := string(payloadBytes)
	if !hmac.Equal(macBytes, sign(key, payload)) {
		return "", errors.New("identity: bad token signature")
	}
	owner, expStr, ok := strings.Cut(payload, "|")
	if !ok {
		return "", errors.New("identity: malformed token claims")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", errors.New("identity: malformed token expiry")
	}
	if time.Now().After(time.Unix(exp, 0)) {
		return "", errors.New("identity: token expired")
	}
	return owner, nil
}

func sign(key []byte, payload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func b64(s any) string {
	switch v := s.(type) {
	case string:
		return base64.RawURLEncoding.EncodeToString([]byte(v))
	case []byte:
		return base64.RawURLEncoding.EncodeToString(v)
	default:
		return ""
	}
}
