// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcpcontrol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	aliasPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

func validateConfig(config Config) (Config, error) {
	config.Alias = strings.TrimSpace(config.Alias)
	config.DisplayName = strings.TrimSpace(config.DisplayName)
	config.Transport = strings.ToLower(strings.TrimSpace(config.Transport))
	config.Command = strings.TrimSpace(config.Command)
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	config.PackageDigest = strings.ToLower(strings.TrimSpace(config.PackageDigest))
	config.Version = strings.TrimSpace(config.Version)
	if !aliasPattern.MatchString(config.Alias) {
		return Config{}, errors.New("MCP alias must use lowercase letters, digits, and hyphens")
	}
	if config.DisplayName == "" || len(config.DisplayName) > 120 {
		return Config{}, errors.New("MCP display_name is required and must be at most 120 characters")
	}
	if config.Version == "" || len(config.Version) > 80 {
		return Config{}, errors.New("MCP version is required and must be at most 80 characters")
	}
	if !digestPattern.MatchString(config.PackageDigest) {
		return Config{}, errors.New("MCP package_digest must be sha256:<64 lowercase hex>")
	}
	if len(config.Args) > 64 {
		return Config{}, errors.New("MCP stdio arguments exceed the 64 item bound")
	}
	for _, arg := range config.Args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return Config{}, errors.New("MCP stdio argument is invalid or too large")
		}
		lower := strings.ToLower(arg)
		if strings.Contains(lower, "token=") || strings.Contains(lower, "secret=") || strings.Contains(lower, "api_key=") || strings.Contains(lower, "apikey=") {
			return Config{}, errors.New("MCP credentials must use encrypted environment or header fields, not command arguments")
		}
	}
	config.EnvKeys = normalizeKeys(config.EnvKeys)
	config.HeaderKeys = normalizeHeaderKeys(config.HeaderKeys)
	switch config.Transport {
	case "stdio":
		if !filepathIsAbs(config.Command) {
			return Config{}, errors.New("MCP stdio command must be an absolute installed executable")
		}
		info, err := os.Stat(config.Command)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return Config{}, errors.New("MCP stdio command is not an executable regular file")
		}
		digest, err := digestCommand(config.Command, config.Args)
		if err != nil {
			return Config{}, fmt.Errorf("MCP stdio package digest: %w", err)
		}
		if digest != config.PackageDigest {
			return Config{}, fmt.Errorf("MCP stdio package digest mismatch: installed executable is %s", digest)
		}
		config.Endpoint = ""
		config.HeaderKeys = nil
	case "http":
		if err := validatePublicHTTPS(config.Endpoint); err != nil {
			return Config{}, fmt.Errorf("MCP HTTP endpoint: %w", err)
		}
		config.Command = ""
		config.Args = nil
		config.EnvKeys = nil
	default:
		return Config{}, errors.New("MCP transport must be stdio or http")
	}
	if config.OAuth != nil {
		if config.Transport != "http" {
			return Config{}, errors.New("MCP OAuth is available only for Streamable HTTP servers")
		}
		oauth := *config.OAuth
		oauth.ResourceURL = strings.TrimSpace(oauth.ResourceURL)
		oauth.RedirectURL = strings.TrimSpace(oauth.RedirectURL)
		oauth.ClientID = strings.TrimSpace(oauth.ClientID)
		oauth.Scopes = normalizeScopes(oauth.Scopes)
		if oauth.ResourceURL == "" {
			oauth.ResourceURL = config.Endpoint
		}
		if err := validatePublicHTTPS(oauth.ResourceURL); err != nil {
			return Config{}, fmt.Errorf("MCP OAuth resource: %w", err)
		}
		if err := validatePublicHTTPS(oauth.RedirectURL); err != nil {
			return Config{}, fmt.Errorf("MCP OAuth redirect: %w", err)
		}
		for label, endpoint := range map[string]string{"authorization": oauth.AuthorizationURL, "token": oauth.TokenURL, "registration": oauth.RegistrationURL} {
			if endpoint != "" {
				if err := validatePublicHTTPS(endpoint); err != nil {
					return Config{}, fmt.Errorf("MCP OAuth %s endpoint: %w", label, err)
				}
			}
		}
		config.OAuth = &oauth
	}
	return config, nil
}

