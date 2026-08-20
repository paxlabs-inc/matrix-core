package projectbrain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

var (
	ErrConflict     = errors.New("project brain immutable conflict")
	ErrNotFound     = errors.New("project brain record not found")
	ErrUnauthorized = errors.New("project brain access denied")
	ErrIntegrity    = errors.New("project brain integrity failure")
)

// RecordID identifies one immutable verified engineering record.
type RecordID string

// Kind is the closed set of durable engineering knowledge.
type Kind string

const (
	KindDecision     Kind = "decision"
	KindInvariant    Kind = "invariant"
	KindOwnership    Kind = "ownership"
	KindPlan         Kind = "plan"
	KindFailure      Kind = "failure"
	KindOutcome      Kind = "outcome"
	KindVerification Kind = "verification"
	KindHazard       Kind = "hazard"
	KindHandoff      Kind = "handoff"
	KindCorrection   Kind = "correction"
)

func (kind Kind) valid() bool {
	switch kind {
	case KindDecision, KindInvariant, KindOwnership, KindPlan, KindFailure,
		KindOutcome, KindVerification, KindHazard, KindHandoff, KindCorrection:
		return true
	default:
		return false
	}
}

// Origin identifies the authoritative evidence family behind a proposal.
type Origin string

const (
	OriginSource     Origin = "source"
	OriginTest       Origin = "test"
	OriginReceipt    Origin = "receipt"
	OriginReview     Origin = "independent_review"
	OriginCorrection Origin = "correction"
)

func (origin Origin) valid() bool {
	switch origin {
	case OriginSource, OriginTest, OriginReceipt, OriginReview, OriginCorrection:
		return true
	default:
		return false
	}
}

// FileEvidence binds a claim to current source bytes.
type FileEvidence struct {
	Path      string                `json:"path"`
	Hash      contracts.ContentHash `json:"hash"`
	StartLine uint32                `json:"start_line"`
	EndLine   uint32                `json:"end_line"`
}

func (evidence FileEvidence) validate() error {
	if err := validateRelativePath(evidence.Path); err != nil {
		return err
	}
	if err := evidence.Hash.Validate(); err != nil {
		return fmt.Errorf("file evidence hash: %w", err)
	}
	if evidence.StartLine == 0 || evidence.EndLine < evidence.StartLine {
		return fmt.Errorf("project brain file evidence lines are invalid")
	}
	return nil
}

// Claim is one bounded engineering assertion with direct evidence.
type Claim struct {
	Statement string                  `json:"statement"`
	Files     []FileEvidence          `json:"files"`
	Evidence  []contracts.EvidenceRef `json:"evidence"`
}

func (claim Claim) validate() error {
	if strings.TrimSpace(claim.Statement) == "" || len(claim.Statement) > 4096 {
		return fmt.Errorf("project brain claim statement must contain 1 to 4096 bytes")
	}
	if len(claim.Files) == 0 && len(claim.Evidence) == 0 {
		return fmt.Errorf("project brain claim requires source or authoritative evidence")
	}
	if len(claim.Files) > 128 || len(claim.Evidence) > 128 {
		return fmt.Errorf("project brain claim evidence exceeds bounds")
	}
	for index := range claim.Files {
		if err := claim.Files[index].validate(); err != nil {
			return fmt.Errorf("project brain claim file %d: %w", index, err)
		}
	}
	for index := range claim.Evidence {
		if err := claim.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("project brain claim evidence %d: %w", index, err)
		}
	}
	return nil
}

// Content is the complete typed body of an engineering record.
type Content struct {
	Summary   string                  `json:"summary"`
	Claims    []Claim                 `json:"claims"`
	Artifacts []contracts.ArtifactRef `json:"artifacts"`
	ExpiresAt *time.Time              `json:"expires_at"`
}

