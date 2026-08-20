package testauthority

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/policy"
)

type Fixture struct {
	store            *policy.Store
	tenant           string
	root             policy.OwnerRoot
	key              ed25519.PrivateKey
	grant            policy.OwnerGrant
	now              func() time.Time
	label            string
	runtimeMu        sync.Mutex
	runtimePublished bool
}

type Published struct {
	Request lease.Request
	Seat    contracts.Seat
	Mandate contracts.Mandate
}

// PublishRuntimeAuthority installs a real owner-signed lease-signing
// delegation for integration paths that construct their own authority store.
func PublishRuntimeAuthority(
	ctx context.Context,
	store *policy.Store,
	organizationID contracts.OrganizationID,
	keyID string,
	publicKey ed25519.PublicKey,
	privateKey ed25519.PrivateKey,
	grant policy.OwnerGrant,
	effectiveAt time.Time,
) error {
	if store == nil {
		return fmt.Errorf("testauthority: policy store is required")
	}
	value := policy.RuntimeAuthority{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             policy.RuntimeAuthorityID(keyID),
		Version:        1,
		OrganizationID: organizationID,
		KeyID:          keyID,
		PublicKey:      base64.RawURLEncoding.EncodeToString(publicKey),
		Purposes:       []string{policy.WakeLeaseSigningPurpose},
		EffectiveAt:    effectiveAt,
	}
	if err := policy.SignRuntimeAuthority(&value, keyID, privateKey); err != nil {
		return err
	}
	return store.PublishRuntimeAuthority(ctx, value, grant)
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenant string,
	organizationID contracts.OrganizationID,
	label string,
	now func() time.Time,
) (*Fixture, error) {
	if pool == nil || userVault == nil || tenant == "" || organizationID == "" ||
		label == "" || now == nil {
		return nil, fmt.Errorf("testauthority: complete real authority inputs are required")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	root := policy.OwnerRoot{
		TenantID: tenant, OrganizationID: organizationID,
		OwnerID: contracts.OwnerID("owner:testauthority:" + label),
		KeyID:   "owner-key:testauthority:" + label, PublicKey: publicKey,
	}
	store, err := policy.New(pool, userVault, root, now)
	if err != nil {
		return nil, err
	}
	current := now()
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: organizationID,
		OwnerID: root.OwnerID, KeyID: root.KeyID, Scope: "authority:write",
		IssuedAt: current.Add(-time.Minute), ExpiresAt: current.Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, root.KeyID, privateKey); err != nil {
		return nil, err
	}
	return &Fixture{
		store: store, tenant: tenant, root: root, key: privateKey,
		grant: grant, now: now, label: label,
	}, nil
}

func (fixture *Fixture) Store() *policy.Store {
	if fixture == nil {
		return nil
	}
	return fixture.store
}

// PublishSeat signs and publishes one exact seat version through the real policy store.
func (fixture *Fixture) PublishSeat(ctx context.Context, seat contracts.Seat) error {
	if fixture == nil || seat.OrganizationID != fixture.root.OrganizationID {
		return fmt.Errorf("testauthority: seat does not match fixture")
	}
	if err := policy.SignSeat(&seat, fixture.root.KeyID, fixture.key); err != nil {
		return err
	}
	return fixture.store.PublishSeat(ctx, seat, fixture.grant)
}

func (fixture *Fixture) Publish(
	ctx context.Context,
	request lease.Request,
	allowedSkills ...contracts.SkillID,
) (Published, error) {
	if fixture == nil || request.OrganizationID != fixture.root.OrganizationID ||
		len(allowedSkills) == 0 {
		return Published{}, fmt.Errorf("testauthority: request does not match fixture")
	}
	current := fixture.now()
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.MandateID, Version: request.MandateVersion,
		OrganizationID: request.OrganizationID,
		DepartmentKind: contracts.DepartmentDeveloper,
		SeatRole:       contracts.SeatExecutor,
		AllowedSkills:  append([]contracts.SkillID(nil), allowedSkills...),
		DataScopes: []contracts.DataScope{{
			Name: "integration", Classification: contracts.ClassificationOrganization,
			Purpose: "Exercise the real signed authority path",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "Current evidence is insufficient",
			Action:    "Escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID:    "no-ambient-authority",
			Description: "Never use ambient or uncompiled authority",
		}},
		EffectiveAt: current.Add(-time.Hour),
	}
	if err := policy.SignMandate(&mandate, fixture.root.KeyID, fixture.key); err != nil {
		return Published{}, err
	}
	identitySuffix := shortToken(fixture.label + "|" + string(request.SeatID))
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.SeatID, Version: 1,
		DID:            contracts.SeatDID("did:matrix:testauthority:" + identitySuffix),
		OrganizationID: request.OrganizationID,
		DepartmentID:   contracts.DepartmentID("department:testauthority:" + identitySuffix),
		Role:           contracts.SeatExecutor,
		MandateID:      request.MandateID, MandateVersion: request.MandateVersion,
		BindingID:      contracts.SeatBindingID("binding:testauthority:" + identitySuffix),
		BindingVersion: 1, EffectiveAt: current.Add(-time.Hour),
	}
	return fixture.PublishExact(ctx, request, seat, mandate)
}

