package businessoutcome

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

func (store *Store) ApplyCorrection(ctx context.Context, value Correction) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID || value.Body.EffectiveAt.After(now) {
		return false, ErrUnauthorized
	}
	exists, err := store.recordPointerExists(ctx, value.Body.Target)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	bound, err := store.recordPointerBelongsToInitiative(ctx, value.Body.Target, value.Body.InitiativeID)
	if err != nil {
		return false, err
	}
	if !bound {
		return false, ErrConflict
	}
	if value.Body.Replacement != nil {
		exists, err := store.recordPointerExists(ctx, *value.Body.Replacement)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, ErrNotFound
		}
		bound, err := store.recordPointerBelongsToInitiative(ctx, *value.Body.Replacement, value.Body.InitiativeID)
		if err != nil {
			return false, err
		}
		if !bound {
			return false, ErrConflict
		}
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.correctionAD(value.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal correction: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin correction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "correction", value.Body.Target.Kind, value.Body.Target.ID); err != nil {
		return false, err
	}
	publicKey, _, _, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || VerifyCorrection(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_business_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
	`, store.tenantID, store.organizationID, value.Body.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("business outcome: inspect correction replay: %w", err)
	}
	var replacementKind, replacementID, replacementHash any
	if value.Body.Replacement != nil {
		replacementKind = value.Body.Replacement.Kind
		replacementID = value.Body.Replacement.ID
		replacementHash = value.Body.Replacement.Hash.Digest
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_corrections (
			tenant_id,organization_id,correction_id,initiative_id,target_kind,target_id,
			target_hash,replacement_kind,replacement_id,replacement_hash,material,
			content_hash,canonical_hash,author_seat_id,key_id,sealed_correction,
			effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.InitiativeID,
		value.Body.Target.Kind, value.Body.Target.ID, value.Body.Target.Hash.Digest,
		replacementKind, replacementID, replacementHash, value.Body.Material,
		value.ContentHash.Digest, canonicalHash.Digest, value.Body.AuthorSeatID,
		value.Signature.KeyID, sealed, value.Body.EffectiveAt, now)
	if err != nil {
		return false, fmt.Errorf("business outcome: insert correction: %w", err)
	}
	_, err = tx.Exec(ctx, `
		WITH RECURSIVE affected(kind,id,hash,depth,material,path) AS (
			SELECT $4::text,$5::text,$6::text,0,$7::boolean,ARRAY[$4||':'||$5||':'||$6]::text[]
			UNION ALL
			SELECT edge.consumer_kind,edge.consumer_id,edge.consumer_hash,
			       affected.depth+1,affected.material OR edge.material,
			       affected.path||(edge.consumer_kind||':'||edge.consumer_id||':'||edge.consumer_hash)
			FROM affected
			JOIN workforce_business_lineage_edges edge
			  ON edge.tenant_id=$1 AND edge.organization_id=$2
			 AND edge.source_kind=affected.kind AND edge.source_id=affected.id
			 AND edge.source_hash=affected.hash
			WHERE affected.depth<128
			  AND NOT (edge.consumer_kind||':'||edge.consumer_id||':'||edge.consumer_hash)=ANY(affected.path)
		)
		INSERT INTO workforce_business_contamination (
			tenant_id,organization_id,correction_id,affected_kind,affected_id,
			affected_hash,derivation_depth,material,state,contaminated_at
		)
		SELECT $1,$2,$3,kind,id,hash,MIN(depth),BOOL_OR(material),'open',$8
		FROM affected GROUP BY kind,id,hash
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.Target.Kind,
		value.Body.Target.ID, value.Body.Target.Hash.Digest, value.Body.Material, now)
	if err != nil {
		return false, fmt.Errorf("business outcome: propagate correction contamination: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit correction: %w", err)
	}
	return false, nil
}

