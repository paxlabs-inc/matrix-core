package contracts

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DeveloperGraphFile is one current source file exposed to a memoryless
// Developer Auditor.
type DeveloperGraphFile struct {
	Path string      `json:"path"`
	Hash ContentHash `json:"hash"`
}

// DeveloperChangedFile binds exact before and after source bytes.
type DeveloperChangedFile struct {
	Path       string      `json:"path"`
	BeforeHash ContentHash `json:"before_hash"`
	AfterHash  ContentHash `json:"after_hash"`
}

// DeveloperImpactNode is one source-grounded CodeGraph blast-radius node.
type DeveloperImpactNode struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	StartLine uint32 `json:"start_line"`
}

// DeveloperInvariantRef identifies one current independently verified
// Project Brain invariant without carrying a writable memory-store handle.
type DeveloperInvariantRef struct {
	RecordID        string      `json:"record_id"`
	ProjectID       ProjectID   `json:"project_id"`
	WorkspaceID     WorkspaceID `json:"workspace_id"`
	SourceRoot      ContentHash `json:"source_root"`
	GraphGeneration uint64      `json:"graph_generation"`
	RecordHash      ContentHash `json:"record_hash"`
	VerifiedAt      time.Time   `json:"verified_at"`
	ExpiresAt       *time.Time  `json:"expires_at"`
}

// DeveloperAuditEvidence is the kernel-signed closed Developer extension to a
// VerdictPacket. The signature prevents a caller from injecting unrelated
// invariants, graph nodes, or changed-source claims.
type DeveloperAuditEvidence struct {
	SchemaVersion   string                  `json:"schema_version"`
	OrganizationID  OrganizationID          `json:"organization_id"`
	ProjectID       ProjectID               `json:"project_id"`
	WorkspaceID     WorkspaceID             `json:"workspace_id"`
	SourceRoot      ContentHash             `json:"source_root"`
	GraphDigest     ContentHash             `json:"graph_digest"`
	ViewDigest      ContentHash             `json:"view_digest"`
	GraphGeneration uint64                  `json:"graph_generation"`
	GraphFiles      []DeveloperGraphFile    `json:"graph_files"`
	Invariants      []DeveloperInvariantRef `json:"invariants"`
	ChangedSource   []DeveloperChangedFile  `json:"changed_source"`
	BlastRadius     []DeveloperImpactNode   `json:"blast_radius"`
	TestEvidence    []EvidenceRef           `json:"test_evidence"`
	AssembledAt     time.Time               `json:"assembled_at"`
	Signature       Signature               `json:"signature"`
}

// Validate enforces the closed, source-grounded Developer Auditor packet.
func (evidence DeveloperAuditEvidence) Validate() error {
	if err := evidence.validateUnsigned(); err != nil {
		return err
	}
	return evidence.Signature.Validate()
}

