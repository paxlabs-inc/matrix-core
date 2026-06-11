// Package oauth extends the self-hosted GoTrue authorization-code (PKCE) flow
// with scope elevation, captures the provider_token + provider_refresh_token
// that the marketplace flow currently discards, and refreshes provider access
// tokens directly against the provider (GoTrue does NOT refresh them — the g2
// gotcha in connections.frozen.kvx).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"time"

	"github.com/paxlabs-inc/uwac/internal/httpx"
)

// ProviderCreds are the OAuth client credentials UWAC needs to refresh provider
// tokens (the same client GoTrue is configured with).
type ProviderCreds struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
}

// Client talks to GoTrue (authorize + code exchange) and to providers (refresh).
type Client struct {
	supabaseURL string
	anonKey     string
	http        *httpx.Client
	creds       map[string]ProviderCreds
}

// New constructs an OAuth client. creds maps provider key -> client creds.
func New(supabaseURL, anonKey string, creds map[string]ProviderCreds) *Client {
	return &Client{
		supabaseURL: strings.TrimRight(supabaseURL, "/"),
		anonKey:     anonKey,
		http:        httpx.New(20 * time.Second),
		creds:       creds,
	}
}

// GeneratePKCE returns a (verifier, S256 challenge) pair.
func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// AuthorizeURL builds the GoTrue authorize URL with elevated scopes + the
// connector's extra query params (e.g. access_type=offline, prompt=consent).
func (c *Client) AuthorizeURL(provider, redirectTo, challenge string, scopes []string, extra map[string]string) string {
	params := url.Values{}
	params.Set("provider", provider)
	params.Set("redirect_to", redirectTo)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "s256")
	if len(scopes) > 0 {
		params.Set("scopes", strings.Join(scopes, " "))
	}
	for k, v := range extra {
		params.Set(k, v)
	}
	return c.supabaseURL + "/auth/v1/authorize?" + params.Encode()
}

// ExchangeResult is the GoTrue PKCE token response, including the provider
// tokens UWAC vaults.
type ExchangeResult struct {
	AccessToken          string `json:"access_token"`
	ProviderToken        string `json:"provider_token"`
	ProviderRefreshToken string `json:"provider_refresh_token"`
	ExpiresIn            int    `json:"expires_in"`
	User                 struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// Exchange swaps the PKCE authorization code + verifier for tokens server-side.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (*ExchangeResult, error) {
	var out ExchangeResult
	body := map[string]string{"auth_code": code, "code_verifier": verifier}
	headers := map[string]string{"apikey": c.anonKey}
	if err := c.http.JSONWithHeaders(ctx, "POST", c.supabaseURL+"/auth/v1/token?grant_type=pkce", headers, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefreshProviderToken mints a fresh provider access token from a refresh token
// (rotating providers like Google). Returns the token + its expiry.
func (c *Client) RefreshProviderToken(ctx context.Context, provider, refreshToken string) (string, time.Time, error) {
	creds, ok := c.creds[provider]
	if !ok || creds.TokenURL == "" {
		return "", time.Time{}, &httpx.Error{Status: 0, Body: "no provider credentials configured for " + provider}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", creds.ClientID)
	form.Set("client_secret", creds.ClientSecret)
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := c.http.Form(ctx, creds.TokenURL, form, nil, &out); err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(time.Duration(maxInt(out.ExpiresIn, 60)) * time.Second)
	return out.AccessToken, exp, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
