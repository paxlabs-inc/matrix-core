package mail

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
)

// Store owns one tenant's durable mailbox protocol.
type Store struct {
	pool     *pgxpool.Pool
	vault    *vault.UserVault
	graph    *dependency.Store
	tenantID string
	config   Config
	now      func() time.Time
}

// New constructs a fail-closed tenant-scoped mail store.
func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	graph *dependency.Store,
	tenantID string,
	config Config,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || graph == nil || tenantID == "" || now == nil {
		return nil, fmt.Errorf("mail: pool, Vault, graph, tenant_id, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("mail: Vault user does not match tenant")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Store{
		pool: pool, vault: userVault, graph: graph, tenantID: tenantID,
		config: config, now: now,
	}, nil
}

// PublishSeatKey binds a current durable seat to a real verification key.
func (store *Store) PublishSeatKey(ctx context.Context, key SeatKey) error {
	if err := key.Address.Validate(); err != nil {
		return err
	}
	if err := validateToken("key_id", key.KeyID); err != nil {
		return err
	}
	if len(key.PublicKey) != ed25519.PublicKeySize ||
		key.EffectiveAt.IsZero() || key.EffectiveAt.Location() != time.UTC {
		return fmt.Errorf("mail: valid Ed25519 public key and UTC effective_at are required")
	}
	var active int
	err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_authority_heads head
		LEFT JOIN workforce_authority_revocations revoked
		  ON revoked.tenant_id=head.tenant_id
		 AND revoked.organization_id=head.organization_id
		 AND revoked.authority_kind=head.authority_kind
		 AND revoked.authority_id=head.authority_id
		 AND revoked.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind='seat' AND head.authority_id=$3
		  AND revoked.authority_id IS NULL
	`, store.tenantID, key.Address.OrganizationID, key.Address.SeatID).Scan(&active)
	if err != nil {
		return fmt.Errorf("%w: verify seat authority: %v", ErrUncertain, err)
	}
	if active != 1 {
		return ErrUnauthorized
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_mail_keys (
			tenant_id,organization_id,department_id,seat_id,key_id,
			public_key,effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT DO NOTHING
	`, store.tenantID, key.Address.OrganizationID, key.Address.DepartmentID,
		key.Address.SeatID, key.KeyID, []byte(key.PublicKey), key.EffectiveAt, store.now())
	if err != nil {
		return fmt.Errorf("%w: publish seat key: %v", ErrUncertain, err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var count int
	err = store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_mail_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND department_id=$3
		  AND seat_id=$4 AND key_id=$5 AND public_key=$6
		  AND effective_at=$7 AND revoked_at IS NULL
	`, store.tenantID, key.Address.OrganizationID, key.Address.DepartmentID,
		key.Address.SeatID, key.KeyID, []byte(key.PublicKey), key.EffectiveAt).Scan(&count)
	if err != nil || count != 1 {
		return ErrConflict
	}
	return nil
}

// RevokeSeatKey prevents new messages while preserving historical verification.
func (store *Store) RevokeSeatKey(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
	keyID string,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE workforce_mail_keys SET revoked_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND seat_id=$4
		  AND key_id=$5 AND revoked_at IS NULL
	`, now, store.tenantID, organizationID, seatID, keyID)
	if err != nil {
		return fmt.Errorf("%w: revoke seat key: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		return ErrUnauthorized
	}
	return nil
}

// Send verifies, seals, and durably queues one at-least-once message delivery.
func (store *Store) Send(
	ctx context.Context,
	envelope contracts.MessageEnvelope,
	options SendOptions,
) (SendResult, error) {
	var result SendResult
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		result, err = store.sendOnce(ctx, envelope, options)
		if !retryableMail(err) {
			return result, err
		}
	}
	return result, fmt.Errorf("%w: send transaction retry budget exhausted: %v", ErrUncertain, err)
}

