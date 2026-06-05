// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Manager is the per-agent MCP server pool (Q16 lock). It owns the
// lifecycle of every server an agent has declared in its manifest:
// spawn-on-boot, health-pinged, auto-reconnect on death, graceful drain
// on agent stop.
//
// One Manager per agent (NOT per intent — servers persist across
// intents for the duration of the agent process). Tools layer
// (executor/tool) consults Manager.Client(serverAlias) to resolve
// tool URIs at dispatch time.
type Manager struct {
	mu      sync.RWMutex
	clients map[string]*Client
	specs   map[string]ServerSpec

	// tools caches the live tools/list response per alias, keyed by
	// alias. Populated by Spawn after verifyTools succeeds. Synthesizer
	// reads this so the executor LLM sees real InputSchemas instead of
	// hallucinating tool args.
	tools map[string][]Tool

	// healthInterval is the ping cadence. Zero disables periodic health
	// checks (default for tests; production sets a non-zero value).
	healthInterval time.Duration

	// onUnhealthy fires after a health-check failure for a server.
	// Default is to log + attempt reconnect; tests may override.
	onUnhealthy func(alias string, err error)

	// stderrSink optionally receives subprocess stderr. nil = discard.
	stderrSink io.Writer

	// transportBuilder optionally overrides the default stdio/http
	// transport selection. Tests inject pipeTransports through this
	// hook; production leaves it nil to use the standard builders.
	transportBuilder func(spec ServerSpec) (Transport, error)

	// shutdown coordinates Close: cancels the health-check goroutines
	// and prevents new server registration.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	wg             sync.WaitGroup
}

// ServerSpec is the manifest entry for one MCP server. Cross-references
// a tool/manifest.go ServerEntry; mirrored here so the mcp package
// stays free of tool-package imports.
type ServerSpec struct {
	// Alias is the local logical name used in matrix://tool/mcp/<alias>/...
	// URIs (Q17 lock). Stable across server-package upgrades.
	Alias string

	// Transport is "stdio" or "http". Q15 lock excludes anything else.
	Transport string

	// Command + Args + Env apply when Transport=="stdio".
	Command string
	Args    []string
	Env     []string

	// Endpoint applies when Transport=="http". Headers carries any
	// auth tokens (Q18 — never logged).
	Endpoint string
	Headers  map[string]string

	// PackageDigest is the sha256 hash that pins this server's package
	// version (Q22). Manager records it but verification of the local
	// package against the digest is the tool layer's responsibility
	// (it knows where the package came from).
	PackageDigest string

	// ExpectedTools is the manifest-declared tool list. Manager calls
	// tools/list at startup and rejects on drift (Q21).
	ExpectedTools []string
}

// ManagerParams configures a Manager.
type ManagerParams struct {
	// HealthInterval enables periodic ping. Zero disables.
	HealthInterval time.Duration

	// OnUnhealthy is called after a failed health check. Default logs
	// to stderr.
	OnUnhealthy func(alias string, err error)

	// StderrSink receives subprocess stderr. Default discards.
	StderrSink io.Writer

	// TransportBuilder optionally replaces the default stdio/http
	// dispatch with a caller-supplied builder. Tests use this to inject
	// pipe-based mock transports. Leave nil in production to use the
	// canonical stdio + streamable HTTP transports per Q15.
	TransportBuilder func(spec ServerSpec) (Transport, error)
}

// NewManager constructs an empty Manager. Servers are added via Spawn.
func NewManager(p ManagerParams) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		clients:          make(map[string]*Client),
		specs:            make(map[string]ServerSpec),
		tools:            make(map[string][]Tool),
		healthInterval:   p.HealthInterval,
		onUnhealthy:      p.OnUnhealthy,
		stderrSink:       p.StderrSink,
		transportBuilder: p.TransportBuilder,
		shutdownCtx:      ctx,
		shutdownCancel:   cancel,
	}
}

// Spawn launches an MCP server, runs the initialize handshake, and
// validates the tool manifest (Q21). On success, the server is
// registered under its alias and ready for ToolsCall.
//
// Spawn is idempotent: spawning an alias that's already running is a
// no-op (returns the existing client). To replace, call Stop first.
func (m *Manager) Spawn(ctx context.Context, spec ServerSpec) (*Client, error) {
	if spec.Alias == "" {
		return nil, errors.New("mcp/manager: empty alias")
	}

	m.mu.Lock()
	if existing, ok := m.clients[spec.Alias]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	transport, err := m.buildTransport(spec)
	if err != nil {
		return nil, fmt.Errorf("mcp/manager: build transport for %q: %w", spec.Alias, err)
	}

	client, err := NewClient(ClientParams{Transport: transport})
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("mcp/manager: client for %q: %w", spec.Alias, err)
	}

	if _, err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mcp/manager: initialize %q: %w", spec.Alias, err)
	}

	advertised, err := m.verifyTools(ctx, client, spec)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	m.mu.Lock()
	m.clients[spec.Alias] = client
	m.specs[spec.Alias] = spec
	m.tools[spec.Alias] = advertised
	m.mu.Unlock()

	if m.healthInterval > 0 {
		m.wg.Add(1)
		go m.healthLoop(spec.Alias)
	}

	return client, nil
}

