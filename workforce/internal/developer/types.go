// Package developer owns Developer-only scoped change authority, skill packs,
// and the closed memoryless Auditor handoff.
package developer

import (
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/projectbrain"
)

// ScopeRequest declares the exact project change requested by one fresh wake.
type ScopeRequest struct {
	SchemaVersion      string                        `json:"schema_version"`
	ProjectID          contracts.ProjectID           `json:"project_id"`
	WorkspaceID        contracts.WorkspaceID         `json:"workspace_id"`
	TaskNodeID         dependency.NodeID             `json:"task_node_id"`
	WorkspaceRoot      string                        `json:"workspace_root"`
	Files              []string                      `json:"files"`
	Symbols            []string                      `json:"symbols"`
	CoordinationPlanID *projectbrain.RecordID        `json:"coordination_plan_id"`
	CoordinationGrant  *projectbrain.CapabilityGrant `json:"coordination_grant"`
	Capability         projectbrain.CapabilityGrant  `json:"capability"`
}

// Validate rejects ambient, unbounded, duplicate, or coordination-free scope.
func (scope ScopeRequest) Validate() error {
	if scope.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("developer scope schema is unsupported")
	}
	for name, value := range map[string]string{
		"project_id": string(scope.ProjectID), "workspace_id": string(scope.WorkspaceID),
		"task_node_id": string(scope.TaskNodeID),
	} {
		if err := token(name, value); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(scope.WorkspaceRoot) || len(scope.WorkspaceRoot) > 4096 {
		return fmt.Errorf("developer scope workspace root must be absolute")
	}
	if len(scope.Files) == 0 || len(scope.Files) > 256 ||
		len(scope.Symbols) == 0 || len(scope.Symbols) > 256 {
		return fmt.Errorf("developer scope requires 1 to 256 files and symbols")
	}
	if err := uniqueBounded("file", scope.Files, true); err != nil {
		return err
	}
	if err := uniqueBounded("symbol", scope.Symbols, false); err != nil {
		return err
	}
	if scope.CoordinationPlanID != nil {
		if err := token("coordination_plan_id", string(*scope.CoordinationPlanID)); err != nil {
			return err
		}
		if scope.CoordinationGrant == nil {
			return fmt.Errorf("developer coordination plan requires a read capability")
		}
		if err := scope.CoordinationGrant.Validate(); err != nil {
			return fmt.Errorf("developer coordination capability: %w", err)
		}
		if scope.CoordinationGrant.Operation != projectbrain.CapabilityRead ||
			scope.CoordinationGrant.ProjectID != scope.ProjectID ||
			scope.CoordinationGrant.WorkspaceID != scope.WorkspaceID ||
			scope.CoordinationGrant.WorkspaceRoot != scope.WorkspaceRoot ||
			scope.CoordinationGrant.Purpose !=
				"developer_coordination:"+string(*scope.CoordinationPlanID) {
			return fmt.Errorf("developer coordination capability does not bind the plan")
		}
	} else if scope.CoordinationGrant != nil {
		return fmt.Errorf("developer coordination capability requires a plan")
	}
	if err := scope.Capability.Validate(); err != nil {
		return fmt.Errorf("developer scope capability: %w", err)
	}
	if scope.Capability.Operation != projectbrain.CapabilityChangeScope ||
		scope.Capability.ProjectID != scope.ProjectID ||
		scope.Capability.WorkspaceID != scope.WorkspaceID ||
		scope.Capability.WorkspaceRoot != scope.WorkspaceRoot ||
		scope.Capability.Purpose != "developer_change_scope:"+string(scope.TaskNodeID) {
		return fmt.Errorf("developer scope capability does not bind the requested scope")
	}
	return nil
}