func (store *Store) sendOnce(
	ctx context.Context,
	envelope contracts.MessageEnvelope,
	options SendOptions,
) (SendResult, error) {
	now, err := store.validateSend(ctx, envelope, options)
	if err != nil {
		return SendResult{}, err
	}
	binding, err := options.binding(envelope.Kind)
	if err != nil {
		return SendResult{}, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return SendResult{}, err
	}
	sum := sha256.Sum256(encoded)
	envelopeHash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.messageAD(envelope.From.OrganizationID, envelope.ID), encoded)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: seal envelope: %v", ErrUncertain, err)
	}
	var sealedBinding []byte
	bindingKind := ""
	bindingState := "ready"
	if binding.Delegation != nil || binding.Correction != nil {
		bindingKind = string(envelope.Kind)
		bindingState = "pending"
		encodedBinding, encodeErr := json.Marshal(binding)
		if encodeErr != nil {
			return SendResult{}, encodeErr
		}
		sealedBinding, err = store.vault.SealRecord(
			store.bindingAD(envelope.From.OrganizationID, envelope.ID), encodedBinding,
		)
		if err != nil {
			return SendResult{}, fmt.Errorf("%w: seal binding: %v", ErrUncertain, err)
		}
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: begin send: %w", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	existing, found, err := store.findExisting(ctx, tx, envelope, envelopeHash)
	if err != nil {
		return SendResult{}, err
	}
	if found {
		if err := tx.Commit(ctx); err != nil {
			return SendResult{}, fmt.Errorf("%w: commit duplicate: %v", ErrUncertain, err)
		}
		existing.Deduplicated = true
		return existing, nil
	}
	if err := store.enforceThreadAndQuotas(ctx, tx, envelope, options, now); err != nil {
		return SendResult{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_mail_messages (
			tenant_id,organization_id,message_id,thread_id,in_reply_to,
			sender_department_id,sender_seat_id,sender_key_id,kind,parent_intent_id,
			priority,deadline,timeout_action,classification,idempotency_key,
			envelope_hash,sealed_envelope,automatic,binding_kind,sealed_binding,
			binding_state,created_at,expires_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
			$16,$17,$18,NULLIF($19,''),$20,$21,$22,$23
		)
	`, store.tenantID, envelope.From.OrganizationID, envelope.ID, envelope.ThreadID,
		optionalMessage(envelope.InReplyTo), envelope.From.DepartmentID,
		envelope.From.SeatID, envelope.Signature.KeyID, envelope.Kind,
		envelope.ParentIntentID, envelope.Priority, envelope.Deadline,
		envelope.TimeoutAction, envelope.Classification, envelope.IdempotencyKey,
		envelopeHash, sealed, options.Automatic, bindingKind, optionalBytes(sealedBinding),
		bindingState, envelope.CreatedAt, envelope.ExpiresAt)
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: insert message: %w", ErrUncertain, err)
	}
	recipients := recipientList(envelope)
	for _, recipient := range recipients {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_mail_recipients (
				tenant_id,organization_id,message_id,recipient_department_id,
				recipient_seat_id,recipient_kind,state,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,'queued',$7)
		`, store.tenantID, envelope.From.OrganizationID, envelope.ID,
			recipient.address.DepartmentID, recipient.address.SeatID,
			recipient.kind, now); err != nil {
			return SendResult{}, fmt.Errorf("%w: insert recipient: %w", ErrUncertain, err)
		}
		if err := store.logAccess(ctx, tx, envelope.From.OrganizationID,
			envelope.ID, recipient.address.SeatID, StateQueued,
			envelope.IdempotencyKey, now); err != nil {
			return SendResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SendResult{}, fmt.Errorf("%w: commit send: %w", ErrUncertain, err)
	}
	result := SendResult{
		MessageID: envelope.ID,
		EnvelopeHash: contracts.ContentHash{
			Algorithm: "sha256", Digest: envelopeHash,
		},
		BindingReady: bindingState == "ready",
	}
	if bindingState == "pending" {
		if _, err := store.ResolveBindings(ctx, envelope.From.OrganizationID, 64); err != nil {
			return result, err
		}
		result.BindingReady = true
	}
	return result, nil
}

// ResolveBindings idempotently applies pending delegation and correction bindings.
func (store *Store) ResolveBindings(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	limit uint32,
) (uint32, error) {
	if limit == 0 || limit > 1000 {
		return 0, fmt.Errorf("mail: binding limit must be 1 to 1000")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT message_id,kind,sealed_binding
		FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2 AND binding_state='pending'
		ORDER BY created_at,message_id
		LIMIT $3
	`, store.tenantID, organizationID, limit)
	if err != nil {
		return 0, fmt.Errorf("%w: query bindings: %v", ErrUncertain, err)
	}
	type pendingBinding struct {
		messageID contracts.MessageID
		kind      contracts.MessageKind
		sealed    []byte
	}
	pending := make([]pendingBinding, 0)
	for rows.Next() {
		var item pendingBinding
		if err := rows.Scan(&item.messageID, &item.kind, &item.sealed); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%w: scan binding: %v", ErrUncertain, err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("%w: iterate bindings: %v", ErrUncertain, err)
	}
	rows.Close()
	var resolved uint32
	for _, item := range pending {
		opened, err := store.vault.OpenRecord(
			store.bindingAD(organizationID, item.messageID), item.sealed,
		)
		if err != nil {
			return resolved, fmt.Errorf("%w: open binding: %v", ErrUncertain, err)
		}
		var binding bindingEnvelope
		if err := json.Unmarshal(opened, &binding); err != nil || binding.Kind != item.kind {
			return resolved, fmt.Errorf("%w: invalid binding", ErrUncertain)
		}
		switch item.kind {
		case contracts.MessageDelegation:
			if binding.Delegation == nil {
				return resolved, fmt.Errorf("%w: missing delegation", ErrUncertain)
			}
			if err := store.graph.PutNode(ctx, binding.Delegation.Node); err != nil {
				return resolved, store.noteBindingError(ctx, organizationID, item.messageID, err)
			}
			if err := store.graph.AddEdge(ctx, binding.Delegation.Edge); err != nil {
				return resolved, store.noteBindingError(ctx, organizationID, item.messageID, err)
			}
		case contracts.MessageCorrection:
			if binding.Correction == nil {
				return resolved, fmt.Errorf("%w: missing correction", ErrUncertain)
			}
			var count int
			err := store.pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM workforce_correction_notices
				WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
				  AND correction_id=$4 AND affected_record_id=$5
				  AND recipient_seat_id=$6 AND state IN ('pending','delivered')
			`, store.tenantID, organizationID, binding.Correction.NoticeID,
				binding.Correction.CorrectionID, binding.Correction.AffectedRecord,
				binding.Correction.RecipientSeatID).Scan(&count)
			if err != nil || count != 1 {
				return resolved, store.noteBindingError(ctx, organizationID,
					item.messageID, ErrUnauthorized)
			}
		default:
			return resolved, fmt.Errorf("%w: invalid binding kind", ErrUncertain)
		}
		command, err := store.pool.Exec(ctx, `
			UPDATE workforce_mail_messages
			SET binding_state='ready',binding_error=NULL
			WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
			  AND binding_state='pending'
		`, store.tenantID, organizationID, item.messageID)
		if err != nil {
			return resolved, fmt.Errorf("%w: mark binding ready: %v", ErrUncertain, err)
		}
		if command.RowsAffected() == 1 {
			resolved++
		}
	}
	return resolved, nil
}

