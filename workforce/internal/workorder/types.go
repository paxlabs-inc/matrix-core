// Package workorder owns bounded owner-signed organizational requests and
// their durable execution context.
package workorder

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

type Budget struct {
	MaxTasks           uint32 `json:"max_tasks"`
	MaxSpendMicrounits uint64 `json:"max_spend_microunits"`
}

type AcceptanceCriterionKind string

const (
	AcceptanceEvidenceHash AcceptanceCriterionKind = "evidence_hash"
	AcceptanceSemantic     AcceptanceCriterionKind = "semantic_review"
)

type AcceptanceCriterion struct {
	ID          string
	Kind        AcceptanceCriterionKind
	Description string
}

func ParseAcceptanceCriterion(index int, value string) (AcceptanceCriterion, error) {
	description := strings.TrimSpace(value)
	if index < 0 || description == "" || len(description) > 512 {
		return AcceptanceCriterion{}, fmt.Errorf(
			"workorder: acceptance criterion is invalid",
		)
	}
	kind := AcceptanceSemantic
	const evidencePrefix = "evidence_hash:"
	if strings.HasPrefix(description, evidencePrefix) {
		kind = AcceptanceEvidenceHash
		description = strings.TrimSpace(
			strings.TrimPrefix(description, evidencePrefix),
		)
		if description == "" {
			return AcceptanceCriterion{}, fmt.Errorf(
				"workorder: evidence_hash criterion requires a description",
			)
		}
	}
	return AcceptanceCriterion{
		ID:          fmt.Sprintf("acceptance-%02d", index+1),
		Kind:        kind,
		Description: description,
	}, nil
}

type Order struct {
	SchemaVersion      string                     `json:"schema_version"`
	ID                 string                     `json:"work_order_id"`
	OrganizationID     contracts.OrganizationID   `json:"organization_id"`
	OwnerID            contracts.OwnerID          `json:"owner_id"`
	Version            uint64                     `json:"version"`
	Objective          string                     `json:"objective"`
	Scope              string                     `json:"scope"`
	ProjectID          contracts.ProjectID        `json:"project_id"`
	WorkspaceID        contracts.WorkspaceID      `json:"workspace_id"`
	ScopeFiles         []string                   `json:"scope_files"`
	ScopeSymbols       []string                   `json:"scope_symbols"`
	Departments        []contracts.DepartmentKind `json:"departments"`
	Priority           int32                      `json:"priority"`
	Budget             Budget                     `json:"budget"`
	Deadline           time.Time                  `json:"deadline"`
	Autonomy           string                     `json:"autonomy"`
	AcceptanceCriteria []string                   `json:"acceptance_criteria"`
	ModelProvider      string                     `json:"model_provider"`
	ModelID            string                     `json:"model_id"`
	MGSReference       string                     `json:"mgs_reference"`
	MGSDigest          string                     `json:"mgs_digest"`
	CreatedAt          time.Time                  `json:"created_at"`
	IdempotencyKey     string                     `json:"idempotency_key"`
	Signature          contracts.Signature        `json:"signature"`
}

func (order Order) Validate() error {
	if order.SchemaVersion != "workforce.work-order.v1" ||
		strings.TrimSpace(order.ID) == "" || len(order.ID) > 96 ||
		order.OrganizationID == "" || order.OwnerID == "" || order.Version != 1 {
		return fmt.Errorf("workorder: identity is invalid")
	}
	if strings.TrimSpace(order.Objective) == "" || len(order.Objective) > 512 ||
		strings.TrimSpace(order.Scope) == "" || len(order.Scope) > 1024 {
		return fmt.Errorf("workorder: objective and scope are required")
	}
	if len(order.Departments) == 0 || len(order.Departments) > 7 {
		return fmt.Errorf("workorder: one to seven departments are required")
	}
	seen := make(map[contracts.DepartmentKind]bool, len(order.Departments))
	developer := false
	for _, department := range order.Departments {
		if !department.Valid() || seen[department] {
			return fmt.Errorf("workorder: departments are invalid")
		}
		seen[department] = true
		developer = developer || department == contracts.DepartmentDeveloper
	}
	if developer {
		if !filepath.IsAbs(order.Scope) ||
			!validOrderID(string(order.ProjectID)) ||
			!validOrderID(string(order.WorkspaceID)) ||
			!validScopeValues(order.ScopeFiles, true) ||
			!validScopeValues(order.ScopeSymbols, false) {
			return fmt.Errorf(
				"workorder: Developer project connection and exact source scope are required",
			)
		}
	} else if order.ProjectID != "" || order.WorkspaceID != "" ||
		len(order.ScopeFiles) != 0 || len(order.ScopeSymbols) != 0 {
		return fmt.Errorf(
			"workorder: Developer project scope requires the Developer department",
		)
	}
	if order.Priority < -1000 || order.Priority > 1000 ||
		order.Budget.MaxTasks == 0 || order.Budget.MaxTasks > 50 {
		return fmt.Errorf("workorder: priority or task budget is invalid")
	}
	if order.CreatedAt.IsZero() || order.CreatedAt.Location() != time.UTC ||
		order.Deadline.Location() != time.UTC ||
		!order.Deadline.After(order.CreatedAt) {
		return fmt.Errorf("workorder: times are invalid")
	}
	switch order.Autonomy {
	case "supervised", "review_required", "bounded_auto":
	default:
		return fmt.Errorf("workorder: autonomy is invalid")
	}
	if len(order.AcceptanceCriteria) == 0 || len(order.AcceptanceCriteria) > 20 {
		return fmt.Errorf("workorder: acceptance criteria are required")
	}
	for index, criterion := range order.AcceptanceCriteria {
		if _, err := ParseAcceptanceCriterion(index, criterion); err != nil {
			return err
		}
	}
	if strings.TrimSpace(order.ModelProvider) == "" ||
		len(order.ModelProvider) > 128 ||
		strings.TrimSpace(order.ModelID) == "" || len(order.ModelID) > 128 ||
		strings.TrimSpace(order.MGSReference) == "" ||
		len(order.MGSReference) > 512 ||
		strings.TrimSpace(order.IdempotencyKey) == "" ||
		len(order.IdempotencyKey) > 128 {
		return fmt.Errorf("workorder: model, MGS, and idempotency are required")
	}
	if len(order.MGSDigest) != 64 {
		return fmt.Errorf("workorder: MGS digest is invalid")
	}
	for _, character := range order.MGSDigest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return fmt.Errorf("workorder: MGS digest is invalid")
		}
	}
	return order.Signature.Validate()
}

