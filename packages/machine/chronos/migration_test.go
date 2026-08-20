package chronos

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedImporterDryRunOwnershipAndIdempotentMapping(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-import", testVault(t))
	defer store.Close()
	shared := SharedAlarm{ID: "shared-1", OwnerDID: "did:matrix:owner", Kind: "cron",
		CronExpr: "@every 45m", Timezone: "UTC", NextFireAt: time.Now().Add(time.Hour),
		WakeMessage: "AUTOMATRIX", Payload: json.RawMessage(`{"private":"value"}`),
		IdempotencyKey: "neo-automatrix-wake", Status: "active"}
	dry := store.ImportShared(ctx, []SharedAlarm{shared}, ImportOptions{OwnerDID: shared.OwnerDID, DryRun: true})
	if len(dry) != 1 || dry[0].Action != "create" {
		t.Fatalf("dry run=%+v", dry)
	}
	alarms, _ := store.List(ctx)
	if len(alarms) != 0 {
		t.Fatal("dry run mutated local alarms")
	}
	rejected := store.ImportShared(ctx, []SharedAlarm{shared}, ImportOptions{OwnerDID: "did:matrix:other"})
	if rejected[0].Action != "rejected" {
		t.Fatalf("ownership result=%+v", rejected)
	}
	first := store.ImportShared(ctx, []SharedAlarm{shared}, ImportOptions{OwnerDID: shared.OwnerDID})
	second := store.ImportShared(ctx, []SharedAlarm{shared}, ImportOptions{OwnerDID: shared.OwnerDID})
	if first[0].Action != "create" || second[0].Action != "already_mapped" || first[0].LocalID != second[0].LocalID {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	alarms, _ = store.List(ctx)
	if len(alarms) != 1 {
		t.Fatalf("alarms=%d, want 1", len(alarms))
	}
}

func TestSharedImporterMapsExistingLocalIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-map", testVault(t))
	defer store.Close()
	_, _, err := store.Create(ctx, CreateRequest{ID: "local-current", IdempotencyKey: "same-key",
		NextFire: time.Now().Add(time.Hour), MisfirePolicy: MisfireFireOnce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "local"}})
	if err != nil {
		t.Fatal(err)
	}
	result := store.ImportShared(ctx, []SharedAlarm{{ID: "shared-old", OwnerDID: "did:owner",
		Kind: "once", NextFireAt: time.Now().Add(2 * time.Hour), WakeMessage: "old",
		IdempotencyKey: "same-key", Status: "active"}}, ImportOptions{OwnerDID: "did:owner"})
	if result[0].Action != "map_existing" || result[0].LocalID != "local-current" {
		t.Fatalf("result=%+v", result)
	}
	alarms, _ := store.List(ctx)
	if len(alarms) != 1 {
		t.Fatalf("import duplicated alarm: %d", len(alarms))
	}
}

func TestRelayLeaseContainsOnlyOpaqueTimingMetadata(t *testing.T) {
	lease, err := NewRelayLease("gene-secret-value", time.Now().Add(time.Hour), 7)
	if err != nil || lease.Validate() != nil {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	encoded, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"gene-secret-value", "payload", "message", "conversation", "recurrence", "automatrix"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("relay lease leaked %q: %s", forbidden, text)
		}
	}
	var keys map[string]interface{}
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 {
		t.Fatalf("relay keys=%v", keys)
	}
}