func (store *Store) ResolveCorrection(ctx context.Context, value CorrectionResolution) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.Validate() != nil || value.Body.OrganizationID != store.organizationID || value.Body.ResolvedAt.After(now) {
		return false, ErrUnauthorized
	}
	exists, err := store.recordPointerExists(ctx, value.Body.Replacement)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	var correctionInitiative string
	err = store.pool.QueryRow(ctx, `
		SELECT initiative_id FROM workforce_business_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
	`, store.tenantID, store.organizationID, value.Body.CorrectionID).Scan(&correctionInitiative)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("business outcome: inspect correction initiative: %w", err)
	}
	bound, err := store.recordPointerBelongsToInitiative(ctx, value.Body.Replacement, correctionInitiative)
	if err != nil {
		return false, err
	}
	if !bound {
		return false, ErrConflict
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return false, err
	}
	canonicalHash, err := contracts.HashCanonical(&value)
	if err != nil {
		return false, err
	}
	sealed, err := store.vault.SealRecord(store.resolutionAD(value.Body.ID), canonical)
	if err != nil {
		return false, fmt.Errorf("business outcome: seal correction resolution: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("business outcome: begin correction resolution: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockScope(ctx, tx, store.tenantID, string(store.organizationID), "correction-resolution", string(value.Body.CorrectionID)); err != nil {
		return false, err
	}
	publicKey, _, _, err := store.resolveCurrentSeatKeyTx(ctx, tx, value.Body.AuthorSeatID, value.Signature.KeyID, now)
	if err != nil || VerifyCorrectionResolution(value, publicKey) != nil {
		return false, ErrUnauthorized
	}
	var declaredKind, declaredID, declaredHash sql.NullString
	err = tx.QueryRow(ctx, `
		SELECT replacement_kind,replacement_id,replacement_hash
		FROM workforce_business_corrections
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, value.Body.CorrectionID).Scan(&declaredKind, &declaredID, &declaredHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("business outcome: load correction for resolution: %w", err)
	}
	if declaredKind.Valid && (declaredKind.String != value.Body.Replacement.Kind ||
		!declaredID.Valid || declaredID.String != value.Body.Replacement.ID ||
		!declaredHash.Valid || declaredHash.String != value.Body.Replacement.Hash.Digest) {
		return false, ErrConflict
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_business_correction_resolutions
		WHERE tenant_id=$1 AND organization_id=$2 AND correction_id=$3
	`, store.tenantID, store.organizationID, value.Body.CorrectionID).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("business outcome: inspect resolution replay: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_business_correction_resolutions (
			tenant_id,organization_id,resolution_id,correction_id,replacement_kind,
			replacement_id,replacement_hash,content_hash,canonical_hash,author_seat_id,
			key_id,sealed_resolution,resolved_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.tenantID, store.organizationID, value.Body.ID, value.Body.CorrectionID,
		value.Body.Replacement.Kind, value.Body.Replacement.ID, value.Body.Replacement.Hash.Digest,
		value.ContentHash.Digest, canonicalHash.Digest, value.Body.AuthorSeatID,
		value.Signature.KeyID, sealed, value.Body.ResolvedAt, now)
	if err != nil {
		return false, fmt.Errorf("business outcome: insert correction resolution: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_business_contamination
		SET state='reconciled',resolved_at=$1,resolution_id=$2,
		    replacement_kind=$3,replacement_id=$4,replacement_hash=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND correction_id=$8 AND state='open'
	`, value.Body.ResolvedAt, value.Body.ID, value.Body.Replacement.Kind,
		value.Body.Replacement.ID, value.Body.Replacement.Hash.Digest,
		store.tenantID, store.organizationID, value.Body.CorrectionID)
	if err != nil {
		return false, fmt.Errorf("business outcome: close contamination: %w", err)
	}
	if command.RowsAffected() == 0 {
		return false, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("business outcome: commit correction resolution: %w", err)
	}
	return false, nil
}

func (store *Store) recordPointerExists(ctx context.Context, value RecordPointer) (bool, error) {
	if value.Validate() != nil {
		return false, fmt.Errorf("business outcome: record pointer is invalid")
	}
	var exists bool
	var err error
	switch value.Kind {
	case "metric":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_metric_definitions
				WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3 AND definition_hash=$4
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest).Scan(&exists)
	case "observation":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_observations
				WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3 AND content_hash=$4
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest).Scan(&exists)
	case "outcome":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_outcomes
				WHERE tenant_id=$1 AND organization_id=$2 AND outcome_id=$3 AND record_hash=$4
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest).Scan(&exists)
	case "gate":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_gate_decisions
				WHERE tenant_id=$1 AND organization_id=$2 AND gate_id=$3 AND decision_hash=$4
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest).Scan(&exists)
	case "source":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_source_events
				WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3 AND source_hash=$4
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest).Scan(&exists)
	default:
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_lineage_edges
				WHERE tenant_id=$1 AND organization_id=$2
				  AND ((source_kind=$3 AND source_id=$4 AND source_hash=$5)
				    OR (consumer_kind=$3 AND consumer_id=$4 AND consumer_hash=$5))
			)
		`, store.tenantID, store.organizationID, value.Kind, value.ID, value.Hash.Digest).Scan(&exists)
	}
	if err != nil {
		return false, fmt.Errorf("business outcome: inspect record pointer: %w", err)
	}
	return exists, nil
}

func (store *Store) recordPointerBelongsToInitiative(ctx context.Context, value RecordPointer, initiativeID string) (bool, error) {
	if value.Validate() != nil || validateToken("initiative_id", initiativeID) != nil {
		return false, fmt.Errorf("business outcome: record lineage scope is invalid")
	}
	var bound bool
	var err error
	switch value.Kind {
	case "metric":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_metric_definitions
				WHERE tenant_id=$1 AND organization_id=$2 AND metric_id=$3
				  AND definition_hash=$4 AND initiative_id=$5
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest, initiativeID).Scan(&bound)
	case "observation":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_observations
				WHERE tenant_id=$1 AND organization_id=$2 AND observation_id=$3
				  AND content_hash=$4 AND initiative_id=$5
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest, initiativeID).Scan(&bound)
	case "outcome":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_outcomes
				WHERE tenant_id=$1 AND organization_id=$2 AND outcome_id=$3
				  AND record_hash=$4 AND initiative_id=$5
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest, initiativeID).Scan(&bound)
	case "gate":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_gate_decisions
				WHERE tenant_id=$1 AND organization_id=$2 AND gate_id=$3
				  AND decision_hash=$4 AND initiative_id=$5
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest, initiativeID).Scan(&bound)
	case "source":
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM workforce_business_source_events source
				JOIN workforce_business_observations observation
				  ON observation.tenant_id=source.tenant_id
				 AND observation.organization_id=source.organization_id
				 AND observation.observation_id=source.observation_id
				WHERE source.tenant_id=$1 AND source.organization_id=$2
				  AND source.event_id=$3 AND source.source_hash=$4
				  AND observation.initiative_id=$5
			)
		`, store.tenantID, store.organizationID, value.ID, value.Hash.Digest, initiativeID).Scan(&bound)
	default:
		err = store.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_business_lineage_edges
				WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
				  AND ((source_kind=$4 AND source_id=$5 AND source_hash=$6)
				    OR (consumer_kind=$4 AND consumer_id=$5 AND consumer_hash=$6))
			)
		`, store.tenantID, store.organizationID, initiativeID, value.Kind, value.ID, value.Hash.Digest).Scan(&bound)
	}
	if err != nil {
		return false, fmt.Errorf("business outcome: inspect record lineage scope: %w", err)
	}
	return bound, nil
}

func (store *Store) correctionAD(id CorrectionID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.correction",
		Stream: strings.Join([]string{string(store.organizationID), string(id)}, "/"), Schema: CorrectionSchemaVersion}
}

func (store *Store) resolutionAD(id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.business-outcome.correction-resolution",
		Stream: strings.Join([]string{string(store.organizationID), id}, "/"), Schema: CorrectionSchemaVersion}
}