func (content Content) validate() error {
	if strings.TrimSpace(content.Summary) == "" || len(content.Summary) > 8192 {
		return fmt.Errorf("project brain summary must contain 1 to 8192 bytes")
	}
	if len(content.Claims) == 0 || len(content.Claims) > 256 {
		return fmt.Errorf("project brain content requires 1 to 256 verified claims")
	}
	if len(content.Artifacts) > 128 {
		return fmt.Errorf("project brain content artifacts exceed bounds")
	}
	for index := range content.Claims {
		if err := content.Claims[index].validate(); err != nil {
			return fmt.Errorf("project brain claim %d: %w", index, err)
		}
	}
	for index := range content.Artifacts {
		if err := content.Artifacts[index].Validate(); err != nil {
			return fmt.Errorf("project brain artifact %d: %w", index, err)
		}
	}
	if content.ExpiresAt != nil && (content.ExpiresAt.IsZero() || content.ExpiresAt.Location() != time.UTC) {
		return fmt.Errorf("project brain expiry must be a non-zero UTC timestamp")
	}
	return nil
}

// Proposal is a seat-signed request to add verified project knowledge.
type Proposal struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             RecordID                 `json:"record_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ProjectID      contracts.ProjectID      `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID    `json:"workspace_id"`
	AuthorSeatID   contracts.SeatID         `json:"author_seat_id"`
	ParentIntentID contracts.IntentID       `json:"parent_intent_id"`
	Kind           Kind                     `json:"kind"`
	Origin         Origin                   `json:"origin"`
	Version        uint64                   `json:"version"`
	Source         GraphSnapshot            `json:"source"`
	Content        Content                  `json:"content"`
	Supersedes     *RecordID                `json:"supersedes"`
	Corrects       *RecordID                `json:"corrects"`
	CreatedAt      time.Time                `json:"created_at"`
	Signature      contracts.Signature      `json:"signature"`
}

// Validate rejects persona memory, ungrounded claims, stale source ambiguity,
// and malformed supersession chains at the trust boundary.
func (proposal Proposal) Validate() error {
	if proposal.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("project brain proposal schema is unsupported")
	}
	for name, value := range map[string]string{
		"record_id": string(proposal.ID), "organization_id": string(proposal.OrganizationID),
		"project_id": string(proposal.ProjectID), "workspace_id": string(proposal.WorkspaceID),
		"author_seat_id":   string(proposal.AuthorSeatID),
		"parent_intent_id": string(proposal.ParentIntentID),
	} {
		if err := validateToken(name, value); err != nil {
			return err
		}
	}
	if !proposal.Kind.valid() || !proposal.Origin.valid() || proposal.Version == 0 {
		return fmt.Errorf("project brain proposal kind, origin, and version are required")
	}
	if err := proposal.Source.Validate(); err != nil {
		return err
	}
	if err := proposal.Content.validate(); err != nil {
		return err
	}
	if proposal.Supersedes != nil && proposal.Corrects != nil {
		return fmt.Errorf("project brain proposal cannot supersede and correct simultaneously")
	}
	if proposal.Kind == KindCorrection && proposal.Corrects == nil {
		return fmt.Errorf("project brain correction requires corrects")
	}
	if proposal.Kind != KindCorrection && proposal.Corrects != nil {
		return fmt.Errorf("only correction records may set corrects")
	}
	if proposal.Supersedes != nil {
		if err := validateToken("supersedes", string(*proposal.Supersedes)); err != nil {
			return err
		}
	}
	if proposal.Corrects != nil {
		if err := validateToken("corrects", string(*proposal.Corrects)); err != nil {
			return err
		}
	}
	if proposal.CreatedAt.IsZero() || proposal.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("project brain created_at must be a non-zero UTC timestamp")
	}
	return proposal.Signature.Validate()
}

// Verification is an independent seat's signed decision over one exact proposal.
type Verification struct {
	SchemaVersion  string                  `json:"schema_version"`
	RecordID       RecordID                `json:"record_id"`
	VerifierSeatID contracts.SeatID        `json:"verifier_seat_id"`
	ProposalHash   contracts.ContentHash   `json:"proposal_hash"`
	Accepted       bool                    `json:"accepted"`
	Procedure      string                  `json:"procedure"`
	Evidence       []contracts.EvidenceRef `json:"evidence"`
	VerifiedAt     time.Time               `json:"verified_at"`
	Signature      contracts.Signature     `json:"signature"`
}

