package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	ErrConflict     = errors.New("audit: immutable conflict")
	ErrUnauthorized = errors.New("audit: independent authority required")
	ErrIntegrity    = errors.New("audit: integrity failure")
)

type Seat struct {
	ID           contracts.SeatID       `json:"seat_id"`
	DepartmentID contracts.DepartmentID `json:"department_id"`
}

type CommitRequest struct {
	ID           contracts.VerdictID
	Packet       contracts.VerdictPacket
	AuditorLease contracts.WakeLease
	Decision     VerifiedDecision
}

type SeatAuthority interface {
	LoadCurrentSeat(context.Context, contracts.SeatID) (contracts.Seat, error)
	AuthorizeLease(context.Context, contracts.LeaseID) error
	LoadLease(context.Context, contracts.LeaseID) (contracts.WakeLease, error)
}

type SamplingPolicy struct {
	Numerator   uint32
	Denominator uint32
	Minimum     uint32
	Maximum     uint32
}

func (value SamplingPolicy) Validate() error {
	if value.Numerator == 0 || value.Denominator == 0 ||
		value.Numerator > value.Denominator || value.Maximum == 0 ||
		value.Minimum > value.Maximum || value.Maximum > 10000 {
		return fmt.Errorf("audit: sampling policy is invalid")
	}
	return nil
}

type PopulationEntry struct {
	VerdictID             contracts.VerdictID    `json:"verdict_id"`
	VerdictHash           contracts.ContentHash  `json:"verdict_hash"`
	ExecutingDepartmentID contracts.DepartmentID `json:"executing_department_id"`
}

type Selection struct {
	VerdictID             contracts.VerdictID    `json:"verdict_id"`
	ReauditorSeatID       contracts.SeatID       `json:"reauditor_seat_id"`
	ReauditorDepartmentID contracts.DepartmentID `json:"reauditor_department_id"`
	Score                 string                 `json:"score"`
}

type SampleProof struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	EpochID        string                   `json:"epoch_id"`
	CutoffAt       time.Time                `json:"cutoff_at"`
	Policy         SamplingPolicy           `json:"policy"`
	Population     []PopulationEntry        `json:"population"`
	Auditors       []Seat                   `json:"auditors"`
	PopulationRoot contracts.ContentHash    `json:"population_root"`
	Seed           string                   `json:"seed"`
	SeedCommitment contracts.ContentHash    `json:"seed_commitment"`
	Selections     []Selection              `json:"selections"`
	CreatedAt      time.Time                `json:"created_at"`
}

type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	keyID      string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	authority  SeatAuthority
	now        func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID, keyID string,
	privateKey ed25519.PrivateKey,
	authority SeatAuthority,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || tenantID == "" || keyID == "" ||
		len(privateKey) != ed25519.PrivateKeySize || authority == nil ||
		now == nil {
		return nil, fmt.Errorf("audit: durable store and signing authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("audit: Vault user does not match tenant")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, keyID: keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		publicKey:  append(ed25519.PublicKey(nil), publicKey...),
		authority:  authority, now: now,
	}, nil
}

