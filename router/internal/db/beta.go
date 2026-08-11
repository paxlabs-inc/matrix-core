// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PublicLaunchDisclosureVersion is the approval policy required before a
// self-service user may claim launch access or provision an environment.
const PublicLaunchDisclosureVersion = "public-launch-1"

// InviteCode mirrors the invite_codes table.
type InviteCode struct {
	Code            string
	MaxRedemptions  int
	RedemptionsUsed int
	ExpiresAt       *time.Time
	CreatedBy       string
	CreatedAt       time.Time
}

// InviteRedemption mirrors the invite_redemptions table.
type InviteRedemption struct {
	Code       string
	UserID     string
	RedeemedAt time.Time
}

// Consent mirrors the beta_consent table.
type Consent struct {
	UserID        string
	TrainingOptIn bool
	PolicyVersion string
	DecidedAt     time.Time
}

// DisclosureAck mirrors the beta_disclosure_ack table.
type DisclosureAck struct {
	UserID            string
	DisclosureVersion string
	AckAt             time.Time
}

// LaunchEntitlement is the authoritative subscription access granted to a user.
type LaunchEntitlement struct {
	UserID    string
	Plan      string
	Source    string
	StartsAt  time.Time
	EndsAt    time.Time
	ClaimedAt time.Time
}

// BugReport mirrors the bug_reports table.
type BugReport struct {
	ID            int64
	UserID        string
	Message       string
	Context       []byte
	AttachmentRef string
	Status        string
	CreatedAt     time.Time
}

// ErrInviteNotFound is returned when an invite code does not exist.
var ErrInviteNotFound = errors.New("db: invite code not found")

// ErrInviteExhausted is returned when an invite code has no redemptions remaining.
var ErrInviteExhausted = errors.New("db: invite code exhausted")

// ErrInviteExpired is returned when an invite code has passed its expiry.
var ErrInviteExpired = errors.New("db: invite code expired")

// ErrAlreadyRedeemed is returned when a user has already redeemed a given code.
var ErrAlreadyRedeemed = errors.New("db: user already redeemed this code")

// GenerateInviteCode inserts a new invite code. The caller is responsible
// for generating the crypto-strong code string.
func (d *DB) GenerateInviteCode(ctx context.Context, code string, maxRedemptions int, expiresAt *time.Time, createdBy string) error {
	const q = `
		INSERT INTO invite_codes (code, max_redemptions, expires_at, created_by)
		VALUES ($1, $2, $3, $4)
	`
	if _, err := d.pool.Exec(ctx, q, code, maxRedemptions, expiresAt, createdBy); err != nil {
		return fmt.Errorf("db: generate invite code: %w", err)
	}
	return nil
}

// ListInviteCodes returns all invite codes ordered by created_at desc.
func (d *DB) ListInviteCodes(ctx context.Context) ([]InviteCode, error) {
	const q = `
		SELECT code, max_redemptions, redemptions_used, expires_at,
		       COALESCE(created_by, ''), created_at
		  FROM invite_codes
		 ORDER BY created_at DESC
	`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list invite codes: %w", err)
	}
	defer rows.Close()
	var out []InviteCode
	for rows.Next() {
		var ic InviteCode
		if err := rows.Scan(&ic.Code, &ic.MaxRedemptions, &ic.RedemptionsUsed,
			&ic.ExpiresAt, &ic.CreatedBy, &ic.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan invite code: %w", err)
		}
		out = append(out, ic)
	}
	return out, rows.Err()
}

