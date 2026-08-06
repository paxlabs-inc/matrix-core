// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package tools is Neo's tool surface. Local filesystem, shell, process, and
// read-only git operations run natively in this process; remote integrations
// continue to use the executor's MCP manager and registry.
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
	"errors"
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
// searching its own durable Neocortex memory. It is the PRIMARY reasoning-time
// retrieval verb (v3 #1): ambient injection is now only a thin seed (or off),
// so the model PULLS what it needs mid-thought — iteratively, with a narrowing
// query, type filter, and optional as-of instant — instead of being force-fed.
const MemoryRecallTool = "memory_recall"

// MemoryMutateTool is the bounded typed write surface for user-directed
// memory create, update, supersede, and delete operations. It replaces shell
// or curl discovery of internal memory endpoints.
const MemoryMutateTool = "memory_mutate"

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
// CONSCIOUSLY persist a reusable recipe as a Neocortex Pattern (P2-2: the
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

// PreviewTool is the synthetic function Neo exposes for an explicitly requested
// development sandbox. Successfully compiled frontends are delivered through
// Paxeer Cloud instead. Like todo it is NOT a real MCP server, so it never enters
// the manifest tool-bijection check; it is advertised only when a preview
// launcher is wired (sandbox previews configured on this daemon).
const PreviewTool = "workspace_preview"

// BuildProjectTool enters Neo's durable asynchronous Build mode. It persists a
// Build job before returning and never exposes the execution runtime or
// AgentCore protocol to the model or browser.
const BuildProjectTool = "build_project"

// DesktopLookTool / DesktopA11yTool are the disposable-desktop grounding
// synthetics (DOJO req 3). They are Neo-owned (they call the omni vision client
// and the AT-SPI exec through the dojo session manager), so they never enter
// the desktop bridge's tool-bijection check; each is advertised only when the
// desktop session manager is wired on this daemon. Like the bridge lane they
// are excluded from every autonomous surface (IsDesktopTool).
const (
	DesktopLookTool = "desktop_look"
	DesktopA11yTool = "desktop_a11y"
)

const maxFileMutationBytes = 16 * 1024

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

type BuildRequest struct {
	Project            string
	Request            string
	AcceptanceCriteria []string
	Constraints        []string
}

// BuildFunc accepts a normalized project brief and returns immediately after
// the durable job is accepted. Completion and questions arrive out of band.
type BuildFunc func(ctx context.Context, request BuildRequest) (string, error)

// DesktopLookFunc grounds the disposable desktop with the stateless MiMo v2.5
// omni vision call (DOJO req 3.2): it captures the desktop, invokes omni with a
// fixed grounding prompt and the given target, and returns structured text
// (interactive elements + a best click point, coordinates already rescaled to
// screen pixels). It is the LAST rung of the driving ladder. Injected by the
// engine wiring; nil until a desktop session manager + omni client are wired.
type DesktopLookFunc func(ctx context.Context, target string) (string, error)

// DesktopA11yFunc dumps the disposable desktop's AT-SPI accessibility tree
// (DOJO req 3.3): the deterministic MIDDLE rung — roles, labels, and
// screen-pixel coordinates with no vision model involved. Injected by the
// engine wiring; nil until a desktop session manager is wired.
type DesktopA11yFunc func(ctx context.Context) (string, error)

// RecallFunc searches the durable memory store and returns a rendered,
// user-presentable digest. It is the PRIMARY reasoning-time retrieval verb
// (v3 #1): the model calls it iteratively with a narrowing query, an optional
// type filter, a result cap, and an optional bi-temporal as-of instant
// (nil = now). Injected from the pager; nil until wired, in which case
// memory_recall is not advertised at all.
type RecallFunc func(ctx context.Context, query string, types []string, k int, asOf *time.Time) (string, error)

// MemoryMutationFunc executes a bounded batch of typed memory mutations.
// Internal Neocortex identifiers are never requested by the model-facing tool.
type MemoryMutationFunc func(ctx context.Context, req memory.MutationRequest) (memory.MutationBatchResult, error)

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

// WriteSkillFunc persists a reusable recipe as a Neocortex Pattern (P2-2). It
// receives a validated PatternSpec (non-empty identity) and returns the Neocortex
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
	manifest    *tool.AgentManifest
	mcp         *mcp.Manager
	registry    *tool.Registry
	classifier  *Classifier
	delegate    DelegateFunc
	recall      RecallFunc
	mutation    MemoryMutationFunc
	swarm       SwarmFunc
	surface     SurfaceFunc
	ask         AskFunc
	writeSkill  WriteSkillFunc
	todo        TodoFunc
	preview     PreviewFunc
	build       BuildFunc
	desktopLook DesktopLookFunc
	desktopA11y DesktopA11yFunc
	media       MediaPersistFunc
	native      *nativeLocal
	maxAgents   int

	// personalization persists the user-confirmed personalization profile
	// (ORACLE task 5.3). Reached only through save_personalization_profile,
	// which only interview agents advertise.
	personalization PersonalizationSaveFunc

	byFunc    map[string]*boundTool
	order     []string // sorted natural func names (advertised)
	escalated []string // sorted escalate func names (NOT advertised)
	warnings  []string // non-fatal spawn failures
}

// DirectResult is the bounded application-facing result of one real tool call.
// It deliberately exposes the same validation and dispatch path used by Neo's
// model loop without exposing the MCP manager or registry themselves.
type DirectResult struct {
	Content        string
	MediaURL       string
	IsError        bool
	FailureClass   tool.FailureClass
	Retryable      bool
	FailureMessage string
}

