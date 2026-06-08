-- 004_streams.sql — PaymentStreams mirror (docs/05-api.md §5.5, Phase 6)

BEGIN;

CREATE TABLE IF NOT EXISTS streams (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain_stream_id    TEXT NOT NULL,
    service_id         UUID NOT NULL REFERENCES services(id),
    caller_did         TEXT NOT NULL,
    caller_wallet      TEXT NOT NULL,
    payee_address      TEXT NOT NULL,
    rate_per_second_wei TEXT NOT NULL,
    cap_wei            TEXT NOT NULL,
    settled_wei        TEXT NOT NULL DEFAULT '0',
    metered_wei        TEXT NOT NULL DEFAULT '0',
    status             TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    open_tx            TEXT,
    last_settle_tx     TEXT,
    close_tx           TEXT,
    opened_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at          TIMESTAMPTZ,
    last_metered_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_streams_caller_status ON streams (caller_did, status);
CREATE INDEX IF NOT EXISTS idx_streams_service ON streams (service_id);

COMMIT;