// Dispatch queues wake requests and marks durable mailbox delivery.
func (store *Store) Dispatch(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	limit uint32,
) (uint32, error) {
	if limit == 0 || limit > 10000 {
		return 0, fmt.Errorf("mail: dispatch limit must be 1 to 10000")
	}
	now, err := store.currentTime()
	if err != nil {
		return 0, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return 0, fmt.Errorf("%w: begin dispatch: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `
		SELECT recipient.message_id,recipient.recipient_seat_id,message.expires_at
		FROM workforce_mail_recipients recipient
		JOIN workforce_mail_messages message
		  ON message.tenant_id=recipient.tenant_id
		 AND message.organization_id=recipient.organization_id
		 AND message.message_id=recipient.message_id
		WHERE recipient.tenant_id=$1 AND recipient.organization_id=$2
		  AND recipient.state='queued' AND message.binding_state='ready'
		ORDER BY message.priority DESC,message.created_at,message.message_id,
			recipient.recipient_seat_id
		LIMIT $3 FOR UPDATE OF recipient SKIP LOCKED
	`, store.tenantID, organizationID, limit)
	if err != nil {
		return 0, fmt.Errorf("%w: query dispatch: %v", ErrUncertain, err)
	}
	type due struct {
		messageID contracts.MessageID
		seatID    contracts.SeatID
		expiresAt time.Time
	}
	items := make([]due, 0)
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.messageID, &item.seatID, &item.expiresAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("%w: scan dispatch: %v", ErrUncertain, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("%w: iterate dispatch: %v", ErrUncertain, err)
	}
	rows.Close()
	var delivered uint32
	for _, item := range items {
		state := StateDelivered
		if !item.expiresAt.After(now) {
			state = StateExpired
		}
		_, err := tx.Exec(ctx, `
				UPDATE workforce_mail_recipients
				SET state=$1,delivered_at=CASE WHEN $1='delivered' THEN $2::timestamptz ELSE NULL END,
					updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4
			  AND message_id=$5 AND recipient_seat_id=$6 AND state='queued'
		`, state, now, store.tenantID, organizationID, item.messageID, item.seatID)
		if err != nil {
			return 0, fmt.Errorf("%w: update dispatch: %v", ErrUncertain, err)
		}
		if err := store.logAccess(ctx, tx, organizationID, item.messageID,
			item.seatID, state, "dispatch:"+string(item.messageID), now); err != nil {
			return 0, err
		}
		if state == StateDelivered {
			wakeID := deterministicID("wake", store.tenantID, string(organizationID),
				string(item.seatID), string(item.messageID))
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_wake_requests (
					tenant_id,organization_id,wake_request_id,seat_id,reason,
					source_id,state,scheduled_at,created_at
				) VALUES ($1,$2,$3,$4,'mail',$5,'queued',$6,$6)
				ON CONFLICT DO NOTHING
			`, store.tenantID, organizationID, wakeID, item.seatID,
				item.messageID, now); err != nil {
				return 0, fmt.Errorf("%w: queue mail wake: %v", ErrUncertain, err)
			}
			delivered++
		} else if err := store.insertTimeout(ctx, tx, organizationID,
			item.messageID, item.seatID, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("%w: commit dispatch: %v", ErrUncertain, err)
	}
	return delivered, nil
}

