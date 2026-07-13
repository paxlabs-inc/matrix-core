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

// TodoTool is the synthetic function Neo exposes to maintain a short, ordered
// task plan with per-item status, surfaced to the user as a live checklist that
// ticks off in real time (neo-smoothness req.3). Like core_execute it is NOT a
// real MCP server, so it never enters the manifest tool-bijection check; it is
// advertised only to the top-level agent (a sub-agent reports through its
// digest, not the user's checklist) and only when a todo emitter is wired.
const TodoTool = "todo"

// PreviewTool is the synthetic function Neo exposes to launch the workbench
// preview: it provisions the active project's on-demand sandbox and the result
// surfaces to the user in the workbench Preview pane via the durable preview.*
// events (NEO-WORKBENCH req 7). Like todo it is NOT a real MCP server, so it
// never enters the manifest tool-bijection check; it is advertised only when a
// preview launcher is wired (sandbox previews configured on this daemon).
const PreviewTool = "workspace_preview"

// DelegateFunc runs a prose intent through the MCL pipeline and returns its
// verifiable outcome. Injected by the agent wiring (see internal/delegate);
// nil until wired, in which case core_execute reports it is unavailable.
type DelegateFunc func(ctx context.Context, proseIntent string) (string, error)

// MediaPersistFunc writes a tool-produced image (base64 payload + MIME) to the
// served media plane and returns its /media/<id> URL. It is the seam that keeps
// a screenshot's bytes OUT of the model transcript (BROWSER-FILMSTRIP req.2):
// the URL travels out-of-band to the surfacing layer while the model-facing
// tool result stays a terse placeholder. Injected by the engine wiring (see
// internal/server); nil until wired, in which case image bytes are simply
// summarized to a placeholder as before (no persistence, no filmstrip). It is
// best-effort by contract: an error is swallowed by the caller and never fails
// the tool call or the turn.
type MediaPersistFunc func(mimeType, base64Data string) (url string, err error)

// TodoStatus is the lifecycle status of one task-list item. Exactly one item
// may be TodoInProgress at a time (enforced by dispatchTodo); an item is moved
// to TodoDone the moment it is finished (not batched at the end).
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoDone       TodoStatus = "done"
)

// TodoItem is one entry in the live task list: a short human description and
// its current status. Order is significant (the plan reads top-to-bottom).
type TodoItem struct {
	Text   string     `json:"text"`
	Status TodoStatus `json:"status"`
}

// TodoFunc records the current task list and surfaces it to the user as a live
// checklist (and persists it through the durable trace). Injected by the engine
// wiring (see internal/server); nil until wired, in which case the todo tool is
// not advertised at all. The items have already been validated by dispatchTodo
// (non-empty, at most one in_progress).
type TodoFunc func(ctx context.Context, items []TodoItem) error

// PreviewFunc launches the workbench preview for the calling run's active
// project (the engine resolves the conversation's project tag to its workspace
// subtree and provisions the on-demand sandbox). It returns a short
// model-facing status line; the lifecycle itself unfolds on the durable
// preview.pending/ready/failed events the client renders. Injected by the
// engine wiring (see internal/server); nil until wired, in which case the
// preview tool is not advertised at all.
type PreviewFunc func(ctx context.Context) (string, error)

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
	todo       TodoFunc
	preview    PreviewFunc
	media      MediaPersistFunc
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
// tool, plus the synthetic core_execute delegation tool ONLY when it actually
// gates something — at least one tool is classified Escalate AND the MCL
// delegate is wired. With the wallet write lane carried directly (NEO
// NO-BARRIER: DefaultEscalatePatterns is empty), nothing is walled, so
// core_execute is NOT advertised — advertising a money tool the prose says
// does not exist makes the context window incoherent and the model falls back
// to it under failure (the 2026-07-13 LayerX deposit transcript). Synthetics
// (memory_recall etc.) append when their seams are wired. Deterministic order.
func (m *Manager) Schemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(m.order)+2)
	for _, fn := range m.order {
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	if len(m.escalated) > 0 && m.delegate != nil {
		out = append(out, coreExecuteSchema())
	}
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
	if m.todo != nil {
		out = append(out, todoSchema())
	}
	if m.preview != nil {
		out = append(out, previewSchema())
	}
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
	content, _, isErr, err := m.dispatch(ctx, funcName, args)
	return content, isErr, err
}

// DispatchMedia is Dispatch plus the out-of-band screenshot URL: when the tool
// returned an image content block (e.g. browser_take_screenshot), its bytes are
// persisted to the media plane and the /media URL is returned SEPARATELY from
// the model-facing content string — which stays a terse placeholder so the
// screenshot never pollutes the transcript (BROWSER-FILMSTRIP req.2). shotURL is
// "" when the tool produced no image or no media sink is wired.
func (m *Manager) DispatchMedia(ctx context.Context, funcName string, args map[string]interface{}) (content, shotURL string, isErr bool, err error) {
	return m.dispatch(ctx, funcName, args)
}

