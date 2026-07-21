-- layerx 008_perps_delegation — owner-signed bounded agent delegations.
-- A delegation is created only from an owner-DID signature over the canonical
-- grant; a delegate can never create, widen, renew, or revoke its own grant.
-- Terminal grants (EXPIRED/REVOKED) never reactivate.

BEGIN;

CREATE TABLE IF NOT EXISTS perp_delegations (
    id                           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_did                    TEXT NOT NULL,
    delegate_did                 TEXT NOT NULL,
    membership_tier              TEXT NOT NULL,
    allowed_markets              TEXT[] NOT NULL CHECK (array_length(allowed_markets, 1) > 0),
    allowed_order_types          TEXT[] NOT NULL CHECK (array_length(allowed_order_types, 1) > 0),
    max_order_notional_usdx      BIGINT NOT NULL CHECK (max_order_notional_usdx > 0),
    max_position_notional_usdx   BIGINT NOT NULL CHECK (max_position_notional_usdx >= max_order_notional_usdx),
    max_leverage_x               BIGINT NOT NULL CHECK (max_leverage_x > 0),
    max_daily_notional_usdx      BIGINT NOT NULL CHECK (max_daily_notional_usdx > 0),
    max_daily_realized_loss_usdx BIGINT NOT NULL CHECK (max_daily_realized_loss_usdx >= 0),
    grant_signature              TEXT NOT NULL,
    public_key                   TEXT NOT NULL,
    status                       TEXT NOT NULL DEFAULT 'PROPOSED'
                                 CHECK (status IN ('PROPOSED', 'ACTIVE', 'EXPIRED', 'REVOKED')),
    expires_at                   TIMESTAMPTZ NOT NULL,
    revoked_at                   TIMESTAMPTZ,
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT perp_delegations_not_self CHECK (owner_did <> delegate_did),
    CONSTRAINT perp_delegations_revoked_at CHECK (status <> 'REVOKED' OR revoked_at IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS perp_delegations_one_active_uidx
    ON perp_delegations (owner_did, delegate_did) WHERE status IN ('PROPOSED', 'ACTIVE');
CREATE INDEX IF NOT EXISTS perp_delegations_owner_idx ON perp_delegations (owner_did, created_at DESC);
CREATE INDEX IF NOT EXISTS perp_delegations_delegate_idx ON perp_delegations (delegate_did) WHERE status = 'ACTIVE';

COMMIT;