func validateSecretKeys(config Config, request CreateRequest) error {
	if !sameKeys(config.EnvKeys, request.SecretEnv) {
		return errors.New("MCP secret_env keys must exactly match config.env_keys")
	}
	if !sameKeys(config.HeaderKeys, request.SecretHeaders) {
		return errors.New("MCP secret_headers keys must exactly match config.header_keys")
	}
	for key, value := range request.SecretEnv {
		if !namePattern.MatchString(key) || strings.TrimSpace(value) == "" || len(value) > 16<<10 {
			return errors.New("MCP encrypted environment value is invalid")
		}
	}
	for key, value := range request.SecretHeaders {
		if strings.ContainsAny(key, "\r\n:") || strings.TrimSpace(value) == "" || len(value) > 16<<10 {
			return errors.New("MCP encrypted header value is invalid")
		}
	}
	if len(request.ClientSecret) > 16<<10 {
		return errors.New("MCP OAuth client secret exceeds the storage bound")
	}
	return nil
}

func (s *Store) SaveProbe(ctx context.Context, alias string, tools []Tool, latency time.Duration) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.getLocked(ctx, alias)
	if err != nil {
		return Server{}, err
	}
	seen := map[string]bool{}
	for index := range tools {
		tools[index].Name = strings.TrimSpace(tools[index].Name)
		if tools[index].Name == "" || seen[tools[index].Name] {
			return Server{}, errors.New("MCP server advertised an empty or duplicate tool name")
		}
		seen[tools[index].Name] = true
		tools[index].Function = canonicalFunction(alias, tools[index].Name)
		for _, previous := range current.Tools {
			if previous.Name == tools[index].Name {
				tools[index].EffectClass = previous.EffectClass
				tools[index].Enabled = previous.Enabled
			}
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	encoded, _ := json.Marshal(tools)
	configJSON, _ := json.Marshal(current.Config)
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Server{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_version SET tools=?,config_hash=?,healthy=1 WHERE alias=? AND generation=?`, encoded, hashConfig(configJSON, encoded), alias, current.DesiredGeneration); err != nil {
		return Server{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET last_healthy_generation=?,health=?,latency_ms=?,failure_count=0,circuit_until=0,last_error='',updated_at=? WHERE alias=?`, current.DesiredGeneration, HealthHealthy, latency.Milliseconds(), now.UnixMilli(), alias); err != nil {
		return Server{}, err
	}
	if err := appendAudit(ctx, tx, alias, current.DesiredGeneration, "probe.success", fmt.Sprintf("%d tools, %d ms", len(tools), latency.Milliseconds()), now); err != nil {
		return Server{}, err
	}
	if err := tx.Commit(); err != nil {
		return Server{}, err
	}
	return s.getLocked(ctx, alias)
}

func (s *Store) ProbeFailure(ctx context.Context, alias string, cause error) error {
	message := SafeRuntimeError(cause).Error()
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE mcp_server SET health=?,last_error=?,updated_at=? WHERE alias=?`, HealthUnhealthy, message, now.UnixMilli(), alias)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO mcp_audit(alias,generation,action,detail,created_at) SELECT alias,desired_generation,'probe.failure',?,? FROM mcp_server WHERE alias=?`, message, now.UnixMilli(), alias)
	return nil
}

func (s *Store) Classify(ctx context.Context, alias string, classifications []Classification) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.getLocked(ctx, alias)
	if err != nil {
		return Server{}, err
	}
	byName := make(map[string]Classification, len(classifications))
	for _, classification := range classifications {
		classification.Name = strings.TrimSpace(classification.Name)
		classification.EffectClass = strings.TrimSpace(classification.EffectClass)
		if !validEffect(classification.EffectClass) {
			return Server{}, fmt.Errorf("invalid effect class for %q", classification.Name)
		}
		if _, duplicate := byName[classification.Name]; duplicate {
			return Server{}, fmt.Errorf("duplicate classification for %q", classification.Name)
		}
		byName[classification.Name] = classification
	}
	if len(byName) != len(current.Tools) {
		return Server{}, ErrUnclassified
	}
	for index := range current.Tools {
		classification, ok := byName[current.Tools[index].Name]
		if !ok {
			return Server{}, fmt.Errorf("%w: %s", ErrUnclassified, current.Tools[index].Name)
		}
		current.Tools[index].EffectClass = classification.EffectClass
		current.Tools[index].Enabled = classification.Enabled
	}
	return s.stageVersionLocked(ctx, current, "tools.classify", fmt.Sprintf("%d tools", len(current.Tools)))
}