func (fixture *Fixture) PublishExact(
	ctx context.Context,
	request lease.Request,
	seat contracts.Seat,
	mandate contracts.Mandate,
) (Published, error) {
	if fixture == nil || request.OrganizationID != fixture.root.OrganizationID ||
		seat.ID != request.SeatID || seat.OrganizationID != request.OrganizationID ||
		seat.MandateID != request.MandateID ||
		seat.MandateVersion != request.MandateVersion ||
		mandate.ID != request.MandateID ||
		mandate.Version != request.MandateVersion ||
		mandate.OrganizationID != request.OrganizationID ||
		mandate.SeatRole != seat.Role {
		return Published{}, fmt.Errorf("testauthority: exact authority does not match request")
	}
	if err := policy.SignSeat(&seat, fixture.root.KeyID, fixture.key); err != nil {
		return Published{}, err
	}
	if err := policy.SignMandate(&mandate, fixture.root.KeyID, fixture.key); err != nil {
		return Published{}, err
	}
	if err := fixture.ensureRuntimeAuthority(ctx); err != nil {
		return Published{}, err
	}
	current := fixture.now()
	policies := make([]contracts.Policy, len(request.Policies))
	for index := range request.Policies {
		value := contracts.Policy{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            request.Policies[index].ID, Version: request.Policies[index].Version,
			OrganizationID: request.OrganizationID, Kind: "integration_authority",
			EffectiveAt: current.Add(-time.Hour),
			Rules: []contracts.PolicyRule{{
				ClauseID: "current-signed-lease", Outcome: "allow",
				Scope: "Only a current registered signed WakeLease",
			}},
		}
		if err := policy.SignPolicy(&value, fixture.root.KeyID, fixture.key); err != nil {
			return Published{}, err
		}
		canonical, err := contracts.EncodeCanonical(&value)
		if err != nil {
			return Published{}, err
		}
		sum := sha256.Sum256(canonical)
		request.Policies[index].Hash = contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		}
		policies[index] = value
	}
	if err := fixture.store.PublishMandate(ctx, mandate, fixture.grant); err != nil {
		return Published{}, err
	}
	if err := fixture.store.PublishSeat(ctx, seat, fixture.grant); err != nil {
		return Published{}, err
	}
	for _, value := range policies {
		if err := fixture.store.PublishPolicy(ctx, value, fixture.grant); err != nil {
			return Published{}, err
		}
	}
	return Published{Request: request, Seat: seat, Mandate: mandate}, nil
}

func (fixture *Fixture) ensureRuntimeAuthority(ctx context.Context) error {
	fixture.runtimeMu.Lock()
	defer fixture.runtimeMu.Unlock()
	if fixture.runtimePublished {
		return nil
	}
	publicKey := fixture.key.Public().(ed25519.PublicKey)
	if err := PublishRuntimeAuthority(
		ctx, fixture.store, fixture.root.OrganizationID,
		fixture.root.KeyID, publicKey, fixture.key, fixture.grant,
		fixture.now().Add(-time.Hour),
	); err != nil {
		return err
	}
	fixture.runtimePublished = true
	return nil
}

func (fixture *Fixture) Register(
	ctx context.Context,
	published Published,
	fence contracts.FenceToken,
	label string,
) (contracts.WakeLease, error) {
	if fixture == nil || published.Request.OrganizationID != fixture.root.OrganizationID {
		return contracts.WakeLease{}, fmt.Errorf("testauthority: published request does not match fixture")
	}
	request := published.Request
	value := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.ID, WakeID: request.WakeID, OrganizationID: request.OrganizationID,
		SeatID: request.SeatID, SeatDID: published.Seat.DID, Reason: "eligible_work",
		MandateID: request.MandateID, MandateVersion: request.MandateVersion,
		Policies:   append([]contracts.PolicyRef(nil), request.Policies...),
		GraphScope: []contracts.IntentID{contracts.IntentID(request.NodeID)},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ModelBindingID("model:testauthority:" + label),
			Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: digest("sampling:" + label),
		},
		MGS: contracts.MGSGenomeRef{
			Reference: "mgs:testauthority:" + label, Digest: digest("mgs:" + label),
		},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             digest("runtime:" + label),
			AuditorBuildDigest:      digest("auditor-runtime:" + label),
			OperationRegistryDigest: digest("registry:" + label),
		},
		SkillCatalogDigest: digest("catalog:" + label),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64(time.Hour / time.Millisecond),
			MaxSteps:          50, MaxModelCalls: 8, MaxToolCalls: 32,
			MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt, Fence: fence,
	}
	if err := policy.SignWakeLease(&value, fixture.root.KeyID, fixture.key); err != nil {
		return contracts.WakeLease{}, err
	}
	if err := fixture.store.RegisterLease(ctx, value); err != nil {
		return contracts.WakeLease{}, err
	}
	return value, nil
}

func digest(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func shortToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
