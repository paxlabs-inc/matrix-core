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

	"matrix/construct/backchannel"
	"matrix/construct/projection"
	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
	"matrix/executor/mcp"
	"matrix/executor/tool"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
)

// CoreExecuteTool is the synthetic function Neo exposes for delegating
// rigorous / money-moving tasks to the MCL pipeline.
const CoreExecuteTool = "core_execute"

// MemoryRecallTool is the synthetic function Neo exposes for explicitly
// searching its own durable cortex memory. It is the PRIMARY reasoning-time
// retrieval verb (v3 #1): ambient injection is now only a thin seed (or off),
// so the model PULLS what it needs mid-thought — iteratively, with a narrowing
// query, type filter, and optional as-of instant — instead of being force-fed.
const MemoryRecallTool = "memory_recall"

// SpawnSubagentsTool is the synthetic function Neo exposes to fan a task out
// to several task-scoped sub-agents that run CONCURRENTLY, each in its own
// isolated context window, and to collect their distilled results. It is a
// context-conservation strategy as much as a parallelism one: the heavy tool
// work (reading a repo, crawling pages) happens in the sub-agents' windows and
// only a compact result returns to Neo's. Reachable only from the top-level
// agent — sub-agents do NOT get this tool (no recursion / fork-bombs).
const SpawnSubagentsTool = "spawn_subagents"

// ConstructRenderTool is the synthetic function Neo exposes to deliberately
// render a Construct surface onto the user's screen — the agent-authored ACTIVE
// tier of the Construct projection engine. Neo IS the projector: it chooses one
// of the 8 frozen primitives and fills it, and the handler validates that
// choice against the frozen schema before emitting (invariant i2 — the agent
// fills a trusted primitive, never emits arbitrary UI). Like core_execute it is
// NOT a real MCP server, so it never enters the manifest tool-bijection check.
const ConstructRenderTool = projection.ConstructRenderTool

// TaskCompleteTool is the synthetic completion gate (Cassandra Phase 1): the
// ONLY legal way for Neo to end a turn that touched state. It carries a
// completeness object (summary + coverage + evidence + open_gaps +
// assumptions) that the agent loop validates against the working transcript
// (the ground truth) before the turn may terminate — positive-proof of
// completion, never the mere absence of further tool calls. Like core_execute
// it is synthetic (no MCP server, no manifest bijection); unlike the others it
// is intercepted in the agent loop, not routed through Manager.Dispatch,
// because the validator needs the live transcript the Manager cannot see.
const TaskCompleteTool = "task_complete"

// WriteSkillTool is the synthetic function Neo exposes for the agent to
// CONSCIOUSLY persist a reusable recipe as a cortex Pattern (P2-2: the
// skill-writing / synthesis loop). After a proven task, the agent authors a
// structured PatternSpec (name, trigger, preconditions, steps, gotchas,
// success_criteria) and this tool validates it and writes it to the durable
// procedural store. The spec is validated against PatternSpec (non-empty
// identity) before the write; coverage starts low and is reinforced on each
// repeat success, so a single authoring does not overfit (procedural.guards).
// Readable on demand via memory_recall. Like memory_recall it is NOT a real
// MCP server, so it never enters the manifest tool-bijection check.
const WriteSkillTool = "write_skill"

// DelegateFunc runs a prose intent through the MCL pipeline and returns its
// verifiable outcome. Injected by the agent wiring (see internal/delegate);
// nil until wired, in which case core_execute reports it is unavailable.
type DelegateFunc func(ctx context.Context, proseIntent string) (string, error)

// RecallFunc searches the durable memory store and returns a rendered,
// user-presentable digest. It is the PRIMARY reasoning-time retrieval verb
// (v3 #1): the model calls it iteratively with a narrowing query, an optional
// type filter, a result cap, and an optional bi-temporal as-of instant
// (nil = now). Injected from the pager; nil until wired, in which case
// memory_recall is not advertised at all.
type RecallFunc func(ctx context.Context, query string, types []string, k int, asOf *time.Time) (string, error)

// SubagentSpec describes one task-scoped sub-agent the model wants to spawn:
// a short human name, a persona/role framing, and the self-contained task it
// should carry out. Mirrors the model-facing spawn_subagents schema.
type SubagentSpec struct {
	Name    string `json:"name"`
	Persona string `json:"persona"`
	Task    string `json:"task"`
}