func (store *Store) Commit(
	ctx context.Context,
	request CommitRequest,
) (contracts.Verdict, error) {
	now, err := store.currentTime()
	if err != nil {
		return contracts.Verdict{}, err
	}
	if err := request.Packet.Validate(); err != nil {
		return contracts.Verdict{}, err
	}
	if err := request.AuditorLease.Validate(); err != nil {
		return contracts.Verdict{}, err
	}
	decision := request.Decision.decision
	if err := decision.Validate(); err != nil {
		return contracts.Verdict{}, err
	}
	decisionBytes, err := contracts.EncodeCanonical(&decision)
	if err != nil ||
		request.Decision.packetDigest != packetHash(request.Packet) ||
		request.Decision.decisionDigest != hashBytes(decisionBytes) {
		return contracts.Verdict{}, ErrUnauthorized
	}
	executingSeat, executingErr := store.authority.LoadCurrentSeat(
		ctx, request.Packet.ExecutingSeatID,
	)
	auditorSeat, auditorErr := store.authority.LoadCurrentSeat(
		ctx, request.Packet.AuditorSeatID,
	)
	leaseErr := store.authority.AuthorizeLease(ctx, request.AuditorLease.ID)
	currentLease, currentLeaseErr := store.authority.LoadLease(
		ctx, request.AuditorLease.ID,
	)
	presentIntent := false
	for _, intentID := range request.AuditorLease.GraphScope {
		if intentID == request.Packet.Intent.ID {
			presentIntent = true
			break
		}
	}
	presentLease, presentLeaseErr := contracts.EncodeCanonical(&request.AuditorLease)
	currentLeaseBytes, currentLeaseEncodeErr := contracts.EncodeCanonical(&currentLease)
	if executingErr != nil || auditorErr != nil || request.ID == "" ||
		leaseErr != nil || currentLeaseErr != nil ||
		presentLeaseErr != nil || currentLeaseEncodeErr != nil ||
		!hmac.Equal(presentLease, currentLeaseBytes) ||
		request.AuditorLease.OrganizationID != request.Packet.OrganizationID ||
		request.AuditorLease.SeatID != request.Packet.AuditorSeatID ||
		!presentIntent ||
		executingSeat.OrganizationID != request.Packet.OrganizationID ||
		auditorSeat.OrganizationID != request.Packet.OrganizationID ||
		executingSeat.Role == contracts.SeatAuditor ||
		auditorSeat.Role != contracts.SeatAuditor ||
		executingSeat.DepartmentID == auditorSeat.DepartmentID ||
		request.Packet.ExecutingSeatID == request.Packet.AuditorSeatID ||
		decision.IntentID != request.Packet.Intent.ID ||
		decision.AuditorSeatID != request.Packet.AuditorSeatID ||
		decision.PacketDigest != packetHash(request.Packet) {
		return contracts.Verdict{}, ErrUnauthorized
	}
	verdict := contracts.Verdict{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.ID, OrganizationID: request.Packet.OrganizationID,
		IntentID:       request.Packet.Intent.ID,
		AuditorSeatID:  request.Packet.AuditorSeatID,
		Outcome:        decision.Outcome,
		VerifierDigest: request.Packet.VerifierDigest,
		Evidence:       append([]contracts.EvidenceRef(nil), decision.Evidence...),
		ReasonCode:     aggregateReason(decision.ReasonCodes),
		CreatedAt:      now,
	}
	if err := store.signVerdict(&verdict); err != nil {
		return contracts.Verdict{}, err
	}
	packetBytes, err := contracts.EncodeCanonical(&request.Packet)
	if err != nil {
		return contracts.Verdict{}, err
	}
	verdictBytes, err := contracts.EncodeCanonical(&verdict)
	if err != nil {
		return contracts.Verdict{}, err
	}
	sealedPacket, err := store.vault.SealRecord(
		store.ad("packet", verdict.OrganizationID, verdict.ID), packetBytes,
	)
	if err != nil {
		return contracts.Verdict{}, err
	}
	sealedVerdict, err := store.vault.SealRecord(
		store.ad("verdict", verdict.OrganizationID, verdict.ID), verdictBytes,
	)
	if err != nil {
		return contracts.Verdict{}, err
	}
	verdictHash := hashBytes(verdictBytes)
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_verdict_records (
			tenant_id,organization_id,verdict_id,intent_id,executing_seat_id,
			executing_department_id,auditor_seat_id,auditor_department_id,outcome,
			procedure_id,procedure_version,procedure_digest,packet_hash,verdict_hash,
			sealed_packet,sealed_verdict,committed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT DO NOTHING
	`, store.tenantID, verdict.OrganizationID, verdict.ID, verdict.IntentID,
		request.Packet.ExecutingSeatID, executingSeat.DepartmentID,
		verdict.AuditorSeatID, auditorSeat.DepartmentID, verdict.Outcome,
		request.Packet.Procedure.ID, request.Packet.Procedure.Version,
		request.Packet.Procedure.Digest.Digest, decision.PacketDigest.Digest,
		verdictHash.Digest, sealedPacket, sealedVerdict, now)
	if err != nil {
		return contracts.Verdict{}, err
	}
	if command.RowsAffected() == 0 {
		existing, loadErr := store.LoadVerdict(ctx, verdict.OrganizationID, verdict.ID)
		if loadErr != nil {
			return contracts.Verdict{}, loadErr
		}
		existingBytes, encodeErr := contracts.EncodeCanonical(&existing)
		if encodeErr != nil {
			return contracts.Verdict{}, encodeErr
		}
		if !hmac.Equal(existingBytes, verdictBytes) {
			return contracts.Verdict{}, ErrConflict
		}
		return existing, nil
	}
	return verdict, nil
}

func (store *Store) LoadVerdict(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	id contracts.VerdictID,
) (contracts.Verdict, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_verdict,verdict_hash FROM workforce_verdict_records
		WHERE tenant_id=$1 AND organization_id=$2 AND verdict_id=$3
	`, store.tenantID, organizationID, id).Scan(&sealed, &expectedHash)
	if err != nil {
		return contracts.Verdict{}, err
	}
	opened, err := store.vault.OpenRecord(store.ad("verdict", organizationID, id), sealed)
	if err != nil || hashBytes(opened).Digest != expectedHash {
		return contracts.Verdict{}, ErrIntegrity
	}
	verdict, err := contracts.DecodeCanonical[contracts.Verdict, *contracts.Verdict](opened)
	if err != nil || store.verifyVerdict(verdict) != nil {
		return contracts.Verdict{}, ErrIntegrity
	}
	return verdict, nil
}

