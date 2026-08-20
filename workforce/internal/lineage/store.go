package lineage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/workcompile"
)

var (
	ErrConflict  = errors.New("lineage: immutable evidence conflict")
	ErrIntegrity = errors.New("lineage: evidence integrity failure")
)

type ModelEvidence struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             contracts.EvidenceID     `json:"evidence_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	WakeID         contracts.WakeID         `json:"wake_id"`
	Model          contracts.ModelBinding   `json:"model"`
	MGS            contracts.MGSGenomeRef   `json:"mgs"`
	Runtime        contracts.RuntimeBinding `json:"runtime"`
	RequestHash    contracts.ContentHash    `json:"request_hash"`
	ResponseHash   contracts.ContentHash    `json:"response_hash"`
	OutputHash     contracts.ContentHash    `json:"output_hash"`
	ReplayRetained bool                     `json:"replay_retained"`
	CreatedAt      time.Time                `json:"created_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value ModelEvidence) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || value.ID == "" ||
		value.OrganizationID == "" || value.WakeID == "" ||
		value.CreatedAt.IsZero() || value.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("lineage: model evidence identity is invalid")
	}
	if err := value.Model.Validate(); err != nil {
		return err
	}
	if err := value.MGS.Validate(); err != nil {
		return err
	}
	if err := value.Runtime.Validate(); err != nil {
		return err
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	if err := value.ResponseHash.Validate(); err != nil {
		return err
	}
	if err := value.OutputHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type ModelExchange struct {
	ID             contracts.EvidenceID
	OrganizationID contracts.OrganizationID
	WakeID         contracts.WakeID
	Model          contracts.ModelBinding
	MGS            contracts.MGSGenomeRef
	Runtime        contracts.RuntimeBinding
	Request        []byte
	Response       []byte
	Output         []byte
	ReplayRequired bool
}

type modelEnvelope struct {
	Evidence ModelEvidence `json:"evidence"`
	Request  []byte        `json:"request"`
	Response []byte        `json:"response"`
	Output   []byte        `json:"output"`
}

type ReceiptInput struct {
	ID               contracts.ReceiptID
	Packet           contracts.WorkPacket
	Plan             workcompile.Plan
	ModelEvidence    ModelEvidence
	ChildIntentIDs   []contracts.IntentID
	Constraints      []string
	Approvals        []contracts.ApprovalID
	Artifacts        []contracts.ArtifactRef
	Evidence         []contracts.EvidenceRef
	Reconciliation   []contracts.ReconciliationLineage
	VerdictID        *contracts.VerdictID
	CostMinor        int64
	LatencyMillis    uint64
	Disposition      contracts.WakeDisposition
	UnresolvedRisk   string
	OperationOutcome string
}

type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	keyID      string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	now        func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID, keyID string,
	privateKey ed25519.PrivateKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || tenantID == "" || keyID == "" ||
		len(privateKey) != ed25519.PrivateKeySize || now == nil {
		return nil, fmt.Errorf("lineage: durable store and signing authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("lineage: Vault user does not match tenant")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, keyID: keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		publicKey:  append(ed25519.PublicKey(nil), publicKey...), now: now,
	}, nil
}

func (store *Store) PutModelEvidence(
	ctx context.Context,
	exchange ModelExchange,
) (ModelEvidence, error) {
	now, err := store.currentTime()
	if err != nil {
		return ModelEvidence{}, err
	}
	if exchange.ID == "" || exchange.OrganizationID == "" || exchange.WakeID == "" ||
		len(exchange.Request) == 0 || len(exchange.Request) > 2<<20 ||
		len(exchange.Response) == 0 || len(exchange.Response) > 2<<20 ||
		len(exchange.Output) == 0 || len(exchange.Output) > 2<<20 {
		return ModelEvidence{}, fmt.Errorf("lineage: model exchange is invalid")
	}
	if err := exchange.Model.Validate(); err != nil {
		return ModelEvidence{}, err
	}
	if err := exchange.MGS.Validate(); err != nil {
		return ModelEvidence{}, err
	}
	if err := exchange.Runtime.Validate(); err != nil {
		return ModelEvidence{}, err
	}
	request, err := canonicalJSON(exchange.Request)
	if err != nil {
		return ModelEvidence{}, fmt.Errorf("lineage: canonical model request: %w", err)
	}
	requestHash := contentHash(request)
	responseHash := contentHash(exchange.Response)
	outputHash := contentHash(exchange.Output)
	evidence := ModelEvidence{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            exchange.ID, OrganizationID: exchange.OrganizationID,
		WakeID: exchange.WakeID, Model: exchange.Model, MGS: exchange.MGS,
		Runtime: exchange.Runtime, RequestHash: requestHash,
		ResponseHash: responseHash, OutputHash: outputHash,
		ReplayRetained: exchange.ReplayRequired,
		CreatedAt:      now,
	}
	if err := store.signModelEvidence(&evidence); err != nil {
		return ModelEvidence{}, err
	}
	envelope := modelEnvelope{
		Evidence: evidence, Output: append([]byte(nil), exchange.Output...),
	}
	if exchange.ReplayRequired {
		envelope.Request = request
		envelope.Response = append([]byte(nil), exchange.Response...)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return ModelEvidence{}, err
	}
	envelopeHash := contentHash(encoded)
	sealed, err := store.vault.SealRecord(
		store.modelAD(exchange.OrganizationID, exchange.ID), encoded,
	)
	if err != nil {
		return ModelEvidence{}, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_model_evidence (
			tenant_id,organization_id,evidence_id,wake_id,request_hash,
			response_hash,output_hash,replay_retained,envelope_hash,
			sealed_envelope,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT DO NOTHING
	`, store.tenantID, exchange.OrganizationID, exchange.ID, exchange.WakeID,
		requestHash.Digest, responseHash.Digest, outputHash.Digest,
		exchange.ReplayRequired, envelopeHash.Digest, sealed, now)
	if err != nil {
		return ModelEvidence{}, err
	}
	if command.RowsAffected() == 0 {
		existing, _, _, loadErr := store.OpenModelEvidence(
			ctx, exchange.OrganizationID, exchange.ID,
		)
		if loadErr != nil {
			return ModelEvidence{}, loadErr
		}
		if existing.RequestHash != requestHash || existing.ResponseHash != responseHash ||
			existing.OutputHash != outputHash ||
			existing.ReplayRetained != exchange.ReplayRequired {
			return ModelEvidence{}, ErrConflict
		}
		return existing, nil
	}
	return evidence, nil
}

func (store *Store) OpenModelEvidence(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	id contracts.EvidenceID,
) (ModelEvidence, []byte, []byte, error) {
	envelope, err := store.openModelEnvelope(ctx, organizationID, id)
	if err != nil {
		return ModelEvidence{}, nil, nil, err
	}
	return envelope.Evidence, envelope.Request, envelope.Response, nil
}

// OpenModelOutput returns the exact seat-visible provider output bound into the
// same signed and sealed lineage envelope as the raw exchange.
func (store *Store) OpenModelOutput(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	id contracts.EvidenceID,
) (ModelEvidence, []byte, error) {
	envelope, err := store.openModelEnvelope(ctx, organizationID, id)
	if err != nil {
		return ModelEvidence{}, nil, err
	}
	return envelope.Evidence, append([]byte(nil), envelope.Output...), nil
}

func (store *Store) openModelEnvelope(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	id contracts.EvidenceID,
) (modelEnvelope, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_envelope,envelope_hash FROM workforce_model_evidence
		WHERE tenant_id=$1 AND organization_id=$2 AND evidence_id=$3
	`, store.tenantID, organizationID, id).Scan(&sealed, &expectedHash)
	if err != nil {
		return modelEnvelope{}, err
	}
	opened, err := store.vault.OpenRecord(store.modelAD(organizationID, id), sealed)
	if err != nil || contentHash(opened).Digest != expectedHash {
		return modelEnvelope{}, ErrIntegrity
	}
	var envelope modelEnvelope
	if err := json.Unmarshal(opened, &envelope); err != nil ||
		envelope.Evidence.Validate() != nil ||
		store.verifyModelEvidence(envelope.Evidence) != nil ||
		envelope.Evidence.OrganizationID != organizationID ||
		envelope.Evidence.ID != id {
		return modelEnvelope{}, ErrIntegrity
	}
	if envelope.Evidence.ReplayRetained {
		request, err := canonicalJSON(envelope.Request)
		if err != nil || contentHash(request) != envelope.Evidence.RequestHash ||
			contentHash(envelope.Response) != envelope.Evidence.ResponseHash {
			return modelEnvelope{}, ErrIntegrity
		}
	} else if len(envelope.Request) != 0 || len(envelope.Response) != 0 {
		return modelEnvelope{}, ErrIntegrity
	}
	if len(envelope.Output) == 0 ||
		contentHash(envelope.Output) != envelope.Evidence.OutputHash {
		return modelEnvelope{}, ErrIntegrity
	}
	return envelope, nil
}

func (store *Store) BuildReceipt(input ReceiptInput) (contracts.Receipt, error) {
	now, err := store.currentTime()
	if err != nil {
		return contracts.Receipt{}, err
	}
	if err := input.Packet.Validate(); err != nil {
		return contracts.Receipt{}, err
	}
	if err := input.Plan.Validate(); err != nil {
		return contracts.Receipt{}, err
	}
	if err := input.ModelEvidence.Validate(); err != nil {
		return contracts.Receipt{}, err
	}
	if input.Plan.WakeID != input.Packet.Lease.WakeID ||
		input.ModelEvidence.WakeID != input.Packet.Lease.WakeID ||
		!hasSkill(input.Packet.Skills, input.Plan.Skill) ||
		input.OperationOutcome == "" {
		return contracts.Receipt{}, ErrConflict
	}
	receipt := contracts.Receipt{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            input.ID, OrganizationID: input.Packet.Lease.OrganizationID,
		DepartmentID: input.Packet.Seat.DepartmentID,
		WakeID:       input.Packet.Lease.WakeID, LeaseID: input.Packet.Lease.ID,
		SeatID: input.Packet.Seat.ID, SeatDID: input.Packet.Seat.DID,
		MandateID: input.Packet.Mandate.ID, MandateVersion: input.Packet.Mandate.Version,
		ParentIntentID: input.Packet.Intent.ID,
		ChildIntentIDs: append([]contracts.IntentID(nil), input.ChildIntentIDs...),
		Inputs:         append([]contracts.RecordRef(nil), input.Packet.VerifiedState...),
		Constraints:    append([]string(nil), input.Constraints...),
		Approvals:      append([]contracts.ApprovalID(nil), input.Approvals...),
		Policies:       append([]contracts.PolicyRef(nil), input.Packet.Policies...),
		Operations: []contracts.OperationLineage{{
			Name:        input.Plan.Operation.Operation,
			EffectClass: string(input.Plan.Operation.EffectClass),
			Digest:      input.Plan.Operation.OperationDigest,
			Outcome:     input.OperationOutcome,
		}},
		Artifacts:      append([]contracts.ArtifactRef(nil), input.Artifacts...),
		Evidence:       append([]contracts.EvidenceRef(nil), input.Evidence...),
		Reconciliation: append([]contracts.ReconciliationLineage(nil), input.Reconciliation...),
		Model:          input.Plan.Model, MGS: input.Plan.MGS, Runtime: input.Plan.Runtime,
		Source: input.Plan.Source, Skill: input.Plan.Skill,
		VerifierDigest:    input.Plan.VerifierDigest,
		ModelRequestHash:  input.ModelEvidence.RequestHash,
		ModelResponseHash: input.ModelEvidence.ResponseHash,
		VerdictID:         input.VerdictID, CostMinor: input.CostMinor,
		Currency:      input.Packet.Lease.Budget.Currency,
		LatencyMillis: input.LatencyMillis, Disposition: input.Disposition,
		UnresolvedRisk: input.UnresolvedRisk, CreatedAt: now,
	}
	if err := store.signReceipt(&receipt); err != nil {
		return contracts.Receipt{}, err
	}
	return receipt, nil
}

func (store *Store) PublishReceipt(
	ctx context.Context,
	receipt contracts.Receipt,
) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := store.verifyReceipt(receipt); err != nil {
		return err
	}
	encoded, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return err
	}
	sealed, err := store.vault.SealRecord(store.receiptAD(receipt), encoded)
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_execution_receipts (
			tenant_id,organization_id,receipt_id,wake_id,intent_id,disposition,
			content_hash,sealed_receipt,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING
	`, store.tenantID, receipt.OrganizationID, receipt.ID, receipt.WakeID,
		receipt.ParentIntentID, receipt.Disposition, receipt.ContentHash.Digest,
		sealed, receipt.CreatedAt)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		existing, loadErr := store.OpenReceipt(ctx, receipt.OrganizationID, receipt.ID)
		if loadErr != nil {
			return loadErr
		}
		if existing.ContentHash != receipt.ContentHash {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) OpenReceipt(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	id contracts.ReceiptID,
) (contracts.Receipt, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_receipt,content_hash FROM workforce_execution_receipts
		WHERE tenant_id=$1 AND organization_id=$2 AND receipt_id=$3
	`, store.tenantID, organizationID, id).Scan(&sealed, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Receipt{}, ErrConflict
	}
	if err != nil {
		return contracts.Receipt{}, err
	}
	ad := vault.AD{
		User: store.tenantID, Store: "workforce.execution.receipt",
		Stream: string(organizationID) + "/" + string(id),
		Schema: contracts.SchemaVersionV1,
	}
	opened, err := store.vault.OpenRecord(ad, sealed)
	if err != nil {
		return contracts.Receipt{}, ErrIntegrity
	}
	receipt, err := contracts.DecodeCanonical[contracts.Receipt, *contracts.Receipt](opened)
	if err != nil || receipt.ContentHash.Digest != expectedHash ||
		receipt.OrganizationID != organizationID || receipt.ID != id ||
		store.verifyReceipt(receipt) != nil {
		return contracts.Receipt{}, ErrIntegrity
	}
	return receipt, nil
}

