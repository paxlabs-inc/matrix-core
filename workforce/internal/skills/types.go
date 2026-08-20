package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

var (
	// ErrInvalidContract means a skill contract is incomplete or internally inconsistent.
	ErrInvalidContract = errors.New("invalid skill contract")
	// ErrDriftBlindAutonomy means an effectful skill lacks an authoritative probe.
	ErrDriftBlindAutonomy = errors.New("drift-blind skill cannot run unattended at high autonomy")
)

// EffectClass classifies the external consequence of a skill operation.
type EffectClass string

const (
	// EffectRead is an authoritative read-only observation.
	EffectRead EffectClass = "read"
	// EffectReversible is an external mutation with a defined compensation.
	EffectReversible EffectClass = "reversible"
	// EffectIrreversible is an external mutation that cannot be safely compensated.
	EffectIrreversible EffectClass = "irreversible"
)

// Valid reports whether the effect class is closed and recognized.
func (class EffectClass) Valid() bool {
	return class == EffectRead || class == EffectReversible || class == EffectIrreversible
}

// ProbeOutcome is the closed authoritative reconciliation result set.
type ProbeOutcome string

const (
	// ProbeUnchanged means the external state still matches the last observation.
	ProbeUnchanged ProbeOutcome = "unchanged"
	// ProbeCompletedOutOfBand means the intended effect completed outside this wake.
	ProbeCompletedOutOfBand ProbeOutcome = "completed_out_of_band"
	// ProbeReversed means an earlier observed effect was subsequently undone.
	ProbeReversed ProbeOutcome = "reversed"
	// ProbeDrifted means external state materially diverged without a direct conflict.
	ProbeDrifted ProbeOutcome = "drifted"
	// ProbeConflicted means another actor produced incompatible external state.
	ProbeConflicted ProbeOutcome = "conflicted"
	// ProbeUnknown means the authoritative external state cannot be established.
	ProbeUnknown ProbeOutcome = "unknown"
)

// Valid reports whether the probe outcome is part of the closed contract.
func (outcome ProbeOutcome) Valid() bool {
	switch outcome {
	case ProbeUnchanged, ProbeCompletedOutOfBand, ProbeReversed,
		ProbeDrifted, ProbeConflicted, ProbeUnknown:
		return true
	default:
		return false
	}
}

// ProbeContract defines one read-only authoritative reconciliation operation.
type ProbeContract struct {
	Operation        string
	OutputSchema     json.RawMessage
	Authority        string
	Timeout          time.Duration
	ReadOnly         bool
	Authoritative    bool
	VerifierDigest   contracts.ContentHash
	UnavailableMeans ProbeOutcome
}

// Operation defines one executable operation within a skill.
type Operation struct {
	Name             string
	EffectClass      EffectClass
	InputSchema      json.RawMessage
	OutputSchema     json.RawMessage
	Capability       string
	DataScopes       []string
	IdempotencyField string
	Compensation     string
	ResourceUnits    uint32
	// Providers is the exact set of adapters the owner signed this operation
	// for. An operation name alone is not authority to reach a credentialed
	// provider, so an effectful operation must name its providers here.
	Providers []string
	// CostMicrounits is the owner-signed price of one resource unit. An
	// irreversible operation's approval cost is derived from it rather than
	// declared by the caller, so an approved ceiling cannot be evaded by
	// pricing an expensive operation at nothing.
	CostMicrounits uint64
}

// ApprovalCost returns the exact owner-signed cost of running this operation.
func (operation Operation) ApprovalCost() uint64 {
	return operation.CostMicrounits * uint64(operation.ResourceUnits)
}

// AuthorizesProvider reports whether the owner signed this operation for the
// exact adapter.
func (operation Operation) AuthorizesProvider(provider string) bool {
	for _, candidate := range operation.Providers {
		if candidate == provider {
			return true
		}
	}
	return false
}

// RetryPolicy bounds attempts and declares which outcomes may retry.
type RetryPolicy struct {
	MaxAttempts uint16
	Backoff     time.Duration
	RetryOn     []string
}

// IdempotencyStrategy defines the stable identity used across retries and wakes.
type IdempotencyStrategy struct {
	Scope      string
	KeyFields  []string
	ProviderID bool
}

// ScheduleEligibility bounds the wake reasons under which the skill may run.
type ScheduleEligibility struct {
	WakeReasons []string
	QuietHours  bool
}

// ResourceEstimate declares bounded cost used by deterministic admission.
type ResourceEstimate struct {
	MaxDuration time.Duration
	ModelCalls  uint16
	EffectCalls uint16
	CostMicros  uint64
	MemoryBytes uint64
}

