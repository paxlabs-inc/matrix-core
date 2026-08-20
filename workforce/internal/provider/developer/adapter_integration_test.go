package developer

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
)

func TestIntegration_DeveloperProviderReadsRealCodeGraph(t *testing.T) {
	codegraph, err := exec.LookPath("cg")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	adapter, err := New(repository, codegraph, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	grant := lease.Grant{
		Request: lease.Request{
			ID: "lease:developer-provider", WakeID: "wake:developer-provider",
			OrganizationID: "organization:developer-provider",
			SeatID:         "seat:developer-lead",
			NodeID:         dependency.NodeID("intent:developer-provider"),
			MandateID:      "mandate:developer-lead", MandateVersion: 1,
			Policies: []contracts.PolicyRef{{
				ID: "policy:baseline", Version: 1,
				Hash: contracts.ContentHash{
					Algorithm: "sha256",
					Digest:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			}},
			IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		Fence: 1, State: lease.StateActive,
	}
	input, err := json.Marshal(inputEnvelope{
		SchemaVersion: contracts.SchemaVersionV1, Grant: grant,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Dispatch(context.Background(), effect.Operation{
		OrganizationID: grant.OrganizationID, SeatID: grant.SeatID,
		LeaseID: grant.ID, Fence: grant.Fence,
		Name: "plan_change", IdempotencyKey: "developer-provider-read",
		Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || result.ExternalID == "" ||
		len(result.Observation) == 0 || !result.ObservedAt.Equal(now) {
		t.Fatalf("real provider observation is incomplete: %#v", result)
	}
}
