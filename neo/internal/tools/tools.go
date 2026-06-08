// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package tools is Neo's tool surface. It reuses the executor's MCP manager
// + tool registry (so Neo's tools are byte-identical to the daemon's: fs,
// web_search, browser, git, shell, fetch, …), advertises each tool's real
// JSON schema to the model as a function, and dispatches calls.
//
// It also owns the execution-surface split (see surface.go): money / signature
// actions are classified Escalate and are NOT exposed as direct functions —
// they are reachable only through the synthetic core_execute tool, which
// delegates to the MCL pipeline (the only thing that can move funds, behind an
// inline approval gate).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"matrix/executor/mcp"
	"matrix/executor/tool"
	"matrix/neo/internal/llm"
)

// CoreExecuteTool is the synthetic function Neo exposes for delegating
// rigorous / money-moving tasks to the MCL pipeline.
const CoreExecuteTool = "core_execute"

// DelegateFunc runs a prose intent through the MCL pipeline and returns its
// verifiable outcome. Injected by the agent wiring (see internal/delegate);
// nil until wired, in which case core_execute reports it is unavailable.
type DelegateFunc func(ctx context.Context, proseIntent string) (string, error)

// boundTool is a manifest tool bound to its canonical URI + advertised schema.
type boundTool struct {
	funcName   string
	uri        string
	alias      string
	name       string
	sideEffect string
	desc       string
	params     map[string]interface{}
	surface    Surface
}

// Manager owns the MCP server pool + registry and the bound tool surface.
type Manager struct {
	manifest   *tool.AgentManifest
	mcp        *mcp.Manager
	registry   *tool.Registry
	classifier *Classifier
	delegate   DelegateFunc

	byFunc    map[string]*boundTool
	order     []string // sorted natural func names (advertised)
	escalated []string // sorted escalate func names (NOT advertised)
	warnings  []string // non-fatal spawn failures
}

// Options configures Spawn.
type Options struct {
	ManifestPath     string
	StderrSink       io.Writer
	SpawnTimeout     time.Duration
	Delegate         DelegateFunc
	EscalatePatterns []string
}

// Spawn loads the agent manifest, starts every declared MCP server (a server
// that fails to start is recorded as a warning and skipped — Neo degrades
// gracefully rather than refusing to boot), and binds the resulting tools.
func Spawn(ctx context.Context, opts Options) (*Manager, error) {
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("neo/tools: ManifestPath required")
	}
	if opts.SpawnTimeout == 0 {
		opts.SpawnTimeout = 90 * time.Second
	}
	if opts.StderrSink == nil {
		opts.StderrSink = os.Stderr
	}

	manifest, err := tool.LoadAgentManifest(opts.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("neo/tools: load manifest %s: %w", opts.ManifestPath, err)
	}

	mgr := mcp.NewManager(mcp.ManagerParams{StderrSink: opts.StderrSink})

	var warnings []string
	spawned := map[string]bool{}
	for i := range manifest.Servers {
		s := &manifest.Servers[i]
		resolved, _, rerr := tool.ResolveEnvList(s.Env, os.LookupEnv)
		if rerr != nil {
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped (env: %v)", s.Alias, rerr))
			continue
		}
		var subEnv []string
		if len(resolved) > 0 || len(s.Env) > 0 {
			subEnv = append(append([]string{}, os.Environ()...), resolved...)
		}
		spec := mcp.ServerSpec{
			Alias:         s.Alias,
			Transport:     s.Transport,
			Command:       s.Command,
			Args:          s.Args,
			Env:           subEnv,
			Endpoint:      s.Endpoint,
			Headers:       resolveHeaderEnv(s.Headers),
			PackageDigest: s.PackageDigest,
			ExpectedTools: toolNames(s.Tools),
		}
		sctx, cancel := context.WithTimeout(ctx, opts.SpawnTimeout)
		_, serr := mgr.Spawn(sctx, spec)
		cancel()
		if serr != nil {
			warnings = append(warnings, fmt.Sprintf("mcp server %q unavailable: %v", s.Alias, serr))
			continue
		}
		spawned[s.Alias] = true
	}

	reg, err := tool.NewRegistry(tool.RegistryParams{Manifest: manifest, MCP: mgr})
	if err != nil {
		_ = mgr.Close()
		return nil, fmt.Errorf("neo/tools: build registry: %w", err)
	}

	m := &Manager{
		manifest:   manifest,
		mcp:        mgr,
		registry:   reg,
		classifier: NewClassifier(opts.EscalatePatterns),
		delegate:   opts.Delegate,
		byFunc:     map[string]*boundTool{},
		warnings:   warnings,
	}
	m.bind(spawned)
	return m, nil
}

// bind builds the function-name → tool map for every tool on a server that
// actually spawned, pulling the live JSON schema from the MCP manager.
func (m *Manager) bind(spawned map[string]bool) {
	for i := range m.manifest.Servers {
		s := &m.manifest.Servers[i]
		if !spawned[s.Alias] {
			continue
		}
		schemas := map[string]json.RawMessage{}
		descs := map[string]string{}
		for _, t := range m.mcp.Tools(s.Alias) {
			schemas[t.Name] = t.InputSchema
			descs[t.Name] = t.Description
		}
		for j := range s.Tools {
			te := &s.Tools[j]
			uri := tool.ToolURI{Provider: "mcp", Server: s.Alias, Name: te.Name, Version: s.Version}.String()
			fn := funcName(s.Alias, te.Name)
			desc := te.Description
			if desc == "" {
				desc = descs[te.Name]
			}
			bt := &boundTool{
				funcName:   fn,
				uri:        uri,
				alias:      s.Alias,
				name:       te.Name,
				sideEffect: te.SideEffectClass,
				desc:       desc,
				params:     schemaToParams(schemas[te.Name]),
				surface:    m.classifier.Classify(te.Name, te.SideEffectClass),
			}
			m.byFunc[fn] = bt
			if bt.surface == Escalate {
				m.escalated = append(m.escalated, fn)
			} else {
				m.order = append(m.order, fn)
			}
		}
	}
	sort.Strings(m.order)
	sort.Strings(m.escalated)
}