func (m *Manager) dispatch(ctx context.Context, funcName string, args map[string]interface{}) (content, shotURL string, isErr bool, err error) {
	switch funcName {
	case CoreExecuteTool:
		c, e, er := m.dispatchCoreExecute(ctx, args)
		return c, "", e, er
	case MemoryRecallTool:
		c, e, er := m.dispatchMemoryRecall(ctx, args)
		return c, "", e, er
	case SpawnSubagentsTool:
		c, e, er := m.dispatchSpawnSubagents(ctx, args)
		return c, "", e, er
	case ConstructRenderTool:
		c, e, er := m.dispatchConstructRender(ctx, args)
		return c, "", e, er
	case WriteSkillTool:
		c, e, er := m.dispatchWriteSkill(ctx, args)
		return c, "", e, er
	case TodoTool:
		c, e, er := m.dispatchTodo(ctx, args)
		return c, "", e, er
	case PreviewTool:
		c, e, er := m.dispatchPreview(ctx)
		return c, "", e, er
	}
	bt, ok := m.byFunc[funcName]
	if !ok {
		return fmt.Sprintf("unknown tool %q — it is not available in this session", funcName), "", true, nil
	}
	if bt.surface == Escalate {
		return fmt.Sprintf("%q moves funds or needs a wallet signature and cannot be called directly; use %q with a clear description of the task so it runs through the secure path under the user's authorization (their inline approval, or a pre-authorized wallet leash).", funcName, CoreExecuteTool), "", true, nil
	}
	t, err := m.registry.Get(bt.uri)
	if err != nil {
		return fmt.Sprintf("tool %q is unavailable: %v", funcName, err), "", true, nil
	}
	res, err := t.Call(ctx, args)
	if err != nil {
		return "", "", true, err
	}
	text := tool.ExtractText(res)
	// Persist any image content block out-of-band (req.2): the /media URL rides
	// the return, never the model-facing text. Best-effort — a persist error
	// leaves shotURL empty and never disturbs the result.
	shotURL = m.persistFirstImage(res)
	if text == "" {
		text = summarizeNonText(res)
	}
	return text, shotURL, res.IsError, nil
}

// persistFirstImage writes the first image content block in a tool result to the
// media plane and returns its /media URL, or "" when there is no image, no media
// sink is wired, or the write fails. Best-effort by contract (req.2 ac_3): the
// screenshot never fails the tool call.
func (m *Manager) persistFirstImage(res *tool.Result) string {
	if m == nil || m.media == nil || res == nil {
		return ""
	}
	for _, c := range res.Content {
		if c.Type == tool.ContentTypeImage && strings.TrimSpace(c.Data) != "" {
			url, err := m.media(c.MimeType, c.Data)
			if err == nil && url != "" {
				return url
			}
		}
	}
	return ""
}

// CaptureViewport fires a viewport JPEG screenshot on the SAME browser MCP
// session that ran sourceFunc, persists it out-of-band, and returns the /media
// URL — the deterministic, model-invisible auto-capture behind the browsing
// filmstrip (BROWSER-FILMSTRIP req.3). It reuses the browser server the source
// call already used (derived from the "<alias>__" prefix), so the screenshot is
// of the page the source action just produced. Returns "" when no media sink is
// wired, the browser has no screenshot tool, or capture fails — best-effort:
// a screenshot failure never blocks the navigation or the turn (req.3 ac_4).
func (m *Manager) CaptureViewport(ctx context.Context, sourceFunc string) string {
	if m == nil || m.media == nil {
		return ""
	}
	shotFunc := "browser_take_screenshot"
	if i := strings.Index(sourceFunc, "__"); i >= 0 {
		shotFunc = sourceFunc[:i] + "__browser_take_screenshot"
	}
	bt, ok := m.byFunc[shotFunc]
	if !ok {
		return ""
	}
	t, err := m.registry.Get(bt.uri)
	if err != nil {
		return ""
	}
	// Viewport JPEG (not full-page PNG): "the section they're viewing", quality/
	// size bounded by the browser's JPEG encoder (req.7.1). fullPage omitted =>
	// viewport only.
	res, err := t.Call(ctx, map[string]interface{}{"type": "jpeg"})
	if err != nil {
		return ""
	}
	return m.persistFirstImage(res)
}

