package contracts

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// DepartmentKind is one of the seven owner-defined Workforce departments.
type DepartmentKind string

// DepartmentKind values are closed and exhaustive.
const (
	// DepartmentDeveloper owns bounded software planning and implementation.
	DepartmentDeveloper DepartmentKind = "developer"
	// DepartmentExecutive owns portfolio synthesis without approval authority.
	DepartmentExecutive DepartmentKind = "executive"
	// DepartmentResearch owns experiments and evidence production.
	DepartmentResearch DepartmentKind = "research_and_development"
	// DepartmentMarketing owns bounded campaign and publication proposals.
	DepartmentMarketing DepartmentKind = "marketing_and_social"
	// DepartmentLegal owns legal procedure and human-signoff escalation.
	DepartmentLegal DepartmentKind = "legal"
	// DepartmentAccounting owns books, reconciliation, and payment proposals.
	DepartmentAccounting DepartmentKind = "accounting"
	// DepartmentBackOffice owns administrative coordination and records.
	DepartmentBackOffice DepartmentKind = "back_office"
)

// AllDepartmentKinds returns the canonical seven-department order.
func AllDepartmentKinds() []DepartmentKind {
	return []DepartmentKind{
		DepartmentDeveloper,
		DepartmentExecutive,
		DepartmentResearch,
		DepartmentMarketing,
		DepartmentLegal,
		DepartmentAccounting,
		DepartmentBackOffice,
	}
}

// Valid reports whether the department is executable by this release.
func (v DepartmentKind) Valid() bool {
	switch v {
	case DepartmentDeveloper, DepartmentExecutive, DepartmentResearch,
		DepartmentMarketing, DepartmentLegal, DepartmentAccounting,
		DepartmentBackOffice:
		return true
	default:
		return false
	}
}

// SeatRole is the closed role of one durable seat.
type SeatRole string

// SeatRole values are closed and exhaustive.
const (
	// SeatLead plans and coordinates mandate-bounded departmental work.
	SeatLead SeatRole = "lead"
	// SeatExecutor performs mandate-bounded work selected by the kernel.
	SeatExecutor SeatRole = "executor"
	// SeatAuditor independently verifies closed evidence without durable memory.
	SeatAuditor SeatRole = "auditor"
)

// AllSeatRoles returns the canonical three-seat order.
func AllSeatRoles() []SeatRole {
	return []SeatRole{SeatLead, SeatExecutor, SeatAuditor}
}

// Valid reports whether the role is executable by this release.
func (v SeatRole) Valid() bool {
	switch v {
	case SeatLead, SeatExecutor, SeatAuditor:
		return true
	default:
		return false
	}
}

// Classification controls which organizational readers may open a record.
type Classification string

// Classification values are ordered from least to most restricted.
const (
	// ClassificationOrganization permits authorized organization-wide access.
	ClassificationOrganization Classification = "organization"
	// ClassificationDepartment restricts access to an authorized department.
	ClassificationDepartment Classification = "department"
	// ClassificationSeat restricts access to an authorized seat.
	ClassificationSeat Classification = "seat"
	// ClassificationProject restricts access to an authorized project scope.
	ClassificationProject Classification = "project"
	// ClassificationRestricted requires an explicit restricted-data grant.
	ClassificationRestricted Classification = "restricted"
)

// Valid reports whether the classification is recognized.
func (v Classification) Valid() bool {
	switch v {
	case ClassificationOrganization, ClassificationDepartment, ClassificationSeat,
		ClassificationProject, ClassificationRestricted:
		return true
	default:
		return false
	}
}

// RecordKind identifies the typed body carried by an organizational record.
type RecordKind string

// RecordKind values are immutable ledger record families.
const (
	// RecordGoal carries a typed durable goal.
	RecordGoal RecordKind = "goal"
	// RecordIntent carries a typed durable intent.
	RecordIntent RecordKind = "intent"
	// RecordFinding carries an evidence-backed finding.
	RecordFinding RecordKind = "finding"
	// RecordDecision carries an authorized organizational decision.
	RecordDecision RecordKind = "decision"
	// RecordDelegation carries a bounded dependency delegation.
	RecordDelegation RecordKind = "delegation"
	// RecordHandoff carries a typed cross-seat handoff.
	RecordHandoff RecordKind = "handoff"
	// RecordArtifact carries an immutable artifact reference.
	RecordArtifact RecordKind = "artifact"
	// RecordApproval carries an owner-authorized approval result.
	RecordApproval RecordKind = "approval"
	// RecordCorrection carries a correction and reconciliation obligation.
	RecordCorrection RecordKind = "correction"
	// RecordAttestation carries an independent verification attestation.
	RecordAttestation RecordKind = "attestation"
	// RecordPolicyChange carries an immutable signed policy transition.
	RecordPolicyChange RecordKind = "policy_change"
	// RecordReceipt carries immutable execution evidence and lineage.
	RecordReceipt RecordKind = "receipt"
)

