package engine

import (
	"context"
	"testing"
	"time"

	"github.com/paxlabs-inc/uwac/internal/config"
	"github.com/paxlabs-inc/uwac/internal/connectors"
	"github.com/paxlabs-inc/uwac/internal/oauth"
	"github.com/paxlabs-inc/uwac/internal/vault"
	"github.com/paxlabs-inc/uwac/pkg/types"
)

const (
	owner = "d17e78e5-0000-4000-8000-000000000abc"
	prov  = "test"
)

func testEngine(t *testing.T, rec *vault.Record) *Engine {
	t.Helper()
	reg := connectors.NewRegistry()
	conn := &connectors.Connector{
		Spec: types.ConnectorSpec{
			ID:       "test/echo",
			Provider: prov,
			Display:  "Echo",
			OAuth:    types.OAuthSpec{Provider: prov, Scopes: []string{"s1"}, Refresh: "static"},
			Tools: []types.ToolSpec{
				{Name: "echo_read", Consequence: types.ConseqNatural, Scopes: []string{"s1"}},
				{Name: "echo_write", Consequence: types.ConseqConfirm, Scopes: []string{"s1"}},
			},
		},
		Handlers: map[string]connectors.Handler{
			"echo_read":  func(_ context.Context, _ *vault.Record, args map[string]any) (any, error) { return args, nil },
			"echo_write": func(_ context.Context, _ *vault.Record, _ map[string]any) (any, error) { return map[string]any{"sent": true}, nil },
		},
	}
	if err := reg.Register(conn); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := vault.NewMemory()
	if rec != nil {
		if err := store.Put(context.Background(), rec); err != nil {
			t.Fatalf("seed vault: %v", err)
		}
	}
	key := make([]byte, 32)
	cfg := &config.Config{VaultKey: key, ChallengeTTL: time.Minute}
	return New(cfg, store, reg, oauth.New("", "", nil), nil)
}

func activeRecord(scopes ...string) *vault.Record {
	return &vault.Record{UserID: owner, Provider: prov, ConnectorID: "test/echo", Scopes: scopes, Status: "active"}
}

func TestInvokeUnknownTool(t *testing.T) {
	e := testEngine(t, activeRecord("s1"))
	env := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "nope"})
	mustFail(t, env, types.CodeInvalidRequest)
}

func TestInvokeNotConnected(t *testing.T) {
	e := testEngine(t, nil)
	env := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "echo_read"})
	mustFail(t, env, types.CodeNotConnected)
}

func TestInvokeScopeMissing(t *testing.T) {
	e := testEngine(t, activeRecord("other"))
	env := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "echo_read"})
	mustFail(t, env, types.CodeScopeMissing)
}

func TestInvokeConfirmGate(t *testing.T) {
	e := testEngine(t, activeRecord("s1"))
	env := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "echo_write"})
	mustFail(t, env, types.CodeNeedsConfirm)

	ok := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "echo_write", Confirmed: true})
	if !ok.Ok {
		t.Fatalf("expected confirmed write to succeed, got %+v", ok.Error)
	}
}

func TestInvokeReadOK(t *testing.T) {
	e := testEngine(t, activeRecord("s1"))
	env := e.Invoke(context.Background(), owner, types.InvokeRequest{Tool: "echo_read", Args: map[string]any{"q": "hi"}})
	if !env.Ok {
		t.Fatalf("expected ok, got %+v", env.Error)
	}
	data, _ := env.Data.(map[string]any)
	if data["q"] != "hi" {
		t.Fatalf("echo mismatch: %+v", env.Data)
	}
}

func TestChallengeVerifyBindsOwner(t *testing.T) {
	e := testEngine(t, nil)
	// A DID whose label is NOT a uuid cannot bind an owner.
	if _, err := e.Verify(types.VerifyRequest{DID: "did:matrix:executor:0123456789abcdef"}); err == nil {
		t.Fatal("expected verify to fail for non-signed/non-uuid did")
	}
}

func mustFail(t *testing.T, env types.Envelope, code string) {
	t.Helper()
	if env.Ok {
		t.Fatalf("expected failure %s, got ok", code)
	}
	if env.Error == nil || env.Error.Code != code {
		t.Fatalf("expected code %s, got %+v", code, env.Error)
	}
}
