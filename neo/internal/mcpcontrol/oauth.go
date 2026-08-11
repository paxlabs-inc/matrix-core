// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcpcontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	oauthPendingTTL = 10 * time.Minute
	oauthMaxBody    = 1 << 20
)

type protectedResourceMetadata struct {
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
	RequiredScopes       []string `json:"required_scopes"`
}

type authorizationMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (s *Store) StartOAuth(ctx context.Context, alias string) (OAuthStart, error) {
	server, err := s.Get(ctx, alias)
	if err != nil {
		return OAuthStart{}, err
	}
	if server.Config.OAuth == nil {
		return OAuthStart{}, errors.New("MCP server does not declare OAuth")
	}
	if s.vault == nil || !s.vault.Encrypting() {
		return OAuthStart{}, ErrEncryptionRequired
	}
	oauth, err := s.ensureOAuthMetadata(ctx, server)
	if err != nil {
		return OAuthStart{}, err
	}
	if oauth.ClientID == "" {
		oauth, err = s.registerOAuthClient(ctx, server.Config.Alias, oauth)
		if err != nil {
			return OAuthStart{}, err
		}
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return OAuthStart{}, err
	}
	state, err := randomURLToken(24)
	if err != nil {
		return OAuthStart{}, err
	}
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
	expires := s.now().UTC().Add(oauthPendingTTL)
	stateHash := hashState(state)
	s.mu.Lock()
	secrets, err := s.loadSecretsLocked()
	if err == nil {
		entry := secrets.Servers[alias]
		if entry.Pending == nil {
			entry.Pending = map[string]oauthPending{}
		}
		for key, pending := range entry.Pending {
			if !pending.ExpiresAt.After(s.now()) {
				delete(entry.Pending, key)
			}
		}
		entry.Pending[stateHash] = oauthPending{Verifier: verifier, CreatedAt: s.now().UTC(), ExpiresAt: expires}
		secrets.Servers[alias] = entry
		err = s.persistSecretsLocked(secrets)
	}
	s.mu.Unlock()
	if err != nil {
		return OAuthStart{}, err
	}
	parameters := url.Values{
		"response_type":         {"code"},
		"client_id":             {oauth.ClientID},
		"redirect_uri":          {oauth.RedirectURL},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {oauth.ResourceURL},
	}
	if len(oauth.Scopes) > 0 {
		parameters.Set("scope", strings.Join(oauth.Scopes, " "))
	}
	authorizationURL := oauth.AuthorizationURL + "?" + parameters.Encode()
	_ = s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.start", strings.Join(oauth.Scopes, " "))
	return OAuthStart{AuthorizationURL: authorizationURL, ExpiresAt: expires}, nil
}

func (s *Store) FinishOAuth(ctx context.Context, state, code string) (Server, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" || code == "" || len(state) > 512 || len(code) > 16<<10 {
		return Server{}, ErrOAuthState
	}
	stateHash := hashState(state)
	s.mu.Lock()
	secrets, err := s.loadSecretsLocked()
	if err != nil {
		s.mu.Unlock()
		return Server{}, err
	}
	alias := ""
	var pending oauthPending
	for candidate, entry := range secrets.Servers {
		if value, ok := entry.Pending[stateHash]; ok {
			alias, pending = candidate, value
			delete(entry.Pending, stateHash)
			secrets.Servers[candidate] = entry
			break
		}
	}
	if alias == "" || !pending.ExpiresAt.After(s.now()) {
		s.mu.Unlock()
		return Server{}, ErrOAuthState
	}
	if err := s.persistSecretsLocked(secrets); err != nil {
		s.mu.Unlock()
		return Server{}, err
	}
	s.mu.Unlock()
	server, err := s.Get(ctx, alias)
	if err != nil || server.Config.OAuth == nil {
		return Server{}, ErrOAuthState
	}
	oauth := server.Config.OAuth
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {oauth.RedirectURL},
		"client_id":     {oauth.ClientID},
		"code_verifier": {pending.Verifier},
		"resource":      {oauth.ResourceURL},
	}
	serverSecret, err := s.serverSecrets(alias)
	if err != nil {
		return Server{}, err
	}
	if serverSecret.ClientSecret != "" {
		form.Set("client_secret", serverSecret.ClientSecret)
	}
	tokens, err := requestToken(ctx, oauth.TokenURL, form)
	if err != nil {
		_ = s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.failure", "token exchange failed")
		return Server{}, err
	}
	if err := s.storeTokens(alias, tokens); err != nil {
		return Server{}, err
	}
	_ = s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.authorized", tokens.Scope)
	return s.Get(ctx, alias)
}