// SwarmFunc runs a set of sub-agents concurrently and returns an aggregated,
// model-readable digest of their results. Injected by the engine wiring (see
// internal/server); nil until wired, in which case spawn_subagents reports it
// is unavailable and is not advertised at all.
type SwarmFunc func(ctx context.Context, specs []SubagentSpec) (string, error)

// SurfaceFunc emits a validated Construct surface onto the active run's event
// stream as a construct.surface event. Injected by the engine wiring (see
// internal/server); nil until wired, in which case construct_render reports it
// is unavailable and is not advertised at all.
type SurfaceFunc func(ctx context.Context, s *schema.Surface) error

// AskFunc emits a validated Ask surface, PARKS the run on the back-channel
// (invariant i5), and returns the human's typed response once it is posted —
// or an error if the run is cancelled or the ask expires unanswered. Injected
// by the engine wiring (see internal/server); nil leaves construct_render able
// to SHOW an ask but not block for an answer.
type AskFunc func(ctx context.Context, s *schema.Surface) (*primitives.AskResponse, error)

// WriteSkillFunc persists a reusable recipe as a cortex Pattern (P2-2). It
// receives a validated PatternSpec (non-empty identity) and returns the cortex
// URI of the written/reinforced pattern. Injected from the pager (which calls
// ReinforcePattern); nil until wired, in which case write_skill is not
// advertised at all. The coverage starts low and is reinforced on each repeat
// success, so a single authoring does not overfit (procedural.guards).
type WriteSkillFunc func(ctx context.Context, spec memory.PatternSpec) (string, error)

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
	recall     RecallFunc
	swarm      SwarmFunc
	surface    SurfaceFunc
	ask        AskFunc
	writeSkill WriteSkillFunc
	maxAgents  int

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
// tool plus the synthetic core_execute delegation tool (and memory_recall
// when a memory store is wired). Deterministic order.
func (m *Manager) Schemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(m.order)+2)
	for _, fn := range m.order {
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	out = append(out, coreExecuteSchema())
	if m.recall != nil {
		out = append(out, memoryRecallSchema())
	}
	if m.swarm != nil {
		out = append(out, spawnSubagentsSchema())
	}
	if m.surface != nil {
		out = append(out, constructRenderSchema())
	}
	if m.writeSkill != nil {
		out = append(out, writeSkillSchema())
	}
	// The completion gate is ALWAYS advertised to the top-level agent: it is
	// the only sanctioned way to end a state-touching turn (Cassandra Phase 1).
	out = append(out, taskCompleteSchema())
	return out
}