func (store *Store) Sample(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	epochID string,
	cutoff time.Time,
	policy SamplingPolicy,
	auditorIDs []contracts.SeatID,
) (SampleProof, error) {
	now, err := store.currentTime()
	if err != nil {
		return SampleProof{}, err
	}
	if organizationID == "" || epochID == "" || !cutoff.Before(now) ||
		cutoff.Location() != time.UTC || policy.Validate() != nil {
		return SampleProof{}, fmt.Errorf("audit: sample request is invalid")
	}
	auditors := make([]Seat, 0, len(auditorIDs))
	for _, id := range auditorIDs {
		seat, err := store.authority.LoadCurrentSeat(ctx, id)
		if err != nil || seat.OrganizationID != organizationID ||
			seat.Role != contracts.SeatAuditor {
			return SampleProof{}, ErrUnauthorized
		}
		auditors = append(auditors, Seat{ID: seat.ID, DepartmentID: seat.DepartmentID})
	}
	auditors = sortedAuditors(auditors)
	if len(auditors) < 2 || !validAuditors(auditors) {
		return SampleProof{}, ErrUnauthorized
	}
	rows, err := store.pool.Query(ctx, `
		SELECT verdict_id,verdict_hash,executing_department_id
		FROM workforce_verdict_records
		WHERE tenant_id=$1 AND organization_id=$2 AND committed_at <= $3
		ORDER BY verdict_id
	`, store.tenantID, organizationID, cutoff)
	if err != nil {
		return SampleProof{}, err
	}
	defer rows.Close()
	population := make([]PopulationEntry, 0)
	for rows.Next() {
		var entry PopulationEntry
		entry.VerdictHash.Algorithm = "sha256"
		if err := rows.Scan(
			&entry.VerdictID, &entry.VerdictHash.Digest,
			&entry.ExecutingDepartmentID,
		); err != nil {
			return SampleProof{}, err
		}
		population = append(population, entry)
	}
	if err := rows.Err(); err != nil {
		return SampleProof{}, err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return SampleProof{}, err
	}
	proof, err := buildProof(
		organizationID, epochID, cutoff, now, policy, auditors, population, seed,
	)
	if err != nil {
		return SampleProof{}, err
	}
	sealedSeed, err := store.vault.SealRecord(
		store.epochAD(organizationID, epochID), seed,
	)
	if err != nil {
		return SampleProof{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SampleProof{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_cross_audit_epochs (
			tenant_id,organization_id,epoch_id,cutoff_at,population_root,
			seed_commitment,sealed_seed,numerator,denominator,minimum_count,
			maximum_count,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, store.tenantID, organizationID, epochID, cutoff,
		proof.PopulationRoot.Digest, proof.SeedCommitment.Digest, sealedSeed,
		policy.Numerator, policy.Denominator, policy.Minimum, policy.Maximum,
		now); err != nil {
		return SampleProof{}, ErrConflict
	}
	for _, selection := range proof.Selections {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_cross_audit_selections (
				tenant_id,organization_id,epoch_id,verdict_id,reauditor_seat_id,
				reauditor_department_id,score,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, store.tenantID, organizationID, epochID, selection.VerdictID,
			selection.ReauditorSeatID, selection.ReauditorDepartmentID,
			selection.Score, now); err != nil {
			return SampleProof{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SampleProof{}, err
	}
	return proof, nil
}

func VerifyProof(proof SampleProof) error {
	if proof.SchemaVersion != contracts.SchemaVersionV1 ||
		proof.OrganizationID == "" || proof.EpochID == "" ||
		proof.CutoffAt.Location() != time.UTC || proof.CreatedAt.Location() != time.UTC ||
		!proof.CutoffAt.Before(proof.CreatedAt) || proof.Policy.Validate() != nil ||
		!validAuditors(sortedAuditors(proof.Auditors)) {
		return ErrIntegrity
	}
	seed, err := hex.DecodeString(proof.Seed)
	if err != nil || len(seed) != 32 ||
		hashBytes(seed) != proof.SeedCommitment ||
		populationRoot(proof.Population) != proof.PopulationRoot {
		return ErrIntegrity
	}
	expected, err := buildProof(
		proof.OrganizationID, proof.EpochID, proof.CutoffAt, proof.CreatedAt,
		proof.Policy, proof.Auditors, proof.Population, seed,
	)
	if err != nil {
		return err
	}
	expectedBytes, _ := json.Marshal(expected.Selections)
	actualBytes, _ := json.Marshal(proof.Selections)
	if !hmac.Equal(expectedBytes, actualBytes) {
		return ErrIntegrity
	}
	return nil
}

func (store *Store) CommitReaudit(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	epochID string,
	originalID contracts.VerdictID,
	request CommitRequest,
) (contracts.Verdict, bool, error) {
	var originalOutcome contracts.VerdictOutcome
	var executingSeat contracts.SeatID
	var executingDepartment contracts.DepartmentID
	var selectedSeat contracts.SeatID
	var selectedDepartment contracts.DepartmentID
	err := store.pool.QueryRow(ctx, `
		SELECT verdict.outcome,verdict.executing_seat_id,
			verdict.executing_department_id,selection.reauditor_seat_id,
			selection.reauditor_department_id
		FROM workforce_cross_audit_selections selection
		JOIN workforce_verdict_records verdict
		  ON verdict.tenant_id=selection.tenant_id
		 AND verdict.organization_id=selection.organization_id
		 AND verdict.verdict_id=selection.verdict_id
		WHERE selection.tenant_id=$1 AND selection.organization_id=$2
		  AND selection.epoch_id=$3 AND selection.verdict_id=$4
	`, store.tenantID, organizationID, epochID, originalID).Scan(
		&originalOutcome, &executingSeat, &executingDepartment,
		&selectedSeat, &selectedDepartment,
	)
	if err != nil {
		return contracts.Verdict{}, false, ErrUnauthorized
	}
	currentExecuting, executingErr := store.authority.LoadCurrentSeat(ctx, executingSeat)
	currentAuditor, auditorErr := store.authority.LoadCurrentSeat(ctx, selectedSeat)
	if executingErr != nil || auditorErr != nil ||
		request.Packet.ExecutingSeatID != executingSeat ||
		request.Packet.AuditorSeatID != selectedSeat ||
		currentExecuting.DepartmentID != executingDepartment ||
		currentAuditor.DepartmentID != selectedDepartment {
		return contracts.Verdict{}, false, ErrUnauthorized
	}
	verdict, err := store.Commit(ctx, request)
	if err != nil {
		return contracts.Verdict{}, false, err
	}
	disagreement := verdict.Outcome != originalOutcome
	encoded, err := contracts.EncodeCanonical(&verdict)
	if err != nil {
		return contracts.Verdict{}, false, err
	}
	sealed, err := store.vault.SealRecord(
		store.ad("reaudit", organizationID, verdict.ID), encoded,
	)
	if err != nil {
		return contracts.Verdict{}, false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return contracts.Verdict{}, false, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Verdict{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_cross_audit_results (
			tenant_id,organization_id,epoch_id,original_verdict_id,
			reaudit_verdict_id,original_outcome,reaudit_outcome,disagreement,
			sealed_verdict,committed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.tenantID, organizationID, epochID, originalID, verdict.ID,
		originalOutcome, verdict.Outcome, disagreement, sealed, now); err != nil {
		return contracts.Verdict{}, false, ErrConflict
	}
	if disagreement {
		incidentHash := sha256.Sum256([]byte(
			epochID + "|" + string(originalID) + "|" + string(verdict.ID),
		))
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_cross_audit_incidents (
				tenant_id,organization_id,incident_id,epoch_id,original_verdict_id,
				reaudit_verdict_id,reason,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,'material_verdict_disagreement',$7)
		`, store.tenantID, organizationID, "audit-incident-"+hex.EncodeToString(incidentHash[:12]),
			epochID, originalID, verdict.ID, now); err != nil {
			return contracts.Verdict{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Verdict{}, false, err
	}
	return verdict, disagreement, nil
}

func buildProof(
	organizationID contracts.OrganizationID,
	epochID string,
	cutoff, createdAt time.Time,
	policy SamplingPolicy,
	auditors []Seat,
	population []PopulationEntry,
	seed []byte,
) (SampleProof, error) {
	auditors = sortedAuditors(auditors)
	ranked := make([]Selection, 0, len(population))
	for _, entry := range population {
		mac := hmac.New(sha256.New, seed)
		_, _ = mac.Write([]byte(string(entry.VerdictID) + "|" + entry.VerdictHash.Digest))
		score := mac.Sum(nil)
		eligible := make([]Seat, 0, len(auditors))
		for _, auditor := range auditors {
			if auditor.DepartmentID != entry.ExecutingDepartmentID {
				eligible = append(eligible, auditor)
			}
		}
		if len(eligible) == 0 {
			return SampleProof{}, ErrUnauthorized
		}
		index := int(score[0]) % len(eligible)
		ranked = append(ranked, Selection{
			VerdictID:             entry.VerdictID,
			ReauditorSeatID:       eligible[index].ID,
			ReauditorDepartmentID: eligible[index].DepartmentID,
			Score:                 hex.EncodeToString(score),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score < ranked[j].Score
		}
		return ranked[i].VerdictID < ranked[j].VerdictID
	})
	count := uint32((uint64(len(ranked))*uint64(policy.Numerator) +
		uint64(policy.Denominator) - 1) / uint64(policy.Denominator))
	if count < policy.Minimum {
		count = policy.Minimum
	}
	if count > policy.Maximum {
		count = policy.Maximum
	}
	if count > uint32(len(ranked)) {
		count = uint32(len(ranked))
	}
	seedHash := hashBytes(seed)
	return SampleProof{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID, EpochID: epochID,
		CutoffAt: cutoff, Policy: policy,
		Population: append([]PopulationEntry(nil), population...),
		Auditors:   auditors, PopulationRoot: populationRoot(population),
		Seed: hex.EncodeToString(seed), SeedCommitment: seedHash,
		Selections: append([]Selection(nil), ranked[:count]...), CreatedAt: createdAt,
	}, nil
}

func populationRoot(population []PopulationEntry) contracts.ContentHash {
	hash := sha256.New()
	for _, entry := range population {
		_, _ = fmt.Fprintf(hash, "%s|%s|%s\n", entry.VerdictID,
			entry.VerdictHash.Digest, entry.ExecutingDepartmentID)
	}
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(hash.Sum(nil)),
	}
}

func sortedAuditors(values []Seat) []Seat {
	result := append([]Seat(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].DepartmentID != result[j].DepartmentID {
			return result[i].DepartmentID < result[j].DepartmentID
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func validAuditors(values []Seat) bool {
	seen := map[contracts.SeatID]bool{}
	departments := map[contracts.DepartmentID]bool{}
	for _, value := range values {
		if value.ID == "" || value.DepartmentID == "" || seen[value.ID] {
			return false
		}
		seen[value.ID] = true
		departments[value.DepartmentID] = true
	}
	return len(departments) >= 2
}

func packetHash(packet contracts.VerdictPacket) contracts.ContentHash {
	encoded, err := contracts.EncodeCanonical(&packet)
	if err != nil {
		return contracts.ContentHash{}
	}
	return hashBytes(encoded)
}

func hashBytes(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func aggregateReason(values []string) string {
	encoded := strings.Join(values, "|")
	sum := sha256.Sum256([]byte(encoded))
	return "predicate-" + hex.EncodeToString(sum[:8])
}

func (store *Store) signVerdict(verdict *contracts.Verdict) error {
	verdict.Signature = placeholder(store.keyID)
	payload, err := json.Marshal(verdict)
	if err != nil {
		return err
	}
	verdict.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: store.keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(store.privateKey, payload)),
	}
	return verdict.Validate()
}

func (store *Store) verifyVerdict(verdict contracts.Verdict) error {
	if verdict.Signature.Algorithm != "ed25519" || verdict.Signature.KeyID != store.keyID {
		return ErrIntegrity
	}
	signature, err := base64.RawURLEncoding.DecodeString(verdict.Signature.Value)
	if err != nil {
		return ErrIntegrity
	}
	verdict.Signature = placeholder(store.keyID)
	payload, err := json.Marshal(verdict)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrIntegrity
	}
	return nil
}

func placeholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func (store *Store) ad(
	kind string,
	organizationID contracts.OrganizationID,
	id contracts.VerdictID,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.audit." + kind,
		Stream: string(organizationID) + "/" + string(id),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) epochAD(
	organizationID contracts.OrganizationID,
	epochID string,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.audit.epoch",
		Stream: string(organizationID) + "/" + epochID,
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("audit: time source must return UTC")
	}
	return now, nil
}
