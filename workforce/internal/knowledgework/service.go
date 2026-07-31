// Package knowledgework owns typed, non-effectful departmental analysis and
// handoff proposal assembly.
package knowledgework

import (
	"context"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/skills"
)

const maxKnowledgePayloadBytes = 1 << 20

type Finding struct {
	Statement   string                 `json:"statement"`
	EvidenceIDs []contracts.EvidenceID `json:"evidence_ids"`
}

type Recommendation struct {
	Action      string                 `json:"action"`
	Rationale   string                 `json:"rationale"`
	EvidenceIDs []contracts.EvidenceID `json:"evidence_ids"`
}

type ExperimentDesign struct {
	Hypothesis       string   `json:"hypothesis"`
	Method           string   `json:"method"`
	SuccessMetrics   []string `json:"success_metrics"`
	StopConditions   []string `json:"stop_conditions"`
	MaximumDuration  string   `json:"maximum_duration"`
	RequiresHumanRun bool     `json:"requires_human_run"`
}

type HandoffDraft struct {
	RecipientDepartment contracts.DepartmentKind `json:"recipient_department"`
	RecipientSeatID     contracts.SeatID         `json:"recipient_seat_id"`
	Subject             string                   `json:"subject"`
	RequiredAction      string                   `json:"required_action"`
	TimeoutAction       contracts.TimeoutAction  `json:"timeout_action"`
	ExpiresAt           time.Time                `json:"expires_at"`
}

type Draft struct {
	Summary         string            `json:"summary"`
	Findings        []Finding         `json:"findings"`
	Recommendations []Recommendation  `json:"recommendations"`
	Experiment      *ExperimentDesign `json:"experiment"`
	Handoff         *HandoffDraft     `json:"handoff"`
	UnresolvedRisks []string          `json:"unresolved_risks"`
	RequiresHuman   bool              `json:"requires_human"`
}

type Input struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Department     contracts.DepartmentKind `json:"department"`
	SeatID         contracts.SeatID         `json:"seat_id"`
	IntentID       contracts.IntentID       `json:"intent_id"`
	SkillID        contracts.SkillID        `json:"skill_id"`
	Objective      string                   `json:"objective"`
	Constraints    []string                 `json:"constraints"`
	Evidence       []contracts.EvidenceRef  `json:"evidence"`
	SourceDigest   contracts.ContentHash    `json:"source_digest"`
	CorrectionOf   *contracts.ContentHash   `json:"correction_of"`
	Draft          Draft                    `json:"draft"`
}

type Result struct {
	SchemaVersion string                  `json:"schema_version"`
	Outcome       string                  `json:"outcome"`
	Artifact      contracts.ArtifactRef   `json:"artifact"`
	Evidence      []contracts.EvidenceRef `json:"evidence"`
	Handoff       *HandoffDraft           `json:"handoff"`
	RequiresHuman bool                    `json:"requires_human"`
	Payload       []byte                  `json:"-"`
}

// Service validates model-authored knowledge work into content-addressed typed
// proposals. It deliberately has no effect gateway, approval store, policy
// signer, or credential-bearing dependency.
type Service struct {
	now func() time.Time
}

func New(now func() time.Time) (*Service, error) {
	if now == nil {
		return nil, fmt.Errorf("knowledgework: UTC time source is required")
	}
	return &Service{now: now}, nil
}

