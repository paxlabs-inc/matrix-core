// Package engine is uwacd's orchestration core: it ties config + vault +
// connector registry + the GoTrue OAuth client + the agent-DID challenge store,
// and implements the request flows the HTTP API exposes — agent-auth
// (challenge/verify), the OAuth connect/callback, and tool invoke (scope +
// consequence gating, provider-token refresh, dispatch).
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paxlabs-inc/uwac/internal/config"
	"github.com/paxlabs-inc/uwac/internal/connectors"
	"github.com/paxlabs-inc/uwac/internal/httpx"
	"github.com/paxlabs-inc/uwac/internal/identity"
	"github.com/paxlabs-inc/uwac/internal/oauth"
	"github.com/paxlabs-inc/uwac/internal/vault"
	"github.com/paxlabs-inc/uwac/pkg/types"
)

// Version is the uwacd build version.
const Version = "0.1.0"

const (
	principalTTL    = 30 * time.Minute
	tokenSkew       = 60 * time.Second // refresh this far ahead of expiry
	connectStateTTL = 10 * time.Minute
)

// Engine holds the wired dependencies.
type Engine struct {
	cfg   *config.Config
	vault vault.Store
	reg   *connectors.Registry
	oauth *oauth.Client
	ch    *identity.Challenges
	log   *slog.Logger

	mu     sync.Mutex
	states map[string]connectState // OAuth connect verifier store (in-memory)
}

type connectState struct {
	verifier    string
	owner       string
	connectorID string
	provider    string
	scopes      []string
	exp         time.Time
}

// New constructs the engine.
func New(cfg *config.Config, v vault.Store, reg *connectors.Registry, oc *oauth.Client, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:    cfg,
		vault:  v,
		reg:    reg,
		oauth:  oc,
		ch:     identity.NewChallenges(cfg.ChallengeTTL),
		log:    log,
		states: map[string]connectState{},
	}
}

// Registry exposes the connector registry (for the MCP tool advertisement).
func (e *Engine) Registry() *connectors.Registry { return e.reg }

// ── agent-DID principal auth ─────────────────────────────────────────────────

// Challenge issues a single-use nonce for a DID.
func (e *Engine) Challenge(did string) (types.ChallengeResponse, error) {
	if _, err := identity.ParseDID(did); err != nil {
		return types.ChallengeResponse{}, err
	}
	nonce, msg := e.ch.Create(did)
	go e.ch.Purge()
	return types.ChallengeResponse{
		DID:       did,
		Nonce:     nonce,
		Message:   msg,
		ExpiresIn: int(e.cfg.ChallengeTTL / time.Second),
	}, nil
}

// Verify checks the signed challenge and mints a principal token bound to the
// owner user id resolved from the DID label.
func (e *Engine) Verify(req types.VerifyRequest) (types.VerifyResponse, error) {
	if err := identity.Verify(req.DID, req.PublicKey, req.Nonce, req.Signature); err != nil {
		return types.VerifyResponse{}, err
	}
	if !e.ch.Consume(req.Nonce, req.DID) {
		return types.VerifyResponse{}, errors.New("challenge invalid: nonce unknown, expired, or already used")
	}
	d, _ := identity.ParseDID(req.DID)
	owner, ok := identity.OwnerFromDID(d)
	if !ok {
		return types.VerifyResponse{}, errors.New("did label is not an owner user id (cannot bind a vault owner)")
	}
	tok := identity.MintToken(e.cfg.VaultKey, owner, principalTTL)
	return types.VerifyResponse{Token: tok, OwnerUserID: owner, ExpiresIn: int(principalTTL / time.Second)}, nil
}

// OwnerFromToken validates a principal token (X-UWAC-Agent header).
func (e *Engine) OwnerFromToken(tok string) (string, error) {
	return identity.VerifyToken(e.cfg.VaultKey, tok)
}

// ── OAuth connect flow ───────────────────────────────────────────────────────

// Connect starts a scope-elevation connect for a connector and returns the
// GoTrue authorize URL the owner's browser must visit.
func (e *Engine) Connect(owner, connectorID string) (string, error) {
	var spec *types.ConnectorSpec
	for _, s := range e.reg.Specs() {
		if s.ID == connectorID {
			cp := s
			spec = &cp
			break
		}
	}
	if spec == nil {
		return "", fmt.Errorf("unknown connector %q", connectorID)
	}
	verifier, challenge, err := oauth.GeneratePKCE()
	if err != nil {
		return "", err
	}
	state := identity.MintToken(e.cfg.VaultKey, owner, connectStateTTL) // opaque, unguessable
	e.mu.Lock()
	e.states[state] = connectState{
		verifier:    verifier,
		owner:       owner,
		connectorID: spec.ID,
		provider:    spec.Provider,
		scopes:      spec.OAuth.Scopes,
		exp:         time.Now().Add(connectStateTTL),
	}
	e.mu.Unlock()
	redirectTo := e.cfg.PublicBaseURL + "/v1/connect/callback?state=" + state
	return e.oauth.AuthorizeURL(spec.Provider, redirectTo, challenge, spec.OAuth.Scopes, spec.OAuth.QueryParams), nil
}