func (verification Verification) Validate() error {
	if verification.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("project brain verification schema is unsupported")
	}
	if err := validateToken("record_id", string(verification.RecordID)); err != nil {
		return err
	}
	if err := validateToken("verifier_seat_id", string(verification.VerifierSeatID)); err != nil {
		return err
	}
	if err := verification.ProposalHash.Validate(); err != nil {
		return err
	}
	if !verification.Accepted {
		return fmt.Errorf("rejected proposal cannot become project brain truth")
	}
	if strings.TrimSpace(verification.Procedure) == "" || len(verification.Procedure) > 512 {
		return fmt.Errorf("project brain verification procedure is required")
	}
	if len(verification.Evidence) == 0 || len(verification.Evidence) > 128 {
		return fmt.Errorf("project brain verification requires bounded evidence")
	}
	for index := range verification.Evidence {
		if err := verification.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("project brain verification evidence %d: %w", index, err)
		}
	}
	if verification.VerifiedAt.IsZero() || verification.VerifiedAt.Location() != time.UTC {
		return fmt.Errorf("project brain verified_at must be a non-zero UTC timestamp")
	}
	return verification.Signature.Validate()
}

// EngineeringRecord is immutable project truth after independent verification.
type EngineeringRecord struct {
	Proposal     Proposal     `json:"proposal"`
	Verification Verification `json:"verification"`
}

// Validate enforces exact proposal-verdict binding and independent authorship.
func (record EngineeringRecord) Validate() error {
	if err := record.Proposal.Validate(); err != nil {
		return err
	}
	if err := record.Verification.Validate(); err != nil {
		return err
	}
	if record.Proposal.ID != record.Verification.RecordID {
		return fmt.Errorf("project brain verification targets a different record")
	}
	if record.Proposal.AuthorSeatID == record.Verification.VerifierSeatID {
		return fmt.Errorf("project brain author cannot verify its own proposal")
	}
	hash, err := proposalHash(record.Proposal)
	if err != nil {
		return err
	}
	if hash != record.Verification.ProposalHash {
		return fmt.Errorf("project brain verification proposal hash mismatch")
	}
	sourceFiles := make(map[string]contracts.ContentHash, len(record.Proposal.Source.Files))
	for _, file := range record.Proposal.Source.Files {
		sourceFiles[file.Path] = file.Hash
	}
	for _, claim := range record.Proposal.Content.Claims {
		for _, evidence := range claim.Files {
			hash, found := sourceFiles[evidence.Path]
			if !found || hash != evidence.Hash {
				return fmt.Errorf("project brain claim source is absent or stale")
			}
		}
	}
	return nil
}

// GraphFile is current source evidence associated with one indexed file.
type GraphFile struct {
	Path      string                `json:"path"`
	Language  string                `json:"language"`
	NodeCount uint64                `json:"node_count"`
	SizeBytes uint64                `json:"size_bytes"`
	Hash      contracts.ContentHash `json:"hash"`
	Indexed   bool                  `json:"indexed"`
}

// GraphSnapshot binds a CodeGraph generation to current source bytes.
type GraphSnapshot struct {
	SchemaVersion string                `json:"schema_version"`
	RootDigest    contracts.ContentHash `json:"root_digest"`
	GraphDigest   contracts.ContentHash `json:"graph_digest"`
	Generation    uint64                `json:"generation"`
	IndexedAt     time.Time             `json:"indexed_at"`
	CapturedAt    time.Time             `json:"captured_at"`
	Fresh         bool                  `json:"fresh"`
	PendingFiles  []string              `json:"pending_files"`
	Files         []GraphFile           `json:"files"`
	NodeCount     uint64                `json:"node_count"`
	EdgeCount     uint64                `json:"edge_count"`
}

