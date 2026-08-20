package customer

import (
	"context"
	"fmt"

	"centra/workforce/internal/contracts"
)

func (store *Store) recordObservation(
	ctx context.Context,
	observation Observation,
) (contracts.ContentHash, error) {
	if err := observation.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	id, err := randomID("cust-observation")
	if err != nil {
		return contracts.ContentHash{}, err
	}
	canonical, hash, sealed, err := store.seal(
		store.observationAD(observation.OrganizationID, id), &observation,
	)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_customer_observations (
			tenant_id,organization_id,observation_id,connection_id,connection_version,
			operation,customer_id,recipient_hash,external_id,idempotency_key,
			external_state,authority,canonical_hash,sealed_record,
			provider_observed_at,captured_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT DO NOTHING
	`, store.tenantID, observation.OrganizationID, id, observation.ConnectionID,
		observation.ConnectionVersion, observation.Operation, observation.CustomerID,
		observation.RecipientHash.Digest, observation.ExternalID,
		observation.IdempotencyKey, observation.State, observation.Authority,
		hash.Digest, sealed, observation.ProviderObservedAt, observation.CapturedAt)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("customer adapter: persist observation: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingID string
		var existingSealed []byte
		err := store.pool.QueryRow(ctx, `
			SELECT observation_id,sealed_record
			FROM workforce_customer_observations
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
			  AND external_state=$7 AND canonical_hash=$8
		`, store.tenantID, observation.OrganizationID, observation.ConnectionID,
			observation.ConnectionVersion, observation.Operation,
			observation.IdempotencyKey, observation.State, hash.Digest).Scan(
			&existingID, &existingSealed,
		)
		if err != nil || !store.matches(
			store.observationAD(observation.OrganizationID, existingID), existingSealed, canonical,
		) {
			return contracts.ContentHash{}, ErrConflict
		}
	}
	return hash, nil
}