// Consume opens one delivered envelope exactly once per recipient identity.
func (store *Store) Consume(
	ctx context.Context,
	request ConsumeRequest,
) (contracts.MessageEnvelope, bool, error) {
	if err := validateToken("organization_id", string(request.OrganizationID)); err != nil {
		return contracts.MessageEnvelope{}, false, err
	}
	if err := validateToken("seat_id", string(request.SeatID)); err != nil {
		return contracts.MessageEnvelope{}, false, err
	}
	if err := validateToken("message_id", string(request.MessageID)); err != nil {
		return contracts.MessageEnvelope{}, false, err
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return contracts.MessageEnvelope{}, false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return contracts.MessageEnvelope{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: begin consume: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var state DeliveryState
	var consumptionKey, envelopeHash, kind string
	var sealed []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT recipient.state,COALESCE(recipient.consumption_key,''),
			message.envelope_hash,message.sealed_envelope,message.expires_at,message.kind
		FROM workforce_mail_recipients recipient
		JOIN workforce_mail_messages message
		  ON message.tenant_id=recipient.tenant_id
		 AND message.organization_id=recipient.organization_id
		 AND message.message_id=recipient.message_id
		WHERE recipient.tenant_id=$1 AND recipient.organization_id=$2
		  AND recipient.message_id=$3 AND recipient.recipient_seat_id=$4
		FOR UPDATE OF recipient
	`, store.tenantID, request.OrganizationID, request.MessageID,
		request.SeatID).Scan(&state, &consumptionKey, &envelopeHash,
		&sealed, &expiresAt, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.MessageEnvelope{}, false, ErrUnauthorized
	}
	if err != nil {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: load delivery: %v", ErrUncertain, err)
	}
	if !expiresAt.After(now) && !state.Terminal() {
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_mail_recipients SET state='expired',updated_at=$1
			WHERE tenant_id=$2 AND organization_id=$3
			  AND message_id=$4 AND recipient_seat_id=$5
		`, now, store.tenantID, request.OrganizationID, request.MessageID,
			request.SeatID); err != nil {
			return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: expire consume: %v", ErrUncertain, err)
		}
		if err := store.insertTimeout(ctx, tx, request.OrganizationID,
			request.MessageID, request.SeatID, now); err != nil {
			return contracts.MessageEnvelope{}, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: commit expiry: %v", ErrUncertain, err)
		}
		return contracts.MessageEnvelope{}, false, ErrExpired
	}
	if state == StateQueued {
		return contracts.MessageEnvelope{}, false, ErrNotReady
	}
	if state == StateExpired || state == StateRejected ||
		state == StateCancelled || state == StateCorrected {
		return contracts.MessageEnvelope{}, false, ErrExpired
	}
	deduplicated := consumptionKey != ""
	if deduplicated && consumptionKey != request.IdempotencyKey {
		return contracts.MessageEnvelope{}, false, ErrConflict
	}
	opened, err := store.vault.OpenRecord(
		store.messageAD(request.OrganizationID, request.MessageID), sealed,
	)
	if err != nil {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: open envelope: %v", ErrUncertain, err)
	}
	sum := sha256.Sum256(opened)
	if hex.EncodeToString(sum[:]) != envelopeHash {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: envelope hash mismatch", ErrUncertain)
	}
	var envelope contracts.MessageEnvelope
	if err := json.Unmarshal(opened, &envelope); err != nil || envelope.Validate() != nil ||
		string(envelope.Kind) != kind {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: invalid sealed envelope", ErrUncertain)
	}
	if !deduplicated {
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_mail_recipients
			SET state='opened',consumption_key=$1,opened_at=$2,updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4
			  AND message_id=$5 AND recipient_seat_id=$6
		`, request.IdempotencyKey, now, store.tenantID, request.OrganizationID,
			request.MessageID, request.SeatID); err != nil {
			return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: open delivery: %v", ErrUncertain, err)
		}
		if err := store.logAccess(ctx, tx, request.OrganizationID, request.MessageID,
			request.SeatID, StateOpened, request.IdempotencyKey, now); err != nil {
			return contracts.MessageEnvelope{}, false, err
		}
		if envelope.Kind == contracts.MessageCorrection {
			if err := store.deliverCorrectionNotice(ctx, tx, request.OrganizationID,
				request.MessageID, request.SeatID); err != nil {
				return contracts.MessageEnvelope{}, false, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.MessageEnvelope{}, false, fmt.Errorf("%w: commit consume: %v", ErrUncertain, err)
	}
	return envelope, deduplicated, nil
}

// Transition advances one recipient through the closed delivery lifecycle.
func (store *Store) Transition(ctx context.Context, request TransitionRequest) error {
	if !request.State.Valid() || request.State == StateQueued ||
		request.State == StateDelivered || request.State == StateOpened ||
		request.State == StateExpired {
		return fmt.Errorf("mail: invalid explicit transition %q", request.State)
	}
	if err := validateToken("idempotency_key", request.IdempotencyKey); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: begin transition: %v", ErrUncertain, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var current DeliveryState
	err = tx.QueryRow(ctx, `
		SELECT state FROM workforce_mail_recipients
		WHERE tenant_id=$1 AND organization_id=$2
		  AND message_id=$3 AND recipient_seat_id=$4
		FOR UPDATE
	`, store.tenantID, request.OrganizationID, request.MessageID,
		request.SeatID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("%w: load transition: %v", ErrUncertain, err)
	}
	if current == request.State {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit duplicate transition: %v", ErrUncertain, err)
		}
		return nil
	}
	if !allowedTransition(current, request.State) {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_mail_recipients SET state=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4
		  AND message_id=$5 AND recipient_seat_id=$6 AND state=$7
	`, request.State, now, store.tenantID, request.OrganizationID,
		request.MessageID, request.SeatID, current); err != nil {
		return fmt.Errorf("%w: update transition: %v", ErrUncertain, err)
	}
	if err := store.logAccess(ctx, tx, request.OrganizationID, request.MessageID,
		request.SeatID, request.State, request.IdempotencyKey, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit transition: %v", ErrUncertain, err)
	}
	return nil
}