// CallDirect runs a named tool through the manager's normal registry,
// validation, side-effect classification, and MCP transport. Product surfaces
// use this only when the human has already selected an explicit operation; it
// is not a bypass around escalated tools.
func (m *Manager) CallDirect(
	ctx context.Context,
	funcName string,
	args map[string]interface{},
) (DirectResult, error) {
	if m == nil {
		return DirectResult{}, errors.New("tool manager is unavailable")
	}
	content, mediaURL, isErr, class, retryable, failureMessage, err :=
		m.dispatch(ctx, funcName, args)
	return DirectResult{
		Content: content, MediaURL: mediaURL, IsError: isErr,
		FailureClass: class, Retryable: retryable,
		FailureMessage: failureMessage,
	}, err
}

// Options configures Spawn.
type Options struct {
	ManifestPath     string
	StderrSink       io.Writer
	SpawnTimeout     time.Duration
	Delegate         DelegateFunc
	EscalatePatterns []string
	NativeRoot       string
	NativeReadRoots  []string
	NativeStateDir   string
	NativeGitPath    string
}

// Spawn loads the agent manifest, starts every declared MCP server (a server
// that fails to start is recorded as a warning and skipped — Neo degrades
// gracefully rather than refusing to boot), and binds the resulting tools.
func Spawn(ctx context.Context, opts Options) (*Manager, error) {
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("neo/tools: ManifestPath required")
	}
	if opts.SpawnTimeout == 0 {
		opts.SpawnTimeout = 15 * time.Second
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
	type spawnResult struct {
		index int
		alias string
		stage string
		err   error
	}
	results := make(chan spawnResult, len(manifest.Servers))
	for index, server := range manifest.Servers {
		go func(index int, s tool.ServerEntry) {
			resolved, _, rerr := tool.ResolveEnvList(s.Env, os.LookupEnv)
			if rerr != nil {
				results <- spawnResult{index: index, alias: s.Alias, stage: "env", err: rerr}
				return
			}
			subEnv, privileged, rerr := tool.MCPEnvironment(s.Alias, os.Environ(), resolved)
			if rerr != nil {
				results <- spawnResult{index: index, alias: s.Alias, stage: "env", err: rerr}
				return
			}
			var runAs *mcp.ProcessIdentity
			if !privileged {
				identity, configured, ierr := tool.AgentIdentityFromEnv()
				if ierr != nil {
					results <- spawnResult{index: index, alias: s.Alias, stage: "identity", err: ierr}
					return
				}
				if configured {
					runAs = &mcp.ProcessIdentity{
						UID: identity.UID, GID: identity.GID,
						Home: identity.Home, User: identity.User,
					}
				}
			}
			spec := mcp.ServerSpec{
				Alias: s.Alias, Transport: s.Transport, Command: s.Command, Args: s.Args,
				Env: subEnv, RunAs: runAs, Endpoint: s.Endpoint, Headers: resolveHeaderEnv(s.Headers),
				PackageDigest: s.PackageDigest, ExpectedTools: toolNames(s.Tools),
			}
			sctx, cancel := context.WithTimeout(ctx, opts.SpawnTimeout)
			_, serr := mgr.Spawn(sctx, spec)
			cancel()
			results <- spawnResult{index: index, alias: s.Alias, stage: "spawn", err: serr}
		}(index, server)
	}
	ordered := make([]spawnResult, len(manifest.Servers))
	for range manifest.Servers {
		result := <-results
		ordered[result.index] = result
	}
	for _, result := range ordered {
		if result.err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped (%s: %v)", result.alias, result.stage, result.err))
			continue
		}
		spawned[result.alias] = true
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
	if strings.TrimSpace(opts.NativeRoot) != "" {
		native, nativeErr := newNativeLocal(opts.NativeRoot, opts.NativeReadRoots, opts.NativeStateDir, opts.NativeGitPath)
		if nativeErr != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("native local tools skipped: %v", nativeErr))
		} else {
			m.native = native
		}
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
			if s.Alias == "fs" && (te.Name == "write_file" || te.Name == "edit_file") {
				hardenFileMutationTool(bt)
			}
			m.byFunc[fn] = bt
			if bt.surface == Escalate {
				m.escalated = append(m.escalated, fn)
			} else if !m.nativeShadows(bt) {
				m.order = append(m.order, fn)
			}
		}
	}
	sort.Strings(m.order)
	sort.Strings(m.escalated)
}