// Contract is the complete immutable executable skill contract.
type Contract struct {
	SchemaVersion       string
	ID                  contracts.SkillID
	Version             uint64
	InputSchema         json.RawMessage
	OutputSchema        json.RawMessage
	Capabilities        []string
	DataScopes          []string
	Preconditions       []string
	Operations          []Operation
	Postconditions      []string
	VerifierDigest      contracts.ContentHash
	Probe               *ProbeContract
	Retry               RetryPolicy
	Idempotency         IdempotencyStrategy
	Approvals           []string
	ScheduleEligibility ScheduleEligibility
	Resources           ResourceEstimate
	Digest              contracts.ContentHash
}

// DriftBlind reports whether effectful execution lacks an authoritative probe.
func (contract Contract) DriftBlind() bool {
	effectful := false
	for _, operation := range contract.Operations {
		if operation.EffectClass != EffectRead {
			effectful = true
			break
		}
	}
	return effectful && (contract.Probe == nil || !contract.Probe.ReadOnly ||
		!contract.Probe.Authoritative)
}

// AuthorizeUnattended rejects high-autonomy execution of drift-blind effects.
func (contract Contract) AuthorizeUnattended(highAutonomy bool) error {
	if highAutonomy && contract.DriftBlind() {
		return ErrDriftBlindAutonomy
	}
	return nil
}

// Validate enforces the complete versioned skill contract and its declared digest.
func (contract Contract) Validate() error {
	if contract.SchemaVersion != contracts.SchemaVersionV1 {
		return invalid("unsupported schema version")
	}
	if err := token("skill_id", string(contract.ID)); err != nil {
		return err
	}
	if contract.Version == 0 {
		return invalid("version must be positive")
	}
	if err := schema("input_schema", contract.InputSchema); err != nil {
		return err
	}
	if err := schema("output_schema", contract.OutputSchema); err != nil {
		return err
	}
	if err := nonemptyTokens("capabilities", contract.Capabilities, 64); err != nil {
		return err
	}
	if err := nonemptyTokens("data_scopes", contract.DataScopes, 64); err != nil {
		return err
	}
	if err := boundedTexts("preconditions", contract.Preconditions, 64); err != nil {
		return err
	}
	if err := boundedTexts("postconditions", contract.Postconditions, 64); err != nil {
		return err
	}
	if len(contract.Operations) == 0 || len(contract.Operations) > 64 {
		return invalid("operations must contain 1 to 64 entries")
	}
	seenOperations := make(map[string]bool, len(contract.Operations))
	for _, operation := range contract.Operations {
		if err := operation.validate(); err != nil {
			return err
		}
		if seenOperations[operation.Name] {
			return invalid("operation names must be unique")
		}
		seenOperations[operation.Name] = true
	}
	if err := contract.VerifierDigest.Validate(); err != nil {
		return invalid("verifier digest: " + err.Error())
	}
	if contract.Probe != nil {
		if err := contract.Probe.validate(seenOperations); err != nil {
			return err
		}
	}
	if contract.Retry.MaxAttempts == 0 || contract.Retry.MaxAttempts > 16 ||
		contract.Retry.Backoff < 0 || contract.Retry.Backoff > 24*time.Hour {
		return invalid("retry policy is outside bounds")
	}
	if err := boundedTokens("retry_on", contract.Retry.RetryOn, 32, true); err != nil {
		return err
	}
	if err := token("idempotency_scope", contract.Idempotency.Scope); err != nil {
		return err
	}
	if err := nonemptyTokens("idempotency_key_fields", contract.Idempotency.KeyFields, 32); err != nil {
		return err
	}
	if err := boundedTokens("approvals", contract.Approvals, 64, true); err != nil {
		return err
	}
	if err := nonemptyTokens("wake_reasons", contract.ScheduleEligibility.WakeReasons, 32); err != nil {
		return err
	}
	if contract.Resources.MaxDuration <= 0 || contract.Resources.MaxDuration > 2*time.Hour ||
		contract.Resources.MemoryBytes == 0 || contract.Resources.MemoryBytes > 2<<30 {
		return invalid("resource estimate is outside bounds")
	}
	if contract.Resources.ModelCalls > 256 || contract.Resources.EffectCalls > 256 {
		return invalid("resource call estimate is outside bounds")
	}
	expected, err := contract.ComputeDigest()
	if err != nil {
		return err
	}
	if contract.Digest != expected {
		return invalid("digest does not match canonical contract")
	}
	return nil
}

