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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"centra/executor/tool"
	"centra/packages/vault"

	_ "modernc.org/sqlite"
)

const (
	secretFile   = "mcp-credentials.vault"
	secretStore  = "neo.mcp.control"
	secretSchema = "mcp.credentials.v1"
)

type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	root   string
	vault  *vault.Session
	user   string
	now    func() time.Time
	closed bool
}

func Open(ctx context.Context, root string, session *vault.Session, user string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("MCP control plane root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("MCP control plane resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("MCP control plane create root: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("MCP control plane secure root: %w", err)
	}
	dsn, err := controlDSN(filepath.Join(abs, "mcp-control.db"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("MCP control plane open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MCP control plane connect: %w", err)
	}
	if err := os.Chmod(filepath.Join(abs, "mcp-control.db"), 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MCP control plane secure database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, root: abs, vault: session, user: strings.TrimSpace(user), now: time.Now}, nil
}

func controlDSN(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	for _, pragma := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(ON)"} {
		query.Add("_pragma", pragma)
	}
	return (&url.URL{Scheme: "file", Path: abs, RawQuery: query.Encode()}).String(), nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS mcp_server (
			alias TEXT PRIMARY KEY, display_name TEXT NOT NULL,
			desired_generation INTEGER NOT NULL, applied_generation INTEGER NOT NULL DEFAULT 0,
			last_healthy_generation INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL,
			health TEXT NOT NULL, latency_ms INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0, circuit_until INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_version (
			alias TEXT NOT NULL, generation INTEGER NOT NULL,
			config BLOB NOT NULL, tools BLOB NOT NULL, config_hash TEXT NOT NULL,
			healthy INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL,
			PRIMARY KEY(alias, generation),
			FOREIGN KEY(alias) REFERENCES mcp_server(alias) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS mcp_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT, alias TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 0, action TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS mcp_audit_alias ON mcp_audit(alias, id DESC)`,
		`CREATE INDEX IF NOT EXISTS mcp_server_state ON mcp_server(enabled, state, alias)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("MCP control plane migrate: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

func (s *Store) Put(ctx context.Context, request CreateRequest) (Server, error) {
	config, err := validateConfig(request.Config)
	if err != nil {
		return Server{}, err
	}
	if err := validateSecretKeys(config, request); err != nil {
		return Server{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	secrets, err := s.loadSecretsLocked()
	if err != nil {
		return Server{}, err
	}
	if hasSecrets(request) && (s.vault == nil || !s.vault.Encrypting()) {
		return Server{}, ErrEncryptionRequired
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Server{}, err
	}
	defer tx.Rollback()
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT desired_generation FROM mcp_server WHERE alias = ?`, config.Alias).Scan(&generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		generation = 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_server(alias, display_name, desired_generation, state, health, updated_at) VALUES(?,?,?,?,?,?)`, config.Alias, config.DisplayName, generation, StateCandidate, HealthUnknown, now.UnixMilli()); err != nil {
			return Server{}, err
		}
	case err != nil:
		return Server{}, err
	default:
		generation++
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_server SET display_name=?, desired_generation=?, enabled=0, state=?, health=?, latency_ms=0, failure_count=0, circuit_until=0, last_error='', updated_at=? WHERE alias=?`, config.DisplayName, generation, StateCandidate, HealthUnknown, now.UnixMilli(), config.Alias); err != nil {
			return Server{}, err
		}
	}
	configJSON, _ := json.Marshal(config)
	toolsJSON := []byte("[]")
	digest := hashConfig(configJSON, toolsJSON)
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_version(alias,generation,config,tools,config_hash,healthy,created_at) VALUES(?,?,?,?,?,0,?)`, config.Alias, generation, configJSON, toolsJSON, digest, now.UnixMilli()); err != nil {
		return Server{}, err
	}
	if err := appendAudit(ctx, tx, config.Alias, generation, "config.stage", config.Transport, now); err != nil {
		return Server{}, err
	}
	if hasSecrets(request) {
		serverSecret := secrets.Servers[config.Alias]
		serverSecret.Env = cloneMap(request.SecretEnv)
		serverSecret.Headers = cloneMap(request.SecretHeaders)
		serverSecret.ClientSecret = request.ClientSecret
		if serverSecret.Pending == nil {
			serverSecret.Pending = map[string]oauthPending{}
		}
		secrets.Servers[config.Alias] = serverSecret
	} else {
		delete(secrets.Servers, config.Alias)
	}
	if err := s.persistSecretsLocked(secrets); err != nil {
		return Server{}, err
	}
	if err := tx.Commit(); err != nil {
		return Server{}, err
	}
	return s.getLocked(ctx, config.Alias)
}

func (s *Store) List(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias FROM mcp_server ORDER BY lower(display_name), alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aliases := make([]string, 0)
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(aliases))
	for _, alias := range aliases {
		server, err := s.Get(ctx, alias)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, alias string) (Server, error) {
	if s == nil || s.db == nil {
		return Server{}, ErrNotFound
	}
	server, err := s.getLocked(ctx, alias)
	if err != nil || server.Config.OAuth == nil {
		return server, err
	}
	s.mu.Lock()
	secrets, secretErr := s.loadSecretsLocked()
	s.mu.Unlock()
	if secretErr == nil {
		server.OAuthConnected = strings.TrimSpace(secrets.Servers[server.Config.Alias].OAuth.AccessToken) != ""
	}
	return server, nil
}

func (s *Store) getLocked(ctx context.Context, alias string) (Server, error) {
	var server Server
	var configJSON, toolsJSON []byte
	var updated, circuitUntil int64
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT v.config,v.tools,s.state,s.enabled,s.desired_generation,s.applied_generation,s.last_healthy_generation,s.health,s.latency_ms,s.failure_count,s.circuit_until,s.last_error,s.updated_at
		FROM mcp_server s JOIN mcp_version v ON v.alias=s.alias AND v.generation=s.desired_generation WHERE s.alias=?`, strings.TrimSpace(alias)).Scan(
		&configJSON, &toolsJSON, &server.State, &enabled, &server.DesiredGeneration,
		&server.AppliedGeneration, &server.LastHealthyGeneration, &server.Health,
		&server.LatencyMS, &server.FailureCount, &circuitUntil, &server.LastError, &updated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal(configJSON, &server.Config); err != nil {
		return Server{}, err
	}
	if err := json.Unmarshal(toolsJSON, &server.Tools); err != nil {
		return Server{}, err
	}
	server.Enabled = enabled != 0
	server.UpdatedAt = time.UnixMilli(updated).UTC()
	if circuitUntil > 0 {
		server.CircuitUntil = time.UnixMilli(circuitUntil).UTC()
	}
	server.Circuit = circuitState(server.FailureCount, server.CircuitUntil, s.now())
	return server, nil
}

func hashConfig(config, tools []byte) string {
	hash := sha256.New()
	hash.Write(config)
	hash.Write([]byte{0})
	hash.Write(tools)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func appendAudit(ctx context.Context, tx *sql.Tx, alias string, generation int64, action, detail string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mcp_audit(alias,generation,action,detail,created_at) VALUES(?,?,?,?,?)`, alias, generation, action, strings.TrimSpace(detail), now.UnixMilli())
	return err
}

func (s *Store) Audit(ctx context.Context, alias string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id,alias,generation,action,detail,created_at FROM mcp_audit`
	args := []any{}
	if strings.TrimSpace(alias) != "" {
		query += ` WHERE alias=?`
		args = append(args, strings.TrimSpace(alias))
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		var created int64
		if err := rows.Scan(&event.ID, &event.Alias, &event.Generation, &event.Action, &event.Detail, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, event)
	}
	return out, rows.Err()
}

func circuitState(failures int, until time.Time, now time.Time) Circuit {
	if failures < 3 {
		return CircuitClosed
	}
	if until.After(now) {
		return CircuitOpen
	}
	return CircuitHalfOpen
}

func cloneMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func hasSecrets(request CreateRequest) bool {
	return request.Config.OAuth != nil || len(request.SecretEnv) > 0 || len(request.SecretHeaders) > 0 || strings.TrimSpace(request.ClientSecret) != ""
}

func (s *Store) secretAD() vault.AD {
	return vault.AD{User: s.user, Store: secretStore, Stream: "credentials", Schema: secretSchema}
}

func (s *Store) loadSecretsLocked() (secretState, error) {
	state := secretState{Servers: map[string]serverSecrets{}}
	data, err := os.ReadFile(filepath.Join(s.root, secretFile))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if s.vault == nil || !s.vault.Encrypting() || s.vault.UserVault() == nil {
		return state, ErrEncryptionRequired
	}
	plain, err := s.vault.UserVault().OpenFile(s.secretAD(), data)
	if err != nil {
		return state, fmt.Errorf("MCP credentials decrypt: %w", err)
	}
	if err := json.Unmarshal(plain, &state); err != nil {
		return state, fmt.Errorf("MCP credentials decode: %w", err)
	}
	if state.Servers == nil {
		state.Servers = map[string]serverSecrets{}
	}
	return state, nil
}

func (s *Store) persistSecretsLocked(state secretState) error {
	if len(state.Servers) == 0 {
		if err := os.Remove(filepath.Join(s.root, secretFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if s.vault == nil || !s.vault.Encrypting() {
		return ErrEncryptionRequired
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}
	sealed, err := s.vault.MaybeSealFile(s.secretAD(), plain)
	for index := range plain {
		plain[index] = 0
	}
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, secretFile)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, sealed, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

func (s *Store) SearchTools(ctx context.Context, query string, limit int) ([]Tool, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	servers, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		tool  Tool
		score int
	}
	items := make([]scored, 0)
	for _, server := range servers {
		for _, candidate := range server.Tools {
			haystack := strings.ToLower(candidate.Function + " " + candidate.Name + " " + candidate.Description + " " + server.Config.DisplayName)
			score := 0
			for _, term := range terms {
				if candidate.Name == term || candidate.Function == term {
					score += 10
				} else if strings.Contains(haystack, term) {
					score += 2
				}
			}
			if len(terms) == 0 || score > 0 {
				items = append(items, scored{tool: candidate, score: score})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].tool.Function < items[j].tool.Function
		}
		return items[i].score > items[j].score
	})
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]Tool, len(items))
	for index := range items {
		out[index] = items[index].tool
	}
	return out, nil
}

func validEffect(effect string) bool { return tool.ValidSideEffectClasses[strings.TrimSpace(effect)] }