// nativeShadows keeps legacy local MCP bindings callable for journal replay
// while removing them from the model-facing inventory whenever the in-process
// native runtime is active. One operation must have one canonical schema.
func (m *Manager) nativeShadows(bound *boundTool) bool {
	if m == nil || m.native == nil || bound == nil {
		return false
	}
	alias := strings.ToLower(strings.TrimSpace(bound.alias))
	name := strings.ToLower(strings.TrimSpace(bound.name))
	switch alias {
	case "fs", "filesystem", "files":
		switch name {
		case "read_file", "read_text_file", "read_multiple_files", "write_file", "edit_file", "patch_files", "create_directory", "list_directory", "directory_tree", "move_file", "search_files", "get_file_info":
			return true
		}
	case "exec", "shell", "terminal":
		switch name {
		case "shell", "run", "exec", "execute", "execute_command", "shell_exec":
			return true
		}
	case "git":
		switch name {
		case "git_status", "status", "git_diff", "diff", "git_log", "log", "git_show", "show", "git_branch", "branch":
			return true
		}
	}
	return false
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
	out := make([]llm.Tool, 0, len(m.order)+24)
	for _, fn := range m.order {
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	if m.native != nil {
		out = append(out, nativeSchemas()...)
	}
	if len(m.escalated) > 0 && m.delegate != nil {
		out = append(out, coreExecuteSchema())
	}
	if m.recall != nil {
		out = append(out, memoryRecallSchema())
	}
	if m.mutation != nil {
		out = append(out, memoryMutateSchema())
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
	if m.build != nil {
		out = append(out, buildProjectSchema())
	}
	if m.desktopLook != nil {
		out = append(out, desktopLookSchema())
	}
	if m.desktopA11y != nil {
		out = append(out, desktopA11ySchema())
	}
	return out
}

// SubagentSchemas is the tool surface advertised to a SUB-AGENT: every Natural
// tool, but NOT core_execute (money stays with the user-facing parent — a
// background sub-agent can't service an inline approval gate), memory_recall,
// or spawn_subagents (no recursion). Deterministic order.
func (m *Manager) SubagentSchemas() []llm.Tool {
	out := make([]llm.Tool, 0, len(m.order)+24)
	for _, fn := range m.order {
		// The disposable-desktop lane is a human-shared, spend-capable surface
		// (DOJO guard_surface): it is driven only by the top-level user-facing
		// agent, never advertised to an autonomous sub-agent / Automatrix run.
		if IsDesktopTool(fn) {
			continue
		}
		bt := m.byFunc[fn]
		out = append(out, llm.NewFunctionTool(fn, bt.desc, bt.params))
	}
	if m.native != nil {
		out = append(out, nativeSchemas()...)
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
	content, _, isErr, _, _, _, err := m.dispatch(ctx, funcName, args)
	return content, isErr, err
}

// DispatchMedia is Dispatch plus the out-of-band screenshot URL: when the tool
// returned an image content block (e.g. browser_take_screenshot), its bytes are
// persisted to the media plane and the /media URL is returned SEPARATELY from
// the model-facing content string — which stays a terse placeholder so the
// screenshot never pollutes the transcript (BROWSER-FILMSTRIP req.2). shotURL is
// "" when the tool produced no image or no media sink is wired.
func (m *Manager) DispatchMedia(ctx context.Context, funcName string, args map[string]interface{}) (content, shotURL string, isErr bool, err error) {
	content, shotURL, isErr, _, _, _, err = m.dispatch(ctx, funcName, args)
	return
}

// DispatchMediaClassified is DispatchMedia plus the semantic failure layer and
// concise model-facing failure text. content always remains the raw evidence.
func (m *Manager) DispatchMediaClassified(ctx context.Context, funcName string, args map[string]interface{}) (content, shotURL string, isErr bool, failureClass tool.FailureClass, retryable bool, failureMessage string, err error) {
	return m.dispatch(ctx, funcName, args)
}

// Preflight verifies that each selected operation is genuinely bound to its
// production implementation before it is exposed to the model. MCP operations
// must resolve through the live registry; synthetic operations must have their
// owning runtime dependency wired.
func (m *Manager) Preflight(funcNames []string) error {
	if m == nil {
		return fmt.Errorf("tool manager is unavailable")
	}
	for _, funcName := range funcNames {
		if isNativeTool(funcName) {
			if m.native == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
			continue
		}
		switch funcName {
		case CoreExecuteTool:
			if m.delegate == nil || len(m.escalated) == 0 {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case MemoryRecallTool:
			if m.recall == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case MemoryMutateTool:
			if m.mutation == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case SpawnSubagentsTool:
			if m.swarm == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case ConstructRenderTool:
			if m.surface == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case WriteSkillTool:
			if m.writeSkill == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case TodoTool:
			if m.todo == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case PreviewTool:
			if m.preview == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case BuildProjectTool:
			if m.build == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case DesktopLookTool:
			if m.desktopLook == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case DesktopA11yTool:
			if m.desktopA11y == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		case SavePersonalizationTool:
			if m.personalization == nil {
				return fmt.Errorf("%s is not connected", funcName)
			}
		default:
			bt, ok := m.byFunc[funcName]
			if !ok {
				return fmt.Errorf("%s has no live binding", funcName)
			}
			if _, err := m.registry.Get(bt.uri); err != nil {
				return fmt.Errorf("%s registry preflight: %w", funcName, err)
			}
		}
	}
	return nil
}

// ValidateAndResolve is the side-effect-free half of dispatch. The runtime
// calls it before durably recording EffectStarted, guaranteeing that malformed
// arguments, missing bindings, and registry drift cannot leave a pending
// effect which never had dispatch prerequisites.
func (m *Manager) ValidateAndResolve(funcName string, args map[string]interface{}) error {
	if err := m.Preflight([]string{funcName}); err != nil {
		return err
	}
	var parameters map[string]interface{}
	if isNativeTool(funcName) {
		for _, schema := range nativeSchemas() {
			if schema.Function.Name == funcName {
				parameters = schema.Function.Parameters
				break
			}
		}
	} else if bound, ok := m.byFunc[funcName]; ok && bound != nil {
		parameters = bound.params
	} else {
		var schema llm.Tool
		switch funcName {
		case CoreExecuteTool:
			schema = coreExecuteSchema()
		case MemoryRecallTool:
			schema = memoryRecallSchema()
		case MemoryMutateTool:
			schema = memoryMutateSchema()
		case SpawnSubagentsTool:
			schema = spawnSubagentsSchema()
		case ConstructRenderTool:
			schema = constructRenderSchema()
		case WriteSkillTool:
			schema = writeSkillSchema()
		case TodoTool:
			schema = todoSchema()
		case PreviewTool:
			schema = previewSchema()
		case BuildProjectTool:
			schema = buildProjectSchema()
		case DesktopLookTool:
			schema = desktopLookSchema()
		case DesktopA11yTool:
			schema = desktopA11ySchema()
		case SavePersonalizationTool:
			schema = PersonalizationSchema()
		}
		parameters = schema.Function.Parameters
	}
	if parameters == nil {
		return fmt.Errorf("%s has no registered argument schema", funcName)
	}
	if message, valid := validateToolArgs(funcName, parameters, args); !valid {
		return errors.New(message)
	}
	return nil
}

func (m *Manager) dispatch(ctx context.Context, funcName string, args map[string]interface{}) (content, shotURL string, isErr bool, failureClass tool.FailureClass, retryable bool, failureMessage string, err error) {
	if isNativeTool(funcName) {
		if m.native == nil {
			return fmt.Sprintf("native tool %q is unavailable in this session", funcName), "", true, tool.FailureInvocation, false, "", nil
		}
		for _, schema := range nativeSchemas() {
			if schema.Function.Name == funcName {
				if msg, ok := validateToolArgs(funcName, schema.Function.Parameters, args); !ok {
					return msg, "", true, tool.FailureValidation, false, msg, nil
				}
				break
			}
		}
		c, e, er := m.native.call(ctx, funcName, args)
		return syntheticResult(c, e, er)
	}
	switch funcName {
	case CoreExecuteTool:
		c, e, er := m.dispatchCoreExecute(ctx, args)
		return syntheticResult(c, e, er)
	case MemoryRecallTool:
		c, e, er := m.dispatchMemoryRecall(ctx, args)
		return syntheticResult(c, e, er)
	case MemoryMutateTool:
		c, e, er := m.dispatchMemoryMutation(ctx, args)
		return syntheticResult(c, e, er)
	case SpawnSubagentsTool:
		c, e, er := m.dispatchSpawnSubagents(ctx, args)
		return syntheticResult(c, e, er)
	case ConstructRenderTool:
		c, e, er := m.dispatchConstructRender(ctx, args)
		return syntheticResult(c, e, er)
	case WriteSkillTool:
		c, e, er := m.dispatchWriteSkill(ctx, args)
		return syntheticResult(c, e, er)
	case TodoTool:
		c, e, er := m.dispatchTodo(ctx, args)
		return syntheticResult(c, e, er)
	case PreviewTool:
		c, e, er := m.dispatchPreview(ctx)
		return syntheticResult(c, e, er)
	case BuildProjectTool:
		c, e, er := m.dispatchBuildProject(ctx, args)
		return syntheticResult(c, e, er)
	case DesktopLookTool:
		c, e, er := m.dispatchDesktopLook(ctx, args)
		return syntheticResult(c, e, er)
	case DesktopA11yTool:
		c, e, er := m.dispatchDesktopA11y(ctx)
		return syntheticResult(c, e, er)
	case SavePersonalizationTool:
		c, e, er := m.dispatchPersonalizationSave(ctx, args)
		return syntheticResult(c, e, er)
	}
	bt, ok := m.byFunc[funcName]
	if !ok {
		return fmt.Sprintf("unknown tool %q — it is not available in this session", funcName), "", true, tool.FailureInvocation, false, "", nil
	}
	if bt.surface == Escalate {
		return fmt.Sprintf("%q moves funds or needs a wallet signature and cannot be called directly; use %q with a clear description of the task so it runs through the secure path under the user's authorization (their inline approval, or a pre-authorized wallet leash).", funcName, CoreExecuteTool), "", true, tool.FailureInvocation, false, "", nil
	}
	if m.mutation != nil && isShellMemoryMutation(bt, args) {
		return "memory mutation through shell, curl, or a memory-store CLI is unavailable; use memory_mutate with an explicit typed operation and bounded target instead", "", true, tool.FailureInvocation, false, "", nil
	}
	if bt.alias == "fs" && (bt.name == "write_file" || bt.name == "edit_file") {
		if size := fileMutationBytes(args); size > maxFileMutationBytes {
			return fmt.Sprintf(
				"file mutation payload is %d bytes; the per-call limit is %d bytes. Write an initial bounded chunk, then continue with anchored edit_file batches until the file is complete",
				size,
				maxFileMutationBytes,
			), "", true, tool.FailureValidation, false, "", nil
		}
		// O1 req.5 ac.2: reject oversized payloads before model generation.
		// Validate edit_file oldText/newText pairs are non-empty.
		if bt.name == "edit_file" {
			if edits, ok := args["edits"].([]interface{}); ok {
				for i, raw := range edits {
					edit, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					old, _ := edit["oldText"].(string)
					if strings.TrimSpace(old) == "" {
						return fmt.Sprintf(
							"edit_file edit %d has empty oldText — every edit must specify the exact existing text to replace",
							i,
						), "", true, tool.FailureValidation, false, "", nil
					}
				}
			}
		}
	}
	// Validate the arguments against the tool's advertised JSON Schema BEFORE the
	// round-trip (errors that teach): a missing required field or a wrong-typed
	// field becomes an exact, corrective message naming the field and the expected
	// shape, caught here instead of surfacing as an opaque downstream server error
	// the model can't act on. Deterministic (identical args fail identically), so
	// it is not retried — the model must correct the call.
	if msg, ok := validateToolArgs(funcName, bt.params, args); !ok {
		return msg, "", true, tool.FailureValidation, false, msg, nil
	}
	t, err := m.registry.Get(bt.uri)
	if err != nil {
		return fmt.Sprintf("tool %q is unavailable: %v", funcName, err), "", true, tool.FailureClassOf(err), false, "", nil
	}
	res, err := t.Call(ctx, args)
	if err != nil {
		return "", "", true, tool.FailureClassOf(err), false, "", err
	}
	text := tool.ExtractText(res)
	// Persist any image content block out-of-band (req.2): the /media URL rides
	// the return, never the model-facing text. Best-effort — a persist error
	// leaves shotURL empty and never disturbs the result.
	shotURL = m.persistFirstImage(res)
	if text == "" {
		text = summarizeNonText(res)
	}
	return text, shotURL, res.IsError, res.FailureClass, res.Retryable, res.FailureMessage, nil
}

func syntheticResult(content string, isErr bool, err error) (string, string, bool, tool.FailureClass, bool, string, error) {
	if err != nil {
		class := tool.FailureClassOf(err)
		return content, "", true, class, class == tool.FailureTransport, err.Error(), err
	}
	if isErr {
		message := strings.Join(strings.Fields(content), " ")
		if message == "" {
			message = "The application rejected the operation."
		}
		return content, "", true, tool.FailureApplication, false, message, nil
	}
	return content, "", false, tool.FailureNone, false, "", nil
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

func (m *Manager) dispatchMemoryMutation(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.mutation == nil {
		return "the typed memory mutation surface is not connected in this session.", true, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "memory_mutate received invalid arguments.", true, nil
	}
	var req memory.MutationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Sprintf("memory_mutate arguments are invalid: %v", err), true, nil
	}
	// Model-facing confirmations never expose internal Neocortex identifiers.
	req.IncludeInternalIDs = false
	result, err := m.mutation(ctx, req)
	if err != nil {
		return fmt.Sprintf("memory mutation failed: %v", err), true, nil
	}
	lines := make([]string, 0, len(result.Results))
	for _, item := range result.Results {
		if text := strings.TrimSpace(item.Description); text != "" {
			lines = append(lines, text)
		}
	}
	if len(lines) == 0 {
		return "Memory updated.", false, nil
	}
	return strings.Join(lines, " "), false, nil
}

func isShellMemoryMutation(bt *boundTool, args map[string]interface{}) bool {
	if bt == nil {
		return false
	}
	toolName := strings.ToLower(bt.funcName + " " + bt.name)
	if !strings.Contains(toolName, "shell") && !strings.Contains(toolName, "exec") && !strings.Contains(toolName, "terminal") {
		return false
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return false
	}
	command := strings.ToLower(string(raw))
	if strings.Contains(command, "memory") && strings.Contains(command, "shell") {
		for _, verb := range []string{" write", " update", " tombstone", " add-edge", " remove-edge"} {
			if strings.Contains(command, verb) {
				return true
			}
		}
	}
	if !strings.Contains(command, "/memory/") && !strings.Contains(command, "/memory\"") {
		return false
	}
	for _, marker := range []string{"-x post", "-x put", "-x patch", "-x delete", "--request post", "--request put", "--request patch", "--request delete", " curl ", "-d ", "--data"} {
		if strings.Contains(command, marker) {
			return true
		}
	}
	return false
}

// dispatchWriteSkill is the P2-2 skill-writing handler: it parses the model's
// write_skill arguments into a validated PatternSpec and persists it as a
// Neocortex Pattern via the injected WriteSkillFunc (which calls
// ReinforcePattern). A spec with no usable identity (empty name, trigger, and
// steps) is rejected in-band so the model reads the error and corrects rather
// than the harness retrying. On success the Neocortex URI is returned so the
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
	return fmt.Sprintf("Persisted skill %q as a Neocortex Pattern (%s). It starts as a low-coverage candidate and is reinforced on each repeat success; it becomes active after %d proven successes.", spec.Name, uri, 0), false, nil
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

func (m *Manager) SetBuildProject(f BuildFunc) { m.build = f }

func (m *Manager) BuildProjectEnabled() bool { return m != nil && m.build != nil }

// SetDesktop wires the disposable-desktop grounding synthetics (DOJO req 3)
// after construction (they need the engine's dojo session manager + omni vision
// client assembled first). Passing nil for either leaves that tool unadvertised.
func (m *Manager) SetDesktop(look DesktopLookFunc, a11y DesktopA11yFunc) {
	if m == nil {
		return
	}
	m.desktopLook = look
	m.desktopA11y = a11y
}

// DesktopEnabled reports whether the desktop grounding lane is wired this
// session (at least desktop_look).
func (m *Manager) DesktopEnabled() bool { return m != nil && m.desktopLook != nil }

// dispatchDesktopLook grounds the desktop with the stateless omni vision call.
func (m *Manager) dispatchDesktopLook(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.desktopLook == nil {
		return "the disposable desktop is not available in this environment", true, nil
	}
	target := strings.TrimSpace(asString(args["target"]))
	out, err := m.desktopLook(ctx, target)
	if err != nil {
		return fmt.Sprintf("could not ground the desktop: %v", err), true, nil
	}
	return out, false, nil
}

// dispatchDesktopA11y dumps the desktop's accessibility tree (deterministic
// middle rung).
func (m *Manager) dispatchDesktopA11y(ctx context.Context) (string, bool, error) {
	if m.desktopA11y == nil {
		return "the disposable desktop is not available in this environment", true, nil
	}
	out, err := m.desktopA11y(ctx)
	if err != nil {
		return fmt.Sprintf("could not read the desktop accessibility tree: %v", err), true, nil
	}
	return out, false, nil
}

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

func (m *Manager) dispatchBuildProject(ctx context.Context, args map[string]interface{}) (string, bool, error) {
	if m.build == nil {
		return "your durable Build mode is not available on this machine", true, nil
	}
	request := strings.TrimSpace(asString(args["request"]))
	if request == "" {
		return "request is required", true, nil
	}
	out, err := m.build(ctx, BuildRequest{
		Project:            strings.TrimSpace(asString(args["project"])),
		Request:            request,
		AcceptanceCriteria: asStringSlice(args["acceptance_criteria"]),
		Constraints:        asStringSlice(args["constraints"]),
	})
	if err != nil {
		return fmt.Sprintf("could not accept the Build job: %v", err), true, nil
	}
	if strings.TrimSpace(out) == "" {
		out = "The Build job was accepted. I will return when it needs input or has a verified result."
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

// SetMemoryMutation wires the typed user-memory mutation surface. When wired,
// the manager also rejects shell-based memory writes before MCP dispatch.
func (m *Manager) SetMemoryMutation(f MemoryMutationFunc) { m.mutation = f }

func (m *Manager) MemoryMutationEnabled() bool { return m != nil && m.mutation != nil }

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
// not advertised. The func persists a validated PatternSpec as a Neocortex
// Pattern via ReinforcePattern.
func (m *Manager) SetWriteSkill(f WriteSkillFunc) { m.writeSkill = f }

// WriteSkillEnabled reports whether the write_skill tool is wired this session
// (a durable Neocortex store is connected and skill authoring is available).
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
// (a durable Neocortex store is connected). The system prompt uses this to teach
// the model to PULL memory mid-thought (v3 #1) only when it can actually do so.
func (m *Manager) RecallEnabled() bool { return m != nil && m.recall != nil }

// SpawnEnabled reports whether spawn_subagents is advertised this session (the
// swarm runner is wired) AND, therefore, whether the top-level agent can
// decompose at all. The self-aware decomposition router (self-model task 4.2)
// surfaces the agent's own context limits at the decision point only when this
// is true — there is no point teaching a decision the agent cannot act on.
func (m *Manager) SpawnEnabled() bool { return m != nil && m.swarm != nil }

func (m *Manager) ConfigureMachineMail(ctx context.Context, apiKey string) error {
	if m.mcp == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	client := m.mcp.Client("machine-mail")
	if client == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	var result struct {
		Configured bool `json:"configured"`
	}
	if err := client.Call(ctx, "machinemail/configure", map[string]string{"api_key": apiKey}, &result); err != nil {
		return err
	}
	if !result.Configured {
		return errors.New("MachineMail did not accept the API key")
	}
	return nil
}

func (m *Manager) LoadMachineMail(ctx context.Context, apiKey string) error {
	if m.mcp == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	client := m.mcp.Client("machine-mail")
	if client == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	return client.Call(ctx, "machinemail/load", map[string]string{"api_key": apiKey}, nil)
}

func (m *Manager) ClearMachineMail(ctx context.Context) error {
	if m.mcp == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	client := m.mcp.Client("machine-mail")
	if client == nil {
		return errors.New("MachineMail tool server is unavailable")
	}
	return client.Call(ctx, "machinemail/clear", struct{}{}, nil)
}

// NaturalToolNames returns the advertised (directly-callable) function names.
func (m *Manager) NaturalToolNames() []string {
	out := append([]string{}, m.order...)
	if m != nil && m.native != nil {
		for _, schema := range nativeSchemas() {
			out = append(out, schema.Function.Name)
		}
	}
	sort.Strings(out)
	return out
}

// EscalateToolNames returns the escalate-class function names (reachable only
// via core_execute), for transparency / system-prompt construction.
func (m *Manager) EscalateToolNames() []string { return append([]string{}, m.escalated...) }

// Warnings returns non-fatal MCP server start failures from Spawn.
func (m *Manager) Warnings() []string { return append([]string{}, m.warnings...) }

// NativeLocalEnabled reports whether Neo has its in-process local tool runtime.
func (m *Manager) NativeLocalEnabled() bool { return m != nil && m.native != nil }

// NativeToolNames returns the deterministic in-process local tool inventory.
func (m *Manager) NativeToolNames() []string {
	if m == nil || m.native == nil {
		return nil
	}
	out := make([]string, 0, len(nativeSchemas()))
	for _, schema := range nativeSchemas() {
		out = append(out, schema.Function.Name)
	}
	return out
}

// Close stops every MCP server.
func (m *Manager) Close() error {
	if m.mcp == nil {
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
		"Search your own durable memory (the Neocortex) — the user's profile, stored facts, past outcomes, preferences, and proven approaches — which persists across conversations and restarts. This is your PRIMARY way to bring in prior context: PULL from it before you reason about the user, their projects, or past work, and before claiming a fact you'd have learned earlier. Use it ITERATIVELY: start broad, read what comes back, then call again with a narrower query (or a type filter) as you learn what you actually need. Each result line shows the memory's type, any contradiction to reconcile, and its Neocortex URI so you can cite it. Returns a rendered digest, ranked by how useful each memory has proven. SELF-MODEL: a query of \"self:\" returns your compact structural self-summary (your own loop, faculties, window assembly, and safety wall, derived from your source), and \"self:<Symbol>\" (e.g. \"self:assembleWindowUserTail\") pages the full graph fragment for one of your own symbols on demand — use it to reason about how you are built and where you are limited.",
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

func memoryMutateSchema() llm.Tool {
	value := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"type": map[string]interface{}{
				"type": "string", "enum": []string{"fact", "preference", "belief", "goal", "constraint"},
				"description": "Required for create; otherwise it must match the target type.",
			},
			"content": map[string]interface{}{
				"type": "string", "description": "The new fact/belief/goal/constraint statement, or preference topic.",
			},
			"subject":   map[string]interface{}{"type": "string", "description": "Optional fact/belief subject override."},
			"predicate": map[string]interface{}{"type": "string", "description": "Optional fact predicate override."},
			"polarity":  map[string]interface{}{"type": "string", "description": "Optional preference or constraint polarity."},
			"strength":  map[string]interface{}{"type": "number", "minimum": 0, "maximum": 1},
			"rationale": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"content"},
	}
	target := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"uri":   map[string]interface{}{"type": "string", "description": "Exact Neocortex URI when already known."},
			"query": map[string]interface{}{"type": "string", "description": "A narrow semantic description of the one current memory to change."},
			"types": map[string]interface{}{
				"type": "array", "items": map[string]interface{}{"type": "string", "enum": []string{"fact", "preference", "belief", "goal", "constraint"}},
				"description": "Optional semantic-target type filter.",
			},
		},
	}
	return llm.NewFunctionTool(
		MemoryMutateTool,
		"Create, update, replace, or delete durable user memory through one bounded typed operation. Use supersede for corrections: it atomically writes the replacement, records provenance, and makes the old value historical-only. Pass several items to correct several facts in one call. For update/supersede/delete, identify exactly one current record by URI or a narrow semantic target; ambiguous targets fail instead of being guessed. Never use shell, curl, or internal API discovery for memory mutation.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"items": map[string]interface{}{
					"type": "array", "minItems": 1, "maxItems": 20,
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"operation":       map[string]interface{}{"type": "string", "enum": []string{"create", "update", "supersede", "delete"}},
							"target":          target,
							"replacement_uri": map[string]interface{}{"type": "string", "description": "Optional distinct existing record to update as the superseding replacement."},
							"value":           value,
							"reason":          map[string]interface{}{"type": "string"},
						},
						"required": []interface{}{"operation"},
					},
				},
			},
			"required": []interface{}{"items"},
		},
	)
}

