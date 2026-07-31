package controlapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

func TestSignedCommandAndStaticAuthentication(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	command := SignedCommand{
		SchemaVersion: SchemaVersion, ID: "command:one",
		OrganizationID: "organization:one", OwnerID: "owner:one",
		Action: "set_policy", ResourceKind: "policy", ResourceID: "policy:one",
		ExpectedVersion: 0, Change: json.RawMessage(`{"enabled":true}`),
		EffectiveAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
	}
	if err := SignCommand(&command, "key:owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := verifyCommand(command, "key:owner", publicKey); err != nil {
		t.Fatal(err)
	}
	tampered := command
	tampered.Change = json.RawMessage(`{"enabled":false}`)
	if err := verifyCommand(tampered, "key:owner", publicKey); err == nil {
		t.Fatal("tampered command signature was accepted")
	}
	principal := Principal{
		TenantID: "tenant:one", OrganizationID: "organization:one", OwnerID: "owner:one",
	}
	auth, err := NewStaticAuthenticator(map[string]Principal{"owner-token": principal})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/v1/workforce/graph", nil)
	request.Header.Set("Authorization", "Bearer owner-token")
	opened, err := auth.Authenticate(request)
	if err != nil || opened != principal {
		t.Fatalf("principal = %#v, %v", opened, err)
	}
}

func TestLifecycleEventsRejectSecretsReasoningAndFalseCompletion(t *testing.T) {
	base := LifecycleEvent{
		SchemaVersion: SchemaVersion, ID: "event:one",
		OrganizationID: "organization:one", Type: "intent.progressed",
		ResourceKind: "intent", ResourceID: "intent:one",
		Fields:    map[string]any{"status": "progressed"},
		CreatedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
	}
	if err := validateEvent(base); err != nil {
		t.Fatal(err)
	}
	falseCompletion := base
	falseCompletion.Type = "intent.completed"
	if err := validateEvent(falseCompletion); err == nil {
		t.Fatal("completion without verified receipt was accepted")
	}
	secret := base
	secret.Fields = map[string]any{"credential_token": "not-for-the-wire"}
	if err := validateEvent(secret); err == nil {
		t.Fatal("credential content was accepted")
	}
	reasoning := base
	reasoning.Fields = map[string]any{"reasoning": "private"}
	if err := validateEvent(reasoning); err == nil {
		t.Fatal("reasoning content was accepted")
	}
	completed := base
	completed.Type = "intent.completed"
	completed.VerifiedCompletion = true
	completed.ReceiptID = contracts.ReceiptID("receipt:one")
	completed.Fields = map[string]any{"status": "completed"}
	if err := validateEvent(completed); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerDropsSlowSubscriberAndKeepsFastSubscriber(t *testing.T) {
	value := newBroker(1)
	slow, cancelSlow := value.subscribe("tenant")
	defer cancelSlow()
	fast, cancelFast := value.subscribe("tenant")
	defer cancelFast()
	first := LifecycleEvent{Cursor: 1}
	value.publish("tenant", first)
	if event := <-fast.events; event.Cursor != 1 {
		t.Fatalf("fast event cursor = %d", event.Cursor)
	}
	value.publish("tenant", LifecycleEvent{Cursor: 2})
	if !slow.dropped.Load() {
		t.Fatal("slow subscriber was not dropped at its bounded capacity")
	}
	if event := <-fast.events; event.Cursor != 2 {
		t.Fatalf("fast second cursor = %d", event.Cursor)
	}
	value.publish("tenant", LifecycleEvent{Cursor: 3})
	if event := <-fast.events; event.Cursor != 3 || fast.dropped.Load() {
		t.Fatalf("fast third event = %#v, dropped=%v", event, fast.dropped.Load())
	}
}
