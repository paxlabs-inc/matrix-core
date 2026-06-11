// Package types holds the wire contracts shared across uwacd: the response
// envelope, error codes, the declarative connector spec, and the invoke
// request/response shapes the MCP proxy (tools/uwac/uwac.mjs) speaks.
package types

// Envelope is the uniform {ok,data,error} response shape (mirrors tachyond's
// pkg/types envelope so the MCP proxy can branch on ok=false).
type Envelope struct {
	Ok    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error is a structured, machine-branchable failure.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Error codes (stable; the planner/agent may branch on these).
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized"    // transport/principal auth failed
	CodeNotConnected   = "not_connected"   // the owner has not linked this provider
	CodeScopeMissing   = "scope_missing"   // connected, but the granted scopes do not cover the tool
	CodeNeedsConfirm   = "needs_confirm"   // irreversible / external-money action awaiting user confirmation
	CodeProvider       = "provider_error"  // the upstream provider API returned an error
	CodeInternal       = "internal"
)

// OK wraps a success payload.
func OK(data any) Envelope { return Envelope{Ok: true, Data: data} }

// Fail wraps an error.
func Fail(err *Error) Envelope { return Envelope{Ok: false, Error: err} }

// NewError constructs an *Error.
func NewError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// Consequence classifies how a tool maps onto the agent's reversible /
// irreversible execution wall (see connections.frozen.kvx [consequence]).
type Consequence string

const (
	// Natural: reversible, low-stakes; fully permissive (read, draft, list).
	ConseqNatural Consequence = "natural"
	// Confirm: irreversible / high-consequence non-money (send, post, delete).
	ConseqConfirm Consequence = "confirm"
	// ExternalMoney: moves real money through a connected app; always confirms.
	ConseqExternalMoney Consequence = "external_money"
)

// ToolSpec is the MCP-facing advertisement of one connector tool plus the
// metadata uwacd needs to gate + scope it. The actual provider HTTP call lives
// in the connector's Go handler, keyed by Name.
type ToolSpec struct {
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	InputSchema     map[string]any `json:"inputSchema"`
	SideEffectClass string         `json:"side_effect_class"`
	Consequence     Consequence    `json:"consequence"`
	// Scopes required for this tool; a subset of the connector's OAuth catalog.
	Scopes []string `json:"scopes"`
}

// OAuthSpec describes how a connector obtains + refreshes provider credentials.
type OAuthSpec struct {
	Provider string   `json:"provider"` // "google" | "github" | ...
	Scopes   []string `json:"scopes"`   // full catalog this connector may request
	// Refresh strategy: "rotating" (Google), "static" (GitHub OAuth app, no
	// expiry/no refresh), or "expiring" (GitHub App). Drives the refresher.
	Refresh string `json:"refresh"`
	// QueryParams appended to the authorize request (e.g. access_type=offline,
	// prompt=consent for Google to actually return a refresh token).
	QueryParams map[string]string `json:"query_params,omitempty"`
}

// EventSource is a connector-declared webhook/poll source the trigger engine
// can subscribe to (e.g. "gmail.new_message").
type EventSource struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"` // "webhook" | "poll"
	Description string `json:"description"`
}

// ConnectorSpec is the declarative {oauth, tools, events} triple for one app.
type ConnectorSpec struct {
	ID           string        `json:"id"`       // e.g. "uwac/google-workspace"
	Provider     string        `json:"provider"` // oauth provider key
	Display      string        `json:"display"`
	OAuth        OAuthSpec     `json:"oauth"`
	Tools        []ToolSpec    `json:"tools"`
	EventSources []EventSource `json:"event_sources,omitempty"`
}

// InvokeRequest is the body of POST /v1/invoke from the MCP proxy.
type InvokeRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	// Confirmed is set true by the proxy when the user has approved an action
	// whose Consequence is Confirm or ExternalMoney.
	Confirmed bool `json:"confirmed,omitempty"`
}

// ChallengeRequest / ChallengeResponse are the agent-DID principal-auth lane.
type ChallengeRequest struct {
	DID string `json:"did"`
}

// ChallengeResponse carries the exact bytes the agent must ed25519-sign.
type ChallengeResponse struct {
	DID       string `json:"did"`
	Nonce     string `json:"nonce"`
	Message   string `json:"message"`
	ExpiresIn int    `json:"expires_in"`
}

// VerifyRequest proves possession of the DID's key over the challenge.
type VerifyRequest struct {
	DID       string `json:"did"`
	PublicKey string `json:"public_key"` // hex(ed25519 pubkey)
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"` // hex(ed25519 signature)
}

// VerifyResponse returns a short-lived principal token bound to the owner.
type VerifyResponse struct {
	Token       string `json:"token"`
	OwnerUserID string `json:"owner_user_id"`
	ExpiresIn   int    `json:"expires_in"`
}