// writeSkillSchema advertises the P2-2 skill-writing tool. The agent authors a
// structured PatternSpec after a proven task; this tool validates it and
// persists it as a Neocortex Pattern (the synthesis loop). The spec mirrors
// PatternSpec exactly (name, trigger, preconditions, steps, gotchas,
// success_criteria).
func writeSkillSchema() llm.Tool {
	return llm.NewFunctionTool(
		WriteSkillTool,
		"Persist a reusable recipe as a durable skill (a Neocortex Pattern) after a task you've proven works. This is the synthesis loop: you CONSCIOUSLY distill the proven tool sequence into a structured recipe so you can reapply it to similar future tasks. The recipe starts as a low-coverage candidate and is reinforced on each repeat success; it only becomes active (injected into future prompts) after it has been proven multiple times, so don't overfit a one-off flow. Provide the full structure: a short name, when to apply it (trigger), what must be true first (preconditions), the exact tool sequence (steps), learned failure modes (gotchas), and how to verify it worked (success_criteria). You can recall any persisted skill later via memory_recall with types=[\"pattern\"].",
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

// previewSchema advertises the workbench preview launcher for explicit
// development-sandbox requests. It is not the delivery path for compiled
// frontend apps or sites.
func previewSchema() llm.Tool {
	return llm.NewFunctionTool(
		PreviewTool,
		"Start (or restart) an isolated development sandbox in the workbench Preview pane only when the user explicitly asks for a sandbox or dev-server preview. Never use this tool to present a frontend app or site after it compiles successfully; that result must be deployed through Paxeer Cloud with paxc and presented with the real paxc URL. Sandbox provisioning is asynchronous, so call this at most once for an explicit sandbox request and do not poll it.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	)
}

func buildProjectSchema() llm.Tool {
	return llm.NewFunctionTool(
		BuildProjectTool,
		"Enter your durable Build mode for a substantial or long-running coding job that should survive this turn and carry checkpoints, autonomous verification, interruption, and resume. You remain the only builder; this is your persistent coding execution mode, not delegation to another agent. Use your native local tools for bounded file work, diagnostics, shell commands, durable services, and read-only git inspection. Provide the complete user request, constraints, concrete acceptance criteria, and project when starting new work. A missing project is created and selected atomically with durable job admission; replays resolve the same project and job. The call returns as soon as your job is persisted and accepted; do not poll it or keep this turn open. After a successful frontend compile, follow your Paxeer Cloud post-build delivery rule.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project": map[string]interface{}{
					"type":        "string",
					"description": "Existing project ID/name, or the name of a new project to create and select atomically with Build admission. Omit only when the conversation already has the correct active project.",
				},
				"request": map[string]interface{}{
					"type":        "string",
					"description": "A self-contained description of exactly what to build or change in the selected project.",
				},
				"acceptance_criteria": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"},
					"description": "Concrete observable behaviors that define success.",
				},
				"constraints": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"},
					"description": "Framework, database, compatibility, security, and no-deployment constraints.",
				},
			},
			"required": []interface{}{"request", "acceptance_criteria"},
		},
	)
}