// Callback completes the connect: exchange the code, capture + vault the
// provider refresh token.
func (e *Engine) Callback(ctx context.Context, state, code string) error {
	e.mu.Lock()
	st, ok := e.states[state]
	if ok {
		delete(e.states, state)
	}
	e.mu.Unlock()
	if !ok || time.Now().After(st.exp) {
		return errors.New("connect state unknown or expired")
	}
	res, err := e.oauth.Exchange(ctx, code, st.verifier)
	if err != nil {
		return err
	}
	if res.ProviderRefreshToken == "" {
		return errors.New("provider returned no refresh token (ensure access_type=offline + prompt=consent and that the user granted offline access)")
	}
	expiry := time.Now().Add(time.Duration(maxInt(res.ExpiresIn, 3600)) * time.Second)
	rec := &vault.Record{
		UserID:       st.owner,
		Provider:     st.provider,
		ConnectorID:  st.connectorID,
		AccessToken:  res.ProviderToken,
		RefreshToken: res.ProviderRefreshToken,
		Scopes:       st.scopes,
		Expiry:       expiry,
		Status:       "active",
	}
	if err := e.vault.Put(ctx, rec); err != nil {
		return err
	}
	e.log.Info("connector linked", "owner", st.owner, "connector", st.connectorID, "scopes", len(st.scopes))
	return nil
}

// Disconnect revokes a connector grant for an owner.
func (e *Engine) Disconnect(ctx context.Context, owner, provider string) error {
	return e.vault.Delete(ctx, owner, provider)
}

// ── tool invoke ──────────────────────────────────────────────────────────────

// Invoke runs one connector tool for an owner: gate -> ensure fresh token ->
// dispatch. Returns a uniform envelope.
func (e *Engine) Invoke(ctx context.Context, owner string, req types.InvokeRequest) types.Envelope {
	conn, tool, handler, ok := e.reg.Lookup(req.Tool)
	if !ok {
		return types.Fail(types.NewError(types.CodeInvalidRequest, "unknown tool: "+req.Tool, false))
	}

	// Consequence gate: irreversible / external-money requires confirmation.
	if (tool.Consequence == types.ConseqConfirm || tool.Consequence == types.ConseqExternalMoney) && !req.Confirmed {
		return types.Fail(types.NewError(types.CodeNeedsConfirm,
			fmt.Sprintf("%s is irreversible and needs the user's confirmation before it runs", tool.Name), false))
	}

	// Vault lookup (owner must have connected this provider).
	rec, err := e.vault.Get(ctx, owner, conn.Spec.Provider)
	if err != nil {
		if errors.Is(err, vault.ErrNotFound) {
			return types.Fail(types.NewError(types.CodeNotConnected,
				fmt.Sprintf("%s is not connected; ask the user to link %s first", conn.Spec.Display, conn.Spec.Display), false))
		}
		return types.Fail(types.NewError(types.CodeInternal, err.Error(), false))
	}

	// Scope check.
	for _, want := range tool.Scopes {
		if !rec.HasScope(want) {
			return types.Fail(types.NewError(types.CodeScopeMissing,
				fmt.Sprintf("%s needs scope %s which the user has not granted; re-connect to elevate", tool.Name, want), false))
		}
	}

	// Ensure a fresh provider access token.
	if err := e.ensureFresh(ctx, conn, rec); err != nil {
		return types.Fail(classify(err))
	}

	data, err := handler(ctx, rec, req.Args)
	if err != nil {
		e.log.Warn("tool failed", "owner", owner, "tool", tool.Name, "error", err.Error())
		return types.Fail(classify(err))
	}
	e.log.Info("tool ok", "owner", owner, "tool", tool.Name, "connector", conn.Spec.ID)
	return types.OK(data)
}

// ensureFresh refreshes the access token if missing/expiring (rotating
// providers). Static providers (no refresh) keep their stored token.
func (e *Engine) ensureFresh(ctx context.Context, conn *connectors.Connector, rec *vault.Record) error {
	if conn.Spec.OAuth.Refresh == "static" || conn.Spec.OAuth.Refresh == "none" {
		return nil
	}
	if rec.AccessToken != "" && !rec.Expiry.IsZero() && time.Now().Before(rec.Expiry.Add(-tokenSkew)) {
		return nil
	}
	if rec.RefreshToken == "" {
		return &httpx.Error{Status: 401, Body: "no refresh token on record; user must re-connect"}
	}
	tok, exp, err := e.oauth.RefreshProviderToken(ctx, conn.Spec.Provider, rec.RefreshToken)
	if err != nil {
		return err
	}
	rec.AccessToken = tok
	rec.Expiry = exp
	// Best-effort write-back of the refreshed access token cache.
	if perr := e.vault.Put(ctx, rec); perr != nil {
		e.log.Warn("vault write-back failed", "owner", rec.UserID, "provider", rec.Provider, "error", perr.Error())
	}
	return nil
}

// classify maps a handler error to a typed envelope error.
func classify(err error) *types.Error {
	var argErr *connectors.ArgError
	if errors.As(err, &argErr) {
		return types.NewError(types.CodeInvalidRequest, argErr.Msg, false)
	}
	var hErr *httpx.Error
	if errors.As(err, &hErr) {
		code := types.CodeProvider
		if hErr.Status == 401 || hErr.Status == 403 {
			code = types.CodeUnauthorized
		}
		retryable := hErr.Status == 429 || (hErr.Status >= 500 && hErr.Status <= 599)
		return types.NewError(code, hErr.Error(), retryable)
	}
	return types.NewError(types.CodeInternal, err.Error(), false)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
