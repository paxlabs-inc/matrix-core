package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

var (
	ErrDenied        = errors.New("approval: denied")
	ErrHumanRequired = errors.New("approval: human approval required")
	ErrEscalate      = errors.New("approval: escalation required")
	ErrUnauthorized  = errors.New("approval: unauthorized")
	ErrExpired       = errors.New("approval: expired")
	ErrConflict      = errors.New("approval: conflict")
	ErrCeiling       = errors.New("approval: aggregate ceiling exceeded")
	ErrUncertain     = errors.New("approval: state is uncertain")
)

type AutonomyPreset string

const (
	PresetSupervised AutonomyPreset = "supervised"
	PresetBounded    AutonomyPreset = "bounded"
	PresetUnattended AutonomyPreset = "unattended"
)

type Permissions struct {
	Initiation            bool
	Scheduling            bool
	DataAccess            bool
	ExternalCommunication bool
	Publication           bool
	Spending              bool
	DestructiveEffects    bool
	RequiredReview        bool
}

func CompilePreset(preset AutonomyPreset) (Permissions, error) {
	switch preset {
	case PresetSupervised:
		return Permissions{DataAccess: true, RequiredReview: true}, nil
	case PresetBounded:
		return Permissions{
			Initiation: true, Scheduling: true, DataAccess: true,
			ExternalCommunication: true, RequiredReview: true,
		}, nil
	case PresetUnattended:
		return Permissions{
			Initiation: true, Scheduling: true, DataAccess: true,
			ExternalCommunication: true, Publication: true, Spending: true,
			RequiredReview: true,
		}, nil
	default:
		return Permissions{}, fmt.Errorf("approval: unknown autonomy preset %q", preset)
	}
}

type Outcome string

const (
	OutcomeAutoApprove   Outcome = "auto_approve"
	OutcomeBatchable     Outcome = "batchable"
	OutcomeHumanRequired Outcome = "human_required"
	OutcomeDeny          Outcome = "deny"
	OutcomeEscalate      Outcome = "escalate"
)

func (outcome Outcome) Valid() bool {
	switch outcome {
	case OutcomeAutoApprove, OutcomeBatchable, OutcomeHumanRequired,
		OutcomeDeny, OutcomeEscalate:
		return true
	default:
		return false
	}
}

type Rule struct {
	ClauseID          string
	Priority          int32
	Outcome           Outcome
	SkillID           string
	Operation         string
	EffectClass       string
	Reversibility     string
	MaxCostMicrounits uint64
	Counterparty      string
	DataScope         string
	Channel           string
	Jurisdiction      string
	WindowStartUTC    *time.Time
	WindowEndUTC      *time.Time
	SeatID            string
	PriorVerdict      string
}

func (rule Rule) Validate() error {
	if strings.TrimSpace(rule.ClauseID) == "" || !rule.Outcome.Valid() {
		return fmt.Errorf("approval: rule clause and outcome are required")
	}
	if rule.Reversibility != "" && rule.Reversibility != "reversible" &&
		rule.Reversibility != "irreversible" {
		return fmt.Errorf("approval: invalid reversibility")
	}
	if (rule.WindowStartUTC == nil) != (rule.WindowEndUTC == nil) {
		return fmt.Errorf("approval: both time-window bounds are required")
	}
	if rule.WindowStartUTC != nil &&
		(rule.WindowStartUTC.Location() != time.UTC ||
			rule.WindowEndUTC.Location() != time.UTC ||
			!rule.WindowEndUTC.After(*rule.WindowStartUTC)) {
		return fmt.Errorf("approval: invalid UTC time window")
	}
	return nil
}

type Request struct {
	RequestID      string
	IntentID       contracts.IntentID
	SkillID        string
	Operation      string
	EffectClass    string
	Reversible     bool
	CostMicrounits uint64
	Counterparty   string
	DataScope      string
	Channel        string
	Jurisdiction   string
	SeatID         contracts.SeatID
	PriorVerdict   string
	RequestedAt    time.Time
}

func (request Request) Validate() error {
	if strings.TrimSpace(request.RequestID) == "" || request.IntentID == "" ||
		strings.TrimSpace(request.SkillID) == "" ||
		strings.TrimSpace(request.Operation) == "" ||
		strings.TrimSpace(request.EffectClass) == "" ||
		request.SeatID == "" || request.RequestedAt.IsZero() ||
		request.RequestedAt.Location() != time.UTC {
		return fmt.Errorf("approval: request is incomplete")
	}
	return nil
}

type Decision struct {
	Outcome  Outcome
	ClauseID string
	Reason   string
}