type payload struct {
	SchemaVersion      string                     `json:"schema_version"`
	ID                 string                     `json:"work_order_id"`
	OrganizationID     contracts.OrganizationID   `json:"organization_id"`
	OwnerID            contracts.OwnerID          `json:"owner_id"`
	Version            uint64                     `json:"version"`
	Objective          string                     `json:"objective"`
	Scope              string                     `json:"scope"`
	ProjectID          contracts.ProjectID        `json:"project_id"`
	WorkspaceID        contracts.WorkspaceID      `json:"workspace_id"`
	ScopeFiles         []string                   `json:"scope_files"`
	ScopeSymbols       []string                   `json:"scope_symbols"`
	Departments        []contracts.DepartmentKind `json:"departments"`
	Priority           int32                      `json:"priority"`
	Budget             Budget                     `json:"budget"`
	Deadline           time.Time                  `json:"deadline"`
	Autonomy           string                     `json:"autonomy"`
	AcceptanceCriteria []string                   `json:"acceptance_criteria"`
	ModelProvider      string                     `json:"model_provider"`
	ModelID            string                     `json:"model_id"`
	MGSReference       string                     `json:"mgs_reference"`
	MGSDigest          string                     `json:"mgs_digest"`
	CreatedAt          time.Time                  `json:"created_at"`
	IdempotencyKey     string                     `json:"idempotency_key"`
}

func (payload) Validate() error { return nil }

func Sign(order *Order, keyID string, privateKey ed25519.PrivateKey) error {
	if order == nil || len(privateKey) != ed25519.PrivateKeySize ||
		strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("workorder: signing authority is invalid")
	}
	signingBytes, err := signingBytes(*order)
	if err != nil {
		return err
	}
	order.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(
			ed25519.Sign(privateKey, signingBytes),
		),
	}
	return order.Validate()
}

func Verify(order Order, keyID string, publicKey ed25519.PublicKey) error {
	if err := order.Validate(); err != nil {
		return err
	}
	if order.Signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("workorder: signature authority mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(order.Signature.Value)
	if err != nil {
		return fmt.Errorf("workorder: signature encoding is invalid")
	}
	signingBytes, err := signingBytes(order)
	if err != nil || !ed25519.Verify(publicKey, signingBytes, signature) {
		return fmt.Errorf("workorder: signature verification failed")
	}
	return nil
}

func signingBytes(order Order) ([]byte, error) {
	return contracts.EncodeCanonical(&payload{
		SchemaVersion: order.SchemaVersion, ID: order.ID,
		OrganizationID: order.OrganizationID, OwnerID: order.OwnerID,
		Version: order.Version, Objective: order.Objective, Scope: order.Scope,
		ProjectID: order.ProjectID, WorkspaceID: order.WorkspaceID,
		ScopeFiles:   append([]string(nil), order.ScopeFiles...),
		ScopeSymbols: append([]string(nil), order.ScopeSymbols...),
		Departments:  append([]contracts.DepartmentKind(nil), order.Departments...),
		Priority:     order.Priority, Budget: order.Budget, Deadline: order.Deadline,
		Autonomy:           order.Autonomy,
		AcceptanceCriteria: append([]string(nil), order.AcceptanceCriteria...),
		ModelProvider:      order.ModelProvider, ModelID: order.ModelID,
		MGSReference: order.MGSReference, MGSDigest: order.MGSDigest,
		CreatedAt: order.CreatedAt, IdempotencyKey: order.IdempotencyKey,
	})
}

func validOrderID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' ||
			character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validScopeValues(values []string, paths bool) bool {
	if len(values) == 0 || len(values) > 256 {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 1024 || seen[value] {
			return false
		}
		if paths && (filepath.IsAbs(value) || filepath.Clean(value) != value ||
			value == "." || strings.HasPrefix(value, ".."+string(filepath.Separator))) {
			return false
		}
		seen[value] = true
	}
	return true
}