// ResolvedScope binds declared work to current source and real CodeGraph impact.
type ResolvedScope struct {
	SchemaVersion      string                        `json:"schema_version"`
	ProjectID          contracts.ProjectID           `json:"project_id"`
	WorkspaceID        contracts.WorkspaceID         `json:"workspace_id"`
	TaskNodeID         dependency.NodeID             `json:"task_node_id"`
	WorkspaceRoot      string                        `json:"workspace_root"`
	Source             projectbrain.GraphSnapshot    `json:"source"`
	Files              []projectbrain.GraphFile      `json:"files"`
	Symbols            []string                      `json:"symbols"`
	BlastRadius        []projectbrain.ImpactNode     `json:"blast_radius"`
	AffectedTests      []string                      `json:"affected_tests"`
	CoordinationPlanID *projectbrain.RecordID        `json:"coordination_plan_id"`
	Coordination       *CoordinationPlan             `json:"coordination"`
	CoordinationGrant  *projectbrain.CapabilityGrant `json:"coordination_grant"`
	Capability         projectbrain.CapabilityGrant  `json:"capability"`
	ResolvedAt         time.Time                     `json:"resolved_at"`
}

// Validate enforces a bounded, content-addressed current change projection.
func (scope ResolvedScope) Validate() error {
	if scope.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("developer resolved scope schema is unsupported")
	}
	if err := token("project_id", string(scope.ProjectID)); err != nil {
		return err
	}
	if err := token("workspace_id", string(scope.WorkspaceID)); err != nil {
		return err
	}
	if err := token("task_node_id", string(scope.TaskNodeID)); err != nil {
		return err
	}
	if !filepath.IsAbs(scope.WorkspaceRoot) || len(scope.WorkspaceRoot) > 4096 {
		return fmt.Errorf("developer resolved scope workspace root must be absolute")
	}
	if err := scope.Capability.Validate(); err != nil {
		return fmt.Errorf("developer resolved scope capability: %w", err)
	}
	if scope.Capability.Operation != projectbrain.CapabilityChangeScope ||
		scope.Capability.ProjectID != scope.ProjectID ||
		scope.Capability.WorkspaceID != scope.WorkspaceID ||
		scope.Capability.WorkspaceRoot != scope.WorkspaceRoot ||
		scope.Capability.Purpose != "developer_change_scope:"+string(scope.TaskNodeID) {
		return fmt.Errorf("developer resolved scope capability does not bind the scope")
	}
	if scope.CoordinationPlanID == nil {
		if scope.Coordination != nil || scope.CoordinationGrant != nil {
			return fmt.Errorf("developer resolved coordination state is inconsistent")
		}
	} else {
		if scope.Coordination == nil || scope.CoordinationGrant == nil ||
			scope.Coordination.RecordID != *scope.CoordinationPlanID {
			return fmt.Errorf("developer resolved coordination plan is incomplete")
		}
		if err := scope.Coordination.Validate(); err != nil {
			return err
		}
	}
	if err := scope.Source.Validate(); err != nil {
		return err
	}
	if len(scope.Files) == 0 || len(scope.Files) > 256 ||
		len(scope.Symbols) == 0 || len(scope.Symbols) > 256 ||
		len(scope.BlastRadius) == 0 || len(scope.BlastRadius) > 10000 ||
		len(scope.AffectedTests) > 10000 {
		return fmt.Errorf("developer resolved scope projection is outside bounds")
	}
	sourceFiles := make(map[string]projectbrain.GraphFile, len(scope.Source.Files))
	for _, file := range scope.Source.Files {
		sourceFiles[file.Path] = file
	}
	for _, file := range scope.Files {
		current, exists := sourceFiles[file.Path]
		if !exists || current.Hash != file.Hash || !file.Indexed {
			return fmt.Errorf("developer scope file is absent, stale, or unindexed")
		}
	}
	if err := uniqueBounded("symbol", scope.Symbols, false); err != nil {
		return err
	}
	for _, node := range scope.BlastRadius {
		if strings.TrimSpace(node.Name) == "" || strings.TrimSpace(node.FilePath) == "" ||
			node.StartLine == 0 {
			return fmt.Errorf("developer blast radius contains an invalid node")
		}
		if err := relativePath(filepath.ToSlash(node.FilePath)); err != nil {
			return fmt.Errorf("developer blast radius path: %w", err)
		}
		if _, exists := sourceFiles[filepath.ToSlash(node.FilePath)]; !exists {
			return fmt.Errorf("developer blast radius is outside the source snapshot")
		}
	}
	for _, test := range scope.AffectedTests {
		if err := relativePath(filepath.ToSlash(test)); err != nil {
			return fmt.Errorf("developer affected test path: %w", err)
		}
		if _, exists := sourceFiles[filepath.ToSlash(test)]; !exists {
			return fmt.Errorf("developer affected test is outside the source snapshot")
		}
	}
	if scope.ResolvedAt.Location() != time.UTC || scope.ResolvedAt.IsZero() ||
		scope.ResolvedAt.Before(scope.Source.CapturedAt) {
		return fmt.Errorf("developer scope resolution time is invalid")
	}
	if scope.Coordination != nil && scope.Coordination.ExpiresAt != nil &&
		!scope.Coordination.ExpiresAt.After(scope.ResolvedAt) {
		return fmt.Errorf("developer coordination plan is expired")
	}
	return nil
}