// Valid reports whether the record kind is recognized.
func (v RecordKind) Valid() bool {
	switch v {
	case RecordGoal, RecordIntent, RecordFinding, RecordDecision, RecordDelegation,
		RecordHandoff, RecordArtifact, RecordApproval, RecordCorrection,
		RecordAttestation, RecordPolicyChange, RecordReceipt:
		return true
	default:
		return false
	}
}

// Validity is the effective truth state of an immutable record version.
type Validity string

// Validity values are closed.
const (
	// ValidityActive marks the current uncontested record version.
	ValidityActive Validity = "active"
	// ValidityContested marks truth paused pending correction reconciliation.
	ValidityContested Validity = "contested"
	// ValiditySuperseded marks a record replaced by a later valid version.
	ValiditySuperseded Validity = "superseded"
	// ValidityRetracted marks a record withdrawn from effective truth.
	ValidityRetracted Validity = "retracted"
	// ValidityExpired marks a record outside its authorized validity window.
	ValidityExpired Validity = "expired"
)

// Valid reports whether the validity state is recognized.
func (v Validity) Valid() bool {
	switch v {
	case ValidityActive, ValidityContested, ValiditySuperseded,
		ValidityRetracted, ValidityExpired:
		return true
	default:
		return false
	}
}

// MessageKind identifies the typed organizational purpose of a mail envelope.
type MessageKind string

// MessageKind values are the complete Workforce Mail v1 protocol.
const (
	// MessageInformation delivers typed informational context.
	MessageInformation MessageKind = "information"
	// MessageQuestion requests a typed answer.
	MessageQuestion MessageKind = "question"
	// MessageAnswer responds to a typed question.
	MessageAnswer MessageKind = "answer"
	// MessageFinding delivers an evidence-backed finding.
	MessageFinding MessageKind = "finding"
	// MessageRequest requests mandate-checked work.
	MessageRequest MessageKind = "request"
	// MessageDelegation creates a bounded dependency delegation.
	MessageDelegation MessageKind = "delegation"
	// MessageHandoff transfers typed work state without authority escalation.
	MessageHandoff MessageKind = "handoff"
	// MessageBlocker reports a typed inability to advance.
	MessageBlocker MessageKind = "blocker"
	// MessageApprovalRequest requests human or compiled approval.
	MessageApprovalRequest MessageKind = "approval_request"
	// MessageApprovalResult delivers an immutable approval outcome.
	MessageApprovalResult MessageKind = "approval_result"
	// MessageCorrection delivers mandatory correction reconciliation.
	MessageCorrection MessageKind = "correction"
	// MessageReceipt delivers immutable outcome evidence.
	MessageReceipt MessageKind = "receipt"
	// MessageEscalation raises an owner-visible incident or decision.
	MessageEscalation MessageKind = "escalation"
	// MessageCancellation propagates deterministic cancellation.
	MessageCancellation MessageKind = "cancellation"
)

// Valid reports whether the message kind is recognized.
func (v MessageKind) Valid() bool {
	switch v {
	case MessageInformation, MessageQuestion, MessageAnswer, MessageFinding,
		MessageRequest, MessageDelegation, MessageHandoff, MessageBlocker,
		MessageApprovalRequest, MessageApprovalResult, MessageCorrection,
		MessageReceipt, MessageEscalation, MessageCancellation:
		return true
	default:
		return false
	}
}

// TimeoutAction is the closed response to an expired delegation or request.
type TimeoutAction string