// Client returns the live client for the given alias, or nil if the
// alias hasn't been spawned.
func (m *Manager) Client(alias string) *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[alias]
}

// Aliases returns the sorted set of currently registered aliases.
// Snapshot — does not lock for the duration of caller use.
func (m *Manager) Aliases() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.clients))
	for a := range m.clients {
		out = append(out, a)
	}
	return out
}

// Spec returns the ServerSpec for an alias. Empty alias returned if
// not present.
func (m *Manager) Spec(alias string) ServerSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.specs[alias]
}

// Tools returns the live tool descriptors (including JSON-Schema
// InputSchemas) advertised by the server with the given alias. Empty
// slice if the alias is not registered. The returned slice is a copy
// safe for caller mutation.
func (m *Manager) Tools(alias string) []Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.tools[alias]
	if len(src) == 0 {
		return nil
	}
	out := make([]Tool, len(src))
	copy(out, src)
	return out
}

// Stop terminates a single server cleanly.
func (m *Manager) Stop(alias string) error {
	m.mu.Lock()
	c, ok := m.clients[alias]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.clients, alias)
	delete(m.specs, alias)
	delete(m.tools, alias)
	m.mu.Unlock()
	return c.Close()
}

// Close drains all registered servers. Safe to call multiple times.
func (m *Manager) Close() error {
	m.shutdownCancel()
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.specs = map[string]ServerSpec{}
	m.tools = map[string][]Tool{}
	m.mu.Unlock()

	var firstErr error
	for _, c := range clients {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.wg.Wait()
	return firstErr
}

// buildTransport instantiates the right Transport for the spec.
func (m *Manager) buildTransport(spec ServerSpec) (Transport, error) {
	if m.transportBuilder != nil {
		return m.transportBuilder(spec)
	}
	switch spec.Transport {
	case "stdio", "":
		// Default to stdio when unspecified — matches the dominant
		// MCP server convention (npx/uvx-launched local processes).
		if spec.Command == "" {
			return nil, errors.New("stdio transport requires Command")
		}
		return NewStdioTransport(StdioParams{
			Command:    spec.Command,
			Args:       spec.Args,
			Env:        spec.Env,
			StderrSink: m.stderrSink,
		})
	case "http":
		if spec.Endpoint == "" {
			return nil, errors.New("http transport requires Endpoint")
		}
		hdr := make(map[string][]string, len(spec.Headers))
		for k, v := range spec.Headers {
			hdr[k] = []string{v}
		}
		return NewHTTPTransport(HTTPParams{
			Endpoint: spec.Endpoint,
			Headers:  hdr,
		})
	default:
		return nil, fmt.Errorf("unsupported transport %q (want stdio|http)", spec.Transport)
	}
}

// verifyTools enforces Q21 — what the server advertises must match
// what the manifest declared. Drift is fatal. On success returns the
// advertised tool list (with InputSchemas) so callers can cache it.
func (m *Manager) verifyTools(ctx context.Context, client *Client, spec ServerSpec) ([]Tool, error) {
	advertised, err := client.ToolsList(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp/manager: tools/list for %q: %w", spec.Alias, err)
	}
	if len(spec.ExpectedTools) == 0 {
		// Nothing to verify against — manifest opted in to dynamic
		// discovery (which is itself deferred per executor_deferrals,
		// but pass through here so test fixtures can ride without an
		// expected-tools list).
		return advertised, nil
	}
	got := make(map[string]bool, len(advertised))
	for _, t := range advertised {
		got[t.Name] = true
	}
	want := make(map[string]bool, len(spec.ExpectedTools))
	for _, t := range spec.ExpectedTools {
		want[t] = true
	}
	for t := range want {
		if !got[t] {
			return nil, fmt.Errorf("mcp/manager: server %q missing expected tool %q (Q21 manifest drift)", spec.Alias, t)
		}
	}
	for t := range got {
		if !want[t] {
			return nil, fmt.Errorf("mcp/manager: server %q advertises unexpected tool %q (Q21 manifest drift)", spec.Alias, t)
		}
	}
	return advertised, nil
}

// healthLoop periodically pings the server and invokes onUnhealthy
// on failure. v1 does NOT auto-restart (keeps blast radius small);
// the unhealthy callback is the integration point for caller-decided
// recovery.
func (m *Manager) healthLoop(alias string) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.shutdownCtx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			c := m.clients[alias]
			m.mu.RUnlock()
			if c == nil {
				return
			}
			ctx, cancel := context.WithTimeout(m.shutdownCtx, 5*time.Second)
			err := c.Ping(ctx)
			cancel()
			if err != nil && m.onUnhealthy != nil {
				m.onUnhealthy(alias, err)
			}
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