func (s *Store) ClearOAuth(ctx context.Context, alias string) error {
	server, err := s.Get(ctx, alias)
	if err != nil {
		return err
	}
	s.mu.Lock()
	secrets, err := s.loadSecretsLocked()
	if err == nil {
		entry := secrets.Servers[alias]
		entry.OAuth = oauthTokens{}
		entry.Pending = map[string]oauthPending{}
		secrets.Servers[alias] = entry
		err = s.persistSecretsLocked(secrets)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.revoke", "")
}

func (s *Store) validAccessToken(ctx context.Context, alias string) (string, error) {
	server, err := s.Get(ctx, alias)
	if err != nil || server.Config.OAuth == nil {
		return "", err
	}
	secrets, err := s.serverSecrets(alias)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(secrets.OAuth.AccessToken) == "" {
		return "", nil
	}
	if secrets.OAuth.ExpiresAt.IsZero() || secrets.OAuth.ExpiresAt.After(s.now().Add(90*time.Second)) {
		return secrets.OAuth.AccessToken, nil
	}
	if strings.TrimSpace(secrets.OAuth.RefreshToken) == "" {
		return "", errors.New("MCP OAuth access token expired and no refresh token is available")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {secrets.OAuth.RefreshToken},
		"client_id":     {server.Config.OAuth.ClientID},
		"resource":      {server.Config.OAuth.ResourceURL},
	}
	if secrets.ClientSecret != "" {
		form.Set("client_secret", secrets.ClientSecret)
	}
	tokens, err := requestToken(ctx, server.Config.OAuth.TokenURL, form)
	if err != nil {
		_ = s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.refresh.failure", "")
		return "", err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = secrets.OAuth.RefreshToken
	}
	if err := s.storeTokens(alias, tokens); err != nil {
		return "", err
	}
	_ = s.auditDirect(ctx, alias, server.DesiredGeneration, "oauth.refresh", tokens.Scope)
	return tokens.AccessToken, nil
}

func (s *Store) OAuthRefreshDue(ctx context.Context) bool {
	servers, err := s.List(ctx)
	if err != nil {
		return false
	}
	for _, server := range servers {
		if !server.Enabled || server.Config.OAuth == nil {
			continue
		}
		secrets, err := s.serverSecrets(server.Config.Alias)
		if err == nil && !secrets.OAuth.ExpiresAt.IsZero() && !secrets.OAuth.ExpiresAt.After(s.now().Add(2*time.Minute)) {
			return true
		}
	}
	return false
}

func (s *Store) ensureOAuthMetadata(ctx context.Context, server Server) (*OAuthConfig, error) {
	oauth := *server.Config.OAuth
	if oauth.AuthorizationURL != "" && oauth.TokenURL != "" {
		return &oauth, nil
	}
	resource, err := url.Parse(oauth.ResourceURL)
	if err != nil {
		return nil, err
	}
	origin := resource.Scheme + "://" + resource.Host
	var protected protectedResourceMetadata
	resourcePath := strings.TrimRight(resource.EscapedPath(), "/")
	protectedCandidates := []string{origin + "/.well-known/oauth-protected-resource" + resourcePath}
	if resourcePath != "" {
		protectedCandidates = append(protectedCandidates, origin+"/.well-known/oauth-protected-resource")
	}
	for _, candidate := range protectedCandidates {
		if getJSON(ctx, candidate, &protected) == nil {
			break
		}
	}
	authorizationServer := oauth.AuthorizationServer
	if authorizationServer == "" && len(protected.AuthorizationServers) > 0 {
		authorizationServer = protected.AuthorizationServers[0]
	}
	if authorizationServer == "" {
		authorizationServer = origin
	}
	if err := validatePublicHTTPS(authorizationServer); err != nil {
		return nil, fmt.Errorf("MCP OAuth authorization server: %w", err)
	}
	base := strings.TrimRight(authorizationServer, "/")
	issuer, _ := url.Parse(base)
	issuerOrigin := issuer.Scheme + "://" + issuer.Host
	issuerPath := strings.TrimRight(issuer.EscapedPath(), "/")
	var metadata authorizationMetadata
	metadataCandidates := []string{
		issuerOrigin + "/.well-known/oauth-authorization-server" + issuerPath,
		base + "/.well-known/openid-configuration",
	}
	if issuerPath != "" {
		metadataCandidates = append(metadataCandidates, base+"/.well-known/oauth-authorization-server")
	}
	var metadataErr error
	for _, candidate := range metadataCandidates {
		metadataErr = getJSON(ctx, candidate, &metadata)
		if metadataErr == nil {
			break
		}
	}
	if metadataErr != nil {
		return nil, fmt.Errorf("MCP OAuth metadata discovery failed: %w", metadataErr)
	}
	for label, endpoint := range map[string]string{"authorization": metadata.AuthorizationEndpoint, "token": metadata.TokenEndpoint} {
		if err := validatePublicHTTPS(endpoint); err != nil {
			return nil, fmt.Errorf("MCP OAuth metadata %s endpoint: %w", label, err)
		}
	}
	if metadata.RegistrationEndpoint != "" {
		if err := validatePublicHTTPS(metadata.RegistrationEndpoint); err != nil {
			return nil, fmt.Errorf("MCP OAuth registration endpoint: %w", err)
		}
	}
	oauth.AuthorizationServer = authorizationServer
	oauth.AuthorizationURL = metadata.AuthorizationEndpoint
	oauth.TokenURL = metadata.TokenEndpoint
	oauth.RegistrationURL = metadata.RegistrationEndpoint
	if len(oauth.Scopes) == 0 {
		scopes := protected.RequiredScopes
		if len(scopes) == 0 {
			scopes = protected.ScopesSupported
		}
		if len(scopes) == 0 {
			scopes = metadata.ScopesSupported
		}
		oauth.Scopes = normalizeScopes(scopes)
	}
	if err := s.updateOAuthConfig(ctx, server.Config.Alias, oauth, "oauth.discovery"); err != nil {
		return nil, err
	}
	return &oauth, nil
}

func (s *Store) registerOAuthClient(ctx context.Context, alias string, oauth *OAuthConfig) (*OAuthConfig, error) {
	if oauth.RegistrationURL == "" {
		return nil, errors.New("MCP OAuth server requires a configured client_id because dynamic registration is unavailable")
	}
	payload := map[string]any{
		"client_name": "Centra AI Neo", "redirect_uris": []string{oauth.RedirectURL},
		"grant_types":    []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	}
	if len(oauth.Scopes) > 0 {
		payload["scope"] = strings.Join(oauth.Scopes, " ")
	}
	var response struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := postJSON(ctx, oauth.RegistrationURL, payload, &response); err != nil {
		return nil, fmt.Errorf("MCP OAuth client registration failed: %w", err)
	}
	if strings.TrimSpace(response.ClientID) == "" {
		return nil, errors.New("MCP OAuth registration did not return a client_id")
	}
	oauth.ClientID = strings.TrimSpace(response.ClientID)
	s.mu.Lock()
	secrets, err := s.loadSecretsLocked()
	if err == nil {
		entry := secrets.Servers[alias]
		entry.ClientSecret = response.ClientSecret
		secrets.Servers[alias] = entry
		err = s.persistSecretsLocked(secrets)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.updateOAuthConfig(ctx, alias, *oauth, "oauth.register"); err != nil {
		return nil, err
	}
	return oauth, nil
}

func (s *Store) updateOAuthConfig(ctx context.Context, alias string, oauth OAuthConfig, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	server, err := s.getLocked(ctx, alias)
	if err != nil {
		return err
	}
	server.Config.OAuth = &oauth
	configJSON, _ := json.Marshal(server.Config)
	toolsJSON, _ := json.Marshal(server.Tools)
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_version SET config=?,config_hash=? WHERE alias=? AND generation=?`, configJSON, hashConfig(configJSON, toolsJSON), alias, server.DesiredGeneration); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, alias, server.DesiredGeneration, action, "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) storeTokens(alias string, response tokenResponse) error {
	if strings.TrimSpace(response.AccessToken) == "" {
		return errors.New("MCP OAuth token response did not include an access token")
	}
	tokens := oauthTokens{
		AccessToken: response.AccessToken, RefreshToken: response.RefreshToken,
		TokenType: response.TokenType, Scope: response.Scope,
	}
	if response.ExpiresIn > 0 {
		tokens.ExpiresAt = s.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	secrets, err := s.loadSecretsLocked()
	if err != nil {
		return err
	}
	entry := secrets.Servers[alias]
	entry.OAuth = tokens
	secrets.Servers[alias] = entry
	return s.persistSecretsLocked(secrets)
}

func requestToken(ctx context.Context, endpoint string, form url.Values) (tokenResponse, error) {
	var response tokenResponse
	if err := postForm(ctx, endpoint, form, &response); err != nil {
		return tokenResponse{}, err
	}
	if strings.TrimSpace(response.AccessToken) == "" {
		return tokenResponse{}, errors.New("MCP OAuth token endpoint returned no access token")
	}
	return response, nil
}

func getJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	return doJSON(request, target)
}

func postJSON(ctx context.Context, endpoint string, payload, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	return doJSON(request, target)
}

func postForm(ctx context.Context, endpoint string, form url.Values, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	return doJSON(request, target)
}

func doJSON(request *http.Request, target any) error {
	client, err := pinnedHTTPSClient(request.URL)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, oauthMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > oauthMaxBody {
		return errors.New("MCP OAuth response exceeds the size bound")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("MCP OAuth endpoint returned HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("MCP OAuth endpoint returned invalid JSON")
	}
	return nil
}

func pinnedHTTPSClient(endpoint *url.URL) (*http.Client, error) {
	if endpoint == nil {
		return nil, errors.New("missing HTTPS endpoint")
	}
	if err := validatePublicHTTPS(endpoint.String()); err != nil {
		return nil, err
	}
	host := endpoint.Hostname()
	addresses, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("HTTPS endpoint host does not resolve")
	}
	public := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() && !address.IsUnspecified() {
			public = append(public, address.String())
		}
	}
	if len(public) != len(addresses) || len(public) == 0 {
		return nil, errors.New("HTTPS endpoint resolves to a non-public address")
	}
	sort.Strings(public)
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			var last error
			for _, ip := range public {
				connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
				if err == nil {
					return connection, nil
				}
				last = err
			}
			return nil, last
		},
	}
	return &http.Client{
		Timeout: 20 * time.Second, Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return errors.New("MCP OAuth redirects are not followed")
		},
	}, nil
}

func randomURLToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func hashState(state string) string {
	digest := sha256.Sum256([]byte(state))
	return hex.EncodeToString(digest[:])
}

func (s *Store) auditDirect(ctx context.Context, alias string, generation int64, action, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_audit(alias,generation,action,detail,created_at) VALUES(?,?,?,?,?)`, alias, generation, action, detail, s.now().UTC().UnixMilli())
	return err
}