// SubagentSchemas is the tool surface advertised to a SUB-AGENT: every Natural
// tool, but NOT core_execute (money stays with the user-facing parent — a
// background sub-agent can't service an inline approval gate), memory_recall,
// or spawn_subagents (no recursion). Deterministic order.
func (m *Manager) SubagentSchemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(m.order))
	for _, fn := range m.order {
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
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
	if funcName == MemoryRecallTool {
		return m.dispatchMemoryRecall(ctx, args)
	}
	if funcName == SpawnSubagentsTool {
		return m.dispatchSpawnSubagents(ctx, args)
	}
	if funcName == ConstructRenderTool {
		return m.dispatchConstructRender(ctx, args)
	}
	if funcName == WriteSkillTool {
		return m.dispatchWriteSkill(ctx, args)
	}
	bt, ok := m.byFunc[funcName]
	if !ok {
		return fmt.Sprintf("unknown tool %q — it is not available in this session", funcName), true, nil
	}
	if bt.surface == Escalate {
		return fmt.Sprintf("%q moves funds or needs a wallet signature and cannot be called directly; use %q with a clear description of the task so it runs through the secure path under the user's authorization (their inline approval, or a pre-authorized wallet leash).", funcName, CoreExecuteTool), true, nil
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

func (m *Manager) dispatchSpawnSubagents(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.swarm == nil {
		return "running sub-agents is not available in this session.", true, nil
	}
	specs := parseSubagentSpecs(args)
	if len(specs) == 0 {
		return "spawn_subagents needs an 'agents' array, each with a 'name', a 'persona' (its role), and a 'task' (a clear, self-contained instruction).", true, nil
	}
	if len(specs) < 2 {
		return "spawn_subagents is for parallel work — give it at least 2 agents, or just do this single task yourself.", true, nil
	}
	if m.maxAgents > 0 && len(specs) > m.maxAgents {
		return fmt.Sprintf("that's %d sub-agents; the most you can run in one call is %d. Group the work into fewer, broader agents.", len(specs), m.maxAgents), true, nil
	}
	out, err := m.swarm(ctx, specs)
	if err != nil {
		return "", true, fmt.Errorf("spawn_subagents: %w", err)
	}
	return out, false, nil
}

// parseSubagentSpecs reads the model's spawn_subagents arguments into specs,
// tolerating the loose JSON shapes models emit (objects vs. maps, missing
// fields). An entry with no task is dropped.
func parseSubagentSpecs(args map[string]interface{}) []SubagentSpec {
	raw, ok := args["agents"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]SubagentSpec, 0, len(raw))
	for i, e := range raw {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		task := strings.TrimSpace(asString(m["task"]))
		if task == "" {
			continue
		}
		name := strings.TrimSpace(asString(m["name"]))
		if name == "" {
			name = fmt.Sprintf("Agent %02d", i+1)
		}
		out = append(out, SubagentSpec{
			Name:    name,
			Persona: strings.TrimSpace(asString(m["persona"])),
			Task:    task,
		})
	}
	return out
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (m *Manager) dispatchMemoryRecall(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.recall == nil {
		return "the durable memory store is not connected in this session.", true, nil
	}
	q, _ := args["query"].(string)
	types := asStringSlice(args["types"])
	k := asInt(args["k"])
	asOf := asTime(args["as_of"])
	out, err := m.recall(ctx, strings.TrimSpace(q), types, k, asOf)
	if err != nil {
		return fmt.Sprintf("memory lookup failed: %v", err), true, nil
	}
	return out, false, nil
}

// dispatchWriteSkill is the P2-2 skill-writing handler: it parses the model's
// write_skill arguments into a validated PatternSpec and persists it as a
// cortex Pattern via the injected WriteSkillFunc (which calls
// ReinforcePattern). A spec with no usable identity (empty name, trigger, and
// steps) is rejected in-band so the model reads the error and corrects rather
// than the harness retrying. On success the cortex URI is returned so the
// agent can cite the persisted skill.
func (m *Manager) dispatchWriteSkill(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.writeSkill == nil {
		return "writing skills is not available in this session (no durable memory store connected).", true, nil
	}
	spec := memory.PatternSpec{
		Name:            strings.TrimSpace(asString(args["name"])),
		Trigger:         strings.TrimSpace(asString(args["trigger"])),
		Preconditions:   asStringSlice(args["preconditions"]),
		Steps:           asStringSlice(args["steps"]),
		Gotchas:         asStringSlice(args["gotchas"]),
		SuccessCriteria: asStringSlice(args["success_criteria"]),
	}
	if spec.IsEmpty() {
		return "write_skill needs at least a 'name' (or 'trigger' or 'steps') to identify the recipe. Provide the proven tool sequence as 'steps' and a short 'name'.", true, nil
	}
	uri, err := m.writeSkill(ctx, spec)
	if err != nil {
		return fmt.Sprintf("write_skill failed: %v", err), true, nil
	}
	return fmt.Sprintf("Persisted skill %q as a cortex Pattern (%s). It starts as a low-coverage candidate and is reinforced on each repeat success; it becomes active after %d proven successes.", spec.Name, uri, 0), false, nil
}

// asStringSlice coerces a JSON array argument into a []string, dropping
// non-string / blank entries. Returns nil for a missing or non-array value.
func asStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s := strings.TrimSpace(asString(e)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// asInt coerces a JSON number argument into an int (JSON unmarshals numbers as
// float64). Returns 0 for a missing or non-numeric value.
func asInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// asTime parses an ISO-8601 / RFC3339 (or bare YYYY-MM-DD) string argument into
// a UTC instant for bi-temporal as-of queries (v3 #2). Returns nil for a
// missing or unparseable value (the caller then reads against "now").
func asTime(v interface{}) *time.Time {
	s := strings.TrimSpace(asString(v))
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// SetDelegate wires the MCL delegation function after construction (the
// delegate often needs the agent assembled first).
func (m *Manager) SetDelegate(d DelegateFunc) { m.delegate = d }

// SetSwarm wires the sub-agent fan-out runner after construction (the runner
// needs the engine assembled first). maxAgents caps how many sub-agents one
// spawn_subagents call may request; <= 0 leaves it unbounded.
func (m *Manager) SetSwarm(s SwarmFunc, maxAgents int) {
	m.swarm = s
	m.maxAgents = maxAgents
}

// SetRecall wires the durable-memory lookup after construction (the pager
// and tool manager are built independently).
func (m *Manager) SetRecall(r RecallFunc) { m.recall = r }

// SetWriteSkill wires the skill-writing function after construction (the
// pager and tool manager are built independently). When nil, write_skill is
// not advertised. The func persists a validated PatternSpec as a cortex
// Pattern via ReinforcePattern.
func (m *Manager) SetWriteSkill(f WriteSkillFunc) { m.writeSkill = f }

// WriteSkillEnabled reports whether the write_skill tool is wired this session
// (a durable cortex store is connected and skill authoring is available).
func (m *Manager) WriteSkillEnabled() bool { return m != nil && m.writeSkill != nil }

// SetSurfaceEmitter wires the Construct surface emitter after construction (the
// emitter needs the engine + per-run event stream assembled first). nil leaves
// construct_render unadvertised.
func (m *Manager) SetSurfaceEmitter(f SurfaceFunc) { m.surface = f }

// SetAskResponder wires the Construct Ask back-channel after construction (it
// needs the engine + per-run session assembled first). nil leaves
// construct_render able to show an ask but not block for an answer.
func (m *Manager) SetAskResponder(f AskFunc) { m.ask = f }

// dispatchConstructRender is the ACTIVE-tier handler: it maps the model's
// construct_render arguments onto a validated Construct surface and emits it.
// A validation error is returned in-band (isError=true, err=nil) so the model
// reads it and corrects the call rather than the harness retrying.
func (m *Manager) dispatchConstructRender(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.surface == nil {
		return "rendering a surface to the screen is not available in this session.", true, nil
	}
	s, err := projection.ParseRender(args)
	if err != nil {
		return err.Error(), true, nil
	}
	// Ask is the one inherently bidirectional primitive (invariant i5): it does
	// not just paint a surface, it BLOCKS for a typed human answer and resumes
	// the agent with it. Route it through the back-channel responder.
	if s.Kind == schema.KindAsk {
		return m.dispatchAsk(ctx, s)
	}
	if err := m.surface(ctx, s); err != nil {
		return "", true, fmt.Errorf("construct_render: %w", err)
	}
	return fmt.Sprintf("Rendered a %s surface (id %q) onto the user's screen.", s.Kind, s.ID), false, nil
}

// dispatchAsk emits an Ask surface and parks the tool call until the human
// answers (or the run is cancelled / the ask expires). The human's typed
// response returns to the model as the tool result — an INPUT it reads and
// acts on, exactly like a user message. When no back-channel responder is
// wired (dev/CLI), the ask is still SHOWN, but the model is told it cannot
// block and should proceed.
func (m *Manager) dispatchAsk(ctx context.Context, s *schema.Surface) (string, bool, error) {
	if m.ask == nil {
		_ = m.surface(ctx, s)
		return "asking the user is not interactive in this session, so I can't wait for an answer here. Proceed without it, or state what you need.", true, nil
	}
	resp, err := m.ask(ctx, s)
	if err != nil {
		return fmt.Sprintf("the user didn't answer (%v). Proceed without that input or try another approach.", err), true, nil
	}
	return backchannel.Summarize(s.Ask, resp), false, nil
}

// SurfaceEnabled reports whether the Construct render tool is wired this
// session (the agent-authored ACTIVE projection tier is available).
func (m *Manager) SurfaceEnabled() bool { return m != nil && m.surface != nil }

// RecallEnabled reports whether the memory_recall tool is wired this session
// (a durable cortex store is connected). The system prompt uses this to teach
// the model to PULL memory mid-thought (v3 #1) only when it can actually do so.
func (m *Manager) RecallEnabled() bool { return m != nil && m.recall != nil }

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
		"Delegate a rigorous or money-moving task to Matrix's secure execution pipeline. Use this for anything that spends or moves funds, signs a transaction, deploys a contract for gas, approves a token, launches/trades/collects-fees on KindleLaunch, or funds/settles a payment stream or channel — and for tasks that need verifiable, auditable, replayable execution. Funds are signed server-side by the user's embedded wallet, never by you. If the user has granted a pre-authorized leash (a spending mode + caps on their wallet), actions within that leash run without a per-action approval prompt; the wallet enforces the limits and cleanly declines anything outside them. Otherwise, for a one-off spend the user is asked to approve it inline before it happens. Provide a clear, complete natural-language description of exactly what to do.",
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

func memoryRecallSchema() llm.Tool {
	return llm.NewFunctionTool(
		MemoryRecallTool,
		"Search your own durable memory (the cortex) — the user's profile, stored facts, past outcomes, preferences, and proven approaches — which persists across conversations and restarts. This is your PRIMARY way to bring in prior context: PULL from it before you reason about the user, their projects, or past work, and before claiming a fact you'd have learned earlier. Use it ITERATIVELY: start broad, read what comes back, then call again with a narrower query (or a type filter) as you learn what you actually need. Each result line shows the memory's type, any contradiction to reconcile, and its cortex URI so you can cite it. Returns a rendered digest, ranked by how useful each memory has proven.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "What to look for (a topic, name, project, or question). Empty returns the most salient memories plus the user profile.",
				},
				"types": map[string]interface{}{
					"type":        "array",
					"description": "Optional filter: restrict results to these memory types. Narrow with this as you iterate (e.g. [\"preference\"] for how the user likes to work, [\"pattern\"] for a proven approach, [\"fact\"] for objective truths).",
					"items": map[string]interface{}{
						"type": "string",
						"enum": []string{
							"fact", "preference", "belief", "event", "goal",
							"constraint", "capability", "pattern", "identity",
						},
					},
				},
				"k": map[string]interface{}{
					"type":        "integer",
					"description": "Optional cap on how many memories to return. Omit for a sensible default; raise it when you want a wider sweep.",
				},
				"as_of": map[string]interface{}{
					"type":        "string",
					"description": "Optional point in time (RFC3339, e.g. \"2026-01-15T00:00:00Z\", or a date \"2026-01-15\") to ask what was true THEN — memories superseded or expired after that instant are excluded. Omit to read the current truth.",
				},
			},
		},
	)
}

// writeSkillSchema advertises the P2-2 skill-writing tool. The agent authors a
// structured PatternSpec after a proven task; this tool validates it and
// persists it as a cortex Pattern (the synthesis loop). The spec mirrors
// PatternSpec exactly (name, trigger, preconditions, steps, gotchas,
// success_criteria).
func writeSkillSchema() llm.Tool {
	return llm.NewFunctionTool(
		WriteSkillTool,
		"Persist a reusable recipe as a durable skill (a cortex Pattern) after a task you've proven works. This is the synthesis loop: you CONSCIOUSLY distill the proven tool sequence into a structured recipe so you can reapply it to similar future tasks. The recipe starts as a low-coverage candidate and is reinforced on each repeat success; it only becomes active (injected into future prompts) after it has been proven multiple times, so don't overfit a one-off flow. Provide the full structure: a short name, when to apply it (trigger), what must be true first (preconditions), the exact tool sequence (steps), learned failure modes (gotchas), and how to verify it worked (success_criteria). You can recall any persisted skill later via memory_recall with types=[\"pattern\"].",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "A short human label for this recipe (e.g. \"deploy ERC-20 on Paxeer\"). This is the skill's identity.",
				},
				"trigger": map[string]interface{}{
					"type":        "string",
					"description": "When to apply this recipe — the task shape that should match it (e.g. \"user asks to launch a token\").",
				},
				"preconditions": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "What must be true BEFORE applying the recipe — checked first (e.g. [\"API key set\", \"wallet funded\"]).",
				},
				"steps": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "The proven tool sequence, in order (e.g. [\"compile\", \"test\", \"deploy\"]). This is the core of the recipe.",
				},
				"gotchas": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Learned failure modes to avoid (e.g. [\"pre-Cancun chain -> pin evm_version=shanghai\"]).",
				},
				"success_criteria": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "How to verify the recipe worked — checked AFTER applying (e.g. [\"receipt status=1\", \"balanceOf>0\"]).",
				},
			},
			"required": []string{"name", "steps"},
		},
	)
}

