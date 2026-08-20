package skills

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestContract_ValidatesCanonicalDigestAndDriftCeiling(t *testing.T) {
	contract := validContract()
	digest, err := contract.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	contract.Digest = digest
	if err := contract.Validate(); err != nil {
		t.Fatalf("valid contract rejected: %v", err)
	}
	if contract.DriftBlind() {
		t.Fatal("authoritative probed contract marked drift blind")
	}
	contract.Probe = nil
	if !contract.DriftBlind() {
		t.Fatal("effectful contract without probe was not marked drift blind")
	}
	if err := contract.AuthorizeUnattended(true); !errors.Is(err, ErrDriftBlindAutonomy) {
		t.Fatalf("high autonomy drift-blind execution error = %v", err)
	}
	if err := contract.AuthorizeUnattended(false); err != nil {
		t.Fatalf("bounded supervised execution rejected: %v", err)
	}
}

func TestContract_RejectsInvalidBoundariesAndDigestDrift(t *testing.T) {
	base := validContract()
	cases := []struct {
		name   string
		mutate func(*Contract)
	}{
		{"schema_version", func(value *Contract) { value.SchemaVersion = "v2" }},
		{"id", func(value *Contract) { value.ID = "" }},
		{"version", func(value *Contract) { value.Version = 0 }},
		{"input_schema", func(value *Contract) { value.InputSchema = json.RawMessage(`[]`) }},
		{"output_schema", func(value *Contract) { value.OutputSchema = nil }},
		{"capabilities", func(value *Contract) { value.Capabilities = nil }},
		{"data_scopes", func(value *Contract) { value.DataScopes = []string{"bad value"} }},
		{"preconditions", func(value *Contract) { value.Preconditions = nil }},
		{"postconditions", func(value *Contract) { value.Postconditions = []string{""} }},
		{"operations", func(value *Contract) { value.Operations = nil }},
		{"duplicate_operation", func(value *Contract) { value.Operations = append(value.Operations, value.Operations[0]) }},
		{"effect_class", func(value *Contract) { value.Operations[0].EffectClass = "write" }},
		{"operation_schema", func(value *Contract) { value.Operations[0].InputSchema = json.RawMessage(`no`) }},
		{"operation_capability", func(value *Contract) { value.Operations[0].Capability = "" }},
		{"operation_scopes", func(value *Contract) { value.Operations[0].DataScopes = nil }},
		{"operation_idempotency", func(value *Contract) { value.Operations[0].IdempotencyField = "" }},
		{"compensation", func(value *Contract) { value.Operations[0].Compensation = "" }},
		{"resource_units", func(value *Contract) { value.Operations[0].ResourceUnits = 0 }},
		{"verifier", func(value *Contract) { value.VerifierDigest = contracts.ContentHash{} }},
		{"probe_operation", func(value *Contract) { value.Probe.Operation = "missing" }},
		{"probe_schema", func(value *Contract) { value.Probe.OutputSchema = nil }},
		{"probe_authority", func(value *Contract) { value.Probe.Authority = "" }},
		{"probe_timeout", func(value *Contract) { value.Probe.Timeout = 0 }},
		{"probe_readonly", func(value *Contract) { value.Probe.ReadOnly = false }},
		{"probe_verifier", func(value *Contract) { value.Probe.VerifierDigest = contracts.ContentHash{} }},
		{"probe_unavailable", func(value *Contract) { value.Probe.UnavailableMeans = ProbeDrifted }},
		{"retry_attempts", func(value *Contract) { value.Retry.MaxAttempts = 0 }},
		{"retry_backoff", func(value *Contract) { value.Retry.Backoff = 25 * time.Hour }},
		{"retry_tokens", func(value *Contract) { value.Retry.RetryOn = []string{"bad value"} }},
		{"idempotency_scope", func(value *Contract) { value.Idempotency.Scope = "" }},
		{"idempotency_fields", func(value *Contract) { value.Idempotency.KeyFields = nil }},
		{"approvals", func(value *Contract) { value.Approvals = []string{"duplicate", "duplicate"} }},
		{"wake_reasons", func(value *Contract) { value.ScheduleEligibility.WakeReasons = nil }},
		{"resource_duration", func(value *Contract) { value.Resources.MaxDuration = 0 }},
		{"resource_memory", func(value *Contract) { value.Resources.MemoryBytes = 0 }},
		{"resource_calls", func(value *Contract) { value.Resources.ModelCalls = 257 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			value := cloneContract(base)
			test.mutate(&value)
			digest, _ := value.ComputeDigest()
			value.Digest = digest
			if err := value.Validate(); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("invalid contract error = %v", err)
			}
		})
	}
	base.Digest = contracts.ContentHash{Algorithm: "sha256", Digest: string(make([]byte, 64))}
	if err := base.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("digest drift error = %v", err)
	}
}

func TestProbeOutcome_RecognizesClosedSet(t *testing.T) {
	for _, outcome := range []ProbeOutcome{
		ProbeUnchanged, ProbeCompletedOutOfBand, ProbeReversed,
		ProbeDrifted, ProbeConflicted, ProbeUnknown,
	} {
		if !outcome.Valid() {
			t.Fatalf("valid outcome %q rejected", outcome)
		}
	}
	if ProbeOutcome("other").Valid() {
		t.Fatal("unknown outcome accepted")
	}
	for _, class := range []EffectClass{EffectRead, EffectReversible, EffectIrreversible} {
		if !class.Valid() {
			t.Fatalf("valid effect class %q rejected", class)
		}
	}
	if EffectClass("write").Valid() {
		t.Fatal("unknown effect class accepted")
	}
}

func validContract() Contract {
	hash := contracts.ContentHash{
		Algorithm: "sha256",
		Digest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	return Contract{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "skill.publish",
		Version:        1,
		InputSchema:    json.RawMessage(`{"type":"object"}`),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
		Capabilities:   []string{"publish"},
		DataScopes:     []string{"channel/public"},
		Preconditions:  []string{"approved"},
		Postconditions: []string{"observable"},
		Operations: []Operation{{
			Name: "publish", EffectClass: EffectReversible,
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability: "publish", DataScopes: []string{"channel/public"},
			IdempotencyField: "intent_id", Compensation: "unpublish", ResourceUnits: 1,
			Providers: []string{"publisher"},
		}, {
			Name: "probe", EffectClass: EffectRead,
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability: "observe", DataScopes: []string{"channel/public"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"publisher"},
		}},
		VerifierDigest: hash,
		Probe: &ProbeContract{
			Operation: "probe", OutputSchema: json.RawMessage(`{"type":"object"}`),
			Authority: "provider", Timeout: time.Minute, ReadOnly: true,
			Authoritative: true, VerifierDigest: hash, UnavailableMeans: ProbeUnknown,
		},
		Retry:       RetryPolicy{MaxAttempts: 3, Backoff: time.Second, RetryOn: []string{"unavailable"}},
		Idempotency: IdempotencyStrategy{Scope: "provider", KeyFields: []string{"intent_id"}, ProviderID: true},
		Approvals:   []string{"publish_external"},
		ScheduleEligibility: ScheduleEligibility{
			WakeReasons: []string{"scheduled", "dependency"},
		},
		Resources: ResourceEstimate{
			MaxDuration: time.Minute, ModelCalls: 1, EffectCalls: 1,
			CostMicros: 10, MemoryBytes: 1 << 20,
		},
	}
}

func cloneContract(source Contract) Contract {
	encoded, _ := json.Marshal(source)
	var result Contract
	_ = json.Unmarshal(encoded, &result)
	return result
}