// Inbox returns a deterministic authorized mailbox projection without payload bytes.
func (store *Store) Inbox(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
	limit uint32,
) ([]Delivery, error) {
	if limit == 0 || limit > 1000 {
		return nil, fmt.Errorf("mail: inbox limit must be 1 to 1000")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT message.message_id,message.thread_id,
			recipient.recipient_department_id,recipient.recipient_seat_id,
			recipient.state,message.priority,message.created_at,message.expires_at,
			COALESCE(recipient.consumption_key,''),(message.binding_state='ready'),
			message.automatic,message.timeout_action,message.parent_intent_id
		FROM workforce_mail_recipients recipient
		JOIN workforce_mail_messages message
		  ON message.tenant_id=recipient.tenant_id
		 AND message.organization_id=recipient.organization_id
		 AND message.message_id=recipient.message_id
		WHERE recipient.tenant_id=$1 AND recipient.organization_id=$2
		  AND recipient.recipient_seat_id=$3
		ORDER BY message.priority DESC,message.created_at,message.message_id
		LIMIT $4
	`, store.tenantID, organizationID, seatID, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: query inbox: %v", ErrUncertain, err)
	}
	defer rows.Close()
	result := make([]Delivery, 0)
	for rows.Next() {
		delivery := Delivery{
			Recipient: contracts.SeatAddress{
				OrganizationID: organizationID, SeatID: seatID,
			},
		}
		if err := rows.Scan(
			&delivery.MessageID, &delivery.ThreadID,
			&delivery.Recipient.DepartmentID, &delivery.Recipient.SeatID,
			&delivery.State, &delivery.Priority, &delivery.CreatedAt,
			&delivery.ExpiresAt, &delivery.ConsumptionKey,
			&delivery.BindingReady, &delivery.Automatic,
			&delivery.TimeoutAction, &delivery.ParentIntentID,
		); err != nil {
			return nil, fmt.Errorf("%w: scan inbox: %v", ErrUncertain, err)
		}
		delivery.CreatedAt = delivery.CreatedAt.UTC()
		delivery.ExpiresAt = delivery.ExpiresAt.UTC()
		result = append(result, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate inbox: %v", ErrUncertain, err)
	}
	return result, nil
}

func (store *Store) validateSend(
	ctx context.Context,
	envelope contracts.MessageEnvelope,
	options SendOptions,
) (time.Time, error) {
	if err := envelope.Validate(); err != nil {
		return time.Time{}, err
	}
	if _, err := options.binding(envelope.Kind); err != nil {
		return time.Time{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return time.Time{}, err
	}
	if envelope.From.OrganizationID == "" ||
		envelope.CreatedAt.After(now.Add(time.Minute)) || !envelope.ExpiresAt.After(now) ||
		envelope.ExpiresAt.Sub(envelope.CreatedAt) > store.config.MaxMessageLifetime {
		return time.Time{}, ErrExpired
	}
	recipients := recipientList(envelope)
	if uint32(len(recipients)) > store.config.MaxRecipients {
		return time.Time{}, ErrQuota
	}
	seen := make(map[contracts.SeatID]bool, len(recipients))
	for _, recipient := range recipients {
		if recipient.address.OrganizationID != envelope.From.OrganizationID ||
			seen[recipient.address.SeatID] {
			return time.Time{}, ErrUnauthorized
		}
		seen[recipient.address.SeatID] = true
	}
	var attachmentBytes uint64
	for _, artifact := range append(
		append([]contracts.ArtifactRef(nil), envelope.Artifacts...),
		envelope.Payload.Artifact,
	) {
		if ^uint64(0)-attachmentBytes < artifact.SizeBytes {
			return time.Time{}, ErrQuota
		}
		attachmentBytes += artifact.SizeBytes
	}
	if attachmentBytes > store.config.MaxAttachmentBytes {
		return time.Time{}, ErrQuota
	}
	if err := store.verifySender(ctx, envelope, now); err != nil {
		return time.Time{}, err
	}
	for _, recipient := range recipients {
		if err := store.verifyRecipient(ctx, recipient.address, now); err != nil {
			return time.Time{}, err
		}
	}
	if envelope.Kind == contracts.MessageDelegation {
		if err := validateDelegation(envelope, *options.Delegation); err != nil {
			return time.Time{}, err
		}
		var owner *string
		err := store.pool.QueryRow(ctx, `
			SELECT owner_seat_id FROM workforce_work_nodes
			WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
			  AND node_kind='intent'
		`, store.tenantID, envelope.From.OrganizationID,
			options.Delegation.Edge.Prerequisite).Scan(&owner)
		if err != nil || owner == nil || *owner != string(envelope.From.SeatID) {
			return time.Time{}, ErrUnauthorized
		}
	}
	if envelope.Kind == contracts.MessageCorrection {
		if err := validateCorrection(envelope, *options.Correction); err != nil {
			return time.Time{}, err
		}
	}
	return now, nil
}

func (store *Store) enforceThreadAndQuotas(
	ctx context.Context,
	tx pgx.Tx,
	envelope contracts.MessageEnvelope,
	options SendOptions,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+string(envelope.From.OrganizationID)+"|mail|"+
			string(envelope.ThreadID)); err != nil {
		return fmt.Errorf("%w: lock thread: %v", ErrUncertain, err)
	}
	var threadCount, automaticCount uint32
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*),COUNT(*) FILTER (WHERE automatic)
		FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2 AND thread_id=$3
	`, store.tenantID, envelope.From.OrganizationID, envelope.ThreadID).Scan(
		&threadCount, &automaticCount,
	); err != nil {
		return fmt.Errorf("%w: inspect thread: %v", ErrUncertain, err)
	}
	if threadCount >= store.config.MaxThreadMessages ||
		(options.Automatic && automaticCount >= store.config.MaxAutoReplies) {
		return ErrQuota
	}
	if envelope.InReplyTo == nil {
		if threadCount != 0 {
			return ErrConflict
		}
	} else {
		var parentThread contracts.ThreadID
		var depth uint32
		err := tx.QueryRow(ctx, `
			WITH RECURSIVE chain(message_id,in_reply_to,depth) AS (
				SELECT message_id,in_reply_to,1
				FROM workforce_mail_messages
				WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
				UNION ALL
				SELECT parent.message_id,parent.in_reply_to,chain.depth+1
				FROM workforce_mail_messages parent
				JOIN chain ON chain.in_reply_to=parent.message_id
				WHERE parent.tenant_id=$1 AND parent.organization_id=$2
				  AND chain.depth<$4
			)
			SELECT message.thread_id,MAX(chain.depth)
			FROM chain
			JOIN workforce_mail_messages message
			  ON message.tenant_id=$1 AND message.organization_id=$2
			 AND message.message_id=$3
			GROUP BY message.thread_id
		`, store.tenantID, envelope.From.OrganizationID, *envelope.InReplyTo,
			store.config.MaxThreadDepth+1).Scan(&parentThread, &depth)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("%w: inspect reply chain: %v", ErrUncertain, err)
		}
		if parentThread != envelope.ThreadID || depth >= store.config.MaxThreadDepth {
			return ErrQuota
		}
	}
	for _, recipient := range recipientList(envelope) {
		var count uint32
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_mail_recipients
			WHERE tenant_id=$1 AND organization_id=$2 AND recipient_seat_id=$3
			  AND state NOT IN ('resolved','expired','rejected','cancelled','corrected')
		`, store.tenantID, envelope.From.OrganizationID,
			recipient.address.SeatID).Scan(&count); err != nil {
			return fmt.Errorf("%w: inspect mailbox quota: %v", ErrUncertain, err)
		}
		if count >= store.config.MaxMailboxMessages {
			return ErrQuota
		}
	}
	_ = now
	return nil
}

func (store *Store) findExisting(
	ctx context.Context,
	tx pgx.Tx,
	envelope contracts.MessageEnvelope,
	envelopeHash string,
) (SendResult, bool, error) {
	var messageID contracts.MessageID
	var storedHash, bindingState string
	err := tx.QueryRow(ctx, `
		SELECT message_id,envelope_hash,binding_state
		FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2
		  AND (message_id=$3 OR (sender_seat_id=$4 AND idempotency_key=$5))
	`, store.tenantID, envelope.From.OrganizationID, envelope.ID,
		envelope.From.SeatID, envelope.IdempotencyKey).Scan(
		&messageID, &storedHash, &bindingState,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SendResult{}, false, nil
	}
	if err != nil {
		return SendResult{}, false, fmt.Errorf("%w: inspect send identity: %v", ErrUncertain, err)
	}
	if messageID != envelope.ID || storedHash != envelopeHash {
		return SendResult{}, false, ErrConflict
	}
	return SendResult{
		MessageID: messageID,
		EnvelopeHash: contracts.ContentHash{
			Algorithm: "sha256", Digest: storedHash,
		},
		BindingReady: bindingState == "ready",
	}, true, nil
}

func (store *Store) verifySender(
	ctx context.Context,
	envelope contracts.MessageEnvelope,
	now time.Time,
) error {
	var departmentID contracts.DepartmentID
	var publicKey []byte
	err := store.pool.QueryRow(ctx, `
		SELECT department_id,public_key FROM workforce_mail_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3
		  AND key_id=$4 AND effective_at<=$5 AND revoked_at IS NULL
	`, store.tenantID, envelope.From.OrganizationID, envelope.From.SeatID,
		envelope.Signature.KeyID, now).Scan(&departmentID, &publicKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("%w: resolve sender key: %v", ErrUncertain, err)
	}
	if departmentID != envelope.From.DepartmentID ||
		len(publicKey) != ed25519.PublicKeySize ||
		envelope.Signature.Algorithm != "ed25519" {
		return ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature.Value)
	if err != nil {
		return ErrUnauthorized
	}
	payload, err := SigningBytes(envelope)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return ErrUnauthorized
	}
	return nil
}

func (store *Store) verifyRecipient(
	ctx context.Context,
	address contracts.SeatAddress,
	now time.Time,
) error {
	var count int
	err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_mail_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND department_id=$3
		  AND seat_id=$4 AND effective_at<=$5 AND revoked_at IS NULL
	`, store.tenantID, address.OrganizationID, address.DepartmentID,
		address.SeatID, now).Scan(&count)
	if err != nil {
		return fmt.Errorf("%w: resolve recipient: %v", ErrUncertain, err)
	}
	if count != 1 {
		return ErrUnauthorized
	}
	return nil
}

