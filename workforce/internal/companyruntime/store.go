package companyruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/portfolio"
)

type Store struct {
	pool              *pgxpool.Pool
	vault             *vault.UserVault
	mission           *mission.Store
	portfolio         *portfolio.Store
	tenantID          string
	organizationID    contracts.OrganizationID
	founderKeyID      string
	founderPublicKey  ed25519.PublicKey
	controllerKeyID   string
	controllerPrivate ed25519.PrivateKey
	controllerPublic  ed25519.PublicKey
	now               func() time.Time
}

var ErrNotStarted = errors.New("company runtime: autonomous controller has not been started")

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	missionStore *mission.Store,
	portfolioStore *portfolio.Store,
	tenantID string,
	organizationID contracts.OrganizationID,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	controllerKeyID string,
	controllerPrivate ed25519.PrivateKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || missionStore == nil || portfolioStore == nil ||
		!validToken(tenantID) || organizationID == "" || !validToken(founderKeyID) ||
		len(founderPublicKey) != ed25519.PublicKeySize || !validToken(controllerKeyID) ||
		len(controllerPrivate) != ed25519.PrivateKeySize || now == nil ||
		userVault.User() != tenantID {
		return nil, fmt.Errorf("company runtime: store dependencies and authorities are required")
	}
	privateKey := append(ed25519.PrivateKey(nil), controllerPrivate...)
	return &Store{
		pool: pool, vault: userVault, mission: missionStore, portfolio: portfolioStore,
		tenantID: tenantID, organizationID: organizationID,
		founderKeyID: founderKeyID, founderPublicKey: append(ed25519.PublicKey(nil), founderPublicKey...),
		controllerKeyID: controllerKeyID, controllerPrivate: privateKey,
		controllerPublic: append(ed25519.PublicKey(nil), privateKey.Public().(ed25519.PublicKey)...),
		now:              now,
	}, nil
}

func DefaultStartDraft(at, expiresAt time.Time, maximumCapital, maximumRisk uint64) StartDraft {
	interval := uint64((24 * time.Hour) / time.Second)
	kinds := []portfolio.CadenceKind{
		portfolio.CadenceCapital, portfolio.CadenceCommercial, portfolio.CadenceDiscovery,
		portfolio.CadenceOperations, portfolio.CadencePortfolio, portfolio.CadenceProduct,
		portfolio.CadenceLearning,
	}
	cadences := make([]CadenceSchedule, len(kinds))
	for index, kind := range kinds {
		cadences[index] = CadenceSchedule{Kind: kind, IntervalSeconds: interval, FirstDueAt: at}
	}
	return StartDraft{
		Version: 1, EffectiveAt: at, ExpiresAt: expiresAt,
		Weights: portfolio.FactorWeights{
			MissionFit: 1200, DemandStrength: 1500, ExpectedValue: 1200,
			UnitEconomics: 1200, TimeToEvidence: 800, CapitalEfficiency: 900,
			OpportunityCost: 600, LegalSafety: 800, SecuritySafety: 800,
			OperatingCapacity: 500, Certainty: 500,
		},
		GOThresholdBPS: 7000, ValidateThresholdBPS: 5000, NoGOThresholdBPS: 3000,
		MinimumLegalSafetyBPS: 7000, MinimumSecuritySafetyBPS: 7000,
		MaximumActiveInitiatives: 4, MaximumNoEvidenceCycles: 3,
		MaximumCapitalMicrounits: maximumCapital, MaximumRiskMicrounits: maximumRisk,
		AuthorityClauses: []string{"company-controller.standard"}, Cadences: cadences,
	}
}