func (service *Service) Execute(ctx context.Context, input Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	now := service.now()
	if now.IsZero() || now.Location() != time.UTC {
		return Result{}, fmt.Errorf("knowledgework: time source must return UTC")
	}
	if err := input.validateAt(now); err != nil {
		return Result{}, err
	}
	payload, err := contracts.EncodeCanonical(&input)
	if err != nil {
		return Result{}, err
	}
	if len(payload) > maxKnowledgePayloadBytes {
		return Result{}, fmt.Errorf("knowledgework: typed payload exceeds one MiB")
	}
	digest, err := contracts.HashCanonical(&input)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		SchemaVersion: contracts.SchemaVersionV1,
		Outcome:       "proposed",
		Artifact: contracts.ArtifactRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID: contracts.ArtifactID(
				"artifact:knowledge:" + digest.Digest[:32],
			),
			Hash:      digest,
			MediaType: "application/vnd.matrix.knowledge-work+json",
			SizeBytes: uint64(len(payload)),
		},
		Evidence: append([]contracts.EvidenceRef(nil), input.Evidence...),
		Handoff:  input.Draft.Handoff,
		RequiresHuman: input.Draft.RequiresHuman ||
			input.Draft.Experiment != nil &&
				input.Draft.Experiment.RequiresHumanRun,
		Payload: append([]byte(nil), payload...),
	}
	if result.RequiresHuman {
		result.Outcome = "requires_human"
	}
	return result, nil
}

// Validate enforces the time-independent canonical contract. Execute applies
// the additional current-time expiry check before accepting the work.
func (input Input) Validate() error {
	return input.validateAt(time.Time{})
}

func (input Input) validateAt(now time.Time) error {
	if input.SchemaVersion != contracts.SchemaVersionV1 ||
		input.OrganizationID == "" || input.SeatID == "" || input.IntentID == "" {
		return fmt.Errorf("knowledgework: input identity is incomplete")
	}
	if input.Department != contracts.DepartmentExecutive &&
		input.Department != contracts.DepartmentResearch {
		return fmt.Errorf("knowledgework: department is outside this service")
	}
	if !skillAllowed(input.Department, input.SkillID) {
		return fmt.Errorf("knowledgework: skill is outside department mandate")
	}
	if strings.TrimSpace(input.Objective) == "" || len(input.Objective) > 4096 ||
		len(input.Constraints) == 0 || len(input.Constraints) > 64 ||
		len(input.Evidence) == 0 || len(input.Evidence) > 256 {
		return fmt.Errorf("knowledgework: objective constraints or evidence are outside bounds")
	}
	for _, constraint := range input.Constraints {
		if strings.TrimSpace(constraint) == "" || len(constraint) > 2048 {
			return fmt.Errorf("knowledgework: constraint is invalid")
		}
	}
	evidence := make(map[contracts.EvidenceID]bool, len(input.Evidence))
	for _, item := range input.Evidence {
		if err := item.Validate(); err != nil {
			return err
		}
		if evidence[item.ID] {
			return fmt.Errorf("knowledgework: evidence is duplicated")
		}
		evidence[item.ID] = true
	}
	if err := input.SourceDigest.Validate(); err != nil {
		return err
	}
	if input.CorrectionOf != nil {
		if err := input.CorrectionOf.Validate(); err != nil {
			return err
		}
		if *input.CorrectionOf == input.SourceDigest {
			return fmt.Errorf("knowledgework: correction must target a prior artifact")
		}
	}
	return input.Draft.validate(input.SkillID, evidence, now)
}

