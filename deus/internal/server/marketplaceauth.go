package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const marketplaceTokenMaxTTL = 10 * time.Minute

type MarketplaceAuth struct {
	secret []byte
	now    func() time.Time
}

type marketplaceTokenPayload struct {
	Version     int    `json:"v"`
	Issuer      string `json:"iss"`
	Audience    string `json:"aud"`
	Subject     string `json:"sub"`
	DID         string `json:"did"`
	DisplayName string `json:"name,omitempty"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

func NewMarketplaceAuth(secret string) *MarketplaceAuth {
	secret = strings.TrimSpace(secret)
	if len(secret) < 32 {
		return nil
	}
	return &MarketplaceAuth{secret: []byte(secret), now: time.Now}
}

func (a *MarketplaceAuth) VerifyToken(token string) (DeveloperPrincipal, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != "mkt1" {
		return DeveloperPrincipal{}, errors.New("malformed marketplace token")
	}
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected, got) {
		return DeveloperPrincipal{}, errors.New("marketplace token signature mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return DeveloperPrincipal{}, errors.New("malformed marketplace token payload")
	}
	var p marketplaceTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return DeveloperPrincipal{}, errors.New("malformed marketplace token payload")
	}
	if p.Version != 1 || p.Issuer != "deusmarkets.com" || p.Audience != "deusd" {
		return DeveloperPrincipal{}, errors.New("marketplace token claims rejected")
	}
	if strings.TrimSpace(p.Subject) == "" || len(p.Subject) > 200 {
		return DeveloperPrincipal{}, errors.New("marketplace token subject rejected")
	}
	if !matrixDIDRe.MatchString(p.DID) {
		return DeveloperPrincipal{}, errors.New("marketplace token did rejected")
	}
	now := a.now().Unix()
	if p.IssuedAt > now+30 || p.ExpiresAt < now || p.ExpiresAt <= p.IssuedAt || p.ExpiresAt-p.IssuedAt > int64(marketplaceTokenMaxTTL/time.Second) {
		return DeveloperPrincipal{}, errors.New("marketplace token expired or invalid")
	}
	return DeveloperPrincipal{
		Kind:        DeveloperPrincipalAccount,
		Subject:     p.Subject,
		Owner:       strings.ToLower(p.DID),
		DisplayName: strings.TrimSpace(p.DisplayName),
	}, nil
}