func desktopLookSchema() llm.Tool {
	return llm.NewFunctionTool(
		DesktopLookTool,
		"Look at the disposable desktop and get grounded, clickable coordinates. Captures the screen and returns the interactive elements with their exact screen-pixel positions, plus the single best click point for a named target. This is the LAST rung of the driving ladder — reach for it only when the browser DOM (for web pages) and desktop_a11y (for native apps) can't locate what you need, e.g. a canvas UI, an icon, or a visual check. Pass the coordinates it returns straight to desktop_click_mouse / desktop_move_mouse.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"target": map[string]interface{}{
					"type":        "string",
					"description": "What you are looking for (e.g. \"the Submit button\", \"the search box\"). Leave empty to list all interactive elements on screen.",
				},
			},
		},
	)
}

func desktopA11ySchema() llm.Tool {
	return llm.NewFunctionTool(
		DesktopA11yTool,
		"Read the disposable desktop's accessibility tree: the interactive elements of native (XFCE/GTK) apps with their roles, labels, and exact screen-pixel coordinates — deterministic, no vision needed. This is the MIDDLE rung of the driving ladder: prefer it over desktop_look for native application windows (menus, dialogs, buttons, fields). If it returns no elements (a canvas or Electron UI), fall back to desktop_look. Pass the coordinates it returns straight to desktop_click_mouse / desktop_move_mouse.",
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

// FunctionName returns the canonical function name used by Neo's registry for
// one server-local MCP operation. Product-owned direct callers must use the
// same name as the model-facing registry rather than guessing the local name.
func FunctionName(alias, name string) string {
	return funcName(alias, name)
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

// validateToolArgs checks a call's arguments against the tool's advertised JSON
// Schema before dispatch (errors that teach). It validates only the two mistakes
// an LLM actually makes — omitting a REQUIRED field, and passing a value whose
// JSON category clearly contradicts the declared type — and returns a corrective
// message that NAMES the field, its expected type, and a copy-pasteable example.
// It is deliberately conservative on types (only clear object/array/scalar
// category mismatches, never scalar coercions a server would accept) so it never
// rejects a well-formed call over a constraint the model couldn't know from the
// schema. Returns ("", true) when the arguments are acceptable. The message
// carries NO "class=" prefix — the dispatch ladder adds the failure-class prefix.
func validateToolArgs(funcName string, params map[string]interface{}, args map[string]interface{}) (string, bool) {
	if params == nil {
		return "", true
	}
	props, _ := params["properties"].(map[string]interface{})
	if required, ok := params["required"].([]interface{}); ok {
		for _, r := range required {
			field, _ := r.(string)
			if field == "" {
				continue
			}
			if v, present := args[field]; !present || isEmptyArg(v) {
				msg := fmt.Sprintf(
					"%s is missing required argument %q. Re-issue the call with every required field: %s.",
					funcName, field, requiredFieldList(props, required),
				)
				if ex := exampleForSchema(props, required); ex != "" {
					msg += " " + ex
				}
				return msg, false
			}
		}
	}
	for k, v := range args {
		spec, ok := props[k].(map[string]interface{})
		if !ok {
			continue // unknown/extra field or a non-object property spec: don't reject
		}
		want, _ := spec["type"].(string)
		if want == "" || jsonTypeMatches(want, v) {
			continue
		}
		return fmt.Sprintf(
			"argument %q of %s must be %s, but a %s was passed. Re-issue the call with %q as %s.",
			k, funcName, want, jsonTypeName(v), k, want,
		), false
	}
	return "", true
}

// isEmptyArg reports whether a decoded argument is effectively absent — nil, or a
// blank/whitespace-only string. A missing-vs-empty required field is the same
// mistake to teach, so both trip the required check.
func isEmptyArg(v interface{}) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

// jsonTypeMatches reports whether a decoded JSON value is compatible with a JSON
// Schema type. Conservative by design: it rejects only clear CATEGORY mismatches
// (an object/array where a scalar is expected, or vice versa) and accepts scalar
// forms a tool server routinely coerces, so a valid call is never rejected.
func jsonTypeMatches(want string, v interface{}) bool {
	switch want {
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "string":
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			return false
		}
		return true
	case "number", "integer":
		switch v.(type) {
		case map[string]interface{}, []interface{}, bool:
			return false
		}
		return true
	case "boolean":
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			return false
		}
		return true
	default:
		return true // union types, "null", unknown: don't reject
	}
}

// jsonTypeName names a decoded JSON value's type for a corrective message.
func jsonTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "value"
	}
}

