package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/mission"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workorder"
	"matrix/workforce/scheduler"
)

const controlPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var controlPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startControlPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "controlapi integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(
			context.Background(), 20*time.Second,
		)
		defer stopCancel()
		_ = exec.CommandContext(
			stopCtx, "docker", "rm", "-f", container,
		).Run()
	}
	controlPool, err = waitControlPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "controlapi integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(
		ctx, controlPool, controlIntegrationTime(),
	); err != nil {
		controlPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "controlapi migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	controlPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_LifecyclePublishIsExactlyIdempotent(t *testing.T) {
	fixture := newControlFixture(t, "lifecycle")
	if page, err := fixture.service.List(
		context.Background(), fixture.principal, "departments", "", 0,
	); err != nil || page.Resource != "departments" {
		t.Fatalf("default resource limit page=%#v error=%v", page, err)
	}
	if page, err := fixture.service.Events(
		context.Background(), fixture.principal, 0, 0,
	); err != nil || page.SchemaVersion != SchemaVersion {
		t.Fatalf("default event limit page=%#v error=%v", page, err)
	}
	if _, err := fixture.service.Events(
		context.Background(), fixture.principal, 0, 501,
	); err == nil {
		t.Fatal("event page above the hard limit was accepted")
	}
	event := LifecycleEvent{
		ID:              "event:wake:running",
		OrganizationID:  fixture.principal.OrganizationID,
		Type:            "wake.running",
		ResourceKind:    "wake",
		ResourceID:      "wake:one",
		ResourceVersion: 1,
		Fields: map[string]any{
			"state": "working", "intent_id": "intent:one",
		},
	}
	wrongOrganization := event
	wrongOrganization.OrganizationID = "organization:other"
	if _, err := fixture.service.Publish(
		context.Background(), fixture.principal, wrongOrganization,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-organization event = %v", err)
	}
	incomplete := event
	incomplete.ID = ""
	if _, err := fixture.service.Publish(
		context.Background(), fixture.principal, incomplete,
	); err == nil {
		t.Fatal("incomplete lifecycle event was published")
	}
	first, err := fixture.service.Publish(
		context.Background(), fixture.principal, event,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed := event
	replayed.Fields = map[string]any{
		"intent_id": "intent:one", "state": "working",
	}
	second, err := fixture.service.Publish(
		context.Background(), fixture.principal, replayed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor == 0 || second.Cursor != first.Cursor ||
		!second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("idempotent lifecycle = first=%+v second=%+v", first, second)
	}
	conflict := event
	conflict.Fields = map[string]any{
		"state": "verifying", "intent_id": "intent:one",
	}
	if _, err := fixture.service.Publish(
		context.Background(), fixture.principal, conflict,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting lifecycle replay = %v", err)
	}
	var count int
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
	`, fixture.principal.TenantID, fixture.principal.OrganizationID,
		event.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("lifecycle rows = %d", count)
	}
}

func TestIntegration_ActivationAndWorkOrderUseExactRuntimeModel(t *testing.T) {
	fixture := newControlFixture(t, "work-order")
	activation, err := fixture.activate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if activation.Departments != 7 || activation.Seats != 21 ||
		activation.Deduplicated || activation.MissionVersion != 1 ||
		activation.ConstitutionVersion != 1 ||
		activation.OrganizationSchema != "workforce.organization.v2" {
		t.Fatalf("activation = %+v", activation)
	}
	var authorityRecords, v2Projection int
	if err := controlPool.QueryRow(context.Background(), `
		SELECT
		  (SELECT COUNT(*) FROM workforce_company_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_organization_v2_projection
		   WHERE tenant_id=$1 AND organization_id=$2)
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&authorityRecords, &v2Projection,
	); err != nil {
		t.Fatal(err)
	}
	if authorityRecords != 4 || v2Projection != 1 {
		t.Fatalf("company authority=%d organization-v2=%d", authorityRecords, v2Projection)
	}
	order := fixture.order("one", "mimo", "mimo-v2.5-pro")
	if err := workorder.Sign(
		&order, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, order,
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.GoalID == "" || len(created.IntentIDs) != 2 ||
		created.WakeID == "" || created.Deduplicated {
		t.Fatalf("created Work Order = %+v", created)
	}
	replayed, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, order,
	)
	if err != nil || !replayed.Deduplicated ||
		replayed.WakeID != created.WakeID {
		t.Fatalf("replayed Work Order = %+v, %v", replayed, err)
	}
	mismatch := fixture.order("mismatch", "mimo", "mimo-v2.5-pro-other")
	if err := workorder.Sign(
		&mismatch, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, mismatch,
	); err == nil || !strings.Contains(
		err.Error(), "does not match the executable runtime",
	) {
		t.Fatalf("mismatched runtime model = %v", err)
	}
	conflict := order
	conflict.Objective = "A conflicting replay with the same idempotency identity"
	if err := SignWorkOrder(&conflict, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, conflict,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Work Order replay = %v", err)
	}
	badTime := fixture.order("bad-time", "mimo", "mimo-v2.5-pro")
	badTime.CreatedAt = fixture.now.Add(-time.Hour)
	if err := SignWorkOrder(&badTime, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(
		context.Background(), fixture.principal, badTime,
	); err == nil {
		t.Fatal("stale Work Order time was accepted")
	}
	noModel := *fixture.service
	noModel.runtimeModelProvider = ""
	noModel.runtimeModelID = ""
	missingModel := fixture.order("no-model", "mimo", "mimo-v2.5-pro")
	if err := SignWorkOrder(&missingModel, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := noModel.CreateWorkOrder(
		context.Background(), fixture.principal, missingModel,
	); err == nil {
		t.Fatal("Work Order without executable runtime model was accepted")
	}
	noScheduler := *fixture.service
	noScheduler.scheduler = nil
	if _, err := noScheduler.CreateWorkOrder(
		context.Background(), fixture.principal, missingModel,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Work Order without scheduler = %v", err)
	}
	var goals, intents, wakes, events int
	if err := controlPool.QueryRow(context.Background(), `
		SELECT
		  COUNT(*) FILTER (WHERE node_kind='goal'),
		  COUNT(*) FILTER (WHERE node_kind='intent')
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&goals, &intents,
	); err != nil {
		t.Fatal(err)
	}
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&wakes,
	); err != nil {
		t.Fatal(err)
	}
	if err := controlPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2
		  AND event_type='work_order.queued'
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&events,
	); err != nil {
		t.Fatal(err)
	}
	if goals != 1 || intents != 2 || wakes != 1 || events != 1 {
		t.Fatalf(
			"atomic projections goals=%d intents=%d wakes=%d events=%d",
			goals, intents, wakes, events,
		)
	}
}

func TestIntegration_InvalidCompanyCommandsNeverMutateProjection(t *testing.T) {
	fixture := newControlFixture(t, "invalid-company-commands")
	ctx := context.Background()
	if _, err := fixture.activate(ctx); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		action       string
		resourceKind string
		resourceID   string
		change       any
	}{
		{"pause wrong resource", "pause_company", "seat", "seat:one", map[string]string{"reason": "wrong scope"}},
		{"pause empty reason", "pause_company", "company", string(fixture.principal.OrganizationID), map[string]string{"reason": ""}},
		{"pause unknown field", "pause_company", "company", string(fixture.principal.OrganizationID), map[string]any{"reason": "valid", "extra": true}},
		{"issuer wrong resource", "revoke_company_issuer", "company", "company", map[string]any{"authority_id": "issuer-policy:one", "version": 1, "reason": "wrong scope"}},
		{"issuer incomplete", "revoke_company_issuer", "company_issuer_policy", "company-issuer-policy:" + string(fixture.principal.OrganizationID), map[string]any{"authority_id": "wrong", "version": 0, "reason": ""}},
		{"issuer stale version", "revoke_company_issuer", "company_issuer_policy", "company-issuer-policy:" + string(fixture.principal.OrganizationID), map[string]any{"authority_id": "company-issuer-policy:" + string(fixture.principal.OrganizationID), "version": 99, "reason": "stale"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := fixture.signedCommand(
				t, fmt.Sprintf("invalid-%d", index), test.action,
				test.resourceKind, test.resourceID, 0, test.change,
			)
			if _, err := fixture.service.ApplyCommand(ctx, fixture.principal, command); err == nil {
				t.Fatal("invalid company command was accepted")
			}
		})
	}
	var state string
	if err := controlPool.QueryRow(ctx, `
		SELECT state FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" {
		t.Fatalf("invalid commands changed company state to %s", state)
	}
	rolledBack, err := controlPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := rolledBack.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cancelCompanyRuntimeLeases(
		ctx, rolledBack, fixture.principal, "rolled-back transaction", fixture.now,
	); err == nil {
		t.Fatal("cancel on a rolled-back transaction succeeded")
	}
}

func TestIntegration_ActivationRejectsTamperAndRollsBackAtomicAuthority(t *testing.T) {
	fixture := newControlFixture(t, "activation-tamper")
	ctx := context.Background()
	preview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "Tamper Proof Workforce", KeyID: fixture.ownerKeyID,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(&preview, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	preview.Authority.Mission.Purpose = "tampered after local signature"
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, Authority: preview.Authority,
			SkillContracts: preview.SkillContracts,
		},
	); err == nil {
		t.Fatal("tampered founder Mission was accepted")
	}
	preview.Authority.Mission.Purpose = testActivationDraft().Purpose
	if err := mission.SignFounderMission(
		&preview.Authority.Mission, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	preview.SkillContracts[0].Signature.Value = base64.RawURLEncoding.EncodeToString(
		make([]byte, ed25519.SignatureSize),
	)
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, Authority: preview.Authority,
			SkillContracts: preview.SkillContracts,
		},
	); err == nil {
		t.Fatal("invalid skill authority was accepted")
	}
	missingContract := preview
	missingContract.SkillContracts = missingContract.SkillContracts[:len(missingContract.SkillContracts)-1]
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: missingContract.Seed, Authority: missingContract.Authority,
			SkillContracts: missingContract.SkillContracts,
		},
	); err == nil {
		t.Fatal("incomplete skill contract set was accepted")
	}
	nonCanonical := preview
	nonCanonical.SkillContracts[1].Contract.ID = "skill:substituted"
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: nonCanonical.Seed, Authority: nonCanonical.Authority,
			SkillContracts: nonCanonical.SkillContracts,
		},
	); err == nil {
		t.Fatal("non-canonical skill contract set was accepted")
	}
	var legacy, company, projection int
	if err := controlPool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_company_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_organization_v2_projection
		   WHERE tenant_id=$1 AND organization_id=$2)
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&legacy, &company, &projection,
	); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 || company != 0 || projection != 0 {
		t.Fatalf("rolled back activation legacy=%d company=%d projection=%d", legacy, company, projection)
	}
}

