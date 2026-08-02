package controlapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

type nonFlushingResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (writer *nonFlushingResponseWriter) Header() http.Header    { return writer.header }
func (writer *nonFlushingResponseWriter) WriteHeader(status int) { writer.status = status }
func (writer *nonFlushingResponseWriter) Write(value []byte) (int, error) {
	return writer.body.Write(value)
}

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
	invalidPayload := command
	invalidPayload.Change = json.RawMessage(`{`)
	if err := invalidPayload.Validate(); err == nil {
		t.Fatal("invalid command JSON was accepted")
	}
	invalidPayload = command
	invalidPayload.Change = nil
	if err := invalidPayload.Validate(); err == nil {
		t.Fatal("empty command change was accepted")
	}
	invalidPayload = command
	invalidPayload.Signature.Value = "invalid"
	if err := invalidPayload.Validate(); err == nil {
		t.Fatal("invalid command signature encoding was accepted")
	}
	incomplete := command
	incomplete.Action = "unknown"
	if err := incomplete.Validate(); err == nil {
		t.Fatal("unknown command action was accepted")
	}
	if err := SignCommand(nil, "key:owner", privateKey); err == nil {
		t.Fatal("nil command was signed")
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

func TestControlValidationAndCursorBoundaries(t *testing.T) {
	if _, err := NewStaticAuthenticator(nil); err == nil {
		t.Fatal("empty authenticator was accepted")
	}
	if _, err := NewStaticAuthenticator(map[string]Principal{"": {}}); err == nil {
		t.Fatal("incomplete authentication binding was accepted")
	}
	auth, err := NewStaticAuthenticator(map[string]Principal{"token": {
		TenantID: "tenant", OrganizationID: "organization", OwnerID: "owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "Basic token", "Bearer unknown"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Authorization", header)
		if _, err := auth.Authenticate(request); err == nil {
			t.Fatalf("authentication header %q was accepted", header)
		}
	}

	cursor := encodePageCursor("departments", 7)
	if offset, err := decodePageCursor("departments", cursor); err != nil || offset != 7 {
		t.Fatalf("cursor offset=%d error=%v", offset, err)
	}
	for _, value := range []string{"not-base64", encodePageCursor("seats", 7),
		base64.RawURLEncoding.EncodeToString([]byte("departments:not-a-number"))} {
		if _, err := decodePageCursor("departments", value); err == nil {
			t.Fatalf("invalid cursor %q was accepted", value)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/?limit=9&after=12", nil)
	if value, err := queryInt(request, "limit", 1); err != nil || value != 9 {
		t.Fatalf("limit=%d error=%v", value, err)
	}
	if value, err := eventCursor(request); err != nil || value != 12 {
		t.Fatalf("event cursor=%d error=%v", value, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/?limit=bad&after=bad", nil)
	if _, err := queryInt(request, "limit", 1); err == nil {
		t.Fatal("invalid query integer was accepted")
	}
	if _, err := eventCursor(request); err == nil {
		t.Fatal("invalid event cursor was accepted")
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Last-Event-ID", "14")
	if value, err := eventCursor(request); err != nil || value != 14 {
		t.Fatalf("Last-Event-ID cursor=%d error=%v", value, err)
	}
	writer := &nonFlushingResponseWriter{header: make(http.Header)}
	(&Service{}).handleEventStream(writer, httptest.NewRequest(http.MethodGet, "/", nil))
	if writer.status != http.StatusInternalServerError {
		t.Fatalf("non-streaming writer status=%d body=%s", writer.status, writer.body.String())
	}
}

func TestFounderRotationAndConstructorRejectInvalidAuthority(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rotation := FounderKeyRotation{
		SchemaVersion: SchemaVersion, OrganizationID: "organization",
		OwnerID: "owner", ExpectedVersion: 1, EffectiveAt: now,
		OldKeyID: "key:old", NewKeyID: "key:new",
		NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublic),
	}
	if err := SignFounderKeyRotation(&rotation, oldPrivate, newPrivate); err != nil {
		t.Fatal(err)
	}
	if err := verifyFounderKeyRotation(rotation, oldPublic, newPublic); err != nil {
		t.Fatal(err)
	}
	wrongPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFounderKeyRotation(rotation, oldPublic, wrongPublic); err == nil {
		t.Fatal("mismatched new public key was accepted")
	}
	tampered := rotation
	tampered.ExpectedVersion++
	if err := verifyFounderKeyRotation(tampered, oldPublic, newPublic); err == nil {
		t.Fatal("tampered rotation was accepted")
	}
	invalid := rotation
	invalid.NewKeyID = invalid.OldKeyID
	if err := invalid.Validate(); err == nil {
		t.Fatal("same-key rotation was accepted")
	}
	if err := SignFounderKeyRotation(nil, oldPrivate, newPrivate); err == nil {
		t.Fatal("nil rotation was signed")
	}

	if _, err := New(nil, nil, nil, nil, 0); err == nil {
		t.Fatal("service without dependencies was accepted")
	}
	service := &Service{runtimeKeyID: "key:runtime", companyIssuerKeyID: "key:issuer"}
	if err := service.AttachVault(nil); err == nil {
		t.Fatal("nil Vault was attached")
	}
	if err := service.AttachScheduler(nil); err == nil {
		t.Fatal("nil scheduler was attached")
	}
	if err := service.AttachRuntimeAuthority("", nil); err == nil {
		t.Fatal("invalid runtime authority was attached")
	}
	if err := service.AttachRuntimeAuthority("key:issuer", newPublic); err == nil {
		t.Fatal("issuer key was reused as runtime authority")
	}
	if err := service.AttachCompanyIssuerAuthority("key:runtime", newPublic); err == nil {
		t.Fatal("runtime key was reused as issuer authority")
	}
	if err := service.AttachRuntimeModel("", ""); err == nil {
		t.Fatal("empty runtime model was attached")
	}
	if _, err := decodePublicKey("invalid"); err == nil {
		t.Fatal("invalid public key was decoded")
	}
}

func TestWorkOrderSigningFacadeUsesRealCanonicalContract(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	order := WorkOrder{
		SchemaVersion: "workforce.work-order.v1", ID: "work-order:unit",
		OrganizationID: "organization", OwnerID: "owner", Version: 1,
		Objective: "Produce verified evidence", Scope: "/root/matrix",
		ProjectID: "project:matrix", WorkspaceID: "workspace:matrix",
		ScopeFiles:   []string{"workforce/internal/controlapi/workorders.go"},
		ScopeSymbols: []string{"SignWorkOrder"},
		Departments:  []contracts.DepartmentKind{contracts.DepartmentDeveloper},
		Priority:     1, Budget: WorkOrderBudget{MaxTasks: 1, MaxSpendMicrounits: 1},
		Deadline: now.Add(time.Hour), Autonomy: "supervised",
		AcceptanceCriteria: []string{"signature verifies against the exact canonical payload"},
		ModelProvider:      "mimo", ModelID: "mimo-v2.5-pro",
		MGSReference: "mgs:workforce:v1", MGSDigest: strings.Repeat("a", 64),
		CreatedAt: now, IdempotencyKey: "work-order:unit",
	}
	if err := SignWorkOrder(&order, "key:owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := verifyWorkOrder(order, OwnerKey{KeyID: "key:owner", PublicKey: publicKey}); err != nil {
		t.Fatal(err)
	}
	order.Objective = "tampered"
	if err := verifyWorkOrder(order, OwnerKey{KeyID: "key:owner", PublicKey: publicKey}); err == nil {
		t.Fatal("tampered Work Order was accepted")
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
	nestedSecret := base
	nestedSecret.Fields = map[string]any{
		"observations": []any{map[string]any{"api_key": "must-not-leak"}},
	}
	if err := validateEvent(nestedSecret); err == nil {
		t.Fatal("nested secret content was accepted")
	}
	nestedCompletion := base
	nestedCompletion.Fields = map[string]any{
		"result": []any{map[string]any{"status": "completed"}},
	}
	if err := validateEvent(nestedCompletion); err == nil {
		t.Fatal("nested completion without a verified receipt was accepted")
	}
	verifiedWithoutReceipt := base
	verifiedWithoutReceipt.VerifiedCompletion = true
	if err := validateEvent(verifiedWithoutReceipt); err == nil {
		t.Fatal("verified event without receipt was accepted")
	}
	if err := decodeTypedChange([]byte(`{} {}`), &struct{}{}); err == nil {
		t.Fatal("trailing command change was accepted")
	}
	if _, err := canonicalJSON([]byte(`{`)); err == nil {
		t.Fatal("invalid canonical JSON was accepted")
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