// Validate enforces explicit staleness and current file hashes.
func (snapshot GraphSnapshot) Validate() error {
	if snapshot.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("project brain graph schema is unsupported")
	}
	if err := snapshot.RootDigest.Validate(); err != nil {
		return err
	}
	if err := snapshot.GraphDigest.Validate(); err != nil {
		return err
	}
	if snapshot.Generation == 0 || snapshot.IndexedAt.IsZero() ||
		snapshot.IndexedAt.Location() != time.UTC || snapshot.CapturedAt.IsZero() ||
		snapshot.CapturedAt.Location() != time.UTC || snapshot.CapturedAt.Before(snapshot.IndexedAt) {
		return fmt.Errorf("project brain graph generation times are invalid")
	}
	if snapshot.Fresh != (len(snapshot.PendingFiles) == 0) {
		return fmt.Errorf("project brain graph freshness and pending files disagree")
	}
	if len(snapshot.Files) > 10000 || len(snapshot.PendingFiles) > 10000 {
		return fmt.Errorf("project brain graph file projection exceeds bounds")
	}
	seen := make(map[string]struct{}, len(snapshot.Files))
	pathBytes := 0
	for index := range snapshot.Files {
		file := snapshot.Files[index]
		pathBytes += len(file.Path)
		if err := validateRelativePath(file.Path); err != nil {
			return fmt.Errorf("project brain graph file %d: %w", index, err)
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return fmt.Errorf("project brain graph contains duplicate file %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if err := file.Hash.Validate(); err != nil {
			return fmt.Errorf("project brain graph file %d hash: %w", index, err)
		}
	}
	for _, path := range snapshot.PendingFiles {
		pathBytes += len(path)
		if err := validateRelativePath(path); err != nil {
			return err
		}
	}
	if pathBytes > 1<<20 {
		return fmt.Errorf("project brain graph paths exceed one MiB")
	}
	return nil
}

