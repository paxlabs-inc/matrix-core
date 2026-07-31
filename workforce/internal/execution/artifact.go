package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

const maxRuntimeArtifactBytes = 2 << 20

// PutArtifact durably seals an immutable kernel-owned wake artifact. It is
// idempotent only when the exact content already exists under the same kind.
func (store *Store) PutArtifact(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	kind string,
	content []byte,
) (contracts.ContentHash, error) {
	if organizationID == "" || wakeID == "" || !validArtifactKind(kind) ||
		len(content) == 0 || len(content) > maxRuntimeArtifactBytes {
		return contracts.ContentHash{}, fmt.Errorf(
			"execution: runtime artifact identity or size is invalid",
		)
	}
	now, err := store.currentTime()
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(content)
	hash := contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
	sealed, err := store.vault.SealRecord(
		store.artifactAD(organizationID, wakeID, kind), content,
	)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf(
			"execution: seal runtime artifact: %w", err,
		)
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_wake_runtime_artifacts (
			tenant_id,organization_id,wake_id,artifact_kind,
			content_hash,sealed_content,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT DO NOTHING
	`, store.tenantID, organizationID, wakeID, kind, hash.Digest, sealed, now)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf(
			"execution: persist runtime artifact: %w", err,
		)
	}
	if command.RowsAffected() == 1 {
		return hash, nil
	}
	var existingHash string
	if err := store.pool.QueryRow(ctx, `
		SELECT content_hash
		FROM workforce_wake_runtime_artifacts
		WHERE tenant_id=$1 AND organization_id=$2
		  AND wake_id=$3 AND artifact_kind=$4
	`, store.tenantID, organizationID, wakeID, kind).Scan(
		&existingHash,
	); err != nil {
		return contracts.ContentHash{}, fmt.Errorf(
			"execution: inspect runtime artifact: %w", err,
		)
	}
	if existingHash != hash.Digest {
		return contracts.ContentHash{}, ErrConflict
	}
	return hash, nil
}

// OpenArtifact returns the exact Vault-opened content after checking its
// durable content hash.
func (store *Store) OpenArtifact(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	kind string,
) ([]byte, contracts.ContentHash, error) {
	if organizationID == "" || wakeID == "" || !validArtifactKind(kind) {
		return nil, contracts.ContentHash{}, fmt.Errorf(
			"execution: runtime artifact identity is invalid",
		)
	}
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT content_hash,sealed_content
		FROM workforce_wake_runtime_artifacts
		WHERE tenant_id=$1 AND organization_id=$2
		  AND wake_id=$3 AND artifact_kind=$4
	`, store.tenantID, organizationID, wakeID, kind).Scan(
		&expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contracts.ContentHash{}, ErrConflict
	}
	if err != nil {
		return nil, contracts.ContentHash{}, fmt.Errorf(
			"execution: load runtime artifact: %w", err,
		)
	}
	opened, err := store.vault.OpenRecord(
		store.artifactAD(organizationID, wakeID, kind), sealed,
	)
	if err != nil {
		return nil, contracts.ContentHash{}, fmt.Errorf(
			"execution: open runtime artifact: %w", err,
		)
	}
	sum := sha256.Sum256(opened)
	actualHash := hex.EncodeToString(sum[:])
	if actualHash != expectedHash || len(opened) == 0 ||
		len(opened) > maxRuntimeArtifactBytes {
		return nil, contracts.ContentHash{}, fmt.Errorf(
			"execution: runtime artifact integrity failure",
		)
	}
	return opened, contracts.ContentHash{
		Algorithm: "sha256", Digest: actualHash,
	}, nil
}

func validArtifactKind(kind string) bool {
	if kind == "" || len(kind) > 64 || strings.TrimSpace(kind) != kind {
		return false
	}
	for _, character := range kind {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func (store *Store) artifactAD(
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	kind string,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.execution.artifact",
		Stream: string(organizationID) + "/" + string(wakeID) + "/" + kind,
		Schema: contracts.SchemaVersionV1,
	}
}