// requiredFieldList renders the required fields with their declared types for a
// corrective message, e.g. `query (string), max_results (integer)`.
func requiredFieldList(props map[string]interface{}, required []interface{}) string {
	parts := make([]string, 0, len(required))
	for _, r := range required {
		field, _ := r.(string)
		if field == "" {
			continue
		}
		parts = append(parts, field+" ("+fieldType(props, field)+")")
	}
	return strings.Join(parts, ", ")
}

// exampleForSchema renders a compact, copy-pasteable JSON object of the required
// fields with typed placeholders, so the corrective message shows the shape of a
// valid call. Empty when there are no required fields.
func exampleForSchema(props map[string]interface{}, required []interface{}) string {
	ex := make(map[string]interface{}, len(required))
	for _, r := range required {
		field, _ := r.(string)
		if field == "" {
			continue
		}
		ex[field] = placeholderForType(fieldType(props, field))
	}
	if len(ex) == 0 {
		return ""
	}
	b, err := json.Marshal(ex)
	if err != nil {
		return ""
	}
	return "Example: " + string(b)
}

// fieldType returns a property's declared JSON Schema type, or "any" when the
// schema does not pin one (a union or absent type).
func fieldType(props map[string]interface{}, field string) string {
	if spec, ok := props[field].(map[string]interface{}); ok {
		if t, ok := spec["type"].(string); ok && t != "" {
			return t
		}
	}
	return "any"
}

