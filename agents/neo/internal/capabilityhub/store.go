// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	mu     sync.Mutex
	db     *sql.DB
	root   string
	now    func() time.Time
	closed bool
}

func Open(ctx context.Context, root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("capability hub root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("capability hub resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("capability hub create root: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("capability hub secure root: %w", err)
	}
	dsn, err := sqliteDSN(filepath.Join(abs, "capabilities.db"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("capability hub open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("capability hub connect: %w", err)
	}
	if err := os.Chmod(filepath.Join(abs, "capabilities.db"), 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("capability hub secure database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, root: abs, now: time.Now}, nil
}

func sqliteDSN(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("capability hub resolve database: %w", err)
	}
	values := url.Values{}
	for _, pragma := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(ON)"} {
		values.Add("_pragma", pragma)
	}
	return (&url.URL{Scheme: "file", Path: abs, RawQuery: values.Encode()}).String(), nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS capability_version (
			slug TEXT NOT NULL,
			version TEXT NOT NULL,
			digest TEXT NOT NULL,
			canonical_hash TEXT NOT NULL,
			display TEXT NOT NULL,
			description TEXT NOT NULL,
			publisher TEXT NOT NULL,
			source_type TEXT NOT NULL,
			source_ref TEXT NOT NULL,
			package_root TEXT NOT NULL,
			state TEXT NOT NULL,
			pinned INTEGER NOT NULL DEFAULT 0,
			declared_tools BLOB NOT NULL,
			declared_subskills BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			verified_at INTEGER,
			activated_at INTEGER,
			last_error TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(slug, version)
		)`,
		`CREATE TABLE IF NOT EXISTS capability_grant (
			slug TEXT NOT NULL,
			version TEXT NOT NULL,
			permission TEXT NOT NULL,
			granted_at INTEGER NOT NULL,
			PRIMARY KEY(slug, version, permission),
			FOREIGN KEY(slug, version) REFERENCES capability_version(slug, version) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS capability_current (
			slug TEXT PRIMARY KEY,
			version TEXT NOT NULL,
			digest TEXT NOT NULL,
			FOREIGN KEY(slug, version) REFERENCES capability_version(slug, version)
		)`,
		`CREATE TABLE IF NOT EXISTS capability_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			slug TEXT NOT NULL,
			version TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			projected INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS capability_version_state ON capability_version(state, slug, version)`,
		`CREATE INDEX IF NOT EXISTS capability_audit_slug ON capability_audit(slug, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("capability hub migrate: %w", err)
		}
	}
	if err := ensureAuditProjectionColumn(ctx, db); err != nil {
		return err
	}
	return nil
}

func ensureAuditProjectionColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(capability_audit)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "projected" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE capability_audit ADD COLUMN projected INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("capability hub add audit projection: %w", err)
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

func (s *Store) List(ctx context.Context, query Query) ([]Capability, error) {
	where := []string{"state <> ?"}
	args := []any{StateUninstalled}
	if query.State != "" {
		where = append(where, "state = ?")
		args = append(args, query.State)
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		where = append(where, "(lower(slug) LIKE ? OR lower(display) LIKE ? OR lower(description) LIKE ?)")
		like := "%" + strings.ToLower(search) + "%"
		args = append(args, like, like, like)
	}
	rows, err := s.db.QueryContext(ctx, selectCapability+" WHERE "+strings.Join(where, " AND ")+" ORDER BY lower(display), slug, version DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("capability hub list: %w", err)
	}
	defer rows.Close()
	var out []Capability
	for rows.Next() {
		capability, err := scanCapability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, capability)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range out {
		grants, err := s.grants(ctx, out[index].Slug, out[index].Version)
		if err != nil {
			return nil, err
		}
		out[index].Granted = grants
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, slug, version string) (Capability, error) {
	row := s.db.QueryRowContext(ctx, selectCapability+" WHERE slug = ? AND version = ?", slug, version)
	capability, err := scanCapability(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Capability{}, ErrNotFound
	}
	if err != nil {
		return Capability{}, err
	}
	capability.Granted, err = s.grants(ctx, slug, version)
	return capability, err
}

func (s *Store) Versions(ctx context.Context, slug string) ([]Capability, error) {
	rows, err := s.db.QueryContext(ctx, selectCapability+" WHERE slug = ? ORDER BY created_at DESC", slug)
	if err != nil {
		return nil, fmt.Errorf("capability hub versions: %w", err)
	}
	defer rows.Close()
	var out []Capability
	for rows.Next() {
		capability, err := scanCapability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, capability)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range out {
		out[index].Granted, err = s.grants(ctx, slug, out[index].Version)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) ActiveInstructions(ctx context.Context, slug string) (Capability, string, error) {
	var version string
	err := s.db.QueryRowContext(ctx, `SELECT version FROM capability_current WHERE slug = ?`, slug).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return Capability{}, "", ErrNotFound
	}
	if err != nil {
		return Capability{}, "", err
	}
	capability, packageRoot, err := s.getWithPackageRoot(ctx, slug, version)
	if err != nil {
		return Capability{}, "", err
	}
	if capability.State != StateActive {
		return Capability{}, "", ErrNotFound
	}
	capability.Granted, err = s.grants(ctx, capability.Slug, capability.Version)
	if err != nil {
		return Capability{}, "", err
	}
	path := filepath.Join(packageRoot, capability.Slug, "SKILL.md")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return capability, "", nil
	}
	if err != nil {
		return Capability{}, "", err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
	if err != nil {
		return Capability{}, "", err
	}
	if len(body) > 128<<10 {
		return Capability{}, "", fmt.Errorf("capability instructions exceed read bound")
	}
	return capability, string(body), nil
}

func (s *Store) Grant(ctx context.Context, slug, version string, permissions []string) (Capability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	capability, err := s.getWithoutGrants(ctx, slug, version)
	if err != nil {
		return Capability{}, err
	}
	if capability.State == StateActive || capability.State == StateUninstalled {
		return Capability{}, ErrInvalidTransition
	}
	allowed := make(map[string]struct{}, len(capability.DeclaredTools)+len(capability.DeclaredSubSkills))
	for _, item := range append(append([]string{}, capability.DeclaredTools...), capability.DeclaredSubSkills...) {
		allowed[item] = struct{}{}
	}
	unique := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		permission = strings.TrimSpace(permission)
		if _, ok := allowed[permission]; !ok {
			return Capability{}, fmt.Errorf("%w: %s", ErrGrantRequired, permission)
		}
		unique[permission] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Capability{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_grant WHERE slug = ? AND version = ?`, slug, version); err != nil {
		return Capability{}, err
	}
	now := s.now().UTC()
	for permission := range unique {
		if _, err := tx.ExecContext(ctx, `INSERT INTO capability_grant(slug, version, permission, granted_at) VALUES(?,?,?,?)`, slug, version, permission, now.UnixMilli()); err != nil {
			return Capability{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE capability_version SET state = ?, verified_at = NULL, updated_at = ?, last_error = '' WHERE slug = ? AND version = ?`, StateQuarantine, now.UnixMilli(), slug, version); err != nil {
		return Capability{}, err
	}
	if err := appendAudit(ctx, tx, slug, version, "grant.replace", fmt.Sprintf("%d permissions", len(unique)), now); err != nil {
		return Capability{}, err
	}
	if err := tx.Commit(); err != nil {
		return Capability{}, err
	}
	return s.Get(ctx, slug, version)
}

func (s *Store) Verify(ctx context.Context, slug, version string, verification Verification) (Capability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	capability, packageRoot, err := s.getWithPackageRoot(ctx, slug, version)
	if err != nil {
		return Capability{}, err
	}
	if capability.State == StateUninstalled {
		return Capability{}, ErrInvalidTransition
	}
	if capability.State == StateActive {
		return Capability{}, ErrInvalidTransition
	}
	grants, err := s.grants(ctx, slug, version)
	if err != nil {
		return Capability{}, err
	}
	granted := make(map[string]struct{}, len(grants))
	for _, permission := range grants {
		granted[permission] = struct{}{}
	}
	for _, permission := range append(append([]string{}, capability.DeclaredTools...), capability.DeclaredSubSkills...) {
		if _, ok := granted[permission]; !ok {
			return s.verificationFailure(ctx, capability, fmt.Errorf("%w: %s", ErrGrantRequired, permission))
		}
	}
	loaded, err := loadPackage(packageRoot, slug, version)
	if err != nil {
		return s.verificationFailure(ctx, capability, err)
	}
	digest, err := digestDirectory(filepath.Join(packageRoot, slug))
	if err != nil {
		return s.verificationFailure(ctx, capability, err)
	}
	if digest != capability.Digest || loaded.CanonicalHash != capability.CanonicalHash {
		return s.verificationFailure(ctx, capability, fmt.Errorf("%w: package content changed after quarantine", ErrUnsafePackage))
	}
	for _, toolURI := range capability.DeclaredTools {
		name := toolName(toolURI)
		resolved, ok := verification.AvailableTools[name]
		if !ok || resolved == "" {
			return s.verificationFailure(ctx, capability, fmt.Errorf("%w: %s", ErrToolUnavailable, toolURI))
		}
	}
	if len(capability.DeclaredTools) > 0 {
		tests, err := loadToolTests(filepath.Join(packageRoot, slug), capability.DeclaredTools)
		if err != nil {
			return s.verificationFailure(ctx, capability, err)
		}
		if verification.RunTool == nil {
			return s.verificationFailure(ctx, capability, fmt.Errorf("%w: live tool runner unavailable", ErrVerificationRequired))
		}
		for _, test := range tests {
			shortName := toolName(test.Tool)
			resolved := verification.AvailableTools[shortName]
			if !verification.ReadOnlyTools[resolved] {
				return s.verificationFailure(ctx, capability, fmt.Errorf("%w: test tool %s is not read-only", ErrVerificationRequired, test.Tool))
			}
			result, err := verification.RunTool(resolved, test.Arguments)
			if err != nil {
				return s.verificationFailure(ctx, capability, fmt.Errorf("%w: %s: %v", ErrVerificationRequired, test.Name, err))
			}
			if result.IsError {
				return s.verificationFailure(ctx, capability, fmt.Errorf("%w: %s returned a tool error", ErrVerificationRequired, test.Name))
			}
			if test.ExpectContains != "" && !strings.Contains(result.Content, test.ExpectContains) {
				return s.verificationFailure(ctx, capability, fmt.Errorf("%w: %s did not produce expected evidence", ErrVerificationRequired, test.Name))
			}
		}
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE capability_version SET state = ?, verified_at = ?, updated_at = ?, last_error = '' WHERE slug = ? AND version = ?`, StateVerified, now.UnixMilli(), now.UnixMilli(), slug, version); err != nil {
		return Capability{}, err
	}
	if err := s.audit(ctx, slug, version, "verify.success", capability.Digest, now); err != nil {
		return Capability{}, err
	}
	return s.Get(ctx, slug, version)
}

func (s *Store) verificationFailure(ctx context.Context, capability Capability, cause error) (Capability, error) {
	now := s.now().UTC()
	_, _ = s.db.ExecContext(ctx, `UPDATE capability_version SET state = ?, verified_at = NULL, updated_at = ?, last_error = ? WHERE slug = ? AND version = ?`, StateQuarantine, now.UnixMilli(), cause.Error(), capability.Slug, capability.Version)
	_ = s.audit(ctx, capability.Slug, capability.Version, "verify.failure", cause.Error(), now)
	return Capability{}, cause
}

func (s *Store) Activate(ctx context.Context, slug, version string) (Capability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	capability, err := s.getWithoutGrants(ctx, slug, version)
	if err != nil {
		return Capability{}, err
	}
	if capability.State != StateVerified && capability.State != StateDisabled && capability.State != StateActive {
		return Capability{}, ErrInvalidTransition
	}
	if capability.State == StateDisabled && capability.VerifiedAt == nil {
		return Capability{}, ErrInvalidTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Capability{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE capability_version SET state = ?, updated_at = ? WHERE slug = ? AND state = ?`, StateDisabled, now.UnixMilli(), slug, StateActive); err != nil {
		return Capability{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE capability_version SET state = ?, activated_at = ?, updated_at = ? WHERE slug = ? AND version = ?`, StateActive, now.UnixMilli(), now.UnixMilli(), slug, version); err != nil {
		return Capability{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO capability_current(slug, version, digest) VALUES(?,?,?) ON CONFLICT(slug) DO UPDATE SET version=excluded.version, digest=excluded.digest`, slug, version, capability.Digest); err != nil {
		return Capability{}, err
	}
	if err := appendAudit(ctx, tx, slug, version, "activate", capability.Digest, now); err != nil {
		return Capability{}, err
	}
	if err := tx.Commit(); err != nil {
		return Capability{}, err
	}
	return s.Get(ctx, slug, version)
}

func (s *Store) Disable(ctx context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT version FROM capability_current WHERE slug = ?`, slug).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE capability_version SET state = ?, updated_at = ? WHERE slug = ? AND version = ?`, StateDisabled, now.UnixMilli(), slug, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_current WHERE slug = ?`, slug); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, slug, version, "disable", "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Pin(ctx context.Context, slug, version string, pinned bool) (Capability, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE capability_version SET pinned = ?, updated_at = ? WHERE slug = ? AND version = ? AND state <> ?`, pinned, now.UnixMilli(), slug, version, StateUninstalled)
	if err != nil {
		return Capability{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return Capability{}, ErrNotFound
	}
	_ = s.audit(ctx, slug, version, "pin", fmt.Sprintf("%t", pinned), now)
	return s.Get(ctx, slug, version)
}

func (s *Store) Rollback(ctx context.Context, slug string) (Capability, error) {
	rows, err := s.Versions(ctx, slug)
	if err != nil {
		return Capability{}, err
	}
	for _, capability := range rows {
		if capability.State == StateDisabled && capability.VerifiedAt != nil {
			return s.Activate(ctx, slug, capability.Version)
		}
	}
	return Capability{}, ErrNotFound
}

func (s *Store) Uninstall(ctx context.Context, slug, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	capability, err := s.getWithoutGrants(ctx, slug, version)
	if err != nil {
		return err
	}
	if capability.State == StateActive || capability.Pinned {
		return ErrInvalidTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE capability_version SET state = ?, updated_at = ? WHERE slug = ? AND version = ?`, StateUninstalled, now.UnixMilli(), slug, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_grant WHERE slug = ? AND version = ?`, slug, version); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, slug, version, "uninstall", capability.Digest, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Audit(ctx context.Context, slug string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, version, action, detail, created_at FROM capability_audit WHERE slug = ? ORDER BY id DESC LIMIT ?`, slug, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var created int64
		if err := rows.Scan(&event.ID, &event.Slug, &event.Version, &event.Action, &event.Detail, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = fromMillis(created)
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) PendingProvenance(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, version, action, detail, created_at FROM capability_audit WHERE projected = 0 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var created int64
		if err := rows.Scan(&event.ID, &event.Slug, &event.Version, &event.Action, &event.Detail, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = fromMillis(created)
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) MarkProvenanceProjected(ctx context.Context, auditID int64) error {
	if auditID <= 0 {
		return fmt.Errorf("capability hub invalid audit id")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE capability_audit SET projected = 1 WHERE id = ?`, auditID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

const selectCapability = `SELECT slug, version, digest, canonical_hash, display, description, publisher, source_type, source_ref, state, pinned, declared_tools, declared_subskills, created_at, updated_at, verified_at, activated_at, last_error FROM capability_version`

type scanner interface{ Scan(...any) error }

func scanCapability(row scanner) (Capability, error) {
	var capability Capability
	var toolsJSON, subsJSON []byte
	var created, updated int64
	var verified, activated sql.NullInt64
	if err := row.Scan(&capability.Slug, &capability.Version, &capability.Digest, &capability.CanonicalHash, &capability.Display, &capability.Description, &capability.Publisher, &capability.SourceType, &capability.SourceRef, &capability.State, &capability.Pinned, &toolsJSON, &subsJSON, &created, &updated, &verified, &activated, &capability.LastError); err != nil {
		return Capability{}, err
	}
	if err := json.Unmarshal(toolsJSON, &capability.DeclaredTools); err != nil {
		return Capability{}, fmt.Errorf("capability hub decode tools: %w", err)
	}
	if err := json.Unmarshal(subsJSON, &capability.DeclaredSubSkills); err != nil {
		return Capability{}, fmt.Errorf("capability hub decode subskills: %w", err)
	}
	capability.CreatedAt = fromMillis(created)
	capability.UpdatedAt = fromMillis(updated)
	if verified.Valid {
		value := fromMillis(verified.Int64)
		capability.VerifiedAt = &value
	}
	if activated.Valid {
		value := fromMillis(activated.Int64)
		capability.ActivatedAt = &value
	}
	return capability, nil
}

func (s *Store) getWithoutGrants(ctx context.Context, slug, version string) (Capability, error) {
	capability, _, err := s.getWithPackageRoot(ctx, slug, version)
	return capability, err
}

func (s *Store) getWithPackageRoot(ctx context.Context, slug, version string) (Capability, string, error) {
	row := s.db.QueryRowContext(ctx, selectCapabilityWithRoot+" WHERE slug = ? AND version = ?", slug, version)
	var packageRoot string
	capability, err := scanCapabilityWithRoot(row, &packageRoot)
	if errors.Is(err, sql.ErrNoRows) {
		return Capability{}, "", ErrNotFound
	}
	return capability, packageRoot, err
}

const selectCapabilityWithRoot = `SELECT slug, version, digest, canonical_hash, display, description, publisher, source_type, source_ref, state, pinned, declared_tools, declared_subskills, created_at, updated_at, verified_at, activated_at, last_error, package_root FROM capability_version`

func scanCapabilityWithRoot(row scanner, packageRoot *string) (Capability, error) {
	var capability Capability
	var toolsJSON, subsJSON []byte
	var created, updated int64
	var verified, activated sql.NullInt64
	if err := row.Scan(&capability.Slug, &capability.Version, &capability.Digest, &capability.CanonicalHash, &capability.Display, &capability.Description, &capability.Publisher, &capability.SourceType, &capability.SourceRef, &capability.State, &capability.Pinned, &toolsJSON, &subsJSON, &created, &updated, &verified, &activated, &capability.LastError, packageRoot); err != nil {
		return Capability{}, err
	}
	if err := json.Unmarshal(toolsJSON, &capability.DeclaredTools); err != nil {
		return Capability{}, err
	}
	if err := json.Unmarshal(subsJSON, &capability.DeclaredSubSkills); err != nil {
		return Capability{}, err
	}
	capability.CreatedAt, capability.UpdatedAt = fromMillis(created), fromMillis(updated)
	if verified.Valid {
		value := fromMillis(verified.Int64)
		capability.VerifiedAt = &value
	}
	if activated.Valid {
		value := fromMillis(activated.Int64)
		capability.ActivatedAt = &value
	}
	return capability, nil
}

func (s *Store) grants(ctx context.Context, slug, version string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT permission FROM capability_grant WHERE slug = ? AND version = ? ORDER BY permission`, slug, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func (s *Store) audit(ctx context.Context, slug, version, action, detail string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO capability_audit(slug, version, action, detail, created_at) VALUES(?,?,?,?,?)`, slug, version, action, detail, now.UnixMilli())
	return err
}

func appendAudit(ctx context.Context, tx *sql.Tx, slug, version, action, detail string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO capability_audit(slug, version, action, detail, created_at) VALUES(?,?,?,?,?)`, slug, version, action, detail, now.UnixMilli())
	return err
}

func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }

func toolName(uri string) string {
	withoutVersion := strings.SplitN(uri, "@", 2)[0]
	parts := strings.Split(strings.Trim(withoutVersion, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