func (s *Store) Enable(ctx context.Context, alias string, enabled bool) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.getLocked(ctx, alias)
	if err != nil {
		return Server{}, err
	}
	if enabled {
		if current.Health != HealthHealthy || current.LastHealthyGeneration != current.DesiredGeneration {
			return Server{}, ErrUnhealthy
		}
		if len(current.Tools) == 0 {
			return Server{}, ErrUnclassified
		}
		for _, discovered := range current.Tools {
			if !validEffect(discovered.EffectClass) {
				return Server{}, fmt.Errorf("%w: %s", ErrUnclassified, discovered.Name)
			}
		}
	}
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `UPDATE mcp_server SET enabled=?,state=?,updated_at=? WHERE alias=?`, enabled, StatePending, now.UnixMilli(), alias)
	if err != nil {
		return Server{}, err
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO mcp_audit(alias,generation,action,detail,created_at) VALUES(?,?,?,?,?)`, alias, current.DesiredGeneration, "activation.stage", fmt.Sprintf("enabled=%t", enabled), now.UnixMilli())
	return s.getLocked(ctx, alias)
}

func (s *Store) stageVersionLocked(ctx context.Context, current Server, action, detail string) (Server, error) {
	configJSON, _ := json.Marshal(current.Config)
	toolsJSON, _ := json.Marshal(current.Tools)
	now := s.now().UTC()
	next := current.DesiredGeneration + 1
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Server{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_version(alias,generation,config,tools,config_hash,healthy,created_at) VALUES(?,?,?,?,?,1,?)`, current.Config.Alias, next, configJSON, toolsJSON, hashConfig(configJSON, toolsJSON), now.UnixMilli()); err != nil {
		return Server{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET desired_generation=?,last_healthy_generation=?,health=?,state=?,enabled=0,last_error='',updated_at=? WHERE alias=?`, next, next, HealthHealthy, StateCandidate, now.UnixMilli(), current.Config.Alias); err != nil {
		return Server{}, err
	}
	if err := appendAudit(ctx, tx, current.Config.Alias, next, action, detail, now); err != nil {
		return Server{}, err
	}
	if err := tx.Commit(); err != nil {
		return Server{}, err
	}
	return s.getLocked(ctx, current.Config.Alias)
}

func (s *Store) RuntimeServers(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias,
		CASE WHEN enabled=1 AND state IN (?,?) THEN desired_generation
		     WHEN state=? AND applied_generation>0 THEN applied_generation ELSE 0 END
		FROM mcp_server
		WHERE (enabled=1 AND state IN (?,?)) OR (state=? AND applied_generation>0)
		ORDER BY alias`, StatePending, StateActive, StateCandidate, StatePending, StateActive, StateCandidate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type runtimeTarget struct {
		alias      string
		generation int64
	}
	targets := make([]runtimeTarget, 0)
	for rows.Next() {
		var target runtimeTarget
		if err := rows.Scan(&target.alias, &target.generation); err != nil {
			return nil, err
		}
		if target.generation > 0 {
			targets = append(targets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(targets))
	for _, target := range targets {
		server, err := s.Get(ctx, target.alias)
		if err != nil {
			return nil, err
		}
		if target.generation != server.DesiredGeneration {
			var configJSON, toolsJSON []byte
			if err := s.db.QueryRowContext(ctx, `SELECT config,tools FROM mcp_version WHERE alias=? AND generation=?`, target.alias, target.generation).Scan(&configJSON, &toolsJSON); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(configJSON, &server.Config); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(toolsJSON, &server.Tools); err != nil {
				return nil, err
			}
			server.DesiredGeneration = target.generation
		}
		out = append(out, server)
	}
	return out, nil
}

func (s *Store) HasPending(ctx context.Context) bool {
	var count int
	return s.db.QueryRowContext(ctx, `SELECT count(*) FROM mcp_server WHERE state=?`, StatePending).Scan(&count) == nil && count > 0
}

func (s *Store) MarkApplied(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT alias,desired_generation,enabled FROM mcp_server WHERE state=?`, StatePending)
	if err != nil {
		return err
	}
	type pending struct {
		alias      string
		generation int64
		enabled    bool
	}
	items := make([]pending, 0)
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.alias, &item.generation, &item.enabled); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		state := StateDisabled
		applied := int64(0)
		if item.enabled {
			state = StateActive
			applied = item.generation
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET state=?,applied_generation=?,last_error='',updated_at=? WHERE alias=?`, state, applied, now.UnixMilli(), item.alias); err != nil {
			return err
		}
		if err := appendAudit(ctx, tx, item.alias, item.generation, "activation.apply", string(state), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RollbackPending(ctx context.Context, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := boundedError(cause)
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT alias,desired_generation,applied_generation FROM mcp_server WHERE state=?`, StatePending)
	if err != nil {
		return err
	}
	type pending struct {
		alias            string
		desired, applied int64
	}
	items := make([]pending, 0)
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.alias, &item.desired, &item.applied); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		if item.applied > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET desired_generation=?,enabled=1,state=?,health=?,last_error=?,updated_at=? WHERE alias=?`, item.applied, StateActive, HealthHealthy, message, now.UnixMilli(), item.alias); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET enabled=0,state=?,health=?,last_error=?,updated_at=? WHERE alias=?`, StateRollback, HealthUnhealthy, message, now.UnixMilli(), item.alias); err != nil {
			return err
		}
		if err := appendAudit(ctx, tx, item.alias, item.desired, "activation.rollback", message, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Rollback(ctx context.Context, alias string) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.getLocked(ctx, alias)
	if err != nil {
		return Server{}, err
	}
	if current.LastHealthyGeneration == 0 {
		return Server{}, ErrUnhealthy
	}
	var configJSON, toolsJSON []byte
	if err := s.db.QueryRowContext(ctx, `SELECT config,tools FROM mcp_version WHERE alias=? AND generation=? AND healthy=1`, alias, current.LastHealthyGeneration).Scan(&configJSON, &toolsJSON); err != nil {
		return Server{}, ErrUnhealthy
	}
	next := current.DesiredGeneration + 1
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Server{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_version(alias,generation,config,tools,config_hash,healthy,created_at) VALUES(?,?,?,?,?,1,?)`, alias, next, configJSON, toolsJSON, hashConfig(configJSON, toolsJSON), now.UnixMilli()); err != nil {
		return Server{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET desired_generation=?,last_healthy_generation=?,enabled=1,state=?,health=?,last_error='',updated_at=? WHERE alias=?`, next, next, StatePending, HealthHealthy, now.UnixMilli(), alias); err != nil {
		return Server{}, err
	}
	if err := appendAudit(ctx, tx, alias, next, "config.rollback", fmt.Sprintf("from generation %d", current.LastHealthyGeneration), now); err != nil {
		return Server{}, err
	}
	if err := tx.Commit(); err != nil {
		return Server{}, err
	}
	return s.getLocked(ctx, alias)
}

func (s *Store) Delete(ctx context.Context, alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT desired_generation FROM mcp_server WHERE alias=?`, alias).Scan(&generation); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendAudit(ctx, tx, alias, generation, "config.remove", "", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_server WHERE alias=?`, alias); err != nil {
		return err
	}
	secrets, err := s.loadSecretsLocked()
	if err != nil {
		return err
	}
	delete(secrets.Servers, alias)
	if err := s.persistSecretsLocked(secrets); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Guard(alias string) error {
	var failures int
	var until int64
	if err := s.db.QueryRow(`SELECT failure_count,circuit_until FROM mcp_server WHERE alias=? AND enabled=1`, alias).Scan(&failures, &until); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if failures >= 3 && time.UnixMilli(until).After(s.now()) {
		return ErrCircuitOpen
	}
	return nil
}

func (s *Store) Observe(alias string, latency time.Duration, callErr error) {
	now := s.now().UTC()
	if callErr == nil {
		_, _ = s.db.Exec(`UPDATE mcp_server SET health=?,latency_ms=?,failure_count=0,circuit_until=0,last_error='',updated_at=? WHERE alias=?`, HealthHealthy, latency.Milliseconds(), now.UnixMilli(), alias)
		return
	}
	message := SafeRuntimeError(callErr).Error()
	var failures int
	_ = s.db.QueryRow(`SELECT failure_count FROM mcp_server WHERE alias=?`, alias).Scan(&failures)
	failures++
	until := int64(0)
	if failures >= 3 {
		until = now.Add(60 * time.Second).UnixMilli()
	}
	_, _ = s.db.Exec(`UPDATE mcp_server SET health=?,latency_ms=?,failure_count=?,circuit_until=?,last_error=?,updated_at=? WHERE alias=?`, HealthUnhealthy, latency.Milliseconds(), failures, until, message, now.UnixMilli(), alias)
}

func (s *Store) serverSecrets(alias string) (serverSecrets, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadSecretsLocked()
	return state.Servers[alias], err
}

func normalizeKeys(keys []string) []string {
	unique := map[string]bool{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if namePattern.MatchString(key) {
			unique[key] = true
		}
	}
	out := make([]string, 0, len(unique))
	for key := range unique {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func normalizeHeaderKeys(keys []string) []string {
	unique := map[string]string{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key != "" && !strings.ContainsAny(key, "\r\n:") {
			unique[strings.ToLower(key)] = key
		}
	}
	out := make([]string, 0, len(unique))
	for _, key := range unique {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func normalizeScopes(scopes []string) []string {
	unique := map[string]bool{}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" && !strings.ContainsAny(scope, " \t\r\n") {
			unique[scope] = true
		}
	}
	out := make([]string, 0, len(unique))
	for scope := range unique {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

func sameKeys(keys []string, values map[string]string) bool {
	if len(keys) != len(values) {
		return false
	}
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func filepathIsAbs(path string) bool {
	return strings.HasPrefix(path, "/") && strings.TrimSpace(path) == path
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 1<<30)); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func digestCommand(command string, args []string) (string, error) {
	hash := sha256.New()
	writeFile := func(path string) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err := io.Copy(hash, io.LimitReader(file, 1<<30)); err != nil {
			return err
		}
		hash.Write([]byte{0})
		return nil
	}
	if err := writeFile(command); err != nil {
		return "", err
	}
	for _, arg := range args {
		hash.Write([]byte(arg))
		hash.Write([]byte{0})
		if !filepathIsAbs(arg) {
			continue
		}
		info, err := os.Stat(arg)
		if err == nil && info.Mode().IsRegular() {
			if err := writeFile(arg); err != nil {
				return "", err
			}
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// CommandDigest canonically pins an installed stdio executable together with
// its argument vector and the bytes of every absolute regular-file argument.
// Operators and the cloud CLI use the same derivation enforced by Put.
func CommandDigest(command string, args []string) (string, error) {
	return digestCommand(command, args)
}

func validatePublicHTTPS(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be a credential-free HTTPS URL without query or fragment")
	}
	addresses, err := net.DefaultResolver.LookupIP(context.Background(), "ip", parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("host does not resolve")
	}
	for _, address := range addresses {
		if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() {
			return errors.New("host resolves to a non-public address")
		}
	}
	return nil
}

func canonicalFunction(alias, name string) string {
	var builder strings.Builder
	for _, character := range alias + "__" + name {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9', character == '_', character == '-':
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
	}
	out := builder.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 1000 {
		message = message[:1000]
	}
	return message
}

// SafeRuntimeError retains an actionable failure category without retaining a
// remote response body, request header, command environment, or token-bearing
// URL in audit/status surfaces.
func SafeRuntimeError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("MCP server qualification timed out")
	case errors.Is(err, context.Canceled):
		return errors.New("MCP server qualification was cancelled")
	default:
		lower := strings.ToLower(err.Error())
		switch {
		case strings.Contains(lower, "certificate"), strings.Contains(lower, "tls"):
			return errors.New("MCP server TLS verification failed")
		case strings.Contains(lower, "no such host"), strings.Contains(lower, "does not resolve"):
			return errors.New("MCP server host could not be resolved")
		case strings.Contains(lower, "connection refused"):
			return errors.New("MCP server refused the connection")
		case strings.Contains(lower, "manifest drift"), strings.Contains(lower, "missing expected tool"), strings.Contains(lower, "unexpected tool"):
			return errors.New("MCP server tool inventory changed after classification")
		default:
			return errors.New("MCP server qualification or transport failed")
		}
	}
}

// Redact removes every credential value associated with alias from remote
// tool text before it can enter a model observation, trace, or client event.
func (s *Store) Redact(alias, content string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	secrets, err := s.serverSecrets(alias)
	if err != nil {
		return content
	}
	values := make([]string, 0, len(secrets.Env)+len(secrets.Headers)+5)
	for _, value := range secrets.Env {
		values = append(values, value)
	}
	for _, value := range secrets.Headers {
		values = append(values, value)
	}
	values = append(values, secrets.ClientSecret, secrets.OAuth.AccessToken, secrets.OAuth.RefreshToken)
	for _, value := range values {
		if len(value) >= 4 {
			content = strings.ReplaceAll(content, value, "[redacted]")
		}
	}
	return content
}