func validateDelegation(
	envelope contracts.MessageEnvelope,
	binding DelegationBinding,
) error {
	if len(envelope.To) != 1 || len(envelope.CC) != 0 ||
		binding.Node.OrganizationID != envelope.From.OrganizationID ||
		binding.Node.Kind != dependency.NodeDelegation ||
		binding.Node.State != dependency.StatePending ||
		binding.Node.OwnerSeatID == nil ||
		*binding.Node.OwnerSeatID != envelope.To[0].SeatID ||
		binding.Edge.OrganizationID != envelope.From.OrganizationID ||
		binding.Edge.Kind != dependency.EdgeDelegation ||
		binding.Edge.Dependent != binding.Node.ID ||
		string(binding.Edge.Prerequisite) != string(envelope.ParentIntentID) ||
		binding.Edge.ExpiresAt == nil ||
		!binding.Edge.ExpiresAt.Equal(envelope.ExpiresAt) ||
		binding.Edge.TimeoutAction != envelope.TimeoutAction {
		return ErrUnauthorized
	}
	if err := binding.Node.Validate(); err != nil {
		return err
	}
	return binding.Edge.Validate()
}

func validateCorrection(
	envelope contracts.MessageEnvelope,
	binding CorrectionBinding,
) error {
	if len(envelope.To) != 1 || len(envelope.CC) != 0 ||
		binding.RecipientSeatID != envelope.To[0].SeatID {
		return ErrUnauthorized
	}
	for _, value := range []string{
		binding.NoticeID, string(binding.CorrectionID), string(binding.AffectedRecord),
	} {
		if err := validateToken("correction binding", value); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) noteBindingError(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
	cause error,
) error {
	_, _ = store.pool.Exec(ctx, `
		UPDATE workforce_mail_messages SET binding_error=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND message_id=$4
		  AND binding_state='pending'
	`, boundedError(cause), store.tenantID, organizationID, messageID)
	return cause
}

func (store *Store) deliverCorrectionNotice(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
	seatID contracts.SeatID,
) error {
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT sealed_binding FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
		  AND kind='correction' AND binding_state='ready'
	`, store.tenantID, organizationID, messageID).Scan(&sealed)
	if err != nil {
		return fmt.Errorf("%w: load correction binding: %v", ErrUncertain, err)
	}
	opened, err := store.vault.OpenRecord(store.bindingAD(organizationID, messageID), sealed)
	if err != nil {
		return fmt.Errorf("%w: open correction binding: %v", ErrUncertain, err)
	}
	var binding bindingEnvelope
	if err := json.Unmarshal(opened, &binding); err != nil ||
		binding.Correction == nil || binding.Correction.RecipientSeatID != seatID {
		return fmt.Errorf("%w: invalid correction binding", ErrUncertain)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_correction_notices SET state='delivered'
		WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
		  AND recipient_seat_id=$4 AND state='pending'
	`, store.tenantID, organizationID, binding.Correction.NoticeID, seatID)
	if err != nil {
		return fmt.Errorf("%w: mark correction delivered: %v", ErrUncertain, err)
	}
	if command.RowsAffected() != 1 {
		var state string
		err := tx.QueryRow(ctx, `
			SELECT state FROM workforce_correction_notices
			WHERE tenant_id=$1 AND organization_id=$2 AND notice_id=$3
			  AND recipient_seat_id=$4
		`, store.tenantID, organizationID, binding.Correction.NoticeID,
			seatID).Scan(&state)
		if err != nil || state != "delivered" {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) insertTimeout(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
	seatID contracts.SeatID,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_mail_timeouts (
			tenant_id,organization_id,message_id,recipient_seat_id,
			timeout_action,created_at
		)
		SELECT $1,$2,$3,$4,timeout_action,$5
		FROM workforce_mail_messages
		WHERE tenant_id=$1 AND organization_id=$2 AND message_id=$3
		ON CONFLICT DO NOTHING
	`, store.tenantID, organizationID, messageID, seatID, now)
	if err != nil {
		return fmt.Errorf("%w: insert timeout: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) logAccess(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
	seatID contracts.SeatID,
	state DeliveryState,
	idempotencyKey string,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_mail_access_log (
			tenant_id,organization_id,message_id,seat_id,action,
			idempotency_key,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT DO NOTHING
	`, store.tenantID, organizationID, messageID, seatID, state,
		idempotencyKey, now)
	if err != nil {
		return fmt.Errorf("%w: append access log: %v", ErrUncertain, err)
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("%w: time source is not UTC", ErrUncertain)
	}
	return now, nil
}

func (store *Store) messageAD(
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.mail.envelope",
		Stream: string(organizationID) + "/" + string(messageID),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) bindingAD(
	organizationID contracts.OrganizationID,
	messageID contracts.MessageID,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.mail.binding",
		Stream: string(organizationID) + "/" + string(messageID),
		Schema: contracts.SchemaVersionV1,
	}
}