// placeholderForType returns a typed placeholder value for an example call.
func placeholderForType(t string) interface{} {
	switch t {
	case "number", "integer":
		return 0
	case "boolean":
		return false
	case "array":
		return []interface{}{}
	case "object":
		return map[string]interface{}{}
	default:
		return "..."
	}
}

func hardenFileMutationTool(bt *boundTool) {
	if bt == nil {
		return
	}
	bt.desc = strings.TrimSpace(bt.desc) + fmt.Sprintf(
		" Each call is limited to %d bytes of mutation text. Build large files incrementally: write one bounded initial chunk, then append the remaining content with anchored edit_file batches.",
		maxFileMutationBytes,
	)
	props, _ := bt.params["properties"].(map[string]interface{})
	if props == nil {
		props = map[string]interface{}{}
		bt.params["properties"] = props
	}
	if bt.name == "write_file" {
		content, _ := props["content"].(map[string]interface{})
		if content == nil {
			content = map[string]interface{}{"type": "string"}
			props["content"] = content
		}
		content["maxLength"] = maxFileMutationBytes
		return
	}
	edits, _ := props["edits"].(map[string]interface{})
	items, _ := edits["items"].(map[string]interface{})
	editProps, _ := items["properties"].(map[string]interface{})
	for _, key := range []string{"oldText", "newText"} {
		field, _ := editProps[key].(map[string]interface{})
		if field != nil {
			field["maxLength"] = maxFileMutationBytes
		}
	}
}

func fileMutationBytes(v interface{}) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []interface{}:
		total := 0
		for _, item := range x {
			total += fileMutationBytes(item)
		}
		return total
	case map[string]interface{}:
		total := 0
		for key, item := range x {
			switch key {
			case "content", "oldText", "newText":
				total += fileMutationBytes(item)
			default:
				if key == "edits" {
					total += fileMutationBytes(item)
				}
			}
		}
		return total
	default:
		return 0
	}
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