// TimeoutAction values fail closed when absent.
const (
	// TimeoutEscalate creates an owner-visible escalation.
	TimeoutEscalate TimeoutAction = "escalate"
	// TimeoutCancel cancels the bounded request or delegation.
	TimeoutCancel TimeoutAction = "cancel"
	// TimeoutReturnToSender returns unresolved work to its sender.
	TimeoutReturnToSender TimeoutAction = "return_to_sender"
	// TimeoutSafeDefault applies a schema-bound, preauthorized safe default.
	TimeoutSafeDefault TimeoutAction = "safe_default"
)

// Valid reports whether the timeout action is recognized.
func (v TimeoutAction) Valid() bool {
	switch v {
	case TimeoutEscalate, TimeoutCancel, TimeoutReturnToSender, TimeoutSafeDefault:
		return true
	default:
		return false
	}
}

// WakeDisposition is the only way a bounded wake may terminate.
type WakeDisposition string

// WakeDisposition values are closed.
const (
	// DispositionProgressed records durable progress without claiming completion.
	DispositionProgressed WakeDisposition = "progressed"
	// DispositionWaitingDependency records a typed dependency wait.
	DispositionWaitingDependency WakeDisposition = "waiting_dependency"
	// DispositionWaitingApproval records an approval wait.
	DispositionWaitingApproval WakeDisposition = "waiting_approval"
	// DispositionBlocked records a non-dependency blocker.
	DispositionBlocked WakeDisposition = "blocked"
	// DispositionGoalCompleted records receipt-backed verified completion.
	DispositionGoalCompleted WakeDisposition = "goal_completed"
	// DispositionBudgetExhausted records bounded resource exhaustion.
	DispositionBudgetExhausted WakeDisposition = "budget_exhausted"
	// DispositionLeaseExpired records authority expiry before completion.
	DispositionLeaseExpired WakeDisposition = "lease_expired"
	// DispositionCancelled records deterministic cancellation.
	DispositionCancelled WakeDisposition = "cancelled"
	// DispositionFailed records a typed terminal failure.
	DispositionFailed WakeDisposition = "failed"
)

// Valid reports whether the disposition is recognized.
func (v WakeDisposition) Valid() bool {
	switch v {
	case DispositionProgressed, DispositionWaitingDependency,
		DispositionWaitingApproval, DispositionBlocked, DispositionGoalCompleted,
		DispositionBudgetExhausted, DispositionLeaseExpired,
		DispositionCancelled, DispositionFailed:
		return true
	default:
		return false
	}
}

// VerdictOutcome is the closed result of deterministic or independent review.
type VerdictOutcome string

// VerdictOutcome values prevent semantic uncertainty from becoming an approval.
const (
	// VerdictPass records authoritative satisfaction of the verifier contract.
	VerdictPass VerdictOutcome = "pass"
	// VerdictFail records authoritative failure of the verifier contract.
	VerdictFail VerdictOutcome = "fail"
	// VerdictRequiresHuman records irreducible semantic judgment.
	VerdictRequiresHuman VerdictOutcome = "requires_human"
)

// Valid reports whether the verdict outcome is recognized.
func (v VerdictOutcome) Valid() bool {
	switch v {
	case VerdictPass, VerdictFail, VerdictRequiresHuman:
		return true
	default:
		return false
	}
}

// ContentHash declares the digest algorithm and lowercase hexadecimal digest.
type ContentHash struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// Validate enforces the v1 SHA-256 hash profile.
func (h ContentHash) Validate() error {
	if h.Algorithm != "sha256" {
		return fmt.Errorf("hash algorithm must be sha256")
	}
	if len(h.Digest) != 64 {
		return fmt.Errorf("sha256 digest must contain 64 lowercase hexadecimal characters")
	}
	for _, r := range h.Digest {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return fmt.Errorf("sha256 digest is not lowercase hexadecimal")
	}
	return nil
}

// Signature declares the signing algorithm, key identity, and unpadded base64url value.
type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"value"`
}

// Validate enforces the v1 Ed25519 signature profile.
func (s Signature) Validate() error {
	if s.Algorithm != "ed25519" {
		return fmt.Errorf("signature algorithm must be ed25519")
	}
	if err := validateID("signature key_id", s.KeyID); err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(s.Value)
	if err != nil {
		return fmt.Errorf("signature value must be unpadded base64url")
	}
	if len(decoded) != ed25519.SignatureSize {
		return fmt.Errorf("Ed25519 signature must contain %d bytes", ed25519.SignatureSize)
	}
	return nil
}
