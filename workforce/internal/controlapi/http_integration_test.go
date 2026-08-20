package controlapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"centra/workforce/internal/policy"
)

func TestIntegration_HTTPFounderActivationRotationAndReadSurface(t *testing.T) {
	fixture := newControlFixture(t, "http-founder-surface")
	server := httptest.NewServer(fixture.service.Handler())
	t.Cleanup(server.Close)
	token := "token:http-founder-surface"

	response := controlHTTPRequest(t, server.URL+"/v1/workforce/session", token, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d", response.StatusCode)
	}
	response.Body.Close()

	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration := ControlKeyRegistration{
		KeyID:     "key:http-browser",
		PublicKey: base64.RawURLEncoding.EncodeToString(newPublic),
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/control-keys", token, registration)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("control key status = %d", response.StatusCode)
	}
	response.Body.Close()

	previewRequest := ActivationPreviewRequest{
		Name: "HTTP Founder Workforce", KeyID: registration.KeyID,
		EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/activation/preview", token, previewRequest)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("activation preview status = %d", response.StatusCode)
	}
	var preview ActivationPreview
	decodeControlResponse(t, response, &preview)
	if err := signControlActivation(&preview, registration.KeyID, newPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/activation", token, ActivationBundle{
		Seed: preview.Seed, Authority: preview.Authority, SkillContracts: preview.SkillContracts,
	})
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("activation status = %d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	order := fixture.order("http", "mimo", "mimo-v2.5-pro")
	if err := SignWorkOrder(&order, registration.KeyID, newPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/work-orders", token, order)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("work order status = %d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	for _, path := range []string{
		"/v1/workforce/company-authority?limit=10",
		"/v1/workforce/departments?limit=10",
		"/v1/workforce/events?after=0&limit=10",
	} {
		response = controlHTTPRequest(t, server.URL+path, token, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, response.StatusCode)
		}
		response.Body.Close()
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/departments?limit=1", token, nil)
	var firstPage ResourcePage
	decodeControlResponse(t, response, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first department page = %#v", firstPage)
	}
	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/departments?limit=1&cursor="+firstPage.NextCursor, token, nil,
	)
	var secondPage ResourcePage
	decodeControlResponse(t, response, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID == firstPage.Items[0].ID {
		t.Fatalf("second department page = %#v", secondPage)
	}

	streamContext, cancelStream := context.WithCancel(context.Background())
	streamRequest, err := http.NewRequestWithContext(
		streamContext, http.MethodGet, server.URL+"/v1/workforce/events/stream?after=0", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.Header.Set("Authorization", "Bearer "+token)
	streamResponse, err := http.DefaultClient.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	count, err := streamResponse.Body.Read(buffer)
	if err != nil || count == 0 || !bytes.Contains(buffer[:count], []byte("event: organization.activated")) {
		t.Fatalf("event stream bytes=%q error=%v", buffer[:count], err)
	}
	cancelStream()
	streamResponse.Body.Close()

	command := SignedCommand{
		SchemaVersion: SchemaVersion, ID: "command:http-pause",
		OrganizationID: fixture.principal.OrganizationID, OwnerID: fixture.principal.OwnerID,
		Action: "pause_company", ResourceKind: "company",
		ResourceID:  string(fixture.principal.OrganizationID),
		Change:      json.RawMessage(`{"reason":"HTTP containment qualification"}`),
		EffectiveAt: fixture.now,
	}
	if err := SignCommand(&command, registration.KeyID, newPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/commands", token, command)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("pause command status = %d", response.StatusCode)
	}
	response.Body.Close()

	changedDraft := testActivationDraft()
	changedDraft.Purpose = "HTTP-reviewed material company authority"
	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/company-authority/preview", token,
		AuthorityChangePreviewRequest{
			KeyID: registration.KeyID, ExpectedVersion: 1,
			EffectiveAt: fixture.now, Authority: changedDraft,
		},
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authority preview status = %d", response.StatusCode)
	}
	var authorityChange AuthorityChangePreview
	decodeControlResponse(t, response, &authorityChange)
	signedChange := MigrationPreview{Authority: authorityChange.Authority}
	if err := signMigrationPreview(&signedChange, registration.KeyID, newPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/company-authority", token,
		AuthorityChangeBundle{ExpectedVersion: 1, Authority: signedChange.Authority},
	)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("authority commit status = %d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	rotatedPublic, rotatedPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotationRequest := FounderKeyRotationPreviewRequest{
		OldKeyID: registration.KeyID, NewKeyID: "key:http-browser:v2",
		NewPublicKey:    base64.RawURLEncoding.EncodeToString(rotatedPublic),
		ExpectedVersion: 2, EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/founder-key/preview", token, rotationRequest)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rotation preview status = %d", response.StatusCode)
	}
	var rotation FounderKeyRotationPreview
	decodeControlResponse(t, response, &rotation)
	changed := MigrationPreview{Authority: rotation.Authority}
	if err := signMigrationPreview(&changed, rotationRequest.NewKeyID, rotatedPrivate); err != nil {
		t.Fatal(err)
	}
	rotation.Authority = changed.Authority
	if err := SignFounderKeyRotation(&rotation.Rotation, newPrivate, rotatedPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/founder-key", token, FounderKeyRotationBundle{
		Rotation: rotation.Rotation, Authority: rotation.Authority,
	})
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("rotation status = %d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
	conflictPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	staleCommand := SignedCommand{
		SchemaVersion: SchemaVersion, ID: "command:http-stale",
		OrganizationID: fixture.principal.OrganizationID, OwnerID: fixture.principal.OwnerID,
		Action: "pause_company", ResourceKind: "company",
		ResourceID: string(fixture.principal.OrganizationID), ExpectedVersion: 99,
		Change: json.RawMessage(`{"reason":"stale HTTP command"}`), EffectiveAt: fixture.now,
	}
	if err := SignCommand(&staleCommand, rotationRequest.NewKeyID, rotatedPrivate); err != nil {
		t.Fatal(err)
	}
	resumeCommand := SignedCommand{
		SchemaVersion: SchemaVersion, ID: "command:http-resume-after-rotation",
		OrganizationID: fixture.principal.OrganizationID, OwnerID: fixture.principal.OwnerID,
		Action: "resume_company", ResourceKind: "company",
		ResourceID: string(fixture.principal.OrganizationID), ExpectedVersion: 1,
		Change: json.RawMessage(`{"reason":"continue HTTP qualification"}`), EffectiveAt: fixture.now,
	}
	if err := SignCommand(&resumeCommand, rotationRequest.NewKeyID, rotatedPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/commands", token, resumeCommand)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("resume after rotation status=%d", response.StatusCode)
	}
	response.Body.Close()
	conflictingOrder := order
	conflictingOrder.Objective = "Conflicting HTTP replay after founder-key rotation"
	if err := SignWorkOrder(&conflictingOrder, rotationRequest.NewKeyID, rotatedPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(t, server.URL+"/v1/workforce/work-orders", token, conflictingOrder)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting Work Order status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/company-authority/preview", token,
		AuthorityChangePreviewRequest{
			KeyID: rotationRequest.NewKeyID, ExpectedVersion: 3,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("post-rotation authority preview status=%d", response.StatusCode)
	}
	var postRotationChange AuthorityChangePreview
	decodeControlResponse(t, response, &postRotationChange)
	postRotationSigned := MigrationPreview{Authority: postRotationChange.Authority}
	if err := signMigrationPreview(
		&postRotationSigned, rotationRequest.NewKeyID, rotatedPrivate,
	); err != nil {
		t.Fatal(err)
	}
	staleAuthorityPreview := AuthorityChangePreviewRequest{
		KeyID: rotationRequest.NewKeyID, ExpectedVersion: 1,
		EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}
	staleRotationPreview := FounderKeyRotationPreviewRequest{
		OldKeyID: rotationRequest.NewKeyID, NewKeyID: "key:http-browser:v3",
		NewPublicKey:    base64.RawURLEncoding.EncodeToString(conflictPublic),
		ExpectedVersion: 1, EffectiveAt: fixture.now, Authority: testActivationDraft(),
	}
	invalidTimePreview := ActivationPreviewRequest{
		Name: "Invalid time", KeyID: rotationRequest.NewKeyID,
		Authority: testActivationDraft(),
	}

	for _, value := range []struct {
		path  string
		token string
		body  any
		want  int
	}{
		{"/v1/workforce/session", "wrong", nil, http.StatusUnauthorized},
		{"/v1/workforce/company-authority?limit=201", token, nil, http.StatusBadRequest},
		{"/v1/workforce/unknown", token, nil, http.StatusNotFound},
		{"/v1/workforce/migration/preview", token, json.RawMessage(`{"bad":true}`), http.StatusBadRequest},
		{"/v1/workforce/migration", token, json.RawMessage(`{"bad":true}`), http.StatusBadRequest},
		{"/v1/workforce/company-authority/preview", token, json.RawMessage(`{"bad":true}`), http.StatusBadRequest},
		{"/v1/workforce/company-authority", token, json.RawMessage(`{"bad":true}`), http.StatusBadRequest},
		{"/v1/workforce/work-orders", token, json.RawMessage(`{"bad":true}`), http.StatusBadRequest},
		{"/v1/workforce/control-keys", token, ControlKeyRegistration{KeyID: registration.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(conflictPublic)}, http.StatusConflict},
		{"/v1/workforce/activation/preview", token, invalidTimePreview, http.StatusBadRequest},
		{"/v1/workforce/activation", token, ActivationBundle{}, http.StatusForbidden},
		{"/v1/workforce/migration/preview", token, MigrationPreviewRequest{KeyID: rotationRequest.NewKeyID, EffectiveAt: fixture.now, Authority: testActivationDraft()}, http.StatusForbidden},
		{"/v1/workforce/migration", token, MigrationBundle{}, http.StatusForbidden},
		{"/v1/workforce/company-authority/preview", token, staleAuthorityPreview, http.StatusConflict},
		{"/v1/workforce/company-authority", token, AuthorityChangeBundle{}, http.StatusForbidden},
		{"/v1/workforce/company-authority", token, AuthorityChangeBundle{ExpectedVersion: 2, Authority: postRotationSigned.Authority}, http.StatusConflict},
		{"/v1/workforce/founder-key/preview", token, staleRotationPreview, http.StatusConflict},
		{"/v1/workforce/founder-key", token, FounderKeyRotationBundle{}, http.StatusForbidden},
		{"/v1/workforce/commands", token, staleCommand, http.StatusConflict},
		{"/v1/workforce/control-keys", token, ControlKeyRegistration{KeyID: "key:bad", PublicKey: "bad"}, http.StatusBadRequest},
		{"/v1/workforce/events?after=bad", token, nil, http.StatusBadRequest},
		{"/v1/workforce/events/stream?after=bad", token, nil, http.StatusBadRequest},
	} {
		response = controlHTTPRequest(t, server.URL+value.path, value.token, value.body)
		if response.StatusCode != value.want {
			t.Fatalf("%s status = %d want %d", value.path, response.StatusCode, value.want)
		}
		response.Body.Close()
	}
}

func TestIntegration_HTTPV1MigrationPreviewAndCommit(t *testing.T) {
	fixture := newControlFixture(t, "http-v1-migration")
	ctx := context.Background()
	activationPreview, err := fixture.service.PreviewActivation(
		ctx, fixture.principal, ActivationPreviewRequest{
			Name: "HTTP v1 Workforce", KeyID: fixture.ownerKeyID,
			EffectiveAt: fixture.now, Authority: testActivationDraft(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := signControlActivation(
		&activationPreview, fixture.ownerKeyID, fixture.ownerPrivate,
	); err != nil {
		t.Fatal(err)
	}
	legacyStore, err := policy.New(
		controlPool, fixture.service.vault,
		policy.OwnerRoot{
			TenantID: fixture.principal.TenantID, OrganizationID: fixture.principal.OrganizationID,
			OwnerID: fixture.principal.OwnerID, KeyID: fixture.ownerKeyID,
			PublicKey: fixture.ownerPrivate.Public().(ed25519.PublicKey),
		},
		func() time.Time { return fixture.now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyStore.PublishSeed(ctx, activationPreview.Seed); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture.service.Handler())
	t.Cleanup(server.Close)
	token := "token:http-v1-migration"
	response := controlHTTPRequest(
		t, server.URL+"/v1/workforce/migration/preview", token,
		MigrationPreviewRequest{
			KeyID: fixture.ownerKeyID, EffectiveAt: fixture.now,
			Authority: testActivationDraft(),
		},
	)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("migration preview status=%d body=%s", response.StatusCode, body)
	}
	var preview MigrationPreview
	decodeControlResponse(t, response, &preview)
	if preview.Impact.CurrentDepartments != 7 || preview.Impact.CurrentSeats != 21 ||
		preview.LegacyOrganizationVersion != 1 {
		t.Fatalf("migration preview = %#v", preview)
	}
	if err := signMigrationPreview(&preview, fixture.ownerKeyID, fixture.ownerPrivate); err != nil {
		t.Fatal(err)
	}
	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/migration", token,
		MigrationBundle{
			Authority:                 preview.Authority,
			LegacyOrganizationVersion: preview.LegacyOrganizationVersion,
		},
	)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("migration commit status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	response = controlHTTPRequest(
		t, server.URL+"/v1/workforce/migration", token,
		MigrationBundle{
			Authority:                 preview.Authority,
			LegacyOrganizationVersion: preview.LegacyOrganizationVersion + 1,
		},
	)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale migration status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func controlHTTPRequest(t *testing.T, endpoint, token string, value any) *http.Response {
	t.Helper()
	method := http.MethodGet
	var body io.Reader
	if value != nil {
		method = http.MethodPost
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeControlResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}