// CoordinationPlan is the exact current Project Brain plan projection allowed
// to coordinate otherwise-overlapping Developer scopes.
type CoordinationPlan struct {
	RecordID  projectbrain.RecordID `json:"record_id"`
	Digest    contracts.ContentHash `json:"digest"`
	Tasks     []dependency.NodeID   `json:"tasks"`
	Seats     []contracts.SeatID    `json:"seats"`
	Files     []string              `json:"files"`
	ExpiresAt *time.Time            `json:"expires_at"`
}

// Validate requires explicit bounded participants and source resources.
func (plan CoordinationPlan) Validate() error {
	if err := token("coordination_record_id", string(plan.RecordID)); err != nil {
		return err
	}
	if err := plan.Digest.Validate(); err != nil {
		return err
	}
	if len(plan.Tasks) < 2 || len(plan.Tasks) > 64 ||
		len(plan.Seats) < 2 || len(plan.Seats) > 64 ||
		len(plan.Files) == 0 || len(plan.Files) > 256 {
		return fmt.Errorf("developer coordination plan participants are outside bounds")
	}
	taskValues := make([]string, len(plan.Tasks))
	for index, task := range plan.Tasks {
		taskValues[index] = string(task)
	}
	if err := uniqueBounded("coordination_task", taskValues, false); err != nil {
		return err
	}
	seatValues := make([]string, len(plan.Seats))
	for index, seat := range plan.Seats {
		seatValues[index] = string(seat)
	}
	if err := uniqueBounded("coordination_seat", seatValues, false); err != nil {
		return err
	}
	if err := uniqueBounded("coordination_file", plan.Files, true); err != nil {
		return err
	}
	if plan.ExpiresAt != nil &&
		(plan.ExpiresAt.IsZero() || plan.ExpiresAt.Location() != time.UTC) {
		return fmt.Errorf("developer coordination expiry is invalid")
	}
	return nil
}

// Grant combines the generic fenced wake lease with its Developer-only change scope.
type Grant struct {
	Lease lease.Grant   `json:"lease"`
	Scope ResolvedScope `json:"scope"`
}

// SourceChange is one exact before-hash-bound source replacement submitted to
// the fenced Developer effect boundary.
type SourceChange struct {
	Path       string                `json:"path"`
	BeforeHash contracts.ContentHash `json:"before_hash"`
	Content    []byte                `json:"content"`
}

// Validate rejects unscoped or oversized source mutation material.
func (change SourceChange) Validate() error {
	if err := relativePath(change.Path); err != nil {
		return err
	}
	if err := change.BeforeHash.Validate(); err != nil {
		return fmt.Errorf("developer source change before hash: %w", err)
	}
	if len(change.Content) == 0 || len(change.Content) > 32<<20 {
		return fmt.Errorf("developer source change content is outside bounds")
	}
	return nil
}

// ChangedFile is current source evidence delivered to a memoryless Auditor.
type ChangedFile struct {
	Path       string                `json:"path"`
	BeforeHash contracts.ContentHash `json:"before_hash"`
	AfterHash  contracts.ContentHash `json:"after_hash"`
}

// AuditPacket is the closed Developer Auditor input. It deliberately has no
// Project Brain store handle, session transcript, or prior verdict state.
type AuditPacket struct {
	SchemaVersion string                             `json:"schema_version"`
	Intent        contracts.Intent                   `json:"intent"`
	ProjectID     contracts.ProjectID                `json:"project_id"`
	WorkspaceID   contracts.WorkspaceID              `json:"workspace_id"`
	ViewDigest    contracts.ContentHash              `json:"view_digest"`
	Invariants    []projectbrain.EngineeringRecord   `json:"invariants"`
	Graph         projectbrain.GraphSnapshot         `json:"graph"`
	ChangedSource []ChangedFile                      `json:"changed_source"`
	BlastRadius   []projectbrain.ImpactNode          `json:"blast_radius"`
	TestEvidence  []contracts.EvidenceRef            `json:"test_evidence"`
	Verifier      contracts.VerificationProcedureRef `json:"verifier"`
	AssembledAt   time.Time                          `json:"assembled_at"`
}