// RedeemInviteCode atomically validates + consumes one redemption of the
// given code for the given user. It checks:
//   - the code exists
//   - the code has not expired
//   - redemptions remain (used < max)
//   - the user has not already redeemed this code
//
// On success it increments redemptions_used and inserts an
// invite_redemptions row in a single transaction. On any failure no
// redemption is consumed.
func (d *DB) RedeemInviteCode(ctx context.Context, code, userID string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: redeem begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		used    int
		maxR    int
		expires *time.Time
	)
	const selQ = `
		SELECT redemptions_used, max_redemptions, expires_at
		  FROM invite_codes
		 WHERE code = $1
		 FOR UPDATE
	`
	err = tx.QueryRow(ctx, selQ, code).Scan(&used, &maxR, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteNotFound
		}
		return fmt.Errorf("db: redeem select: %w", err)
	}

	if expires != nil && time.Now().After(*expires) {
		return ErrInviteExpired
	}
	if used >= maxR {
		return ErrInviteExhausted
	}

	// Check for duplicate redemption by this user.
	const dupQ = `SELECT 1 FROM invite_redemptions WHERE code = $1 AND user_id = $2`
	var dummy int
	err = tx.QueryRow(ctx, dupQ, code, userID).Scan(&dummy)
	if err == nil {
		return ErrAlreadyRedeemed
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("db: redeem dup-check: %w", err)
	}

	const incrQ = `UPDATE invite_codes SET redemptions_used = redemptions_used + 1 WHERE code = $1`
	if _, err := tx.Exec(ctx, incrQ, code); err != nil {
		return fmt.Errorf("db: redeem incr: %w", err)
	}

	const insQ = `INSERT INTO invite_redemptions (code, user_id) VALUES ($1, $2)`
	if _, err := tx.Exec(ctx, insQ, code, userID); err != nil {
		return fmt.Errorf("db: redeem insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: redeem commit: %w", err)
	}
	return nil
}

// HasRedeemedInvite returns true if the user has redeemed any valid
// (non-expired-at-redemption-time) invite code. Checks all redemption
// rows for the user.
func (d *DB) HasRedeemedInvite(ctx context.Context, userID string) (bool, error) {
	const q = `SELECT 1 FROM invite_redemptions WHERE user_id = $1 LIMIT 1`
	var dummy int
	err := d.pool.QueryRow(ctx, q, userID).Scan(&dummy)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("db: has redeemed invite: %w", err)
}

// HasCompletedFirstRunApprovals returns true only after the user has made a
// training-data choice and acknowledged the current public-launch disclosure.
func (d *DB) HasCompletedFirstRunApprovals(ctx context.Context, userID, disclosureVersion string) (bool, error) {
	const q = `
		SELECT EXISTS (SELECT 1 FROM beta_consent WHERE user_id = $1)
		   AND EXISTS (
		       SELECT 1
		         FROM beta_disclosure_ack
		        WHERE user_id = $1 AND disclosure_version = $2
		   )
	`
	var approved bool
	if err := d.pool.QueryRow(ctx, q, userID, disclosureVersion).Scan(&approved); err != nil {
		return false, fmt.Errorf("db: first-run approvals: %w", err)
	}
	return approved, nil
}

// ClaimLaunchEntitlement atomically grants the launch promotion once per user.
// Repeated claims return the original timestamps and never extend the period.
func (d *DB) ClaimLaunchEntitlement(ctx context.Context, userID string, duration time.Duration) (*LaunchEntitlement, error) {
	const q = `
		INSERT INTO launch_entitlements (user_id, plan, source, starts_at, ends_at)
		VALUES ($1, 'unlimited', 'launch_48h', now(), now() + $2::interval)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING user_id, plan, source, starts_at, ends_at, claimed_at
	`
	var entitlement LaunchEntitlement
	interval := fmt.Sprintf("%d seconds", int64(duration/time.Second))
	if err := d.pool.QueryRow(ctx, q, userID, interval).Scan(
		&entitlement.UserID,
		&entitlement.Plan,
		&entitlement.Source,
		&entitlement.StartsAt,
		&entitlement.EndsAt,
		&entitlement.ClaimedAt,
	); err != nil {
		return nil, fmt.Errorf("db: claim launch entitlement: %w", err)
	}
	return &entitlement, nil
}

// GetLaunchEntitlement returns nil when a user has not claimed the promotion.
func (d *DB) GetLaunchEntitlement(ctx context.Context, userID string) (*LaunchEntitlement, error) {
	const q = `
		SELECT user_id, plan, source, starts_at, ends_at, claimed_at
		  FROM launch_entitlements
		 WHERE user_id = $1
	`
	var entitlement LaunchEntitlement
	if err := d.pool.QueryRow(ctx, q, userID).Scan(
		&entitlement.UserID,
		&entitlement.Plan,
		&entitlement.Source,
		&entitlement.StartsAt,
		&entitlement.EndsAt,
		&entitlement.ClaimedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: get launch entitlement: %w", err)
	}
	return &entitlement, nil
}

