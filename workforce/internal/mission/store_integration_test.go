package mission

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/ledger"
)

const missionPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var missionPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startMissionPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mission integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	}
	missionPool, err = waitMissionPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "mission integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, missionPool, missionTestTime()); err != nil {
		missionPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "mission migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	missionPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_StoreCommitsActivationAndMaterialVersion(t *testing.T) {
	ctx := context.Background()
	store, value, privateKey := missionStoreFixture(t, "versions")
	prepared, err := store.PrepareActivation(value)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	deduplicated, err := store.CommitActivationTx(ctx, tx, prepared, 1, missionTestTime())
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if deduplicated {
		t.Fatal("first activation was reported as a replay")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err = missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	deduplicated, err = store.CommitActivationTx(ctx, tx, prepared, 1, missionTestTime())
	if err != nil || !deduplicated {
		_ = tx.Rollback(ctx)
		t.Fatalf("exact activation replay = %v, %v", deduplicated, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	draft := validDraft()
	draft.Purpose = "A materially amended verified company purpose"
	next, err := BuildAuthorityDraft(
		store.organizationID, store.ownerID, missionTestTime(), store.keyID,
		value.IssuerPolicy.IssuerKeyID,
		ed25519.PublicKey(mustDecodeBase64URL(t, value.IssuerPolicy.IssuerPublicKey)),
		2, draft,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signActivation(&next, store.keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	preparedNext, err := store.PrepareActivation(next)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitVersionTx(ctx, tx, preparedNext, 1, missionTestTime()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var records, receipts int
	var state string
	if err := missionPool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_company_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_company_authority_change_receipts
		   WHERE tenant_id=$1 AND organization_id=$2),state
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.tenantID, store.organizationID).Scan(&records, &receipts, &state); err != nil {
		t.Fatal(err)
	}
	if records != 8 || receipts != 4 || state != "paused" {
		t.Fatalf("records=%d receipts=%d state=%s", records, receipts, state)
	}
}

func TestIntegration_StoreRejectsConflictingReplayAndStaleVersion(t *testing.T) {
	ctx := context.Background()
	store, value, privateKey := missionStoreFixture(t, "conflict")
	prepared, err := store.PrepareActivation(value)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitActivationTx(ctx, tx, prepared, 1, missionTestTime()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	value.Mission.Purpose = "a different same-version value"
	if err := signActivation(&value, store.keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	conflicting, err := store.PrepareActivation(value)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitActivationTx(
		ctx, tx, conflicting, 1, missionTestTime(),
	); err != ErrConflict {
		_ = tx.Rollback(ctx)
		t.Fatalf("conflicting replay = %v", err)
	}
	_ = tx.Rollback(ctx)
	tx, err = missionPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitVersionTx(
		ctx, tx, prepared, 1, missionTestTime(),
	); err != ErrConflict {
		_ = tx.Rollback(ctx)
		t.Fatalf("stale authority version = %v", err)
	}
	_ = tx.Rollback(ctx)
}

func missionStoreFixture(t *testing.T, label string) (*Store, ActivationAuthority, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := "tenant:mission:" + label
	organizationID := contracts.OrganizationID("organization:mission:" + label)
	ownerID := contracts.OwnerID("owner:mission:" + label)
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(
		missionPool, session.UserVault(), tenantID, organizationID, ownerID,
		"key:founder:"+label, publicKey, func() time.Time { return missionTestTime() },
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := BuildActivationDraft(
		organizationID, ownerID, missionTestTime(), store.keyID,
		"key:issuer:"+label, issuerPublic, validDraft(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signActivation(&value, store.keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	return store, value, privateKey
}

func missionTestTime() time.Time {
	return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
}

func mustDecodeBase64URL(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func startMissionPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-mission-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		missionPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
	if err != nil {
		return container, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf("invalid PostgreSQL port %q", address)
	}
	return container, "postgres://postgres:workforce-test-password@127.0.0.1:" +
		address[index+1:] + "/workforce?sslmode=disable", nil
}

func waitMissionPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if err := pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
