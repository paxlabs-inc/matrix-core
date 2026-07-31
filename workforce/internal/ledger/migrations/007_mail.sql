CREATE TABLE workforce_mail_keys (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    department_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    key_id TEXT NOT NULL,
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    effective_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, seat_id, key_id)
);

CREATE UNIQUE INDEX workforce_mail_active_key_idx
    ON workforce_mail_keys (tenant_id, organization_id, seat_id)
    WHERE revoked_at IS NULL;

CREATE TABLE workforce_mail_messages (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    in_reply_to TEXT,
    sender_department_id TEXT NOT NULL,
    sender_seat_id TEXT NOT NULL,
    sender_key_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    parent_intent_id TEXT NOT NULL,
    priority INTEGER NOT NULL,
    deadline TIMESTAMPTZ,
    timeout_action TEXT NOT NULL,
    classification TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    envelope_hash CHAR(64) NOT NULL,
    sealed_envelope BYTEA NOT NULL CHECK (octet_length(sealed_envelope) > 0),
    automatic BOOLEAN NOT NULL,
    binding_kind TEXT CHECK (binding_kind IN ('delegation','correction')),
    sealed_binding BYTEA,
    binding_state TEXT NOT NULL CHECK (binding_state IN ('ready','pending','failed')),
    binding_error TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, message_id),
    UNIQUE (tenant_id, organization_id, sender_seat_id, idempotency_key),
    FOREIGN KEY (tenant_id, organization_id, in_reply_to)
        REFERENCES workforce_mail_messages (tenant_id, organization_id, message_id)
);

CREATE INDEX workforce_mail_thread_idx
    ON workforce_mail_messages (
        tenant_id, organization_id, thread_id, created_at, message_id
    );

CREATE TABLE workforce_mail_recipients (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    recipient_department_id TEXT NOT NULL,
    recipient_seat_id TEXT NOT NULL,
    recipient_kind TEXT NOT NULL CHECK (recipient_kind IN ('to','cc')),
    state TEXT NOT NULL CHECK (
        state IN (
            'queued','delivered','opened','acknowledged','replied',
            'resolved','expired','rejected','cancelled','corrected'
        )
    ),
    consumption_key TEXT,
    delivered_at TIMESTAMPTZ,
    opened_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, message_id, recipient_seat_id),
    FOREIGN KEY (tenant_id, organization_id, message_id)
        REFERENCES workforce_mail_messages (
            tenant_id, organization_id, message_id
        ) ON DELETE RESTRICT
);

CREATE INDEX workforce_mail_inbox_idx
    ON workforce_mail_recipients (
        tenant_id, organization_id, recipient_seat_id, state, updated_at
    );

CREATE TABLE workforce_mail_access_log (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (
        action IN (
            'queued','delivered','opened','acknowledged','replied',
            'resolved','expired','rejected','cancelled','corrected'
        )
    ),
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, message_id,
        seat_id, action, idempotency_key
    )
);

CREATE TABLE workforce_mail_timeouts (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    recipient_seat_id TEXT NOT NULL,
    timeout_action TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (
        tenant_id, organization_id, message_id, recipient_seat_id
    )
);

CREATE TABLE workforce_wake_requests (
    tenant_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    wake_request_id TEXT NOT NULL,
    seat_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    source_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued','dispatched','failed','coalesced')),
    scheduled_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, organization_id, wake_request_id),
    UNIQUE (tenant_id, organization_id, seat_id, reason, source_id)
);

CREATE INDEX workforce_wake_due_idx
    ON workforce_wake_requests (tenant_id, state, scheduled_at);

CREATE OR REPLACE FUNCTION workforce_guard_mail_message_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE'
       OR OLD.tenant_id IS DISTINCT FROM NEW.tenant_id
       OR OLD.organization_id IS DISTINCT FROM NEW.organization_id
       OR OLD.message_id IS DISTINCT FROM NEW.message_id
       OR OLD.thread_id IS DISTINCT FROM NEW.thread_id
       OR OLD.in_reply_to IS DISTINCT FROM NEW.in_reply_to
       OR OLD.sender_department_id IS DISTINCT FROM NEW.sender_department_id
       OR OLD.sender_seat_id IS DISTINCT FROM NEW.sender_seat_id
       OR OLD.sender_key_id IS DISTINCT FROM NEW.sender_key_id
       OR OLD.kind IS DISTINCT FROM NEW.kind
       OR OLD.parent_intent_id IS DISTINCT FROM NEW.parent_intent_id
       OR OLD.priority IS DISTINCT FROM NEW.priority
       OR OLD.deadline IS DISTINCT FROM NEW.deadline
       OR OLD.timeout_action IS DISTINCT FROM NEW.timeout_action
       OR OLD.classification IS DISTINCT FROM NEW.classification
       OR OLD.idempotency_key IS DISTINCT FROM NEW.idempotency_key
       OR OLD.envelope_hash IS DISTINCT FROM NEW.envelope_hash
       OR OLD.sealed_envelope IS DISTINCT FROM NEW.sealed_envelope
       OR OLD.automatic IS DISTINCT FROM NEW.automatic
       OR OLD.binding_kind IS DISTINCT FROM NEW.binding_kind
       OR OLD.sealed_binding IS DISTINCT FROM NEW.sealed_binding
       OR OLD.created_at IS DISTINCT FROM NEW.created_at
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at
    THEN
        RAISE EXCEPTION 'immutable Workforce mail message mutation'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workforce_mail_messages_immutable
    BEFORE UPDATE OR DELETE ON workforce_mail_messages
    FOR EACH ROW EXECUTE FUNCTION workforce_guard_mail_message_mutation();

CREATE TRIGGER workforce_mail_access_immutable
    BEFORE UPDATE OR DELETE ON workforce_mail_access_log
    FOR EACH ROW EXECUTE FUNCTION workforce_reject_immutable_mutation();