// Validate enforces the exact memoryless Auditor boundary.
func (packet AuditPacket) Validate() error {
	if packet.SchemaVersion != contracts.SchemaVersionV1 {
		return fmt.Errorf("developer audit packet schema is unsupported")
	}
	if err := packet.Intent.Validate(); err != nil {
		return err
	}
	if err := packet.Graph.Validate(); err != nil {
		return err
	}
	if !packet.Graph.Fresh {
		return fmt.Errorf("developer audit packet requires a fresh graph")
	}
	if err := token("project_id", string(packet.ProjectID)); err != nil {
		return err
	}
	if err := token("workspace_id", string(packet.WorkspaceID)); err != nil {
		return err
	}
	if err := packet.ViewDigest.Validate(); err != nil {
		return err
	}
	if packet.AssembledAt.IsZero() || packet.AssembledAt.Location() != time.UTC ||
		packet.AssembledAt.Before(packet.Graph.CapturedAt) {
		return fmt.Errorf("developer audit packet assembly time is invalid")
	}
	if len(packet.ChangedSource) == 0 || len(packet.ChangedSource) > 256 ||
		len(packet.BlastRadius) == 0 || len(packet.BlastRadius) > 10000 ||
		len(packet.TestEvidence) == 0 || len(packet.TestEvidence) > 256 ||
		len(packet.Invariants) > 256 {
		return fmt.Errorf("developer audit packet evidence is outside bounds")
	}
	for _, invariant := range packet.Invariants {
		if invariant.Proposal.Kind != projectbrain.KindInvariant {
			return fmt.Errorf("developer audit packet accepts verified invariants only")
		}
		if err := invariant.Validate(); err != nil {
			return err
		}
		if invariant.Proposal.OrganizationID != packet.Intent.OrganizationID ||
			invariant.Proposal.ProjectID != packet.ProjectID ||
			invariant.Proposal.WorkspaceID != packet.WorkspaceID ||
			invariant.Proposal.Source.RootDigest != packet.Graph.RootDigest ||
			invariant.Proposal.Source.Generation != packet.Graph.Generation ||
			invariant.Verification.VerifiedAt.After(packet.AssembledAt) ||
			invariant.ContentExpired(packet.AssembledAt) {
			return fmt.Errorf("developer audit invariant is unrelated, stale, or not current")
		}
	}
	graphFiles := make(map[string]projectbrain.GraphFile, len(packet.Graph.Files))
	for _, file := range packet.Graph.Files {
		graphFiles[file.Path] = file
	}
	changedPaths := make(map[string]bool, len(packet.ChangedSource))
	for _, changed := range packet.ChangedSource {
		if err := relativePath(changed.Path); err != nil {
			return err
		}
		if err := changed.BeforeHash.Validate(); err != nil {
			return err
		}
		if err := changed.AfterHash.Validate(); err != nil {
			return err
		}
		if changed.BeforeHash == changed.AfterHash {
			return fmt.Errorf("developer changed source must prove a byte change")
		}
		path := filepath.ToSlash(changed.Path)
		current, exists := graphFiles[path]
		if !exists || current.Hash != changed.AfterHash || changedPaths[path] {
			return fmt.Errorf("developer changed source is not current and unique")
		}
		changedPaths[path] = true
	}
	for _, node := range packet.BlastRadius {
		path := filepath.ToSlash(node.FilePath)
		if strings.TrimSpace(node.Name) == "" || strings.TrimSpace(node.Kind) == "" ||
			node.StartLine == 0 {
			return fmt.Errorf("developer audit blast radius contains an invalid node")
		}
		if err := relativePath(path); err != nil {
			return err
		}
		if _, exists := graphFiles[path]; !exists {
			return fmt.Errorf("developer audit blast radius is outside current source")
		}
	}
	for _, evidence := range packet.TestEvidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return packet.Verifier.Validate()
}