func TestIntegration_V1MigrationPreservesLegacyAuthorityAndRejectsStaleVersion(t *testing.T) {
	fixture := newControlFixture(t, "v1-migration")
	ctx := context.Background()
	activationPreview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "Qualified v1 Workforce", KeyID: fixture.ownerKeyID,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(
		&activationPreview, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	legacyStore, err := policy.New(
		controlPool, fixture.service.vault,
		policy.OwnerRoot{
			TenantID:       fixture.principal.TenantID,
			OrganizationID: fixture.principal.OrganizationID,
			OwnerID:        fixture.principal.OwnerID, KeyID: fixture.ownerKeyID,
			PublicKey: fixture.ownerPrivate.Public().(ed25519.PublicKey),
		},
		func() time.Time { return fixture.now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyStore.PublishSeed(ctx, activationPreview.Seed); err != nil {
		t.Fatal(err)
	}
	root, err := fixture.service.RuntimeOwnerRoot(ctx, fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if root.KeyID != fixture.ownerKeyID || root.OwnerID != fixture.principal.OwnerID {
		t.Fatalf("legacy runtime root = %#v", root)
	}
	wrongOwner := fixture.principal
	wrongOwner.OwnerID = "owner:wrong"
	if _, err := fixture.service.RuntimeOwnerRoot(ctx, wrongOwner); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-owner legacy runtime root = %v", err)
	}
	var legacyHash string
	if err := controlPool.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='organization'
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(&legacyHash); err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.service.PreviewMigration(
		ctx, fixture.principal, MigrationPreviewRequest{
			KeyID: fixture.ownerKeyID, EffectiveAt: fixture.now,
			Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Impact.CurrentDepartments != 7 || preview.Impact.CurrentSeats != 21 ||
		preview.Impact.TargetTemplateID != "organization-template:default-v1" ||
		len(preview.Impact.NewAuthorityKinds) != 4 ||
		len(preview.Impact.IrreversibleConsequences) != 3 ||
		preview.Impact.StartingMicrounits != testActivationDraft().StartingMicrounits {
		t.Fatalf("migration impact = %#v", preview.Impact)
	}
	if err := signMigrationPreview(&preview, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	stale := MigrationBundle{
		Authority:                 preview.Authority,
		LegacyOrganizationVersion: preview.LegacyOrganizationVersion + 1,
	}
	if _, err := fixture.service.MigrateOrganization(
		ctx, fixture.principal, stale,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale migration = %v", err)
	}
	result, err := fixture.service.MigrateOrganization(
		ctx, fixture.principal, MigrationBundle{
			Authority:                 preview.Authority,
			LegacyOrganizationVersion: preview.LegacyOrganizationVersion,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrganizationSchema != "workforce.organization.v2" || result.Deduplicated {
		t.Fatalf("migration result = %#v", result)
	}
	var preservedHash string
	var companyRows int
	if err := controlPool.QueryRow(ctx, `
		SELECT
		  (SELECT canonical_hash FROM workforce_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='organization'),
		  (SELECT COUNT(*) FROM workforce_company_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2)
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&preservedHash, &companyRows,
	); err != nil {
		t.Fatal(err)
	}
	if preservedHash != legacyHash || companyRows != 4 {
		t.Fatalf("legacy hash changed=%v company rows=%d", preservedHash != legacyHash, companyRows)
	}
}

func TestIntegration_CompanyPauseResumeAndIssuerRevocationFailClosed(t *testing.T) {
	fixture := newControlFixture(t, "company-stop")
	ctx := context.Background()
	if _, err := fixture.activate(ctx); err != nil {
		t.Fatal(err)
	}
	pause := fixture.signedCommand(
		t, "pause", "pause_company", "company",
		string(fixture.principal.OrganizationID), 0,
		map[string]string{"reason": "founder containment"},
	)
	if _, err := fixture.service.ApplyCommand(ctx, fixture.principal, pause); err != nil {
		t.Fatal(err)
	}
	order := fixture.order("paused", "mimo", "mimo-v2.5-pro")
	if err := workorder.Sign(&order, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(
		ctx, fixture.principal, order,
	); err == nil || !strings.Contains(err.Error(), "company initiation is paused") {
		t.Fatalf("paused company Work Order = %v", err)
	}
	resume := fixture.signedCommand(
		t, "resume", "resume_company", "company",
		string(fixture.principal.OrganizationID), 1,
		map[string]string{"reason": "founder reviewed containment"},
	)
	if _, err := fixture.service.ApplyCommand(ctx, fixture.principal, resume); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateWorkOrder(ctx, fixture.principal, order); err != nil {
		t.Fatalf("resumed company Work Order = %v", err)
	}
	revoke := fixture.signedCommand(
		t, "revoke-issuer", "revoke_company_issuer", "company_issuer_policy",
		"company-issuer-policy:"+string(fixture.principal.OrganizationID), 0,
		map[string]any{
			"authority_id": "company-issuer-policy:" + string(fixture.principal.OrganizationID),
			"version":      uint64(1), "reason": "founder revoked delegated issuance",
		},
	)
	if _, err := fixture.service.ApplyCommand(ctx, fixture.principal, revoke); err != nil {
		t.Fatal(err)
	}
	resumeAfterRevocation := fixture.signedCommand(
		t, "resume-after-revocation", "resume_company", "company",
		string(fixture.principal.OrganizationID), 2,
		map[string]string{"reason": "attempted resume"},
	)
	if _, err := fixture.service.ApplyCommand(
		ctx, fixture.principal, resumeAfterRevocation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("resume after issuer revocation = %v", err)
	}
}

func TestIntegration_MaterialCompanyAuthorityChangeVersionsAndPauses(t *testing.T) {
	fixture := newControlFixture(t, "authority-change")
	ctx := context.Background()
	if _, err := fixture.activate(ctx); err != nil {
		t.Fatal(err)
	}
	draft := testActivationDraft()
	draft.Purpose = "Build verified products under the amended founder mission"
	preview, err := fixture.service.PreviewAuthorityChange(
		ctx, fixture.principal, AuthorityChangePreviewRequest{
			KeyID: fixture.ownerKeyID, ExpectedVersion: 1,
			EffectiveAt: fixture.now, Authority: draft,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	migrationPreview := MigrationPreview{Authority: preview.Authority}
	if err := signMigrationPreview(
		&migrationPreview, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.ChangeAuthority(
		ctx, fixture.principal, AuthorityChangeBundle{
			ExpectedVersion: 1, Authority: migrationPreview.Authority,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.MissionVersion != 2 || result.ConstitutionVersion != 2 {
		t.Fatalf("authority change = %#v", result)
	}
	var records, receipts int
	var state string
	var missionVersion uint64
	if err := controlPool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_company_authority_records
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_company_authority_change_receipts
		   WHERE tenant_id=$1 AND organization_id=$2),
		  state,mission_version
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&records, &receipts, &state, &missionVersion,
	); err != nil {
		t.Fatal(err)
	}
	if records != 8 || receipts != 4 || state != "paused" || missionVersion != 2 {
		t.Fatalf("records=%d receipts=%d state=%s mission=%d", records, receipts, state, missionVersion)
	}
	if _, err := fixture.service.ChangeAuthority(
		ctx, fixture.principal, AuthorityChangeBundle{
			ExpectedVersion: 1, Authority: migrationPreview.Authority,
		},
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale authority replay = %v", err)
	}
}

func TestIntegration_RuntimeOwnerRootFollowsActivatedControlKey(t *testing.T) {
	fixture := newControlFixture(t, "runtime-owner-root")
	ctx := context.Background()
	if _, err := fixture.service.RuntimeOwnerRoot(
		ctx, fixture.principal,
	); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("root before activation = %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "key:browser:runtime-owner-root"
	if err := fixture.service.RegisterControlKey(
		ctx, fixture.principal, ControlKeyRegistration{
			KeyID:     keyID,
			PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		},
	); err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "Browser-owned Workforce", KeyID: keyID,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(
		&preview, keyID, privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, Authority: preview.Authority,
			SkillContracts: preview.SkillContracts,
		},
	); err != nil {
		t.Fatal(err)
	}
	root, err := fixture.service.RuntimeOwnerRoot(ctx, fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if root.KeyID != keyID || root.OwnerID != fixture.principal.OwnerID ||
		!root.PublicKey.Equal(publicKey) {
		t.Fatalf("activated runtime owner root = %#v", root)
	}
}

func TestIntegration_FounderKeyRotationRequiresDualProofAndMovesRuntimeRoot(t *testing.T) {
	fixture := newControlFixture(t, "founder-key-rotation")
	ctx := context.Background()
	if _, err := fixture.activate(ctx); err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newKeyID := "key:owner:founder-key-rotation:v2"
	draft := testActivationDraft()
	draft.Purpose = "Build verified products after founder key rotation"
	preview, err := fixture.service.PreviewFounderKeyRotation(
		ctx, fixture.principal, FounderKeyRotationPreviewRequest{
			OldKeyID: fixture.ownerKeyID, NewKeyID: newKeyID,
			NewPublicKey:    base64.RawURLEncoding.EncodeToString(newPublic),
			ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: draft,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	change := MigrationPreview{Authority: preview.Authority}
	if err := signMigrationPreview(&change, newKeyID, newPrivate); err != nil {
		t.Fatal(err)
	}
	preview.Authority = change.Authority
	if err := SignFounderKeyRotation(
		&preview.Rotation, fixture.ownerPrivate, newPrivate,
	); err != nil {
		t.Fatal(err)
	}

	forged := preview
	forged.Rotation.NewKeyID = "key:attacker"
	if _, err := fixture.service.RotateFounderKey(
		ctx, fixture.principal, FounderKeyRotationBundle{
			Rotation: forged.Rotation, Authority: forged.Authority,
		},
	); err == nil {
		t.Fatal("tampered dual-signed rotation was accepted")
	}

	result, err := fixture.service.RotateFounderKey(
		ctx, fixture.principal, FounderKeyRotationBundle{
			Rotation: preview.Rotation, Authority: preview.Authority,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.MissionVersion != 2 || result.ConstitutionVersion != 2 {
		t.Fatalf("rotation result = %#v", result)
	}
	root, err := fixture.service.RuntimeOwnerRoot(ctx, fixture.principal)
	if err != nil {
		t.Fatal(err)
	}
	if root.KeyID != newKeyID || !root.PublicKey.Equal(newPublic) {
		t.Fatalf("rotated runtime root = %#v", root)
	}
	if _, err := fixture.service.PreviewAuthorityChange(
		ctx, fixture.principal, AuthorityChangePreviewRequest{
			KeyID: fixture.ownerKeyID, ExpectedVersion: 2,
			EffectiveAt: fixture.now, Authority: draft,
		},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked founder key preview = %v", err)
	}
	var rotations, revocations int
	var state string
	if err := controlPool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_founder_key_rotations
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_owner_control_key_revocations
		   WHERE tenant_id=$1 AND organization_id=$2),state
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.principal.TenantID, fixture.principal.OrganizationID).Scan(
		&rotations, &revocations, &state,
	); err != nil {
		t.Fatal(err)
	}
	if rotations != 1 || revocations != 1 || state != "paused" {
		t.Fatalf("rotation rows=%d revocations=%d state=%s", rotations, revocations, state)
	}
	if _, err := fixture.service.RotateFounderKey(
		ctx, fixture.principal, FounderKeyRotationBundle{
			Rotation: preview.Rotation, Authority: preview.Authority,
		},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed revoked-key rotation = %v", err)
	}
}

func TestIntegration_ClosedRealDatabaseFailsEveryControlWriteClosed(t *testing.T) {
	fixture := newControlFixture(t, "closed-database")
	ctx := context.Background()
	if _, err := fixture.activate(ctx); err != nil {
		t.Fatal(err)
	}

	activation, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "Closed database qualification", KeyID: fixture.ownerKeyID,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(&activation, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	authorityPreview, err := fixture.service.PreviewAuthorityChange(
		ctx, fixture.principal, AuthorityChangePreviewRequest{
			KeyID: fixture.ownerKeyID, ExpectedVersion: 1,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	signedAuthority := MigrationPreview{Authority: authorityPreview.Authority}
	if err := signMigrationPreview(&signedAuthority, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotationPreview, err := fixture.service.PreviewFounderKeyRotation(
		ctx, fixture.principal, FounderKeyRotationPreviewRequest{
			OldKeyID: fixture.ownerKeyID, NewKeyID: "key:closed-database:v2",
			NewPublicKey:    base64.RawURLEncoding.EncodeToString(newPublic),
			ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	signedRotationAuthority := MigrationPreview{Authority: rotationPreview.Authority}
	if err := signMigrationPreview(
		&signedRotationAuthority, rotationPreview.Rotation.NewKeyID, newPrivate,
	); err != nil {
		t.Fatal(err)
	}
	rotationPreview.Authority = signedRotationAuthority.Authority
	if err := SignFounderKeyRotation(
		&rotationPreview.Rotation, fixture.ownerPrivate, newPrivate,
	); err != nil {
		t.Fatal(err)
	}

	closedPool, err := pgxpool.New(ctx, controlPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closedPool.Close()
	closed := *fixture.service
	closed.pool = closedPool
	closed.broker = newBroker(4)
	server := httptest.NewServer(closed.Handler())
	defer server.Close()
	for _, path := range []string{
		"/v1/workforce/departments?limit=1",
		"/v1/workforce/events?after=0&limit=1",
		"/v1/workforce/events/stream?after=0",
	} {
		response := controlHTTPRequest(t, server.URL+path, "token:closed-database", nil)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("closed database HTTP %s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}

	order := fixture.order("closed", "mimo", "mimo-v2.5-pro")
	if err := SignWorkOrder(&order, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	command := fixture.signedCommand(
		t, "closed", "pause_company", "company",
		string(fixture.principal.OrganizationID), 0,
		map[string]string{"reason": "closed database qualification"},
	)
	for _, value := range []struct {
		path   string
		body   any
		status int
	}{
		{"/v1/workforce/control-keys", ControlKeyRegistration{KeyID: "key:closed-http", PublicKey: base64.RawURLEncoding.EncodeToString(newPublic)}, http.StatusBadRequest},
		{"/v1/workforce/activation", ActivationBundle{Seed: activation.Seed, Authority: activation.Authority, SkillContracts: activation.SkillContracts}, http.StatusForbidden},
		{"/v1/workforce/migration", MigrationBundle{Authority: activation.Authority, LegacyOrganizationVersion: 1}, http.StatusForbidden},
		{"/v1/workforce/company-authority", AuthorityChangeBundle{ExpectedVersion: 1, Authority: signedAuthority.Authority}, http.StatusForbidden},
		{"/v1/workforce/founder-key", FounderKeyRotationBundle{Rotation: rotationPreview.Rotation, Authority: rotationPreview.Authority}, http.StatusForbidden},
		{"/v1/workforce/work-orders", order, http.StatusForbidden},
		{"/v1/workforce/commands", command, http.StatusForbidden},
	} {
		response := controlHTTPRequest(
			t, server.URL+value.path, "token:closed-database", value.body,
		)
		if response.StatusCode != value.status {
			t.Fatalf("closed database HTTP %s status=%d", value.path, response.StatusCode)
		}
		response.Body.Close()
	}
	event := LifecycleEvent{
		ID: "event:closed-database", OrganizationID: fixture.principal.OrganizationID,
		Type: "database.checked", ResourceKind: "database", ResourceID: "postgres",
		ResourceVersion: 1, Fields: map[string]any{"state": "closed"},
	}

	checks := []struct {
		name string
		run  func() error
	}{
		{"runtime root", func() error { _, err := closed.RuntimeOwnerRoot(ctx, fixture.principal); return err }},
		{"authority preview", func() error {
			_, err := closed.PreviewAuthorityChange(ctx, fixture.principal, AuthorityChangePreviewRequest{KeyID: fixture.ownerKeyID, ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft()})
			return err
		}},
		{"rotation preview", func() error {
			_, err := closed.PreviewFounderKeyRotation(ctx, fixture.principal, FounderKeyRotationPreviewRequest{OldKeyID: fixture.ownerKeyID, NewKeyID: "key:new", NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublic), ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft()})
			return err
		}},
		{"register key", func() error {
			return closed.RegisterControlKey(ctx, fixture.principal, ControlKeyRegistration{KeyID: "key:closed", PublicKey: base64.RawURLEncoding.EncodeToString(newPublic)})
		}},
		{"list", func() error { _, err := closed.List(ctx, fixture.principal, "departments", "", 1); return err }},
		{"events", func() error { _, err := closed.Events(ctx, fixture.principal, 0, 1); return err }},
		{"publish", func() error { _, err := closed.Publish(ctx, fixture.principal, event); return err }},
		{"command", func() error { _, err := closed.ApplyCommand(ctx, fixture.principal, command); return err }},
		{"work order", func() error { _, err := closed.CreateWorkOrder(ctx, fixture.principal, order); return err }},
		{"activation", func() error {
			_, err := closed.ActivateOrganization(ctx, fixture.principal, ActivationBundle{Seed: activation.Seed, Authority: activation.Authority, SkillContracts: activation.SkillContracts})
			return err
		}},
		{"migration", func() error {
			_, err := closed.MigrateOrganization(ctx, fixture.principal, MigrationBundle{Authority: activation.Authority, LegacyOrganizationVersion: 1})
			return err
		}},
		{"authority change", func() error {
			_, err := closed.ChangeAuthority(ctx, fixture.principal, AuthorityChangeBundle{ExpectedVersion: 1, Authority: signedAuthority.Authority})
			return err
		}},
		{"rotation", func() error {
			_, err := closed.RotateFounderKey(ctx, fixture.principal, FounderKeyRotationBundle{Rotation: rotationPreview.Rotation, Authority: rotationPreview.Authority})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil {
				t.Fatal("closed database operation succeeded")
			}
		})
	}
}

func TestIntegration_ControlBoundaryFailuresFailClosedBeforeMutation(t *testing.T) {
	fixture := newControlFixture(t, "control-boundaries")
	ctx := context.Background()
	request := ActivationPreviewRequest{
		Name: "Boundary Workforce", KeyID: fixture.ownerKeyID,
		EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}

	invalidTime := request
	invalidTime.EffectiveAt = time.Time{}
	if _, err := fixture.service.PreviewActivation(ctx, fixture.principal, invalidTime); err == nil {
		t.Fatal("zero activation time was accepted")
	}
	unauthorized := request
	unauthorized.KeyID = "key:unknown"
	if _, err := fixture.service.PreviewActivation(ctx, fixture.principal, unauthorized); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown activation key = %v", err)
	}
	noRuntime := *fixture.service
	noRuntime.runtimeKeyID = ""
	noRuntime.runtimePublic = nil
	if _, err := noRuntime.PreviewActivation(ctx, fixture.principal, request); err == nil {
		t.Fatal("activation without runtime authority was accepted")
	}
	noIssuer := *fixture.service
	noIssuer.companyIssuerKeyID = ""
	noIssuer.companyIssuerPublic = nil
	if _, err := noIssuer.PreviewActivation(ctx, fixture.principal, request); err == nil {
		t.Fatal("activation without company issuer was accepted")
	}
	migrationRequest := MigrationPreviewRequest{
		KeyID: fixture.ownerKeyID, EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}
	invalidMigrationTime := migrationRequest
	invalidMigrationTime.EffectiveAt = time.Time{}
	if _, err := fixture.service.PreviewMigration(
		ctx, fixture.principal, invalidMigrationTime,
	); err == nil {
		t.Fatal("migration with invalid effective time was accepted")
	}
	if _, err := noIssuer.PreviewMigration(
		ctx, fixture.principal, migrationRequest,
	); err == nil {
		t.Fatal("migration without company issuer was accepted")
	}

	if _, err := fixture.service.PreviewMigration(
		ctx, fixture.principal, migrationRequest,
	); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("migration before v1 activation = %v", err)
	}
	if _, err := fixture.service.PreviewAuthorityChange(
		ctx, fixture.principal, AuthorityChangePreviewRequest{
			KeyID: fixture.ownerKeyID, ExpectedVersion: 1,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("authority change before activation = %v", err)
	}
	if _, err := fixture.service.PreviewAuthorityChange(
		ctx, fixture.principal, AuthorityChangePreviewRequest{
			KeyID: fixture.ownerKeyID, EffectiveAt: time.Time{}, Authority: testActivationDraft(),
		},
	); err == nil {
		t.Fatal("authority change with invalid time and version was accepted")
	}
	newPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.PreviewFounderKeyRotation(
		ctx, fixture.principal, FounderKeyRotationPreviewRequest{
			OldKeyID: fixture.ownerKeyID, NewKeyID: "key:new",
			NewPublicKey:    base64.RawURLEncoding.EncodeToString(newPublic),
			ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("rotation before activation = %v", err)
	}
	if _, err := fixture.service.PreviewFounderKeyRotation(
		ctx, fixture.principal, FounderKeyRotationPreviewRequest{
			OldKeyID: fixture.ownerKeyID, NewKeyID: fixture.ownerKeyID,
			NewPublicKey:    base64.RawURLEncoding.EncodeToString(newPublic),
			ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	); err == nil {
		t.Fatal("same-ID founder rotation was accepted")
	}
	if _, err := fixture.service.PreviewFounderKeyRotation(
		ctx, fixture.principal, FounderKeyRotationPreviewRequest{
			OldKeyID: fixture.ownerKeyID, NewKeyID: "key:new",
			NewPublicKey: "invalid", ExpectedVersion: 1,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	); err == nil {
		t.Fatal("malformed founder rotation key was accepted")
	}

	if _, err := fixture.service.ChangeAuthority(
		ctx, fixture.principal, AuthorityChangeBundle{},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty authority change = %v", err)
	}
	if _, err := fixture.service.RotateFounderKey(
		ctx, fixture.principal, FounderKeyRotationBundle{},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty rotation = %v", err)
	}
	withoutVault := *fixture.service
	withoutVault.vault = nil
	if _, err := withoutVault.MigrateOrganization(
		ctx, fixture.principal, MigrationBundle{},
	); err == nil {
		t.Fatal("migration without Vault was accepted")
	}
	if _, err := withoutVault.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{},
	); err == nil {
		t.Fatal("activation without Vault was accepted")
	}

	if err := fixture.service.RegisterControlKey(
		ctx, Principal{}, ControlKeyRegistration{},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty control key principal = %v", err)
	}
	if err := fixture.service.RegisterControlKey(
		ctx, fixture.principal, ControlKeyRegistration{KeyID: "key:invalid", PublicKey: "invalid"},
	); err == nil {
		t.Fatal("malformed control public key was accepted")
	}

	command := fixture.signedCommand(
		t, "boundary", "pause_company", "company",
		string(fixture.principal.OrganizationID), 0,
		map[string]string{"reason": "boundary qualification"},
	)
	wrongPrincipal := fixture.principal
	wrongPrincipal.OwnerID = "owner:other"
	if _, err := fixture.service.ApplyCommand(ctx, wrongPrincipal, command); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-owner command = %v", err)
	}
	tampered := command
	tampered.Change = json.RawMessage(`{"reason":"tampered"}`)
	if _, err := fixture.service.ApplyCommand(ctx, fixture.principal, tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered command = %v", err)
	}

	badClock := *fixture.service
	badClock.now = func() time.Time { return time.Time{} }
	if _, err := badClock.Publish(ctx, fixture.principal, LifecycleEvent{
		ID: "event:bad-clock", OrganizationID: fixture.principal.OrganizationID,
		Type: "clock.checked", ResourceKind: "clock", ResourceID: "utc",
		ResourceVersion: 1, Fields: map[string]any{"state": "invalid"},
	}); err == nil {
		t.Fatal("zero control-plane clock was accepted")
	}
}

type controlFixture struct {
	service      *Service
	principal    Principal
	ownerKeyID   string
	ownerPrivate ed25519.PrivateKey
	now          time.Time
}

func newControlFixture(t *testing.T, label string) controlFixture {
	t.Helper()
	now := controlIntegrationTime()
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtimePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		TenantID:       "tenant:" + label,
		OrganizationID: contracts.OrganizationID("organization:" + label),
		OwnerID:        contracts.OwnerID("owner:" + label),
	}
	auth, err := NewStaticAuthenticator(map[string]Principal{
		"token:" + label: principal,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerKeyID := "key:owner:" + label
	service, err := New(
		controlPool, auth,
		map[string]OwnerKey{
			principal.TenantID: {
				KeyID: ownerKeyID, PublicKey: ownerPublic,
			},
		},
		func() time.Time { return now }, 16,
	)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(),
		UserDID: principal.TenantID,
		KEKHex: hex.EncodeToString(
			[]byte("0123456789abcdef0123456789abcdef"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachVault(session.UserVault()); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeAuthority(
		"key:runtime:"+label, runtimePublic,
	); err != nil {
		t.Fatal(err)
	}
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachCompanyIssuerAuthority(
		"key:company-issuer:"+label, issuerPublic,
	); err != nil {
		t.Fatal(err)
	}
	if err := service.AttachRuntimeModel("mimo", "mimo-v2.5-pro"); err != nil {
		t.Fatal(err)
	}
	schedulerStore, err := scheduler.New(
		controlPool, session.UserVault(), principal.TenantID,
		scheduler.Config{
			MaxOrganizationConcurrency: 4,
			MaxSeatConcurrency:         1,
			DailyTaskLimit:             100,
			DailySpendMicrounits:       1_000_000,
			ClaimLease:                 2 * time.Minute,
		},
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AttachScheduler(schedulerStore); err != nil {
		t.Fatal(err)
	}
	return controlFixture{
		service: service, principal: principal,
		ownerKeyID: ownerKeyID, ownerPrivate: ownerPrivate, now: now,
	}
}

func (fixture controlFixture) activate(
	ctx context.Context,
) (ActivationResult, error) {
	preview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name:  "Workforce " + string(fixture.principal.OrganizationID),
			KeyID: fixture.ownerKeyID, EffectiveAt: fixture.now,
			Authority: testActivationDraft(),
		},
	)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := signControlActivation(
		&preview, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		return ActivationResult{}, err
	}
	return fixture.service.ActivateOrganization(
		ctx, fixture.principal, ActivationBundle{
			Seed: preview.Seed, Authority: preview.Authority,
			SkillContracts: preview.SkillContracts,
		},
	)
}

func signControlActivation(
	preview *ActivationPreview,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	for departmentIndex := range preview.Seed.Organization.Departments {
		for seatIndex := range preview.Seed.Organization.Departments[departmentIndex].Seats {
			if err := policy.SignSeat(
				&preview.Seed.Organization.Departments[departmentIndex].Seats[seatIndex],
				keyID, privateKey,
			); err != nil {
				return err
			}
		}
	}
	for index := range preview.Seed.Mandates {
		if err := policy.SignMandate(
			&preview.Seed.Mandates[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	if err := policy.SignRuntimeAuthority(
		&preview.Seed.RuntimeAuthority, keyID, privateKey,
	); err != nil {
		return err
	}
	for index := range preview.Seed.Policies {
		if err := policy.SignPolicy(
			&preview.Seed.Policies[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	if err := policy.SignOrganization(
		&preview.Seed.Organization, keyID, privateKey,
	); err != nil {
		return err
	}
	if err := mission.SignFounderMission(
		&preview.Authority.Mission, keyID, privateKey,
	); err != nil {
		return err
	}
	if err := mission.SignCompanyConstitution(
		&preview.Authority.Constitution, keyID, privateKey,
	); err != nil {
		return err
	}
	if err := mission.SignCapitalEnvelope(
		&preview.Authority.Capital, keyID, privateKey,
	); err != nil {
		return err
	}
	if err := mission.SignCompanyIssuerPolicy(
		&preview.Authority.IssuerPolicy, keyID, privateKey,
	); err != nil {
		return err
	}
	for index := range preview.SkillContracts {
		if err := skills.SignContract(
			&preview.SkillContracts[index], keyID, privateKey,
		); err != nil {
			return err
		}
	}
	return nil
}

func signMigrationPreview(
	preview *MigrationPreview,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if err := mission.SignFounderMission(&preview.Authority.Mission, keyID, privateKey); err != nil {
		return err
	}
	if err := mission.SignCompanyConstitution(&preview.Authority.Constitution, keyID, privateKey); err != nil {
		return err
	}
	if err := mission.SignCapitalEnvelope(&preview.Authority.Capital, keyID, privateKey); err != nil {
		return err
	}
	return mission.SignCompanyIssuerPolicy(&preview.Authority.IssuerPolicy, keyID, privateKey)
}

func testActivationDraft() mission.ActivationDraft {
	return mission.ActivationDraft{
		Purpose:                  "Build verified useful software for approved customers",
		PermittedBusinessDomains: []string{"software"},
		StrategicPrinciples:      []string{"evidence before expansion"},
		TargetOutcomes:           []string{"verified customer value"},
		SuccessConditions:        []string{"authoritative customer outcome"},
		FailureConditions:        []string{"unreconciled external state"},
		LegalProhibitions:        []string{"no unlawful activity"},
		EthicalProhibitions:      []string{"no deceptive claims"},
		PermittedJurisdictions:   []string{"DE"},
		DataBoundaries:           []string{"purpose-bound customer data"},
		PermittedCounterparties:  []string{"owner-approved"},
		RiskTolerance:            mission.RiskToleranceLow,
		Autonomy:                 mission.AutonomyReviewRequired,
		EscalationConditions:     []string{"unverifiable material claim"},
		PauseConditions:          []string{"authority uncertainty"},
		ShutdownConditions:       []string{"founder emergency stop"},
		Currency:                 "EUR", StartingMicrounits: 1_000_000_000,
		SpendCeilingMicrounits:    100_000_000,
		ExposureCeilingMicrounits: 100_000_000,
		MinimumRunwayDays:         180, MaxWorkOrderMicrounits: 10_000_000,
	}
}

func (fixture controlFixture) order(
	label, provider, modelID string,
) WorkOrder {
	return WorkOrder{
		SchemaVersion:  "workforce.work-order.v1",
		ID:             "work-order:" + label + ":" + string(fixture.principal.OrganizationID),
		OrganizationID: fixture.principal.OrganizationID,
		OwnerID:        fixture.principal.OwnerID,
		Version:        1,
		Objective:      "Produce a receipt-backed bounded result",
		Scope:          "/root/matrix",
		ProjectID:      "project:matrix",
		WorkspaceID:    "workspace:matrix",
		ScopeFiles: []string{
			"workforce/internal/wakeruntime/recovery.go",
		},
		ScopeSymbols: []string{"RunClaim"},
		Departments: []contracts.DepartmentKind{
			contracts.DepartmentDeveloper,
			contracts.DepartmentExecutive,
		},
		Priority: 10,
		Budget: WorkOrderBudget{
			MaxTasks: 5, MaxSpendMicrounits: 1000,
		},
		Deadline: fixture.now.Add(24 * time.Hour),
		Autonomy: "supervised",
		AcceptanceCriteria: []string{
			"evidence_hash: authoritative provider evidence is content-addressed",
		},
		ModelProvider: provider, ModelID: modelID,
		MGSReference: "mgs:workforce:v1",
		MGSDigest:    strings.Repeat("a", 64),
		CreatedAt:    fixture.now,
		IdempotencyKey: "work-order:" + label + ":" +
			string(fixture.principal.OrganizationID),
	}
}

func (fixture controlFixture) signedCommand(
	t *testing.T,
	label, action, resourceKind, resourceID string,
	expectedVersion uint64,
	change any,
) SignedCommand {
	t.Helper()
	encoded, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	command := SignedCommand{
		SchemaVersion:  SchemaVersion,
		ID:             "command:" + label + ":" + string(fixture.principal.OrganizationID),
		OrganizationID: fixture.principal.OrganizationID,
		OwnerID:        fixture.principal.OwnerID,
		Action:         action, ResourceKind: resourceKind, ResourceID: resourceID,
		ExpectedVersion: expectedVersion, Change: encoded,
		EffectiveAt: fixture.now,
	}
	if err := SignCommand(&command, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	return command
}

func controlIntegrationTime() time.Time {
	return time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
}

func startControlPostgres(
	ctx context.Context,
) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-control-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", controlPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(
		ctx, "docker", "port", container, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return container, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf(
			"invalid PostgreSQL port %q", address,
		)
	}
	return container,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			address[index+1:] + "/workforce?sslmode=disable",
		nil
}

func waitControlPostgres(
	ctx context.Context,
	databaseURL string,
) (*pgxpool.Pool, error) {
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