func (store *Store) signModelEvidence(value *ModelEvidence) error {
	value.Signature = signaturePlaceholder(store.keyID)
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: store.keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(store.privateKey, payload)),
	}
	return value.Validate()
}

func (store *Store) verifyModelEvidence(value ModelEvidence) error {
	signature, err := decodeSignature(value.Signature, store.keyID)
	if err != nil {
		return err
	}
	value.Signature = signaturePlaceholder(store.keyID)
	payload, err := json.Marshal(value)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrIntegrity
	}
	return nil
}

func (store *Store) signReceipt(value *contracts.Receipt) error {
	value.ContentHash = contracts.ContentHash{}
	value.Signature = signaturePlaceholder(store.keyID)
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	value.ContentHash = contentHash(payload)
	value.Signature = signaturePlaceholder(store.keyID)
	payload, err = json.Marshal(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: store.keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(store.privateKey, payload)),
	}
	return value.Validate()
}

func (store *Store) verifyReceipt(value contracts.Receipt) error {
	signature, err := decodeSignature(value.Signature, store.keyID)
	if err != nil {
		return err
	}
	signing := value
	signing.Signature = signaturePlaceholder(store.keyID)
	payload, err := json.Marshal(signing)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrIntegrity
	}
	hashValue := value
	hashValue.ContentHash = contracts.ContentHash{}
	hashValue.Signature = signaturePlaceholder(store.keyID)
	payload, err = json.Marshal(hashValue)
	if err != nil || contentHash(payload) != value.ContentHash {
		return ErrIntegrity
	}
	return nil
}

func canonicalJSON(value []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return json.Marshal(decoded)
}

func hasSkill(values []contracts.SkillRef, target contracts.SkillRef) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func contentHash(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func decodeSignature(value contracts.Signature, keyID string) ([]byte, error) {
	if value.Algorithm != "ed25519" || value.KeyID != keyID {
		return nil, ErrIntegrity
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return nil, ErrIntegrity
	}
	return decoded, nil
}

func (store *Store) modelAD(
	organizationID contracts.OrganizationID,
	id contracts.EvidenceID,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.model.evidence",
		Stream: string(organizationID) + "/" + string(id),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) receiptAD(receipt contracts.Receipt) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.execution.receipt",
		Stream: string(receipt.OrganizationID) + "/" + string(receipt.ID),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("lineage: time source must return UTC")
	}
	return now, nil
}