// View is a bounded read-only Project Brain projection for one fresh wake.
type View struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ProjectID      contracts.ProjectID      `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID    `json:"workspace_id"`
	Source         GraphSnapshot            `json:"source"`
	Records        []EngineeringRecord      `json:"records"`
	StaleRecordIDs []RecordID               `json:"stale_record_ids"`
	NextCursor     *RecordID                `json:"next_cursor"`
	Digest         contracts.ContentHash    `json:"digest"`
	ExpiresAt      time.Time                `json:"expires_at"`
}

// Validate enforces a bounded, current, content-addressed read projection.
func (view View) Validate() error {
	if view.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("project brain view schema is unsupported")
	}
	for name, value := range map[string]string{
		"organization_id": string(view.OrganizationID),
		"project_id":      string(view.ProjectID), "workspace_id": string(view.WorkspaceID),
	} {
		if err := validateToken(name, value); err != nil {
			return err
		}
	}
	if err := view.Source.Validate(); err != nil {
		return err
	}
	if len(view.Records) > 1024 || len(view.StaleRecordIDs) > 1024 {
		return fmt.Errorf("project brain view record projection exceeds bounds")
	}
	for index := range view.Records {
		if err := view.Records[index].Validate(); err != nil {
			return fmt.Errorf("project brain view record %d: %w", index, err)
		}
	}
	for _, id := range view.StaleRecordIDs {
		if err := validateToken("stale_record_id", string(id)); err != nil {
			return err
		}
	}
	if view.NextCursor != nil {
		if err := validateToken("next_cursor", string(*view.NextCursor)); err != nil {
			return err
		}
	}
	if err := view.Digest.Validate(); err != nil {
		return err
	}
	if view.ExpiresAt.IsZero() || view.ExpiresAt.Location() != time.UTC {
		return fmt.Errorf("project brain view expiry is invalid")
	}
	return nil
}

// CapabilityOperation is the closed Project Brain authorization set.
type CapabilityOperation string

const (
	CapabilityRead  CapabilityOperation = "read"
	CapabilityWrite CapabilityOperation = "write"
	// CapabilityChangeScope authorizes one Developer change-scope acquisition.
	// It does not authorize a Project Brain record read or write by itself.
	CapabilityChangeScope CapabilityOperation = "change_scope"
)

// SeatKeyBinding binds one seat identity to one exact Ed25519 key.
type SeatKeyBinding struct {
	SeatID         contracts.SeatID        `json:"seat_id"`
	SeatVersion    uint64                  `json:"seat_version"`
	SeatDID        contracts.SeatDID       `json:"seat_did"`
	BindingID      contracts.SeatBindingID `json:"binding_id"`
	BindingVersion uint64                  `json:"binding_version"`
	KeyID          string                  `json:"key_id"`
	PublicKey      string                  `json:"public_key"`
}

func (binding SeatKeyBinding) validate() error {
	if err := validateToken("seat_id", string(binding.SeatID)); err != nil {
		return err
	}
	if err := validateToken("seat_did", string(binding.SeatDID)); err != nil {
		return err
	}
	if err := validateToken("binding_id", string(binding.BindingID)); err != nil {
		return err
	}
	if binding.SeatVersion == 0 || binding.BindingVersion == 0 {
		return fmt.Errorf("project brain seat binding versions must be positive")
	}
	if err := validateToken("key_id", binding.KeyID); err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(binding.PublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return fmt.Errorf("project brain seat public key is invalid")
	}
	return nil
}

func (binding SeatKeyBinding) publicKey() (ed25519.PublicKey, error) {
	if err := binding.validate(); err != nil {
		return nil, err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(binding.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("project brain decode seat public key: %w", err)
	}
	return ed25519.PublicKey(decoded), nil
}

// CapabilityGrant is a kernel-signed, exact project/workspace read or write authority.
type CapabilityGrant struct {
	SchemaVersion           string                   `json:"schema_version"`
	ID                      string                   `json:"grant_id"`
	TenantID                string                   `json:"tenant_id"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	ProjectID               contracts.ProjectID      `json:"project_id"`
	WorkspaceID             contracts.WorkspaceID    `json:"workspace_id"`
	WorkspaceRoot           string                   `json:"workspace_root"`
	Filter                  string                   `json:"filter"`
	Operation               CapabilityOperation      `json:"operation"`
	RequesterSeatID         contracts.SeatID         `json:"requester_seat_id"`
	RequesterSeatVersion    uint64                   `json:"requester_seat_version"`
	RequesterSeatDID        contracts.SeatDID        `json:"requester_seat_did"`
	RequesterBindingID      contracts.SeatBindingID  `json:"requester_binding_id"`
	RequesterBindingVersion uint64                   `json:"requester_binding_version"`
	RecordID                *RecordID                `json:"record_id"`
	Author                  *SeatKeyBinding          `json:"author"`
	Verifier                *SeatKeyBinding          `json:"verifier"`
	Purpose                 string                   `json:"purpose"`
	AfterRecordID           *RecordID                `json:"after_record_id"`
	MaxRecords              uint32                   `json:"max_records"`
	IssuedAt                time.Time                `json:"issued_at"`
	ExpiresAt               time.Time                `json:"expires_at"`
	Signature               contracts.Signature      `json:"signature"`
}