func spawnSubagentsSchema() llm.Tool {
	return llm.NewFunctionTool(
		SpawnSubagentsTool,
		"Spawn several task-scoped sub-agents that run CONCURRENTLY and return their combined results. Use this for work that splits into independent parts — e.g. analyzing different modules of a codebase, researching several topics at once, or comparing options in parallel. Each sub-agent runs in its OWN fresh context with the full reversible toolset (shell, files, browser, web, git), so heavy exploration stays out of your window and only the distilled findings come back. Give each a clear, self-contained task that does NOT depend on another sub-agent's output (they run at the same time and can't talk to each other). They CANNOT move funds (no core_execute) or spawn their own sub-agents. Use only when the task genuinely parallelizes; otherwise just do it yourself.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agents": map[string]interface{}{
					"type":        "array",
					"description": "The sub-agents to run in parallel (2 or more). Keep them coarse — a few broad agents beat many tiny ones.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"name": map[string]interface{}{
								"type":        "string",
								"description": "A short human name for this sub-agent, shown to the user (e.g. \"Go Code Analyst\").",
							},
							"persona": map[string]interface{}{
								"type":        "string",
								"description": "The role/expertise framing it should adopt (e.g. \"a senior Go reviewer focused on concurrency and error handling\").",
							},
							"task": map[string]interface{}{
								"type":        "string",
								"description": "A complete, self-contained instruction: what to investigate or do, where to look, and exactly what to report back. Include any context it needs — it does NOT see this conversation.",
							},
						},
						"required": []interface{}{"name", "task"},
					},
				},
			},
			"required": []interface{}{"agents"},
		},
	)
}