func (store *Store) PreviewStart(ctx context.Context, draft StartDraft) (StartConfiguration, error) {
	now, err := store.currentTime()
	if err != nil {
		return StartConfiguration{}, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return StartConfiguration{}, err
	}
	if !current.Executable(now) || draft.Version == 0 || !validUTC(draft.EffectiveAt) ||
		!validUTC(draft.ExpiresAt) || draft.EffectiveAt.Before(now.Add(-5*time.Minute)) ||
		!draft.ExpiresAt.After(draft.EffectiveAt) || len(draft.Cadences) != 7 ||
		draft.ExpiresAt.After(current.Authority.IssuerPolicy.ExpiresAt) ||
		draft.MaximumCapitalMicrounits == 0 || draft.MaximumRiskMicrounits == 0 ||
		draft.MaximumCapitalMicrounits > current.Authority.Capital.SpendCeilingMicrounits ||
		draft.MaximumRiskMicrounits > current.Authority.Capital.ExposureCeilingMicrounits {
		return StartConfiguration{}, fmt.Errorf("company runtime: start draft is outside current company authority")
	}
	authority := current.Authority
	missionHash, err := contracts.HashCanonical(&authority.Mission)
	if err != nil {
		return StartConfiguration{}, err
	}
	constitutionHash, err := contracts.HashCanonical(&authority.Constitution)
	if err != nil {
		return StartConfiguration{}, err
	}
	capitalHash, err := contracts.HashCanonical(&authority.Capital)
	if err != nil {
		return StartConfiguration{}, err
	}
	issuerHash, err := contracts.HashCanonical(&authority.IssuerPolicy)
	if err != nil {
		return StartConfiguration{}, err
	}
	expiresAt := draft.ExpiresAt
	procedure := portfolio.DecisionProcedure{
		SchemaVersion: portfolio.ProcedureSchemaVersion,
		ID:            "portfolio-procedure:" + string(store.organizationID), Version: draft.Version,
		OrganizationID: store.organizationID, Weights: draft.Weights,
		GOThresholdBPS: draft.GOThresholdBPS, ValidateThresholdBPS: draft.ValidateThresholdBPS,
		NO_GOThresholdBPS:        draft.NoGOThresholdBPS,
		MinimumLegalSafetyBPS:    draft.MinimumLegalSafetyBPS,
		MinimumSecuritySafetyBPS: draft.MinimumSecuritySafetyBPS,
		MaximumActiveInitiatives: draft.MaximumActiveInitiatives,
		MaximumNoEvidenceCycles:  draft.MaximumNoEvidenceCycles,
		MaximumCapitalMicrounits: draft.MaximumCapitalMicrounits,
		MaximumRiskMicrounits:    draft.MaximumRiskMicrounits,
		EffectiveAt:              draft.EffectiveAt, ExpiresAt: &expiresAt,
		AuthorityClauses: slices.Clone(draft.AuthorityClauses),
		Signature:        signaturePlaceholder(store.founderKeyID),
	}
	cadences := make([]portfolio.Cadence, len(draft.Cadences))
	seen := make(map[portfolio.CadenceKind]struct{}, len(draft.Cadences))
	for index, schedule := range draft.Cadences {
		if !schedule.Kind.Valid() || schedule.IntervalSeconds < 300 ||
			!validUTC(schedule.FirstDueAt) || schedule.FirstDueAt.Before(draft.EffectiveAt) {
			return StartConfiguration{}, fmt.Errorf("company runtime: cadence schedule is invalid")
		}
		if _, duplicate := seen[schedule.Kind]; duplicate {
			return StartConfiguration{}, fmt.Errorf("company runtime: cadence kind is duplicated")
		}
		seen[schedule.Kind] = struct{}{}
		cadences[index] = portfolio.Cadence{
			SchemaVersion: "workforce.company-cadence.v1",
			ID:            "company-cadence:" + string(schedule.Kind), Version: draft.Version,
			OrganizationID: store.organizationID, Kind: schedule.Kind,
			IntervalSeconds: schedule.IntervalSeconds, FirstDueAt: schedule.FirstDueAt,
			EffectiveAt: draft.EffectiveAt, ExpiresAt: &expiresAt,
			Signature: signaturePlaceholder(store.founderKeyID),
		}
	}
	slices.SortFunc(cadences, func(left, right portfolio.Cadence) int {
		if left.Kind < right.Kind {
			return -1
		}
		if left.Kind > right.Kind {
			return 1
		}
		return 0
	})
	value := StartConfiguration{
		SchemaVersion: StartConfigurationSchemaVersion,
		ID:            "company-runtime:" + string(store.organizationID), Version: draft.Version,
		OrganizationID: store.organizationID,
		MissionVersion: authority.Mission.Version, MissionHash: missionHash,
		ConstitutionVersion: authority.Constitution.Version, ConstitutionHash: constitutionHash,
		CapitalEnvelopeVersion: authority.Capital.Version, CapitalEnvelopeHash: capitalHash,
		IssuerPolicyVersion: authority.IssuerPolicy.Version, IssuerPolicyHash: issuerHash,
		Procedure: procedure, Cadences: cadences,
		EffectiveAt: draft.EffectiveAt, ExpiresAt: draft.ExpiresAt,
		Signature: signaturePlaceholder(store.founderKeyID),
	}
	return value, nil
}

