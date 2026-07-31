package skills

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

func TestCatalogBindsImmutableCurrentSkillVersions(t *testing.T) {
	first := catalogContract(t, "plan", 1)
	second := catalogContract(t, "implement", 3)
	catalog, err := NewCatalog([]Contract{first, second})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := catalog.Resolve([]contracts.SkillID{"implement", "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Version != 3 || refs[1].Version != 1 {
		t.Fatalf("unexpected resolved refs: %#v", refs)
	}
	changed := second
	changed.Version = 4
	changed.Digest = contracts.ContentHash{}
	changed.Digest, err = changed.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	changedCatalog, err := NewCatalog([]Contract{first, changed})
	if err != nil {
		t.Fatal(err)
	}
	if changedCatalog.Digest() == catalog.Digest() {
		t.Fatal("material skill version change did not change the catalog digest")
	}
	if _, err := catalog.Resolve([]contracts.SkillID{"unknown"}); err == nil {
		t.Fatal("catalog resolved an unknown skill")
	}
}

func catalogContract(t *testing.T, id contracts.SkillID, version uint64) Contract {
	t.Helper()
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)}
	value := Contract{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            id, Version: version,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
		Capabilities: []string{"read"}, DataScopes: []string{"project"},
		Preconditions: []string{"lease active"},
		Operations: []Operation{{
			Name: "inspect", EffectClass: EffectRead,
			InputSchema:  json.RawMessage(`{"type":"object"}`),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
			Capability:   "read", DataScopes: []string{"project"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"filesystem"},
		}},
		Postconditions: []string{"evidence produced"},
		VerifierDigest: hash,
		Retry:          RetryPolicy{MaxAttempts: 1},
		Idempotency: IdempotencyStrategy{
			Scope: "intent", KeyFields: []string{"intent_id"},
		},
		ScheduleEligibility: ScheduleEligibility{WakeReasons: []string{"eligible_work"}},
		Resources: ResourceEstimate{
			MaxDuration: time.Minute, ModelCalls: 1, MemoryBytes: 1 << 20,
		},
	}
	var err error
	value.Digest, err = value.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