// Validate enforces exact read/write authority with no caller-mintable widening.
func (grant CapabilityGrant) Validate() error {
	if grant.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("project brain capability schema is unsupported")
	}
	for name, value := range map[string]string{
		"grant_id": grant.ID, "tenant_id": grant.TenantID,
		"organization_id": string(grant.OrganizationID),
		"project_id":      string(grant.ProjectID), "workspace_id": string(grant.WorkspaceID),
		"requester_seat_id":    string(grant.RequesterSeatID),
		"requester_seat_did":   string(grant.RequesterSeatDID),
		"requester_binding_id": string(grant.RequesterBindingID),
	} {
		if err := validateToken(name, value); err != nil {
			return err
		}
		if grant.RequesterSeatVersion == 0 || grant.RequesterBindingVersion == 0 {
			return fmt.Errorf("project brain requester seat versions must be positive")
		}
	}
	if !filepath.IsAbs(grant.WorkspaceRoot) || len(grant.WorkspaceRoot) > 4096 {
		return fmt.Errorf("project brain capability workspace root must be absolute")
	}
	if err := validateFilter(grant.Filter); err != nil {
		return err
	}
	if strings.TrimSpace(grant.Purpose) == "" || len(grant.Purpose) > 512 {
		return fmt.Errorf("project brain capability purpose is required")
	}
	if grant.IssuedAt.IsZero() || grant.IssuedAt.Location() != time.UTC ||
		grant.ExpiresAt.IsZero() || grant.ExpiresAt.Location() != time.UTC ||
		!grant.ExpiresAt.After(grant.IssuedAt) {
		return fmt.Errorf("project brain capability times are invalid")
	}
	switch grant.Operation {
	case CapabilityRead:
		if grant.RecordID != nil || grant.Author != nil || grant.Verifier != nil ||
			grant.MaxRecords == 0 || grant.MaxRecords > 1024 {
			return fmt.Errorf("project brain read capability is malformed")
		}
		if grant.AfterRecordID != nil {
			if err := validateToken("after_record_id", string(*grant.AfterRecordID)); err != nil {
				return err
			}
		}
	case CapabilityWrite:
		if grant.RecordID == nil || grant.Author == nil || grant.Verifier == nil ||
			grant.AfterRecordID != nil || grant.MaxRecords != 0 {
			return fmt.Errorf("project brain write capability is malformed")
		}
		if err := validateToken("record_id", string(*grant.RecordID)); err != nil {
			return err
		}
		if err := grant.Author.validate(); err != nil {
			return err
		}
		if err := grant.Verifier.validate(); err != nil {
			return err
		}
		if grant.Author.SeatID == grant.Verifier.SeatID {
			return fmt.Errorf("project brain author and verifier capability seats must differ")
		}
	case CapabilityChangeScope:
		if grant.RecordID != nil || grant.Author != nil || grant.Verifier != nil ||
			grant.AfterRecordID != nil || grant.MaxRecords != 0 ||
			grant.Filter != "" {
			return fmt.Errorf("project brain change-scope capability is malformed")
		}
	default:
		return fmt.Errorf("project brain capability operation is invalid")
	}
	return grant.Signature.Validate()
}

// SignProposal signs the canonical proposal fields using real Ed25519 authority.
func SignProposal(proposal *Proposal, keyID string, privateKey ed25519.PrivateKey) error {
	if proposal == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("project brain proposal signing authority is invalid")
	}
	proposal.Signature = placeholderSignature(keyID)
	payload, err := proposalSigningBytes(*proposal)
	if err != nil {
		return err
	}
	proposal.Signature = signatureFor(payload, keyID, privateKey)
	return nil
}

// ProposalHash returns the canonical identity independently signed by the
// verifier for one exact Developer Project Brain proposal.
func ProposalHash(proposal Proposal) (contracts.ContentHash, error) {
	if err := proposal.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	return proposalHash(proposal)
}

// SignVerification signs the canonical verification fields using real Ed25519 authority.
func SignVerification(verification *Verification, keyID string, privateKey ed25519.PrivateKey) error {
	if verification == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("project brain verification signing authority is invalid")
	}
	verification.Signature = placeholderSignature(keyID)
	payload, err := verificationSigningBytes(*verification)
	if err != nil {
		return err
	}
	verification.Signature = signatureFor(payload, keyID, privateKey)
	return nil
}

// SignCapabilityGrant signs an exact capability using kernel authority.
func SignCapabilityGrant(
	grant *CapabilityGrant,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if grant == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("project brain capability signing authority is invalid")
	}
	grant.Signature = contracts.Signature{Algorithm: "ed25519", KeyID: keyID}
	payload, err := grantSigningBytes(*grant)
	if err != nil {
		return err
	}
	grant.Signature = signatureFor(payload, keyID, privateKey)
	return nil
}