func (evidence DeveloperAuditEvidence) validateUnsigned() error {
	if evidence.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("developer audit evidence schema is unsupported")
	}
	for name, value := range map[string]string{
		"organization_id": string(evidence.OrganizationID),
		"project_id":      string(evidence.ProjectID),
		"workspace_id":    string(evidence.WorkspaceID),
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := evidence.SourceRoot.Validate(); err != nil {
		return err
	}
	if err := evidence.GraphDigest.Validate(); err != nil {
		return err
	}
	if err := evidence.ViewDigest.Validate(); err != nil {
		return err
	}
	if evidence.GraphGeneration == 0 ||
		evidence.AssembledAt.IsZero() || evidence.AssembledAt.Location() != time.UTC {
		return fmt.Errorf("developer audit graph generation and assembly time are required")
	}
	if len(evidence.GraphFiles) == 0 || len(evidence.GraphFiles) > 10000 ||
		len(evidence.ChangedSource) == 0 || len(evidence.ChangedSource) > 256 ||
		len(evidence.BlastRadius) == 0 || len(evidence.BlastRadius) > 10000 ||
		len(evidence.TestEvidence) == 0 || len(evidence.TestEvidence) > 256 ||
		len(evidence.Invariants) > 256 {
		return fmt.Errorf("developer audit evidence is outside bounds")
	}
	files := make(map[string]ContentHash, len(evidence.GraphFiles))
	for _, file := range evidence.GraphFiles {
		path := filepath.ToSlash(file.Path)
		if err := developerRelativePath(path); err != nil {
			return err
		}
		if err := file.Hash.Validate(); err != nil {
			return err
		}
		if _, exists := files[path]; exists {
			return fmt.Errorf("developer audit graph file is duplicated")
		}
		files[path] = file.Hash
	}
	changed := make(map[string]bool, len(evidence.ChangedSource))
	for _, file := range evidence.ChangedSource {
		path := filepath.ToSlash(file.Path)
		if err := developerRelativePath(path); err != nil {
			return err
		}
		if err := file.BeforeHash.Validate(); err != nil {
			return err
		}
		if err := file.AfterHash.Validate(); err != nil {
			return err
		}
		current, exists := files[path]
		if !exists || current != file.AfterHash ||
			file.BeforeHash == file.AfterHash || changed[path] {
			return fmt.Errorf("developer audit changed source is not current and unique")
		}
		changed[path] = true
	}
	for _, node := range evidence.BlastRadius {
		path := filepath.ToSlash(node.FilePath)
		if strings.TrimSpace(node.Name) == "" || strings.TrimSpace(node.Kind) == "" ||
			node.StartLine == 0 {
			return fmt.Errorf("developer audit blast radius node is invalid")
		}
		if err := developerRelativePath(path); err != nil {
			return err
		}
		if _, exists := files[path]; !exists {
			return fmt.Errorf("developer audit blast radius is outside graph source")
		}
	}
	for _, invariant := range evidence.Invariants {
		if err := validateID("invariant record_id", invariant.RecordID); err != nil {
			return err
		}
		if invariant.ProjectID != evidence.ProjectID ||
			invariant.WorkspaceID != evidence.WorkspaceID ||
			invariant.SourceRoot != evidence.SourceRoot ||
			invariant.GraphGeneration != evidence.GraphGeneration {
			return fmt.Errorf("developer audit invariant is outside packet scope")
		}
		if err := invariant.RecordHash.Validate(); err != nil {
			return err
		}
		if invariant.VerifiedAt.IsZero() ||
			invariant.VerifiedAt.Location() != time.UTC ||
			invariant.VerifiedAt.After(evidence.AssembledAt) ||
			invariant.ExpiresAt != nil && !invariant.ExpiresAt.After(evidence.AssembledAt) {
			return fmt.Errorf("developer audit invariant is stale or chronologically invalid")
		}
	}
	for _, item := range evidence.TestEvidence {
		if err := item.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// SignDeveloperAuditEvidence binds the exact closed Developer evidence to the
// kernel authority used by the real Auditor process boundary.
func SignDeveloperAuditEvidence(
	evidence *DeveloperAuditEvidence,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if evidence == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("developer audit signing authority is invalid")
	}
	if err := validateID("developer audit key_id", keyID); err != nil {
		return err
	}
	if err := evidence.validateUnsigned(); err != nil {
		return err
	}
	payload, err := developerAuditSigningBytes(*evidence)
	if err != nil {
		return err
	}
	evidence.Signature = Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return evidence.Validate()
}

// VerifyDeveloperAuditEvidence verifies the trusted kernel signature.
func VerifyDeveloperAuditEvidence(
	evidence DeveloperAuditEvidence,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.Signature.KeyID != keyID || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("developer audit authority does not match")
	}
	signature, err := base64.RawURLEncoding.DecodeString(evidence.Signature.Value)
	if err != nil {
		return err
	}
	payload, err := developerAuditSigningBytes(evidence)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("developer audit signature is invalid")
	}
	return nil
}

type developerAuditSigningPayload DeveloperAuditEvidence

func (payload developerAuditSigningPayload) Validate() error {
	value := DeveloperAuditEvidence(payload)
	value.Signature = Signature{}
	return value.validateUnsigned()
}

func developerAuditSigningBytes(evidence DeveloperAuditEvidence) ([]byte, error) {
	evidence.Signature = Signature{}
	payload := developerAuditSigningPayload(evidence)
	return EncodeCanonical(&payload)
}

func developerRelativePath(value string) error {
	if strings.TrimSpace(value) == "" || filepath.IsAbs(value) || len(value) > 4096 {
		return fmt.Errorf("developer audit path must be bounded and relative")
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("developer audit path escapes workspace")
	}
	return nil
}