func (store *Store) Activate(ctx context.Context, value StartConfiguration) (StartResult, error) {
	now, err := store.currentTime()
	if err != nil {
		return StartResult{}, err
	}
	if err := VerifyStartConfiguration(value, store.founderKeyID, store.founderPublicKey); err != nil {
		return StartResult{}, err
	}
	if err := portfolio.VerifyProcedure(value.Procedure, store.founderKeyID, store.founderPublicKey); err != nil {
		return StartResult{}, err
	}
	for index := range value.Cadences {
		if err := portfolio.VerifyCadence(value.Cadences[index], store.founderKeyID, store.founderPublicKey); err != nil {
			return StartResult{}, err
		}
	}
	if value.OrganizationID != store.organizationID || value.EffectiveAt.After(now) || !now.Before(value.ExpiresAt) {
		return StartResult{}, fmt.Errorf("company runtime: start configuration is not currently activatable")
	}
	if err := store.verifyCurrentRoots(ctx, value, now); err != nil {
		return StartResult{}, err
	}
	for index := range value.Cadences {
		if err := store.portfolio.RegisterCadence(ctx, value.Cadences[index], store.founderPublicKey); err != nil {
			return StartResult{}, err
		}
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return StartResult{}, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.configAD(value.ID, value.Version), canonical)
	if err != nil {
		return StartResult{}, fmt.Errorf("company runtime: seal configuration: %w", err)
	}
	procedureHash, err := contracts.HashCanonical(&value.Procedure)
	if err != nil {
		return StartResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return StartResult{}, fmt.Errorf("company runtime: begin activation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|company-runtime"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return StartResult{}, err
	}
	var currentVersion uint64
	var currentHash string
	err = tx.QueryRow(ctx, `
		SELECT head.version,config.canonical_hash
		FROM workforce_company_runtime_heads head
		JOIN workforce_company_runtime_configs config
		  ON config.tenant_id=head.tenant_id AND config.organization_id=head.organization_id
		 AND config.config_id=head.config_id AND config.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 FOR UPDATE
	`, store.tenantID, store.organizationID).Scan(&currentVersion, &currentHash)
	if err == nil {
		if currentVersion == value.Version && currentHash == canonicalHash {
			if err := tx.Commit(ctx); err != nil {
				return StartResult{}, err
			}
			return StartResult{Configuration: value, Deduplicated: true, ActivatedAt: now}, nil
		}
		if value.Version != currentVersion+1 {
			return StartResult{}, fmt.Errorf("company runtime: stale configuration version")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return StartResult{}, err
	} else if value.Version != 1 {
		return StartResult{}, fmt.Errorf("company runtime: initial configuration must be version one")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_runtime_configs (
			tenant_id,organization_id,config_id,version,mission_version,mission_hash,
			constitution_version,constitution_hash,capital_envelope_version,
			capital_envelope_hash,issuer_policy_version,issuer_policy_hash,
			procedure_id,procedure_version,procedure_hash,effective_at,expires_at,
			canonical_hash,sealed_config,signature_key_id,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
	`, store.tenantID, store.organizationID, value.ID, value.Version,
		value.MissionVersion, value.MissionHash.Digest, value.ConstitutionVersion,
		value.ConstitutionHash.Digest, value.CapitalEnvelopeVersion,
		value.CapitalEnvelopeHash.Digest, value.IssuerPolicyVersion,
		value.IssuerPolicyHash.Digest, value.Procedure.ID, value.Procedure.Version,
		procedureHash.Digest, value.EffectiveAt, value.ExpiresAt, canonicalHash,
		sealed, value.Signature.KeyID, now); err != nil {
		return StartResult{}, fmt.Errorf("company runtime: insert configuration: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_runtime_heads (
			tenant_id,organization_id,config_id,version,state,updated_at
		) VALUES ($1,$2,$3,$4,'active',$5)
		ON CONFLICT (tenant_id,organization_id) DO UPDATE SET
			config_id=EXCLUDED.config_id,version=EXCLUDED.version,state='active',updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.ID, value.Version, now); err != nil {
		return StartResult{}, fmt.Errorf("company runtime: advance configuration head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StartResult{}, fmt.Errorf("company runtime: commit activation: %w", err)
	}
	return StartResult{Configuration: value, ActivatedAt: now}, nil
}

func (store *Store) LoadCurrent(ctx context.Context) (StartConfiguration, error) {
	var configID, expectedHash, state string
	var version uint64
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT head.config_id,head.version,head.state,config.canonical_hash,config.sealed_config
		FROM workforce_company_runtime_heads head
		JOIN workforce_company_runtime_configs config
		  ON config.tenant_id=head.tenant_id AND config.organization_id=head.organization_id
		 AND config.config_id=head.config_id AND config.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
	`, store.tenantID, store.organizationID).Scan(&configID, &version, &state, &expectedHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return StartConfiguration{}, ErrNotStarted
	}
	if err != nil {
		return StartConfiguration{}, fmt.Errorf("company runtime: load current configuration: %w", err)
	}
	if state != "active" {
		return StartConfiguration{}, fmt.Errorf("company runtime: controller is not active")
	}
	canonical, err := store.vault.OpenRecord(store.configAD(configID, version), sealed)
	if err != nil || digest(canonical) != expectedHash {
		return StartConfiguration{}, fmt.Errorf("company runtime: configuration integrity failure")
	}
	value, err := contracts.DecodeCanonical[StartConfiguration, *StartConfiguration](canonical)
	if err != nil || value.ID != configID || value.Version != version ||
		VerifyStartConfiguration(value, store.founderKeyID, store.founderPublicKey) != nil {
		return StartConfiguration{}, fmt.Errorf("company runtime: configuration authentication failure")
	}
	now, err := store.currentTime()
	if err != nil || now.Before(value.EffectiveAt) || !now.Before(value.ExpiresAt) ||
		store.verifyCurrentRoots(ctx, value, now) != nil {
		return StartConfiguration{}, fmt.Errorf("company runtime: configuration authority is not current")
	}
	return value, nil
}

func (store *Store) Controller(ctx context.Context) (*portfolio.Controller, StartConfiguration, error) {
	configuration, err := store.LoadCurrent(ctx)
	if err != nil {
		return nil, StartConfiguration{}, err
	}
	controller, err := portfolio.NewController(store.portfolio, configuration.Procedure, store.founderPublicKey)
	return controller, configuration, err
}

func (store *Store) verifyCurrentRoots(ctx context.Context, value StartConfiguration, now time.Time) error {
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil || !current.Executable(now) {
		return fmt.Errorf("company runtime: current company root is not executable")
	}
	authority := current.Authority
	missionHash, missionErr := contracts.HashCanonical(&authority.Mission)
	constitutionHash, constitutionErr := contracts.HashCanonical(&authority.Constitution)
	capitalHash, capitalErr := contracts.HashCanonical(&authority.Capital)
	issuerHash, issuerErr := contracts.HashCanonical(&authority.IssuerPolicy)
	if missionErr != nil || constitutionErr != nil || capitalErr != nil || issuerErr != nil ||
		value.MissionVersion != authority.Mission.Version || value.MissionHash != missionHash ||
		value.ConstitutionVersion != authority.Constitution.Version || value.ConstitutionHash != constitutionHash ||
		value.CapitalEnvelopeVersion != authority.Capital.Version || value.CapitalEnvelopeHash != capitalHash ||
		value.IssuerPolicyVersion != authority.IssuerPolicy.Version || value.IssuerPolicyHash != issuerHash {
		return fmt.Errorf("company runtime: configuration root drifted")
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("company runtime: time source must return UTC")
	}
	return now, nil
}

func (store *Store) configAD(id string, version uint64) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-runtime.config",
		Stream: string(store.organizationID) + "/" + id,
		Schema: fmt.Sprintf("%s.v%d", StartConfigurationSchemaVersion, version),
	}
}

func (store *Store) gateAD(id string) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-runtime.gate",
		Stream: string(store.organizationID) + "/" + id,
		Schema: GateAuthorizationSchemaVersion,
	}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