func (m *Manager) dispatchCoreExecute(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if len(m.escalated) == 0 {
		// Not advertised on this surface (nothing is walled behind escalation):
		// a call here is a hallucinated ghost tool. Answer exactly like any
		// other unknown name — the wallet tools ARE the money lane.
		return fmt.Sprintf("unknown tool %q — it is not available in this session; your advertised wallet tools are the money lane, use them directly", CoreExecuteTool), true, nil
	}
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

// dispatchTodo is the live task-list handler (neo-smoothness req.3). It parses
// the model's ordered plan, ENFORCES the lifecycle invariants — at most one
// item in_progress at a time (req.3.2) — and surfaces the list through the
// injected emitter. It no-ops gracefully on a trivial single-step turn (req.3.4)
// so the feature adds visibility on real multi-step work without ceremony.
// Validation failures are returned in-band (isError=true, err=nil) so the model
// reads the steer and corrects rather than the harness retrying.
func (m *Manager) dispatchTodo(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.todo == nil {
		return "the task list isn't available in this session.", true, nil
	}
	items := parseTodoItems(args)
	// No ceremony on trivial work: a zero/one-step plan doesn't get a checklist.
	// Graceful (not an error) so the model simply proceeds and reports the result.
	if len(items) < 2 {
		return "A single step doesn't need a task list — just do it and report the result. Use the task list only for genuinely multi-step work.", false, nil
	}
	// Enforce exactly one in_progress at a time.
	inProgress := 0
	for _, it := range items {
		if it.Status == TodoInProgress {
			inProgress++
		}
	}
	if inProgress > 1 {
		return fmt.Sprintf("Keep exactly ONE item in_progress at a time (you marked %d). Set only the step you're working on now to in_progress; mark finished steps done and the rest pending.", inProgress), true, nil
	}
	if err := m.todo(ctx, items); err != nil {
		return "", true, fmt.Errorf("todo: %w", err)
	}
	return summarizeTodo(items), false, nil
}

// ParseTodoItems is the exported todo-argument parser: the agent core's task
// graph consumes the SAME item vocabulary the todo surface renders (one
// vocabulary, no parallel plan state — epistemic-core req.7.3).
func ParseTodoItems(args map[string]interface{}) []TodoItem { return parseTodoItems(args) }

// parseTodoItems reads the model's todo arguments into an ordered item list,
// tolerating the loose JSON shapes models emit (text under text/title/content/
// step, status synonyms). An entry with no text is dropped. Order is preserved.
func parseTodoItems(args map[string]interface{}) []TodoItem {
	raw, ok := args["items"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]TodoItem, 0, len(raw))
	for _, e := range raw {
		switch v := e.(type) {
		case string:
			// A bare string is a pending item.
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, TodoItem{Text: s, Status: TodoPending})
			}
		case map[string]interface{}:
			text := strings.TrimSpace(asString(v["text"]))
			if text == "" {
				text = strings.TrimSpace(asString(v["title"]))
			}
			if text == "" {
				text = strings.TrimSpace(asString(v["content"]))
			}
			if text == "" {
				text = strings.TrimSpace(asString(v["step"]))
			}
			if text == "" {
				continue
			}
			out = append(out, TodoItem{Text: text, Status: normalizeTodoStatus(asString(v["status"]))})
		}
	}
	return out
}

// normalizeTodoStatus maps the loose status strings models emit onto the three
// canonical statuses, defaulting to pending for anything unrecognized.
func normalizeTodoStatus(s string) TodoStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "in_progress", "in-progress", "inprogress", "doing", "active", "current", "started":
		return TodoInProgress
	case "done", "complete", "completed", "finished", "closed":
		return TodoDone
	default:
		return TodoPending
	}
}

// summarizeTodo renders a short, plain-language acknowledgment of the recorded
// list for the model's tool result (the user sees the rich live checklist; this
// is the in-transcript echo).
func summarizeTodo(items []TodoItem) string {
	done, inProgress, pending := 0, 0, 0
	for _, it := range items {
		switch it.Status {
		case TodoDone:
			done++
		case TodoInProgress:
			inProgress++
		default:
			pending++
		}
	}
	return fmt.Sprintf("Task list updated (%d steps: %d done, %d in progress, %d to do).", len(items), done, inProgress, pending)
}

// SetTodo wires the live task-list emitter after construction (the emitter
// needs the engine + per-run event stream assembled first). nil leaves the todo
// tool unadvertised.
func (m *Manager) SetTodo(f TodoFunc) { m.todo = f }

// TodoEnabled reports whether the live task-list tool is wired this session.
func (m *Manager) TodoEnabled() bool { return m != nil && m.todo != nil }

// SetPreview wires the workbench preview launcher after construction (the
// launcher needs the engine's sandbox controller + project registry assembled
// first). nil leaves the preview tool unadvertised.
func (m *Manager) SetPreview(f PreviewFunc) { m.preview = f }

