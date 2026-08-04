package developer_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/controlapi"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/developer"
)

func TestIntegration_ControlAPIAuthorizationReplayPaginationAndStaleMutation(t *testing.T) {
	ctx := context.Background()
	now := developer.ControlAPITestNow()
	principalA := controlapi.Principal{
		TenantID: "tenant:control-a", OrganizationID: "organization:control-a",
		OwnerID: "owner:control-a",
	}
	principalB := controlapi.Principal{
		TenantID: "tenant:control-b", OrganizationID: "organization:control-b",
		OwnerID: "owner:control-b",
	}
	graphA, err := dependency.New(developer.ControlAPITestPool(), principalA.TenantID, developer.ControlAPITestNow)
	if err != nil {
		t.Fatal(err)
	}
	graphB, err := dependency.New(developer.ControlAPITestPool(), principalB.TenantID, developer.ControlAPITestNow)
	if err != nil {
		t.Fatal(err)
	}
	for index, id := range []dependency.NodeID{"goal:control-a:one", "goal:control-a:two"} {
		if err := graphA.PutNode(ctx, controlNode(principalA.OrganizationID, id, now.Add(time.Duration(index)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if err := graphB.PutNode(ctx, controlNode(
		principalB.OrganizationID, "goal:control-b:private", now,
	)); err != nil {
		t.Fatal(err)
	}
	auth, err := controlapi.NewStaticAuthenticator(map[string]controlapi.Principal{
		"token-control-a": principalA, "token-control-b": principalB,
	})
	if err != nil {
		t.Fatal(err)
	}
	publicA, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := controlapi.New(
		developer.ControlAPITestPool(), auth,
		map[string]controlapi.OwnerKey{
			principalA.TenantID: {KeyID: "key:control-a", PublicKey: publicA},
			principalB.TenantID: {KeyID: "key:control-b", PublicKey: publicB},
		},
		developer.ControlAPITestNow, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	defer server.Close()

	for _, resource := range []string{
		"mail", "approvals", "receipts", "policies", "project-brain",
		"corrections", "audit-disagreements", "replay-lineage", "effect-status",
	} {
		page := controlGET[controlapi.ResourcePage](
			t, server.URL+"/v1/workforce/"+resource+"?limit=25", "token-control-a",
		)
		if page.Resource != resource || page.SchemaVersion != controlapi.SchemaVersion {
			t.Fatalf("%s projection identity = %#v", resource, page)
		}
		for _, item := range page.Items {
			if strings.Contains(item.ID, "control-b") {
				t.Fatalf("%s projection leaked tenant B item %#v", resource, item)
			}
		}
	}

	first := controlGET[controlapi.ResourcePage](
		t, server.URL+"/v1/workforce/work-orders?limit=1", "token-control-a",
	)
	if len(first.Items) != 1 || first.NextCursor == "" ||
		strings.Contains(first.Items[0].ID, "control-b") {
		t.Fatalf("first tenant page = %#v", first)
	}
	second := controlGET[controlapi.ResourcePage](
		t, server.URL+"/v1/workforce/work-orders?limit=1&cursor="+first.NextCursor,
		"token-control-a",
	)
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID ||
		strings.Contains(second.Items[0].ID, "control-b") {
		t.Fatalf("second tenant page = %#v", second)
	}
	unauthorized, err := http.Get(server.URL + "/v1/workforce/work-orders")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", unauthorized.StatusCode)
	}

	eventOne, err := service.Publish(ctx, principalA, controlapi.LifecycleEvent{
		ID: "event:control-a:one", OrganizationID: principalA.OrganizationID,
		Type: "intent.progressed", ResourceKind: "intent",
		ResourceID: "intent:control-a", ResourceVersion: 1,
		Fields: map[string]any{"status": "progressed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, principalB, controlapi.LifecycleEvent{
		ID: "event:control-b:private", OrganizationID: principalB.OrganizationID,
		Type: "intent.progressed", ResourceKind: "intent",
		ResourceID: "intent:control-b", ResourceVersion: 1,
		Fields: map[string]any{"status": "progressed"},
	}); err != nil {
		t.Fatal(err)
	}
	eventTwo, err := service.Publish(ctx, principalA, controlapi.LifecycleEvent{
		ID: "event:control-a:two", OrganizationID: principalA.OrganizationID,
		Type: "intent.waiting", ResourceKind: "intent",
		ResourceID: "intent:control-a", ResourceVersion: 2,
		Fields: map[string]any{"status": "waiting_dependency"},
	})
	if err != nil {
		t.Fatal(err)
	}
	replay := controlGET[controlapi.EventPage](
		t, server.URL+"/v1/workforce/events?after="+uintString(eventOne.Cursor),
		"token-control-a",
	)
	if len(replay.Events) != 1 || replay.Events[0].Cursor != eventTwo.Cursor ||
		strings.Contains(replay.Events[0].ResourceID, "control-b") {
		t.Fatalf("tenant replay = %#v", replay)
	}
	assertSSEReplay(t, server.URL, "token-control-a", eventOne.Cursor, eventTwo.Cursor)

	browserPublic, browserPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if status := registerControlKey(
		t, server.URL, "token-control-a", "key:browser-a", browserPublic,
	); status != http.StatusCreated {
		t.Fatalf("control key registration status = %d", status)
	}
	command := controlapi.SignedCommand{
		SchemaVersion: controlapi.SchemaVersion, ID: "command:control-a:first",
		OrganizationID: principalA.OrganizationID, OwnerID: principalA.OwnerID,
		Action: "set_policy", ResourceKind: "policy", ResourceID: "policy:autonomy",
		ExpectedVersion: 0, Change: json.RawMessage(`{"value":{"autonomy":"bounded"}}`),
		EffectiveAt: developer.ControlAPITestNow(),
	}
	if err := controlapi.SignCommand(&command, "key:browser-a", browserPrivate); err != nil {
		t.Fatal(err)
	}
	if status := postControlCommand(t, server.URL, "token-control-a", command); status != http.StatusCreated {
		t.Fatalf("first command status = %d", status)
	}
	stale := command
	stale.ID = "command:control-a:stale"
	stale.Signature = contracts.Signature{}
	if err := controlapi.SignCommand(&stale, "key:browser-a", browserPrivate); err != nil {
		t.Fatal(err)
	}
	if status := postControlCommand(t, server.URL, "token-control-a", stale); status != http.StatusConflict {
		t.Fatalf("stale command status = %d", status)
	}
	if status := postControlCommand(t, server.URL, "token-control-b", command); status != http.StatusForbidden {
		t.Fatalf("cross-tenant command status = %d", status)
	}
}

func registerControlKey(
	t *testing.T,
	baseURL, token, keyID string,
	publicKey ed25519.PublicKey,
) int {
	t.Helper()
	body, err := json.Marshal(controlapi.ControlKeyRegistration{
		KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, baseURL+"/v1/workforce/control-keys", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode >= http.StatusBadRequest {
		t.Logf("POST /v1/workforce/commands = %d: %s", response.StatusCode, responseBody)
	}
	return response.StatusCode
}

func controlNode(
	organizationID contracts.OrganizationID,
	id dependency.NodeID,
	now time.Time,
) dependency.Node {
	return dependency.Node{
		ID: id, OrganizationID: organizationID, Kind: dependency.NodeGoal,
		Title: "Bounded owner Work Order", State: dependency.StateEligible,
		BasePriority: 10, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
}

func controlGET[T any](t *testing.T, endpoint, token string) T {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET %s = %d: %s", endpoint, response.StatusCode, body)
	}
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func postControlCommand(
	t *testing.T,
	baseURL, token string,
	command controlapi.SignedCommand,
) int {
	t.Helper()
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, baseURL+"/v1/workforce/commands", bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func assertSSEReplay(
	t *testing.T,
	baseURL, token string,
	after, expected uint64,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		baseURL+"/v1/workforce/events/stream?after="+uintString(after), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "id: "+uintString(expected) {
			return
		}
	}
	t.Fatalf("SSE replay did not include cursor %d: %v", expected, scanner.Err())
}

func uintString(value uint64) string {
	return strconv.FormatUint(value, 10)
}