// UpsertConsent writes or updates the training consent for a user.
func (d *DB) UpsertConsent(ctx context.Context, userID string, trainingOptIn bool, policyVersion string) error {
	const q = `
		INSERT INTO beta_consent (user_id, training_opt_in, policy_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE
		   SET training_opt_in = EXCLUDED.training_opt_in,
		       policy_version  = EXCLUDED.policy_version,
		       decided_at      = now()
	`
	if _, err := d.pool.Exec(ctx, q, userID, trainingOptIn, policyVersion); err != nil {
		return fmt.Errorf("db: upsert consent: %w", err)
	}
	return nil
}

// GetConsent reads the training consent for a user. Returns nil + nil
// error when no row exists (user has not yet consented).
func (d *DB) GetConsent(ctx context.Context, userID string) (*Consent, error) {
	const q = `
		SELECT user_id, training_opt_in, policy_version, decided_at
		  FROM beta_consent
		 WHERE user_id = $1
	`
	var c Consent
	err := d.pool.QueryRow(ctx, q, userID).Scan(&c.UserID, &c.TrainingOptIn, &c.PolicyVersion, &c.DecidedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: get consent: %w", err)
	}
	return &c, nil
}

// InsertDisclosureAck records a beta disclosure acknowledgement. If the
// user already acked this disclosure version, the row is unchanged
// (ON CONFLICT DO NOTHING).
func (d *DB) InsertDisclosureAck(ctx context.Context, userID, disclosureVersion string) error {
	const q = `
		INSERT INTO beta_disclosure_ack (user_id, disclosure_version)
		VALUES ($1, $2)
		ON CONFLICT (user_id, disclosure_version) DO NOTHING
	`
	if _, err := d.pool.Exec(ctx, q, userID, disclosureVersion); err != nil {
		return fmt.Errorf("db: insert disclosure ack: %w", err)
	}
	return nil
}

// InsertBugReport creates a new bug/feedback report and returns its id.
func (d *DB) InsertBugReport(ctx context.Context, userID, message string, contextJSON []byte, attachmentRef string) (int64, error) {
	const q = `
		INSERT INTO bug_reports (user_id, message, context, attachment_ref)
		VALUES ($1, $2, $3::jsonb, $4)
		RETURNING id
	`
	var id int64
	if err := d.pool.QueryRow(ctx, q, userID, message, string(contextJSON), attachmentRef).Scan(&id); err != nil {
		return 0, fmt.Errorf("db: insert bug report: %w", err)
	}
	return id, nil
}

// ListBugReports returns all bug reports ordered by created_at desc.
func (d *DB) ListBugReports(ctx context.Context) ([]BugReport, error) {
	const q = `
		SELECT id, user_id, message, context, COALESCE(attachment_ref, ''), status, created_at
		  FROM bug_reports
		 ORDER BY created_at DESC
	`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list bug reports: %w", err)
	}
	defer rows.Close()
	var out []BugReport
	for rows.Next() {
		var br BugReport
		if err := rows.Scan(&br.ID, &br.UserID, &br.Message, &br.Context,
			&br.AttachmentRef, &br.Status, &br.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan bug report: %w", err)
		}
		out = append(out, br)
	}
	return out, rows.Err()
}

// GetBugReport returns a single bug report by id.
func (d *DB) GetBugReport(ctx context.Context, id int64) (*BugReport, error) {
	const q = `
		SELECT id, user_id, message, context, COALESCE(attachment_ref, ''), status, created_at
		  FROM bug_reports
		 WHERE id = $1
	`
	var br BugReport
	err := d.pool.QueryRow(ctx, q, id).Scan(&br.ID, &br.UserID, &br.Message, &br.Context,
		&br.AttachmentRef, &br.Status, &br.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: get bug report: %w", err)
	}
	return &br, nil
}