type recipient struct {
	address contracts.SeatAddress
	kind    string
}

func recipientList(envelope contracts.MessageEnvelope) []recipient {
	result := make([]recipient, 0, len(envelope.To)+len(envelope.CC))
	for _, address := range envelope.To {
		result = append(result, recipient{address: address, kind: "to"})
	}
	for _, address := range envelope.CC {
		result = append(result, recipient{address: address, kind: "cc"})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].address.SeatID != result[right].address.SeatID {
			return result[left].address.SeatID < result[right].address.SeatID
		}
		return result[left].kind < result[right].kind
	})
	return result
}

func allowedTransition(current, target DeliveryState) bool {
	switch current {
	case StateDelivered, StateOpened:
		return target == StateAcknowledged || target == StateRejected ||
			target == StateCancelled || target == StateCorrected
	case StateAcknowledged:
		return target == StateReplied || target == StateResolved ||
			target == StateRejected || target == StateCancelled || target == StateCorrected
	case StateReplied:
		return target == StateResolved || target == StateCorrected
	default:
		return false
	}
}

func optionalMessage(value *contracts.MessageID) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func deterministicID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + ":" + hex.EncodeToString(sum[:16])
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func retryableMail(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) &&
		(databaseError.Code == "40001" || databaseError.Code == "40P01" ||
			databaseError.Code == "23505")
}