// ComputeDigest returns the canonical SHA-256 digest with the digest field omitted.
func (contract Contract) ComputeDigest() (contracts.ContentHash, error) {
	copyContract := contract
	copyContract.Digest = contracts.ContentHash{}
	copyContract.Capabilities = sortedCopy(copyContract.Capabilities)
	copyContract.DataScopes = sortedCopy(copyContract.DataScopes)
	copyContract.Approvals = sortedCopy(copyContract.Approvals)
	copyContract.Retry.RetryOn = sortedCopy(copyContract.Retry.RetryOn)
	copyContract.Idempotency.KeyFields = sortedCopy(copyContract.Idempotency.KeyFields)
	copyContract.ScheduleEligibility.WakeReasons = sortedCopy(copyContract.ScheduleEligibility.WakeReasons)
	for index := range copyContract.Operations {
		copyContract.Operations[index].DataScopes = sortedCopy(copyContract.Operations[index].DataScopes)
	}
	sort.Slice(copyContract.Operations, func(left, right int) bool {
		return copyContract.Operations[left].Name < copyContract.Operations[right].Name
	})
	encoded, err := json.Marshal(copyContract)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("%w: canonical encode: %v", ErrInvalidContract, err)
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func (operation Operation) validate() error {
	if err := token("operation", operation.Name); err != nil {
		return err
	}
	if !operation.EffectClass.Valid() {
		return invalid("operation effect class is invalid")
	}
	if err := schema("operation input_schema", operation.InputSchema); err != nil {
		return err
	}
	if err := schema("operation output_schema", operation.OutputSchema); err != nil {
		return err
	}
	if err := token("operation capability", operation.Capability); err != nil {
		return err
	}
	if err := nonemptyTokens("operation data_scopes", operation.DataScopes, 32); err != nil {
		return err
	}
	if err := token("operation idempotency_field", operation.IdempotencyField); err != nil {
		return err
	}
	if operation.EffectClass == EffectReversible && strings.TrimSpace(operation.Compensation) == "" {
		return invalid("reversible operation requires compensation")
	}
	if operation.Compensation != "" {
		if err := token("operation compensation", operation.Compensation); err != nil {
			return err
		}
	}
	if operation.ResourceUnits == 0 {
		return invalid("operation resource_units must be positive")
	}
	if err := nonemptyTokens("operation providers", operation.Providers, 32); err != nil {
		return err
	}
	switch operation.EffectClass {
	case EffectIrreversible:
		if operation.CostMicrounits == 0 ||
			operation.ApprovalCost()/uint64(operation.ResourceUnits) != operation.CostMicrounits {
			return invalid("irreversible operation requires a bounded signed cost")
		}
	default:
		if operation.CostMicrounits != 0 {
			return invalid("only an irreversible operation carries approval cost")
		}
	}
	return nil
}

func (probe ProbeContract) validate(operations map[string]bool) error {
	if err := token("probe operation", probe.Operation); err != nil {
		return err
	}
	if !operations[probe.Operation] {
		return invalid("probe operation is not declared by the skill")
	}
	if err := schema("probe output_schema", probe.OutputSchema); err != nil {
		return err
	}
	if err := token("probe authority", probe.Authority); err != nil {
		return err
	}
	if probe.Timeout <= 0 || probe.Timeout > 5*time.Minute ||
		!probe.ReadOnly || !probe.Authoritative {
		return invalid("probe must be read-only, authoritative, and bounded")
	}
	if err := probe.VerifierDigest.Validate(); err != nil {
		return invalid("probe verifier digest: " + err.Error())
	}
	if probe.UnavailableMeans != ProbeUnknown {
		return invalid("unavailable probe authority must map to unknown")
	}
	return nil
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidContract, message)
}

func token(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return invalid(name + " must contain 1 to 128 bytes")
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' || char == '/' {
			continue
		}
		return invalid(name + " contains an invalid character")
	}
	return nil
}

func schema(name string, value json.RawMessage) error {
	if len(value) == 0 || len(value) > 64<<10 || !json.Valid(value) {
		return invalid(name + " must be bounded valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil || object == nil {
		return invalid(name + " must be a JSON object")
	}
	return nil
}

func nonemptyTokens(name string, values []string, limit int) error {
	return boundedTokens(name, values, limit, false)
}

func boundedTokens(name string, values []string, limit int, emptyAllowed bool) error {
	if (!emptyAllowed && len(values) == 0) || len(values) > limit {
		return invalid(fmt.Sprintf("%s must contain %d to %d entries", name, boolInt(!emptyAllowed), limit))
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := token(name, value); err != nil {
			return err
		}
		if seen[value] {
			return invalid(name + " contains duplicates")
		}
		seen[value] = true
	}
	return nil
}

func boundedTexts(name string, values []string, limit int) error {
	if len(values) == 0 || len(values) > limit {
		return invalid(name + " is empty or exceeds its entry limit")
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 4096 {
			return invalid(name + " contains an invalid entry")
		}
	}
	return nil
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