func (draft Draft) validate(
	skillID contracts.SkillID,
	evidence map[contracts.EvidenceID]bool,
	now time.Time,
) error {
	if strings.TrimSpace(draft.Summary) == "" || len(draft.Summary) > 8192 ||
		len(draft.Findings) > 128 || len(draft.Recommendations) > 128 ||
		len(draft.UnresolvedRisks) > 64 {
		return fmt.Errorf("knowledgework: draft is outside bounds")
	}
	if len(draft.Findings) == 0 && len(draft.Recommendations) == 0 &&
		draft.Experiment == nil && draft.Handoff == nil {
		return fmt.Errorf("knowledgework: draft has no typed work product")
	}
	for _, finding := range draft.Findings {
		if err := validateEvidenceStatement(
			finding.Statement, finding.EvidenceIDs, evidence,
		); err != nil {
			return err
		}
	}
	for _, recommendation := range draft.Recommendations {
		if strings.TrimSpace(recommendation.Action) == "" ||
			strings.TrimSpace(recommendation.Rationale) == "" ||
			len(recommendation.Action) > 2048 ||
			len(recommendation.Rationale) > 4096 {
			return fmt.Errorf("knowledgework: recommendation is invalid")
		}
		if err := validateEvidenceIDs(
			recommendation.EvidenceIDs, evidence,
		); err != nil {
			return err
		}
	}
	if skillID == skills.ExperimentDesignSkill {
		if draft.Experiment == nil {
			return fmt.Errorf("knowledgework: experiment-design requires a design")
		}
		if err := draft.Experiment.validate(); err != nil {
			return err
		}
	} else if draft.Experiment != nil {
		return fmt.Errorf("knowledgework: only experiment-design may emit an experiment")
	}
	if skillID == skills.TypedHandoffSkill {
		if draft.Handoff == nil {
			return fmt.Errorf("knowledgework: typed-handoff requires a handoff")
		}
		if err := draft.Handoff.validate(now); err != nil {
			return err
		}
	} else if draft.Handoff != nil {
		return fmt.Errorf("knowledgework: only typed-handoff may emit a handoff")
	}
	for _, risk := range draft.UnresolvedRisks {
		if strings.TrimSpace(risk) == "" || len(risk) > 2048 {
			return fmt.Errorf("knowledgework: unresolved risk is invalid")
		}
	}
	return nil
}

func (experiment ExperimentDesign) validate() error {
	if strings.TrimSpace(experiment.Hypothesis) == "" ||
		strings.TrimSpace(experiment.Method) == "" ||
		strings.TrimSpace(experiment.MaximumDuration) == "" ||
		len(experiment.SuccessMetrics) == 0 ||
		len(experiment.StopConditions) == 0 {
		return fmt.Errorf("knowledgework: experiment design is incomplete")
	}
	for _, values := range [][]string{
		experiment.SuccessMetrics, experiment.StopConditions,
	} {
		if len(values) > 32 {
			return fmt.Errorf("knowledgework: experiment bounds are exceeded")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 2048 {
				return fmt.Errorf("knowledgework: experiment clause is invalid")
			}
		}
	}
	return nil
}

func (handoff HandoffDraft) validate(now time.Time) error {
	if !handoff.RecipientDepartment.Valid() || handoff.RecipientSeatID == "" ||
		strings.TrimSpace(handoff.Subject) == "" ||
		strings.TrimSpace(handoff.RequiredAction) == "" ||
		!handoff.TimeoutAction.Valid() ||
		handoff.ExpiresAt.IsZero() || handoff.ExpiresAt.Location() != time.UTC ||
		!now.IsZero() && !handoff.ExpiresAt.After(now) {
		return fmt.Errorf("knowledgework: handoff is invalid")
	}
	return nil
}

func skillAllowed(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
) bool {
	var allowed []contracts.SkillID
	if department == contracts.DepartmentExecutive {
		allowed = skills.ExecutiveSkillIDs()
	} else {
		allowed = skills.ResearchSkillIDs()
	}
	for _, candidate := range allowed {
		if candidate == skillID {
			return true
		}
	}
	return false
}

func validateEvidenceStatement(
	statement string,
	ids []contracts.EvidenceID,
	evidence map[contracts.EvidenceID]bool,
) error {
	if strings.TrimSpace(statement) == "" || len(statement) > 4096 {
		return fmt.Errorf("knowledgework: finding statement is invalid")
	}
	return validateEvidenceIDs(ids, evidence)
}

func validateEvidenceIDs(
	ids []contracts.EvidenceID,
	evidence map[contracts.EvidenceID]bool,
) error {
	if len(ids) == 0 || len(ids) > 64 {
		return fmt.Errorf("knowledgework: evidence references are outside bounds")
	}
	seen := make(map[contracts.EvidenceID]bool, len(ids))
	for _, id := range ids {
		if !evidence[id] || seen[id] {
			return fmt.Errorf("knowledgework: evidence reference is missing or duplicated")
		}
		seen[id] = true
	}
	return nil
}
