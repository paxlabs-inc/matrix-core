// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package channelgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matrix/vault"

	_ "modernc.org/sqlite"
)

var ErrIdempotencyConflict = errors.New("channel idempotency key was reused with different content")

const (
	payloadStore  = "neo.channel.gateway"
	payloadSchema = "channel.envelope.v1"
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
		return nil, errors.New("channel gateway root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("channel gateway resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("channel gateway create root: %w", err)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("channel gateway secure root: %w", err)
	}
	dsn, err := gatewayDSN(filepath.Join(abs, "channel-gateway.db"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("channel gateway open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("channel gateway connect: %w", err)
	}
	dbPath := filepath.Join(abs, "channel-gateway.db")
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("channel gateway secure database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, root: abs, vault: session, user: strings.TrimSpace(user), now: time.Now}, nil
}

func gatewayDSN(path string) (string, error) {
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
		`CREATE TABLE IF NOT EXISTS channel_binding (
			channel TEXT NOT NULL, account_id TEXT NOT NULL, external_conversation_id TEXT NOT NULL,
			neo_conversation_id TEXT NOT NULL, scope TEXT NOT NULL, participant_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			PRIMARY KEY(channel, account_id, external_conversation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_inbound (
			channel TEXT NOT NULL, account_id TEXT NOT NULL, event_key TEXT NOT NULL,
			envelope_id TEXT NOT NULL, digest TEXT NOT NULL, status TEXT NOT NULL,
			neo_conversation_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '', received_at INTEGER NOT NULL, completed_at INTEGER,
			PRIMARY KEY(channel, account_id, event_key)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_delivery (
			id TEXT PRIMARY KEY, channel TEXT NOT NULL, account_id TEXT NOT NULL,
			idempotency_key TEXT NOT NULL, digest TEXT NOT NULL, envelope BLOB NOT NULL, sealed INTEGER NOT NULL,
			state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
			external_message_id TEXT NOT NULL DEFAULT '', receipt_code TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			UNIQUE(channel, account_id, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS channel_delivery_due ON channel_delivery(channel, account_id, state, next_attempt_at)`,
		`CREATE TABLE IF NOT EXISTS channel_rate (
			channel TEXT NOT NULL, account_id TEXT NOT NULL, next_allowed_at INTEGER NOT NULL,
			PRIMARY KEY(channel, account_id)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_pending (
			channel TEXT NOT NULL, account_id TEXT NOT NULL, external_conversation_id TEXT NOT NULL,
			kind TEXT NOT NULL, run_id TEXT NOT NULL, node_id TEXT NOT NULL, created_at INTEGER NOT NULL,
			PRIMARY KEY(channel, account_id, external_conversation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT, channel TEXT NOT NULL, account_id TEXT NOT NULL,
			action TEXT NOT NULL, object_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS channel_audit_channel ON channel_audit(channel, account_id, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("channel gateway migrate: %w", err)
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

func (s *Store) Bind(ctx context.Context, address Address, neoConversation string) error {
	if s == nil {
		return errors.New("channel gateway is disabled")
	}
	neoConversation = strings.TrimSpace(neoConversation)
	if strings.TrimSpace(string(address.Channel)) == "" || strings.TrimSpace(address.AccountID) == "" || strings.TrimSpace(address.ConversationID) == "" || neoConversation == "" {
		return errors.New("complete channel and Neo conversation identities are required")
	}
	if address.Scope != ScopeDirect && address.Scope != ScopeGroup {
		return errors.New("scope must be direct or group")
	}
	now := s.now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_binding
		(channel, account_id, external_conversation_id, neo_conversation_id, scope, participant_id, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(channel, account_id, external_conversation_id) DO UPDATE SET
		neo_conversation_id=excluded.neo_conversation_id, scope=excluded.scope,
		participant_id=excluded.participant_id, updated_at=excluded.updated_at`,
		address.Channel, address.AccountID, address.ConversationID, neoConversation, address.Scope, address.ParticipantID, now, now)
	if err != nil {
		return fmt.Errorf("channel gateway bind: %w", err)
	}
	return s.auditLocked(ctx, address.Channel, address.AccountID, "binding.upsert", neoConversation, string(address.Scope), now)
}

func (s *Store) Resolve(ctx context.Context, address Address) (string, bool, error) {
	if s == nil {
		return "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var neoConversation string
	err := s.db.QueryRowContext(ctx, `SELECT neo_conversation_id FROM channel_binding
		WHERE channel=? AND account_id=? AND external_conversation_id=?`,
		address.Channel, address.AccountID, address.ConversationID).Scan(&neoConversation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return neoConversation, err == nil, err
}

func (s *Store) Unbind(ctx context.Context, channel Channel, accountID, externalConversation string) error {
	if s == nil {
		return nil
	}
	now := s.now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM channel_binding WHERE channel=? AND account_id=? AND external_conversation_id=?`,
		channel, accountID, externalConversation); err != nil {
		return err
	}
	return s.auditLocked(ctx, channel, accountID, "binding.remove", externalConversation, "", now)
}

func (s *Store) RecordAudit(ctx context.Context, channel Channel, accountID, action, objectID, detail string) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(string(channel)) == "" || strings.TrimSpace(action) == "" {
		return errors.New("channel and audit action are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auditLocked(ctx, channel, accountID, action, objectID, detail, s.now().UTC().UnixMilli())
}

func (s *Store) ClaimInbound(ctx context.Context, envelope Envelope) (InboundClaim, error) {
	if s == nil {
		return InboundClaim{State: ClaimNew, EnvelopeID: envelope.ID, Status: "received"}, nil
	}
	if envelope.Direction != Inbound {
		return InboundClaim{}, errors.New("inbound claim requires an inbound envelope")
	}
	if err := envelope.Validate(); err != nil {
		return InboundClaim{}, err
	}
	eventKey := strings.TrimSpace(envelope.ExternalEventID)
	if eventKey == "" {
		eventKey = envelope.IdempotencyKey
	}
	_, digest, err := canonicalEnvelope(envelope)
	if err != nil {
		return InboundClaim{}, err
	}
	if envelope.ID == "" {
		envelope.ID = newID()
	}
	now := s.now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `INSERT INTO channel_inbound
		(channel, account_id, event_key, envelope_id, digest, status, neo_conversation_id, received_at)
		VALUES(?,?,?,?,?,'received',?,?) ON CONFLICT(channel, account_id, event_key) DO NOTHING`,
		envelope.Address.Channel, envelope.Address.AccountID, eventKey, envelope.ID, digest, envelope.NeoConversation, now)
	if err != nil {
		return InboundClaim{}, fmt.Errorf("channel gateway claim inbound: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return InboundClaim{}, err
	}
	if rows == 1 {
		_ = s.auditLocked(ctx, envelope.Address.Channel, envelope.Address.AccountID, "inbound.claim", envelope.ID, string(envelope.Kind), now)
		return InboundClaim{State: ClaimNew, EnvelopeID: envelope.ID, NeoConversation: envelope.NeoConversation, Status: "received"}, nil
	}
	var existingDigest string
	claim := InboundClaim{State: ClaimDuplicate}
	err = s.db.QueryRowContext(ctx, `SELECT envelope_id, digest, status, neo_conversation_id, run_id
		FROM channel_inbound WHERE channel=? AND account_id=? AND event_key=?`,
		envelope.Address.Channel, envelope.Address.AccountID, eventKey).Scan(
		&claim.EnvelopeID, &existingDigest, &claim.Status, &claim.NeoConversation, &claim.RunID)
	if err != nil {
		return InboundClaim{}, err
	}
	if existingDigest != digest {
		return InboundClaim{}, ErrIdempotencyConflict
	}
	return claim, nil
}

func (s *Store) SetPending(ctx context.Context, pending PendingAction) error {
	if s == nil {
		return errors.New("channel gateway is disabled")
	}
	if pending.Kind != KindApproval || strings.TrimSpace(pending.RunID) == "" || strings.TrimSpace(pending.NodeID) == "" {
		return errors.New("complete pending approval identity is required")
	}
	if strings.TrimSpace(string(pending.Address.Channel)) == "" || strings.TrimSpace(pending.Address.AccountID) == "" || strings.TrimSpace(pending.Address.ConversationID) == "" {
		return errors.New("pending approval address is required")
	}
	now := pending.CreatedAt.UTC().UnixMilli()
	if pending.CreatedAt.IsZero() {
		now = s.now().UTC().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_pending(channel, account_id, external_conversation_id, kind, run_id, node_id, created_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(channel, account_id, external_conversation_id) DO UPDATE SET
		kind=excluded.kind, run_id=excluded.run_id, node_id=excluded.node_id, created_at=excluded.created_at`,
		pending.Address.Channel, pending.Address.AccountID, pending.Address.ConversationID, pending.Kind, pending.RunID, pending.NodeID, now)
	if err != nil {
		return err
	}
	return s.auditLocked(ctx, pending.Address.Channel, pending.Address.AccountID, "pending.upsert", pending.RunID, string(pending.Kind), now)
}

func (s *Store) Pending(ctx context.Context, address Address) (PendingAction, bool, error) {
	if s == nil {
		return PendingAction{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending PendingAction
	var created int64
	pending.Address = address
	err := s.db.QueryRowContext(ctx, `SELECT kind, run_id, node_id, created_at FROM channel_pending
		WHERE channel=? AND account_id=? AND external_conversation_id=?`, address.Channel, address.AccountID, address.ConversationID).Scan(
		&pending.Kind, &pending.RunID, &pending.NodeID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingAction{}, false, nil
	}
	if err != nil {
		return PendingAction{}, false, err
	}
	pending.CreatedAt = time.UnixMilli(created).UTC()
	return pending, true, nil
}

func (s *Store) ClearPending(ctx context.Context, address Address, runID, nodeID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM channel_pending WHERE channel=? AND account_id=? AND external_conversation_id=? AND run_id=? AND node_id=?`,
		address.Channel, address.AccountID, address.ConversationID, runID, nodeID)
	if err != nil {
		return err
	}
	return s.auditLocked(ctx, address.Channel, address.AccountID, "pending.clear", runID, "", s.now().UTC().UnixMilli())
}

func (s *Store) CompleteInbound(ctx context.Context, envelope Envelope, neoConversation, runID string) error {
	return s.finishInbound(ctx, envelope, "completed", neoConversation, runID, "")
}

func (s *Store) FailInbound(ctx context.Context, envelope Envelope, message string) error {
	return s.finishInbound(ctx, envelope, "failed", envelope.NeoConversation, envelope.RunID, message)
}

func (s *Store) finishInbound(ctx context.Context, envelope Envelope, status, neoConversation, runID, message string) error {
	if s == nil {
		return nil
	}
	eventKey := strings.TrimSpace(envelope.ExternalEventID)
	if eventKey == "" {
		eventKey = envelope.IdempotencyKey
	}
	now := s.now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE channel_inbound SET status=?, neo_conversation_id=?, run_id=?,
		last_error=?, completed_at=? WHERE channel=? AND account_id=? AND event_key=?`,
		status, neoConversation, runID, boundedError(message), now,
		envelope.Address.Channel, envelope.Address.AccountID, eventKey)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return errors.New("channel inbound claim does not exist")
	}
	return s.auditLocked(ctx, envelope.Address.Channel, envelope.Address.AccountID, "inbound."+status, runID, "", now)
}

func (s *Store) QueueOutbound(ctx context.Context, envelope Envelope) (Delivery, bool, error) {
	if s == nil {
		return Delivery{}, false, errors.New("channel gateway is disabled")
	}
	if envelope.Direction != Outbound {
		return Delivery{}, false, errors.New("outbound queue requires an outbound envelope")
	}
	if err := envelope.Validate(); err != nil {
		return Delivery{}, false, err
	}
	if envelope.ID == "" {
		envelope.ID = newID()
	}
	if envelope.OccurredAt.IsZero() {
		envelope.OccurredAt = s.now().UTC()
	}
	_, digest, err := canonicalEnvelope(envelope)
	if err != nil {
		return Delivery{}, false, err
	}
	persisted, err := json.Marshal(envelope)
	if err != nil {
		return Delivery{}, false, err
	}
	id := newID()
	payload, sealed, err := s.sealEnvelope(id, persisted)
	if err != nil {
		return Delivery{}, false, err
	}
	now := s.now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `INSERT INTO channel_delivery
		(id, channel, account_id, idempotency_key, digest, envelope, sealed, state, next_attempt_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,'queued',?,?,?) ON CONFLICT(channel, account_id, idempotency_key) DO NOTHING`,
		id, envelope.Address.Channel, envelope.Address.AccountID, envelope.IdempotencyKey, digest, payload, sealed, now, now, now)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("channel gateway queue outbound: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Delivery{}, false, err
	}
	if rows == 1 {
		_ = s.auditLocked(ctx, envelope.Address.Channel, envelope.Address.AccountID, "delivery.queue", id, string(envelope.Kind)+":"+envelope.SideEffectClass, now)
		delivery, err := s.deliveryLocked(ctx, id)
		return delivery, true, err
	}
	var existingID, existingDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT id, digest FROM channel_delivery
		WHERE channel=? AND account_id=? AND idempotency_key=?`,
		envelope.Address.Channel, envelope.Address.AccountID, envelope.IdempotencyKey).Scan(&existingID, &existingDigest); err != nil {
		return Delivery{}, false, err
	}
	if existingDigest != digest {
		return Delivery{}, false, ErrIdempotencyConflict
	}
	delivery, err := s.deliveryLocked(ctx, existingID)
	return delivery, false, err
}

func (s *Store) Due(ctx context.Context, channel Channel, accountID string, limit int) ([]Delivery, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM channel_delivery WHERE channel=? AND account_id=?
		AND state IN ('queued','retrying','sending') AND next_attempt_at<=? ORDER BY created_at, id LIMIT ?`,
		channel, accountID, s.now().UTC().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	items := make([]Delivery, 0, len(ids))
	for _, id := range ids {
		item, err := s.deliveryLocked(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) BeginAttempt(ctx context.Context, id string, minimumInterval time.Duration) (Delivery, time.Duration, error) {
	if s == nil {
		return Delivery{}, 0, errors.New("channel gateway is disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.deliveryLocked(ctx, id)
	if err != nil {
		return Delivery{}, 0, err
	}
	if item.State == DeliveryDelivered || item.State == DeliveryFailed {
		return item, 0, nil
	}
	now := s.now().UTC()
	var nextAllowed int64
	err = s.db.QueryRowContext(ctx, `SELECT next_allowed_at FROM channel_rate WHERE channel=? AND account_id=?`,
		item.Envelope.Address.Channel, item.Envelope.Address.AccountID).Scan(&nextAllowed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, 0, err
	}
	if nextAllowed > now.UnixMilli() {
		wait := time.Duration(nextAllowed-now.UnixMilli()) * time.Millisecond
		return item, wait, nil
	}
	next := now.Add(minimumInterval).UnixMilli()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO channel_rate(channel, account_id, next_allowed_at) VALUES(?,?,?)
		ON CONFLICT(channel, account_id) DO UPDATE SET next_allowed_at=excluded.next_allowed_at`,
		item.Envelope.Address.Channel, item.Envelope.Address.AccountID, next); err != nil {
		return Delivery{}, 0, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE channel_delivery SET state='sending', attempts=attempts+1,
		next_attempt_at=?, updated_at=? WHERE id=?`, now.Add(2*time.Minute).UnixMilli(), now.UnixMilli(), id); err != nil {
		return Delivery{}, 0, err
	}
	updated, err := s.deliveryLocked(ctx, id)
	return updated, 0, err
}

func (s *Store) MarkDelivered(ctx context.Context, id string, receipt SendReceipt) error {
	return s.markDelivery(ctx, id, DeliveryDelivered, receipt, "", time.Time{})
}

func (s *Store) MarkFailed(ctx context.Context, id, message, code string) error {
	return s.markDelivery(ctx, id, DeliveryFailed, SendReceipt{Code: code}, message, time.Time{})
}

func (s *Store) MarkRetry(ctx context.Context, id, message, code string, at time.Time) error {
	return s.markDelivery(ctx, id, DeliveryRetrying, SendReceipt{Code: code}, message, at)
}

func (s *Store) markDelivery(ctx context.Context, id string, state DeliveryState, receipt SendReceipt, message string, at time.Time) error {
	if s == nil {
		return nil
	}
	now := s.now().UTC()
	if at.IsZero() {
		at = now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var channel Channel
	var accountID string
	if err := s.db.QueryRowContext(ctx, `SELECT channel, account_id FROM channel_delivery WHERE id=?`, id).Scan(&channel, &accountID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE channel_delivery SET state=?, next_attempt_at=?, external_message_id=?,
		receipt_code=?, last_error=?, updated_at=? WHERE id=?`, state, at.UnixMilli(), receipt.ExternalMessageID,
		receipt.Code, boundedError(message), now.UnixMilli(), id)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return s.auditLocked(ctx, channel, accountID, "delivery."+string(state), id, receipt.Code, now.UnixMilli())
}

func (s *Store) Delivery(ctx context.Context, id string) (Delivery, error) {
	if s == nil {
		return Delivery{}, sql.ErrNoRows
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveryLocked(ctx, id)
}

func (s *Store) deliveryLocked(ctx context.Context, id string) (Delivery, error) {
	var item Delivery
	var payload []byte
	var sealed int
	var channel string
	var created, updated, next int64
	err := s.db.QueryRowContext(ctx, `SELECT id, channel, envelope, sealed, state, attempts, next_attempt_at,
		external_message_id, receipt_code, last_error, created_at, updated_at FROM channel_delivery WHERE id=?`, id).Scan(
		&item.ID, &channel, &payload, &sealed, &item.State, &item.Attempts, &next,
		&item.ExternalMessageID, &item.ReceiptCode, &item.LastError, &created, &updated)
	if err != nil {
		return Delivery{}, err
	}
	plain, err := s.openEnvelope(id, payload, sealed != 0)
	if err != nil {
		return Delivery{}, err
	}
	if err := json.Unmarshal(plain, &item.Envelope); err != nil {
		return Delivery{}, fmt.Errorf("channel gateway decode delivery: %w", err)
	}
	item.CreatedAt = time.UnixMilli(created).UTC()
	item.UpdatedAt = time.UnixMilli(updated).UTC()
	item.NextAttemptAt = time.UnixMilli(next).UTC()
	return item, nil
}

func (s *Store) sealEnvelope(id string, plain []byte) ([]byte, int, error) {
	if s.vault == nil {
		return append([]byte(nil), plain...), 0, nil
	}
	sealed, err := s.vault.MaybeSealRecord(s.payloadAD(id), plain)
	if err != nil {
		return nil, 0, err
	}
	if s.vault.Encrypting() {
		return sealed, 1, nil
	}
	return sealed, 0, nil
}

func (s *Store) openEnvelope(id string, payload []byte, sealed bool) ([]byte, error) {
	if !sealed {
		return append([]byte(nil), payload...), nil
	}
	if s.vault == nil || !s.vault.Encrypting() || s.vault.UserVault() == nil {
		return nil, vault.ErrVaultRequired
	}
	return s.vault.UserVault().OpenRecord(s.payloadAD(id), payload)
}

func (s *Store) payloadAD(id string) vault.AD {
	return vault.AD{User: s.user, Store: payloadStore, Stream: id, Schema: payloadSchema}
}

func (s *Store) auditLocked(ctx context.Context, channel Channel, accountID, action, objectID, detail string, now int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_audit(channel, account_id, action, object_id, detail, created_at)
		VALUES(?,?,?,?,?,?)`, channel, accountID, action, objectID, boundedError(detail), now)
	return err
}

func canonicalEnvelope(envelope Envelope) ([]byte, string, error) {
	canonical := envelope
	canonical.ID = ""
	canonical.OccurredAt = time.Time{}
	canonical.NeoConversation = ""
	canonical.RunID = ""
	data, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

func newID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("channel gateway random source unavailable: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

func boundedError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}