// constructRenderSchema advertises the single agent-facing render tool, whose
// contract is owned by the construct module (projection.RenderTools), so the
// vocabulary stays single-source.
func constructRenderSchema() llm.Tool {
	spec := projection.RenderTools()[0]
	return llm.NewFunctionTool(spec.Name, spec.Description, spec.Params)
}

// taskCompleteSchema advertises the completion gate (Cassandra Phase 1). The
// model calls it to declare the turn done, carrying the completeness object the
// agent loop validates against the working transcript before it may terminate.
func taskCompleteSchema() llm.Tool {
	return llm.NewFunctionTool(
		TaskCompleteTool,
		"Declare this turn complete. Call this ONLY when you are actually done — it is the single way to finish a turn in which you took an action, ran a tool, changed state, or made a claim about real-world facts. Provide an honest completeness object: what you accomplished, whether every requested deliverable was produced, the concrete evidence behind your load-bearing claims, anything still unfinished or unconfirmed, and any assumptions you made. Be truthful — claims you cannot back with real tool results, and gaps you leave open, are checked against what you actually did. If you are not done, keep working instead of calling this.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"summary": map[string]interface{}{
					"type":        "string",
					"description": "Your final answer / narration to the user, in plain human terms — the substance they are meant to read.",
				},
				"coverage": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"full", "partial"},
					"description": "\"full\" only if EVERY explicitly requested deliverable was produced; otherwise \"partial\".",
				},
				"evidence": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Concrete evidence backing your load-bearing claims: the tool results, command output, file paths, URLs, or transaction hashes you actually obtained this turn. Each item must correspond to something you really did — do not invent evidence.",
				},
				"open_gaps": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Things still unsatisfied or NOT confirmed that arguably should have been — phrased as concrete items. An empty list is an explicit claim of \"nothing left open\", so only leave it empty if that is true. Required to be empty when coverage is \"full\".",
				},
				"assumptions": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Defaults you silently chose that materially shape the result, surfaced rather than buried.",
				},
			},
			"required": []string{"summary", "coverage"},
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