// Digest returns the canonical closed-packet identity without retaining state.
func (packet AuditPacket) Digest() (contracts.ContentHash, error) {
	if err := packet.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	return contracts.HashCanonical(&packet)
}

// AttachToVerdict binds this Project Brain and source projection into the real
// VerdictPacket consumed by workforce-auditor.
func (packet AuditPacket) AttachToVerdict(
	verdict contracts.VerdictPacket,
	keyID string,
	privateKey ed25519.PrivateKey,
) (contracts.VerdictPacket, error) {
	if err := packet.Validate(); err != nil {
		return contracts.VerdictPacket{}, err
	}
	if verdict.Intent != packet.Intent ||
		verdict.OrganizationID != packet.Intent.OrganizationID ||
		verdict.Source.RootDigest != packet.Graph.RootDigest ||
		verdict.Source.GraphGeneration != packet.Graph.Generation ||
		verdict.Procedure != packet.Verifier {
		return contracts.VerdictPacket{}, fmt.Errorf(
			"developer audit packet does not match verdict identity",
		)
	}
	evidence := contracts.DeveloperAuditEvidence{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: packet.Intent.OrganizationID,
		ProjectID:      packet.ProjectID, WorkspaceID: packet.WorkspaceID,
		SourceRoot: packet.Graph.RootDigest, GraphDigest: packet.Graph.GraphDigest,
		ViewDigest: packet.ViewDigest, GraphGeneration: packet.Graph.Generation,
		TestEvidence: append([]contracts.EvidenceRef(nil), packet.TestEvidence...),
		AssembledAt:  packet.AssembledAt,
	}
	for _, file := range packet.Graph.Files {
		evidence.GraphFiles = append(evidence.GraphFiles, contracts.DeveloperGraphFile{
			Path: file.Path, Hash: file.Hash,
		})
	}
	for _, file := range packet.ChangedSource {
		evidence.ChangedSource = append(
			evidence.ChangedSource, contracts.DeveloperChangedFile{
				Path: file.Path, BeforeHash: file.BeforeHash, AfterHash: file.AfterHash,
			},
		)
	}
	for _, node := range packet.BlastRadius {
		evidence.BlastRadius = append(
			evidence.BlastRadius, contracts.DeveloperImpactNode{
				Name: node.Name, Kind: node.Kind,
				FilePath: node.FilePath, StartLine: node.StartLine,
			},
		)
	}
	for _, invariant := range packet.Invariants {
		recordHash, err := contracts.HashCanonical(&invariant)
		if err != nil {
			return contracts.VerdictPacket{}, err
		}
		evidence.Invariants = append(
			evidence.Invariants, contracts.DeveloperInvariantRef{
				RecordID:  string(invariant.Proposal.ID),
				ProjectID: packet.ProjectID, WorkspaceID: packet.WorkspaceID,
				SourceRoot:      packet.Graph.RootDigest,
				GraphGeneration: packet.Graph.Generation,
				RecordHash:      recordHash,
				VerifiedAt:      invariant.Verification.VerifiedAt,
				ExpiresAt:       invariant.Proposal.Content.ExpiresAt,
			},
		)
	}
	if err := contracts.SignDeveloperAuditEvidence(
		&evidence, keyID, privateKey,
	); err != nil {
		return contracts.VerdictPacket{}, err
	}
	verdict.Developer = &evidence
	if err := verdict.Validate(); err != nil {
		return contracts.VerdictPacket{}, err
	}
	return verdict, nil
}

func uniqueBounded(name string, values []string, paths bool) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if paths {
			if err := relativePath(value); err != nil {
				return err
			}
		} else if err := token(name, value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("developer %s claims contain duplicates", name)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func relativePath(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 4096 || filepath.IsAbs(value) {
		return fmt.Errorf("developer file claim must be a bounded relative path")
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("developer file claim escapes the workspace")
	}
	return nil
}

func token(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return fmt.Errorf("developer %s is empty or oversized", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("-_.:/#", char) {
			continue
		}
		return fmt.Errorf("developer %s contains an invalid character", name)
	}
	return nil
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	write := 0
	for _, value := range result {
		if write == 0 || result[write-1] != value {
			result[write] = value
			write++
		}
	}
	return result[:write]
}