func verifyRecordSignatures(
	record EngineeringRecord,
	authorKey, verifierKey ed25519.PublicKey,
) error {
	if len(authorKey) != ed25519.PublicKeySize || len(verifierKey) != ed25519.PublicKeySize {
		return fmt.Errorf("project brain verification keys are invalid")
	}
	proposalBytes, err := proposalSigningBytes(record.Proposal)
	if err != nil {
		return err
	}
	proposalSignature, err := base64.RawURLEncoding.DecodeString(record.Proposal.Signature.Value)
	if err != nil || !ed25519.Verify(authorKey, proposalBytes, proposalSignature) {
		return fmt.Errorf("project brain proposal signature is invalid")
	}
	verificationBytes, err := verificationSigningBytes(record.Verification)
	if err != nil {
		return err
	}
	verificationSignature, err := base64.RawURLEncoding.DecodeString(record.Verification.Signature.Value)
	if err != nil || !ed25519.Verify(verifierKey, verificationBytes, verificationSignature) {
		return fmt.Errorf("project brain verification signature is invalid")
	}
	return nil
}

func proposalHash(proposal Proposal) (contracts.ContentHash, error) {
	payload, err := proposalSigningBytes(proposal)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(payload)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func proposalSigningBytes(proposal Proposal) ([]byte, error) {
	payload := proposalSigningPayload{Proposal: proposal, KeyID: proposal.Signature.KeyID}
	payload.Proposal.Signature = placeholderSignature(proposal.Signature.KeyID)
	return contracts.EncodeCanonical(&payload)
}

func verificationSigningBytes(verification Verification) ([]byte, error) {
	payload := verificationSigningPayload{Verification: verification, KeyID: verification.Signature.KeyID}
	payload.Verification.Signature = placeholderSignature(verification.Signature.KeyID)
	return contracts.EncodeCanonical(&payload)
}

type proposalSigningPayload struct {
	Proposal Proposal `json:"proposal"`
	KeyID    string   `json:"key_id"`
}

func (payload proposalSigningPayload) Validate() error {
	if payload.Proposal.Signature.KeyID != payload.KeyID {
		return fmt.Errorf("project brain proposal signing key mismatch")
	}
	return payload.Proposal.Validate()
}

type verificationSigningPayload struct {
	Verification Verification `json:"verification"`
	KeyID        string       `json:"key_id"`
}

func (payload verificationSigningPayload) Validate() error {
	if payload.Verification.Signature.KeyID != payload.KeyID {
		return fmt.Errorf("project brain verification signing key mismatch")
	}
	return payload.Verification.Validate()
}

type grantSigningPayload struct {
	Grant CapabilityGrant `json:"grant"`
	KeyID string          `json:"key_id"`
}

func (payload grantSigningPayload) Validate() error {
	if payload.Grant.Signature.KeyID != payload.KeyID {
		return fmt.Errorf("project brain capability signing key mismatch")
	}
	return payload.Grant.Validate()
}

func grantSigningBytes(grant CapabilityGrant) ([]byte, error) {
	payload := grantSigningPayload{Grant: grant, KeyID: grant.Signature.KeyID}
	payload.Grant.Signature = placeholderSignature(grant.Signature.KeyID)
	return contracts.EncodeCanonical(&payload)
}

func placeholderSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func signatureFor(payload []byte, keyID string, privateKey ed25519.PrivateKey) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
}

func digestBytes(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("project brain %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("project brain %s contains an invalid character", name)
	}
	return nil
}

func validateRelativePath(path string) error {
	if strings.TrimSpace(path) == "" || len(path) > 4096 || filepath.IsAbs(path) ||
		strings.HasPrefix(path, "-") ||
		strings.IndexFunc(path, func(character rune) bool {
			return character == 0 || character < 0x20 || character == 0x7f
		}) >= 0 {
		return fmt.Errorf("project brain path must be a bounded relative path")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("project brain path escapes its workspace")
	}
	return nil
}

func sortRecords(records []EngineeringRecord) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].Proposal.Kind != records[right].Proposal.Kind {
			return records[left].Proposal.Kind < records[right].Proposal.Kind
		}
		if records[left].Proposal.Version != records[right].Proposal.Version {
			return records[left].Proposal.Version < records[right].Proposal.Version
		}
		return records[left].Proposal.ID < records[right].Proposal.ID
	})
}
