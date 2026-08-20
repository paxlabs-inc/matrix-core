package wakeruntime

import (
	"time"

	"centra/workforce/internal/contracts"
)

// legacyWorkPacket preserves the exact pre-company JSON shape so an in-flight
// owner wake remains recoverable after the company execution field is added.
type legacyWorkPacket struct {
	SchemaVersion   string                      `json:"schema_version"`
	Lease           contracts.WakeLease         `json:"lease"`
	Seat            contracts.Seat              `json:"seat"`
	Mandate         contracts.Mandate           `json:"mandate"`
	Goal            contracts.Goal              `json:"goal"`
	Intent          contracts.Intent            `json:"intent"`
	VerifiedState   []contracts.RecordRef       `json:"verified_state"`
	Dependencies    []contracts.IntentID        `json:"dependencies"`
	Artifacts       []contracts.ArtifactRef     `json:"artifacts"`
	Evidence        []contracts.EvidenceRef     `json:"evidence"`
	Inbox           []contracts.MessageEnvelope `json:"inbox"`
	Tools           []contracts.ToolRef         `json:"tools"`
	Skills          []contracts.SkillRef        `json:"skills"`
	Policies        []contracts.PolicyRef       `json:"policies"`
	RequiredOutputs []contracts.RequiredOutput  `json:"required_outputs"`
	ProjectBrain    *contracts.ProjectBrainRef  `json:"project_brain"`
	AssembledAt     time.Time                   `json:"assembled_at"`
}

func (value legacyWorkPacket) current() contracts.WorkPacket {
	return contracts.WorkPacket{
		SchemaVersion:   value.SchemaVersion,
		Lease:           value.Lease,
		Seat:            value.Seat,
		Mandate:         value.Mandate,
		Goal:            value.Goal,
		Intent:          value.Intent,
		VerifiedState:   value.VerifiedState,
		Dependencies:    value.Dependencies,
		Artifacts:       value.Artifacts,
		Evidence:        value.Evidence,
		Inbox:           value.Inbox,
		Tools:           value.Tools,
		Skills:          value.Skills,
		Policies:        value.Policies,
		RequiredOutputs: value.RequiredOutputs,
		ProjectBrain:    value.ProjectBrain,
		AssembledAt:     value.AssembledAt,
	}
}

func (value legacyWorkPacket) Validate() error {
	return value.current().Validate()
}