// Schemas returns the function schemas advertised to the model: every Natural
// tool plus the synthetic core_execute delegation tool. Deterministic order.
func (m *Manager) Schemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(m.order)+1)
	for _, fn := range m.order {
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	out = append(out, coreExecuteSchema())
	return out
}

// Dispatch executes a tool call by function name.
//
// Returns (content, isError, err): err is a transport/invocation failure that
// feeds the recovery ladder (retry/adapt); isError=true with err==nil is an
// in-band failure the model should see and adapt to; both empty err means the
// tool ran. Unknown names and escalate-guard rejections come back as
// (message, true, nil) so the model reads and corrects rather than the harness
// retrying a doomed call.
func (m *Manager) Dispatch(ctx context.Context, funcName string, args map[string]interface{}) (string, bool, error) {
	if funcName == CoreExecuteTool {
		return m.dispatchCoreExecute(ctx, args)
	}
	bt, ok := m.byFunc[funcName]
	if !ok {
		return fmt.Sprintf("unknown tool %q — it is not available in this session", funcName), true, nil
	}
	if bt.surface == Escalate {
		return fmt.Sprintf("%q moves funds or needs a wallet signature and cannot be called directly; use %q with a clear description of the task so it runs through the secure path with your approval.", funcName, CoreExecuteTool), true, nil
	}
	t, err := m.registry.Get(bt.uri)
	if err != nil {
		return fmt.Sprintf("tool %q is unavailable: %v", funcName, err), true, nil
	}
	res, err := t.Call(ctx, args)
	if err != nil {
		return "", true, err
	}
	text := tool.ExtractText(res)
	if text == "" {
		text = summarizeNonText(res)
	}
	return text, res.IsError, nil
}

func (m *Manager) dispatchCoreExecute(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	intent, _ := args["intent"].(string)
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return "core_execute needs a non-empty 'intent' describing exactly what to do.", true, nil
	}
	if m.delegate == nil {
		return "the secure execution path is not connected in this session, so I can't perform actions that move funds or need a signature right now.", true, nil
	}
	out, err := m.delegate(ctx, intent)
	if err != nil {
		return "", true, fmt.Errorf("core_execute: %w", err)
	}
	return out, false, nil
}

// SetDelegate wires the MCL delegation function after construction (the
// delegate often needs the agent assembled first).
func (m *Manager) SetDelegate(d DelegateFunc) { m.delegate = d }

// NaturalToolNames returns the advertised (directly-callable) function names.
func (m *Manager) NaturalToolNames() []string { return append([]string{}, m.order...) }

// EscalateToolNames returns the escalate-class function names (reachable only
// via core_execute), for transparency / system-prompt construction.
func (m *Manager) EscalateToolNames() []string { return append([]string{}, m.escalated...) }

// Warnings returns non-fatal MCP server start failures from Spawn.
func (m *Manager) Warnings() []string { return append([]string{}, m.warnings...) }

// Close stops every MCP server.
func (m *Manager) Close() error {
	if m == nil || m.mcp == nil {
		return nil
	}
	return m.mcp.Close()
}

func coreExecuteSchema() llm.Tool {
	return llm.NewFunctionTool(
		CoreExecuteTool,
		"Delegate a rigorous or money-moving task to Matrix's secure execution pipeline. Use this for anything that spends or moves funds, signs a transaction, deploys a contract for gas, approves a token, or funds/settles a payment stream or channel — and for tasks that need verifiable, auditable, replayable execution. The user is asked to approve any spend inline before it happens. Provide a clear, complete natural-language description of exactly what to do.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"intent": map[string]interface{}{
					"type":        "string",
					"description": "A clear, self-contained description of the task to execute (what, with which inputs, and the success condition).",
				},
			},
			"required": []string{"intent"},
		},
	)
}

func funcName(alias, name string) string {
	return sanitizeFuncName(alias + "__" + name)
}

// sanitizeFuncName coerces an "<alias>__<tool>" id into the OpenAI function
// name charset (^[A-Za-z0-9_-]{1,64}$).
func sanitizeFuncName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func schemaToParams(raw json.RawMessage) map[string]interface{} {
	empty := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	if len(raw) == 0 {
		return empty
	}
	var p map[string]interface{}
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return empty
	}
	if _, ok := p["type"]; !ok {
		p["type"] = "object"
	}
	if _, ok := p["properties"]; !ok {
		p["properties"] = map[string]interface{}{}
	}
	return p
}

func summarizeNonText(res *tool.Result) string {
	if res == nil || len(res.Content) == 0 {
		return "(tool returned no content)"
	}
	var parts []string
	for _, c := range res.Content {
		switch c.Type {
		case tool.ContentTypeImage:
			parts = append(parts, fmt.Sprintf("[image %s]", c.MimeType))
		case tool.ContentTypeResource:
			parts = append(parts, fmt.Sprintf("[resource %s]", c.URI))
		default:
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	if len(parts) == 0 {
		return "(tool returned no text content)"
	}
	return strings.Join(parts, "\n")
}

func toolNames(list []tool.ToolEntry) []string {
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Name)
	}
	return out
}

func resolveHeaderEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, _ := tool.ResolveEnv(v, os.LookupEnv)
		out[k] = resolved
	}
	return out
}