func Evaluate(rules []Rule, request Request) (Decision, error) {
	if err := request.Validate(); err != nil {
		return Decision{}, err
	}
	type match struct {
		rule        Rule
		specificity int
	}
	var matches []match
	for _, rule := range rules {
		if err := rule.Validate(); err != nil {
			return Decision{}, err
		}
		specificity, ok := ruleMatch(rule, request)
		if ok {
			matches = append(matches, match{rule: rule, specificity: specificity})
		}
	}
	if len(matches) == 0 {
		if request.Reversible {
			return Decision{Outcome: OutcomeHumanRequired,
				Reason: "no matching approval rule"}, ErrHumanRequired
		}
		return Decision{Outcome: OutcomeDeny,
			Reason: "irreversible operation has no explicit approval"}, ErrDenied
	}
	sort.Slice(matches, func(left, right int) bool {
		if outcomeRank(matches[left].rule.Outcome) != outcomeRank(matches[right].rule.Outcome) {
			return outcomeRank(matches[left].rule.Outcome) >
				outcomeRank(matches[right].rule.Outcome)
		}
		if matches[left].rule.Priority != matches[right].rule.Priority {
			return matches[left].rule.Priority > matches[right].rule.Priority
		}
		if matches[left].specificity != matches[right].specificity {
			return matches[left].specificity > matches[right].specificity
		}
		return matches[left].rule.ClauseID < matches[right].rule.ClauseID
	})
	selected := matches[0].rule
	decision := Decision{Outcome: selected.Outcome, ClauseID: selected.ClauseID,
		Reason: "deterministic policy match"}
	switch selected.Outcome {
	case OutcomeDeny:
		return decision, ErrDenied
	case OutcomeHumanRequired:
		return decision, ErrHumanRequired
	case OutcomeEscalate:
		return decision, ErrEscalate
	default:
		return decision, nil
	}
}

func ResolveTimeout(request Request, explicit *Outcome) (Decision, error) {
	if err := request.Validate(); err != nil {
		return Decision{}, err
	}
	if explicit == nil {
		if !request.Reversible {
			return Decision{Outcome: OutcomeDeny,
				Reason: "irreversible approval timeout defaults deny"}, ErrDenied
		}
		return Decision{Outcome: OutcomeHumanRequired,
			Reason: "timeout behavior absent"}, ErrHumanRequired
	}
	if !explicit.Valid() || *explicit == OutcomeAutoApprove ||
		*explicit == OutcomeBatchable {
		return Decision{}, fmt.Errorf("approval: unsafe timeout outcome")
	}
	decision := Decision{Outcome: *explicit, Reason: "explicit timeout policy"}
	switch *explicit {
	case OutcomeDeny:
		return decision, ErrDenied
	case OutcomeEscalate:
		return decision, ErrEscalate
	default:
		return decision, ErrHumanRequired
	}
}

func IntentSetHash(intents []contracts.IntentID) (string, error) {
	if len(intents) == 0 || len(intents) > 10000 {
		return "", fmt.Errorf("approval: intent set must contain 1 to 10000 intents")
	}
	values := make([]string, len(intents))
	seen := make(map[string]bool, len(intents))
	for index, intent := range intents {
		value := strings.TrimSpace(string(intent))
		if value == "" || seen[value] {
			return "", fmt.Errorf("approval: intent set contains an invalid duplicate")
		}
		seen[value] = true
		values[index] = value
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func ruleMatch(rule Rule, request Request) (int, bool) {
	fields := [][2]string{
		{rule.SkillID, request.SkillID}, {rule.Operation, request.Operation},
		{rule.EffectClass, request.EffectClass},
		{rule.Counterparty, request.Counterparty}, {rule.DataScope, request.DataScope},
		{rule.Channel, request.Channel}, {rule.Jurisdiction, request.Jurisdiction},
		{rule.SeatID, string(request.SeatID)}, {rule.PriorVerdict, request.PriorVerdict},
	}
	specificity := 0
	for _, field := range fields {
		if field[0] != "" {
			if field[0] != field[1] {
				return 0, false
			}
			specificity++
		}
	}
	if rule.Reversibility != "" {
		actual := "irreversible"
		if request.Reversible {
			actual = "reversible"
		}
		if rule.Reversibility != actual {
			return 0, false
		}
		specificity++
	}
	if rule.MaxCostMicrounits != 0 {
		if request.CostMicrounits > rule.MaxCostMicrounits {
			return 0, false
		}
		specificity++
	}
	if rule.WindowStartUTC != nil {
		if request.RequestedAt.Before(*rule.WindowStartUTC) ||
			!request.RequestedAt.Before(*rule.WindowEndUTC) {
			return 0, false
		}
		specificity++
	}
	return specificity, true
}

func outcomeRank(outcome Outcome) int {
	switch outcome {
	case OutcomeDeny:
		return 5
	case OutcomeEscalate:
		return 4
	case OutcomeHumanRequired:
		return 3
	case OutcomeBatchable:
		return 2
	default:
		return 1
	}
}