// PreviewEnabled reports whether the workbench preview tool is wired this
// session.
func (m *Manager) PreviewEnabled() bool { return m != nil && m.preview != nil }

// dispatchPreview launches the active project's sandbox preview. The launcher
// is asynchronous by contract: it kicks provisioning and returns immediately;
// readiness (or failure) reaches the user through the preview.* events the
// workbench renders — the model should not poll or wait for the URL.
func (m *Manager) dispatchPreview(ctx context.Context) (string, bool, error) {
	if m.preview == nil {
		return "the workbench preview is not available in this environment", true, nil
	}
	out, err := m.preview(ctx)
	if err != nil {
		return fmt.Sprintf("could not start the preview: %v", err), true, nil
	}
	if out == "" {
		out = "Preview is starting — it will appear in the workbench Preview pane when ready."
	}
	return out, false, nil
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

// SetMediaPersist wires the media-plane sink for tool-produced images after
// construction (the engine owns the served media dir). nil leaves image bytes
// summarized to a placeholder as before (no persistence, no filmstrip).
func (m *Manager) SetMediaPersist(f MediaPersistFunc) { m.media = f }

// MediaPersistEnabled reports whether a media sink is wired (browser stills can
// be persisted + surfaced). Used by the agent to skip auto-capture cheaply when
// there is nowhere to write.
func (m *Manager) MediaPersistEnabled() bool { return m != nil && m.media != nil }

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

// SpawnEnabled reports whether spawn_subagents is advertised this session (the
// swarm runner is wired) AND, therefore, whether the top-level agent can
// decompose at all. The self-aware decomposition router (self-model task 4.2)
// surfaces the agent's own context limits at the decision point only when this
// is true — there is no point teaching a decision the agent cannot act on.
func (m *Manager) SpawnEnabled() bool { return m != nil && m.swarm != nil }

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
		"Search your own durable memory (the cortex) — the user's profile, stored facts, past outcomes, preferences, and proven approaches — which persists across conversations and restarts. This is your PRIMARY way to bring in prior context: PULL from it before you reason about the user, their projects, or past work, and before claiming a fact you'd have learned earlier. Use it ITERATIVELY: start broad, read what comes back, then call again with a narrower query (or a type filter) as you learn what you actually need. Each result line shows the memory's type, any contradiction to reconcile, and its cortex URI so you can cite it. Returns a rendered digest, ranked by how useful each memory has proven. SELF-MODEL: a query of \"self:\" returns your compact structural self-summary (your own loop, faculties, window assembly, and safety wall, derived from your source), and \"self:<Symbol>\" (e.g. \"self:assembleWindowUserTail\") pages the full graph fragment for one of your own symbols on demand — use it to reason about how you are built and where you are limited.",
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

// todoSchema advertises the live task-list tool (neo-smoothness req.3). The
// model lays out an ordered, multi-step plan and keeps it updated as it works;
// the user sees it as a checklist that ticks off in real time.
func todoSchema() llm.Tool {
	return llm.NewFunctionTool(
		TodoTool,
		"Maintain a short, ordered checklist of the steps for a multi-step task — shown to the user as a live to-do list that ticks off in real time. Call it when you begin a task with several distinct steps to lay out the plan, then call it again to update statuses as you work. Keep EXACTLY ONE item in_progress at a time, and mark an item done the MOMENT it is finished — don't batch the updates to the end. Don't use it for a single trivial step; it's for giving the user visibility on genuinely multi-step work. Pass the full current list each time (it replaces the previous one).",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"items": map[string]interface{}{
					"type":        "array",
					"description": "The ordered task list, top to bottom. Provide the complete current list on every call.",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"text": map[string]interface{}{
								"type":        "string",
								"description": "A short, plain description of the step (e.g. \"Fetch the latest block height\").",
							},
							"status": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"pending", "in_progress", "done"},
								"description": "The step's status. At most one item may be in_progress at a time.",
							},
						},
						"required": []interface{}{"text", "status"},
					},
				},
			},
			"required": []interface{}{"items"},
		},
	)
}

// previewSchema advertises the workbench preview launcher (NEO-WORKBENCH
// req 7): fire-and-forget — provisioning is asynchronous and the user watches
// the Preview pane, so the model calls it once when the project is runnable
// and moves on.
func previewSchema() llm.Tool {
	return llm.NewFunctionTool(
		PreviewTool,
		"Start (or restart) the live preview of the active project in the user's workbench: it provisions an isolated sandbox, runs the project's dev command there, and the running app appears in the workbench Preview pane. Call it ONCE when the project is in a runnable state (after your file writes and any install/build steps) so the user can see the app live — this is how you show working software; never deploy anywhere just to show work. It returns immediately: readiness or failure reaches the user through the Preview pane, so do not poll, wait for a URL, or call it repeatedly.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
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
