// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package server turns Neo into a production conversational service: it speaks
// the daemon's POST /chat + GET /events SSE contract (so the existing web and
// Telegram clients work unchanged), streams the agent loop's work — including
// live web-search snippets and source cards — and reverse-proxies everything
// else to the co-located MCL daemon. core_execute delegates rigorous / money
// tasks to that daemon over HTTP exactly as the frozen spec's
// [relation.delegation] prescribes.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrix/construct/backchannel"
	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
	"matrix/construct/transport"
	"matrix/neo/internal/agent"
	"matrix/neo/internal/automatrixlog"
	"matrix/neo/internal/automatrixsettings"
	"matrix/neo/internal/briefhistory"
	"matrix/neo/internal/briefsettings"
	"matrix/neo/internal/capabilityhub"
	"matrix/neo/internal/channelgateway"
	"matrix/neo/internal/cloudchannels"
	"matrix/neo/internal/codingruntime"
	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/dojo"
	"matrix/neo/internal/improvement"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/machinemailsettings"
	"matrix/neo/internal/mcpcontrol"
	"matrix/neo/internal/mediastudio"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/notify"
	"matrix/neo/internal/preview"
	"matrix/neo/internal/runrecord"
	profileStore "matrix/neo/internal/runtime/profile"
	"matrix/neo/internal/sandbox"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/task"
	"matrix/neo/internal/telegramsettings"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/trace"
	"matrix/vault"
)

// Engine holds the process-wide shared dependencies (models, the native plus
// integration tool surface, the Neocortex pager, the background consolidator) and hands each
// conversation its own agent loop over them.
type Engine struct {
	cfg   config.Config
	main  *llm.Client
	cheap *llm.Client
	// subMain is the main-capability model with EXTENDED REASONING OFF, used
	// for background sub-agents (spawn_subagents). Only the user-facing Neo loop
	// (e.main) and the core MCL pipeline think; background/headless agents run
	// without thinking to cut latency + token burn. nil falls back to e.main.
	subMain            *llm.Client
	tools              *tools.Manager
	pager              *memory.Pager
	runtime            *agent.ResurrectionRuntime
	consolidator       agent.Consolidator
	conv               *conversation.Store // durable chat-thread history (per conversation_id)
	tasks              *task.Store         // durable task-supervision ledger (survives restart/suspend)
	runRecords         *runrecord.Store
	journal            *sessionjournal.Store
	trace              *trace.Store         // durable per-run workspace timeline ("Neo's Computer"); sidecar, never Neocortex
	automatrix         *automatrixlog.Store // durable Automatrix completion inbox (in-app surprise results); sidecar, never Neocortex
	briefHistory       *briefhistory.Store  // durable morning-brief recommendation history + feedback (ORACLE task 5.5); sidecar, never Neocortex
	capabilities       *capabilityhub.Store // optional, independently failing capability package registry
	capabilityErr      error
	capabilityLibrary  string
	improvement        *improvement.Store
	improvementErr     error
	improvementAlarms  improvementAlarmController
	improvementWG      sync.WaitGroup
	mcpControl         *mcpcontrol.Store
	mcpControlErr      error
	mcpBoundary        sync.Mutex
	mcpLoaded          bool
	mcpDirty           bool
	channelGateway     *channelgateway.Store
	channelDispatch    *channelgateway.Dispatcher
	channelGatewayErr  error
	cloudChannels      *cloudChannelBridge
	telegram           *telegramBridge
	machineMail        *machineMailBridge
	mediaDir           string // machine-volume dir for generated + uploaded media ("" disables)
	mediaStudio        *mediastudio.Service
	mediaStudioErr     error
	buildJobs          *codingruntime.Store
	buildWorker        *buildWorkerService
	buildDispatch      func(codingruntime.Job)
	buildInterrupt     func(context.Context, codingruntime.Job) error
	voiceASRURL        string
	voiceASRKey        string
	voiceControllerURL string
	workspaceRoot      string         // VM workspace root; projects are its subdirectories ("" disables the workbench surface)
	vault              *vault.Session // fail-closed data-at-rest encryption session (nil = plaintext dev/CLI)
	vaultUser          string         // user DID bound into sealed objects' associated data

	// projects is the workbench project registry (lazily built over
	// workspaceRoot; nil when the workbench surface is disabled).
	projectsOnce sync.Once
	projects     *projectRegistry

	// preview is the on-demand Railway-sandbox preview controller (nil when
	// unconfigured — the route answers 503 and the workbench renders the
	// honest empty state).
	preview *preview.Manager

	// dojo is the disposable-desktop session lifecycle manager (nil when
	// unconfigured — no standing desktop service ever exists; DOJO req 1).
	dojo *dojo.Manager

	// omni is the stateless MiMo v2.5 omni vision client used ONLY inside
	// desktop_look grounding (DOJO req 3.2); the session model is never swapped
	// to it. nil disables desktop_look (a11y + DOM still ground the desktop).
	omni *llm.Client

	// dojoBridgeToken gates the loopback /dojo/computer-use proxy the desktop
	// bridge posts to, so only the co-located bridge can drive the desktop
	// (external router-forwarded traffic is rejected). "" disables the proxy.
	dojoBridgeToken string

	backendURL   string // co-located MCL daemon (core_execute + reverse proxy)
	backendToken string // optional bearer for the daemon

	// userProfile holds the onboarding profile fetched from the daemon's
	// /profile endpoint. Per-user-stable; injected into every agent via
	// SetUserProfile so it lives in the stable prompt prefix. Refreshed on
	// a TTL at session rebuild so a later edit (Settings, req 8.2) reflects
	// on subsequent conversations/turns without a process restart. Guarded
	// by profileMu because rebuilds run concurrently across sessions.
	profileMu            sync.Mutex
	userAgentName        string
	userPreferredName    string
	userExpertiseDomains []string
	profileFetchedAt     time.Time
	profileStore         *profileStore.Store
	profileStoreErr      error

	broker   *broker
	sessions *sessionRegistry

	mu   sync.Mutex
	runs map[string]*run // active runs by id, for gate-answer routing

	// gateClaims serialises approval-gate answering across the per-call
	// delegate clients (engine.coreExecute builds a fresh delegate.Client per
	// call). Keyed on MCL intent id + gate node id, it lets only ONE delegate
	// service a given gate, so two concurrent attaches to the same in-flight
	// intent (a re-dispatch joining a parked launch) cannot double-answer it
	// (req 2.4). This is the delegate-side reconcile chosen over a daemon-side
	// idempotent-create: it lives entirely on the conversational seam and never
	// touches the signed MCL path.
	gateClaimsMu sync.Mutex
	gateClaims   map[string]bool

	// Automatrix idle-wake handler seams (task 3.3). automatrixGov is the
	// per-user opt-in + Chronos-alarm bookkeeping seam (satisfied by task 6.1);
	// automatrixRunner dispatches the supervised restricted-surface run for a
	// picked opportunity (the task 4.1 hand-off). Both are nil until their wave
	// wires them: a wake with no governor does nothing (fail closed — proactive
	// work never runs without an authoritative opt-in), and a pick with no
	// runner reschedules without running. automatrixRand is the injectable,
	// seedable RNG behind the jitter + probabilistic-skip reschedule (guarded by
	// automatrixRandMu because rand.Rand is not safe for concurrent use);
	// automatrixSkipProb is the per-fire skip probability (config-like; tests
	// pin it to 0/1 for determinism).
	automatrixGov    AutomatrixGovernor
	automatrixRunner AutomatrixRunner
	// briefGov is the ORACLE morning-brief opt-in + MORNING_BRIEF Chronos-alarm
	// bookkeeping seam (task 5.2). Nil until wired (BriefSettingsDir set, or a
	// test seam via SetBriefGovernor); the wake handler + control routes fail
	// closed without it.
	briefGov *briefGovernor
	// briefRunner dispatches the restricted, supervised morning-brief run for a
	// fired MORNING_BRIEF wake (task 5.4). Nil until wired (serve.go sets it to
	// RunMorningBrief; tests may install a seam via SetBriefRunner): a wake with
	// no runner does nothing (fail closed). briefInflight is the busy signal
	// that keeps a fresh wake from double-dispatching a brief already underway;
	// briefWG tracks the dispatch goroutines so Close drains them (bounded) —
	// a run that outlives the bound stays durably running in the task ledger and
	// resumes on the restricted brief surface at next boot.
	briefRunner        BriefRunner
	briefInflight      atomic.Int32
	briefWG            sync.WaitGroup
	automatrixRandMu   sync.Mutex
	automatrixRand     *rand.Rand
	automatrixSkipProb float64

	// automatrixInflight counts autonomous Automatrix runs currently executing
	// (task 4.1). It is the busy signal that makes a fresh wake DEFER while a
	// proactive run is already underway (req 4.5): Automatrix never runs two
	// proactive tasks at once for a user, and never competes with the run it
	// already started. Bumped around the supervised dispatch goroutine.
	automatrixInflight atomic.Int32
	// automatrixWG tracks the same dispatch goroutines for shutdown: Close
	// waits (bounded) so an in-flight settle never touches the pager after the
	// caller has closed it. A run that outlives the bound simply stays
	// in_progress and is resumed at next boot (ResumeInProgressAutomatrix).
	automatrixWG sync.WaitGroup

	// automatrixNotifier is the out-of-app completion ping (task 5.3). It is
	// built from config in NewEngine (ntfy default + optional Apprise fan-out;
	// a noop when neither is configured) and fired BEST-EFFORT + non-blocking on
	// a GENUINE, gate-passed autonomous completion — AFTER the durable in-app
	// record is written, so a failed external send neither blocks the run nor
	// disturbs the canonical record (req 6.4, honest failure). Tests/wiring may
	// override it via SetAutomatrixNotifier with a real Notifier pointed at an
	// httptest server. Never nil for a built engine.
	automatrixNotifier notify.Notifier

	// Q2 warm-on-open: the FIRST inbound HTTP request fires a one-shot,
	// non-blocking warm of the embedder + HNSW semantic substrate (the only real
	// cold-start on the memory path — Activate/StorySoFar are cheap materialized
	// reads), so the first message's relevance recall doesn't pay it. warmOnce
	// makes it idempotent; warmed flips true once the background warm completes
	// (observable for tests + readiness). A nil pager, a missing embedder, or a
	// cleared WarmOnOpen flag makes Warm a no-op.
	warmOnce sync.Once
	warmed   atomic.Bool
	// warmStop + warmWG bound the warm goroutine's lifetime to the engine's:
	// Close closes warmStop (cancelling an in-flight warm read) and waits on
	// warmWG, so a caller that closes the pager right after Close can never
	// race the warm's Retrieve.
	warmStop     chan struct{}
	warmStopOnce sync.Once
	warmWG       sync.WaitGroup
}

// EngineOptions configures NewEngine. Main + Tools are required; the rest are
// optional (a nil pager/consolidator degrades gracefully).
type EngineOptions struct {
	Config                 config.Config
	Main                   *llm.Client
	Cheap                  *llm.Client
	SubMain                *llm.Client // main-capability model, thinking OFF, for background sub-agents (nil falls back to Main)
	Tools                  *tools.Manager
	Pager                  *memory.Pager
	Runtime                *agent.ResurrectionRuntime
	Consolidator           agent.Consolidator
	ConversationDir        string // durable conversation store dir ("" disables persistence)
	TaskDir                string // durable task-ledger dir ("" disables; reaper needs it to resume after restart)
	RunDir                 string
	Journal                *sessionjournal.Store
	TraceDir               string // durable workspace-trace dir ("" disables; the reopen-survives-reload store, F3)
	AutomatrixDir          string // durable Automatrix completion-inbox dir ("" disables; the in-app surprise-results store)
	AutomatrixSettingsDir  string // durable Automatrix opt-in settings dir ("" = in-memory only; wiring the production governor)
	BriefSettingsDir       string // durable morning-brief schedule sidecar dir ("" = in-memory only; wiring the production brief governor)
	BriefHistoryDir        string // durable morning-brief recommendation-history dir ("" disables; the no-repeat + feedback store)
	CapabilityDir          string // durable private Capability Hub dir ("" disables the optional subsystem)
	CapabilityLibraryDir   string // trusted read-only Matrix skill corpus used by library imports
	ImprovementDir         string // sealed verified-improvement observations and proposals (empty disables)
	MCPControlDir          string // encrypted, versioned MCP configuration and OAuth control plane
	ChannelGatewayDir      string // durable normalized channel identities, idempotency, and delivery receipts
	CloudChannelDir        string // encrypted Slack and Discord connection state ("" disables cloud adapters)
	TelegramSettingsDir    string // encrypted per-user Telegram bot/channel state ("" disables the integration)
	MachineMailSettingsDir string // encrypted per-user MachineMail API key ("" disables the integration)
	MediaDir               string // machine-volume media dir ("" disables image/video/audio I/O)
	BuildDir               string // sealed durable asynchronous coding Build jobs ("" disables Build)
	NovitaAPIKey           string
	VoiceASRURL            string
	VoiceASRKey            string
	VoiceControllerURL     string
	WorkspaceDir           string             // VM workspace root for the coding workbench ("" disables /workspace and /projects)
	Sandbox                sandbox.Client     // Railway sandbox client for previews (nil disables /workspace/preview)
	PreviewCfg             preview.Config     // preview controller config (user id, router door, TTL, image, cap)
	DojoRailway            dojo.RailwayClient // live-schema Railway sandbox client for dojo desktops (nil disables the dojo lane)
	DojoCfg                dojo.Config        // dojo session manager config (pinned image, idle/lifetime caps)
	Omni                   *llm.Client        // stateless MiMo v2.5 omni vision client for desktop_look grounding (nil disables desktop_look)
	DojoBridgeToken        string             // shared secret gating the loopback /dojo/computer-use proxy the desktop bridge posts to ("" disables the proxy)
	BackendURL             string
	BackendToken           string
	ProfilePath            string
	Vault                  *vault.Session // fail-closed encryption session for data-at-rest ("" nil = plaintext dev/CLI)
}

// NewEngine assembles the engine and wires core_execute delegation through the
// shared tool manager.
func NewEngine(o EngineOptions) *Engine {
	e := &Engine{
		cfg:                o.Config,
		main:               o.Main,
		cheap:              o.Cheap,
		subMain:            o.SubMain,
		tools:              o.Tools,
		pager:              o.Pager,
		runtime:            o.Runtime,
		consolidator:       o.Consolidator,
		conv:               conversation.Open(o.ConversationDir),
		tasks:              task.Open(o.TaskDir),
		runRecords:         runrecord.Open(o.RunDir),
		journal:            o.Journal,
		trace:              trace.Open(o.TraceDir),
		automatrix:         automatrixlog.Open(o.AutomatrixDir),
		briefHistory:       briefhistory.Open(o.BriefHistoryDir),
		capabilityLibrary:  strings.TrimRight(strings.TrimSpace(o.CapabilityLibraryDir), "/"),
		buildJobs:          codingruntime.Open(o.BuildDir),
		mediaDir:           strings.TrimRight(o.MediaDir, "/"),
		voiceASRURL:        strings.TrimSpace(o.VoiceASRURL),
		voiceASRKey:        strings.TrimSpace(o.VoiceASRKey),
		voiceControllerURL: strings.TrimRight(o.VoiceControllerURL, "/"),
		workspaceRoot:      resolveWorkspaceDir(strings.TrimRight(o.WorkspaceDir, "/")),
		vault:              o.Vault,
		backendURL:         strings.TrimRight(o.BackendURL, "/"),
		backendToken:       o.BackendToken,
		warmStop:           make(chan struct{}),
		broker:             newBroker(),
		runs:               map[string]*run{},
		gateClaims:         map[string]bool{},
		// Automatrix jitter/skip RNG (task 3.3): seeded from wall-clock so each
		// process reschedules on its own unpredictable cadence; tests reseed it
		// (and pin automatrixSkipProb) for determinism.
		automatrixRand:     rand.New(rand.NewSource(time.Now().UnixNano())),
		automatrixSkipProb: automatrixSkipProbability,
		// Out-of-app completion ping (task 5.3). Built from config: ntfy is the
		// default (topic from NtfyTopic, else derived from the agent DID), with
		// an optional Apprise fan-out; when neither is configured this is a noop
		// and only the durable in-app record is written. Best-effort + honest:
		// see announceAutomatrixCompletion.
		automatrixNotifier: notify.New(notify.Config{
			NtfyServer: o.Config.NtfyServer,
			NtfyTopic:  o.Config.NtfyTopic,
			AppriseURL: o.Config.AppriseURL,
			AgentDID:   o.Config.ActorDID,
		}),
	}
	if strings.TrimSpace(o.CapabilityDir) != "" {
		e.capabilities, e.capabilityErr = capabilityhub.Open(context.Background(), o.CapabilityDir)
	}
	if e.capabilities != nil && e.tools != nil {
		e.tools.SetCapabilityHub(e.snapshotCapabilities, e.createCapabilityCandidate)
	}
	// Data-at-rest: seal the durable JSONL conversation store (hot + archive)
	// under the per-user vault. The user DID bound into each record's associated
	// data matches the key booted in serve.go (ActorDID, else MATRIX_USER_ID),
	// so a record cannot be replayed across users, stores, conversations, or
	// positions. A nil session leaves the store writing legacy plaintext.
	vaultUser := o.Config.ActorDID
	if vaultUser == "" {
		vaultUser = os.Getenv("MATRIX_USER_ID")
	}
	e.vaultUser = vaultUser
	if o.Config.ImprovementEnabled && strings.TrimSpace(o.ImprovementDir) != "" {
		e.improvement, e.improvementErr = improvement.Open(o.ImprovementDir, o.Vault, vaultUser)
		e.improvementAlarms = &mcpImprovementAlarmController{tools: o.Tools}
	}
	if strings.TrimSpace(o.MCPControlDir) != "" {
		e.mcpControl, e.mcpControlErr = mcpcontrol.Open(context.Background(), o.MCPControlDir, o.Vault, vaultUser)
		if e.mcpControl != nil && e.tools != nil {
			e.tools.SetDynamicMCPHooks(e.mcpControl.Guard, e.mcpControl.Observe, e.mcpControl.Redact)
			_, e.mcpControlErr = e.applyMCPBound(context.Background(), true)
		}
	}
	if strings.TrimSpace(o.ChannelGatewayDir) != "" {
		e.channelGateway, e.channelGatewayErr = channelgateway.Open(context.Background(), o.ChannelGatewayDir, o.Vault, vaultUser)
		if e.channelGateway != nil {
			e.channelDispatch = channelgateway.NewDispatcher(e.channelGateway, channelgateway.RetryPolicy{
				MinimumInterval: 35 * time.Millisecond,
				BaseBackoff:     time.Second,
				MaximumBackoff:  2 * time.Minute,
				MaximumAttempts: 8,
			})
		}
	}
	if strings.TrimSpace(o.CloudChannelDir) != "" {
		cloudStore, cloudErr := cloudchannels.Open(o.CloudChannelDir, o.Vault, vaultUser)
		e.cloudChannels = newCloudChannelBridge(e, cloudStore, cloudErr)
	}
	if strings.TrimSpace(o.ProfilePath) != "" {
		e.profileStore, e.profileStoreErr = profileStore.Open(o.ProfilePath, o.Vault)
	}
	e.conv.SetVault(o.Vault, vaultUser)
	e.trace.SetVault(o.Vault, vaultUser)
	// Seal the whole-file/JSONL atomic stores under the same per-user vault
	// (ORACLE task 2.2): the durable task ledger and the Automatrix completion
	// inbox. The settings sidecar is sealed where it is opened below.
	e.tasks.SetVault(o.Vault, vaultUser)
	e.runRecords.SetVault(o.Vault, vaultUser)
	e.automatrix.SetVault(o.Vault, vaultUser)
	e.briefHistory.SetVault(o.Vault, vaultUser)
	e.buildJobs.SetVault(o.Vault, vaultUser)
	e.sessions = newSessionRegistry(e)
	// On-demand sandbox previews (NEO-WORKBENCH req 7): built only when a
	// sandbox client is configured; events flow through publishPreview onto
	// the conversation's run topic (live + durable trace).
	if o.Sandbox != nil {
		e.preview = preview.New(o.Sandbox, e.publishPreview, o.PreviewCfg)
	}
	// Disposable desktop sessions (DOJO req 1): built only when the live
	// Railway sandbox client is configured; lifecycle events flow through
	// publishDojo onto the conversation's run topic (live + durable trace).
	if o.DojoRailway != nil {
		e.dojo = dojo.New(o.DojoRailway, e.publishDojo, o.DojoCfg)
	}
	e.omni = o.Omni
	e.dojoBridgeToken = o.DojoBridgeToken
	// Fetch the onboarding profile from the daemon so it can be injected
	// into every agent's stable system prompt prefix (req 2.4/2.5).
	// Best-effort: a missing daemon or absent profile falls back to the
	// default "Neo" identity cleanly.
	e.fetchUserProfile()
	// Persist the durable workspace timeline (F3): the broker tap routes every
	// workspace event to the sidecar trace store so "Neo's Computer" survives a
	// reload / reopen-from-history, not just the 2-minute in-memory replay
	// window. The tap is a non-blocking enqueue, off the publish hot path.
	if e.trace.Enabled() {
		e.broker.setTap(e.recordTrace)
	}
	if e.tools != nil {
		e.tools.SetDelegate(e.coreExecute)
		// Task-scoped concurrent sub-agents (the Agent Swarm). Capped per call
		// by config so one spawn can't fan out unbounded.
		e.tools.SetSwarm(e.runSwarm, o.Config.MaxSubagents)
		// Construct ACTIVE tier: let the agent render typed surfaces onto the
		// user's screen via the construct_render tool (pure side-channel).
		e.tools.SetSurfaceEmitter(e.emitConstructSurface)
		// Construct Ask back-channel: an Ask surface BLOCKS the agent for a
		// typed human answer (invariant i5), delivered over Neo's gate-style
		// answer endpoint and returned to the tool call as its result.
		e.tools.SetAskResponder(e.respondAsk)
		// Live task-list (neo-smoothness req.3): the todo tool streams the
		// ordered checklist onto the run's event stream as a tool.todo event
		// (pure side-channel) and the trace persists it so it survives reopen.
		e.tools.SetTodo(e.emitTodo)
		// Personalization interview (ORACLE task 5.3): the confirmation-gated
		// save_personalization_profile tool (advertised only to interview
		// agents) persists the confirmed profile as the single versioned
		// Neocortex record through the real pager.
		if e.pager != nil {
			e.tools.SetPersonalizationSave(e.savePersonalizationProfile)
		}
		// Workbench preview (NEO-WORKBENCH req 7): let the agent launch the
		// active project's sandbox preview itself, so "show the user the app"
		// has a first-class action that is NOT a deploy. Only wired when the
		// sandbox controller is configured; otherwise the tool stays
		// unadvertised and the prompt omits it.
		if e.preview != nil && e.preview.Enabled() {
			e.tools.SetPreview(e.agentPreview)
		}
		// Disposable-desktop grounding (DOJO req 3): the deterministic AT-SPI
		// rung (desktop_a11y) needs only the session manager; the vision rung
		// (desktop_look) additionally needs the omni client. Wire whichever is
		// available so the driving ladder degrades cleanly.
		if e.dojo != nil && e.dojo.Enabled() {
			var look tools.DesktopLookFunc
			if e.omni != nil {
				look = e.desktopLook
			}
			e.tools.SetDesktop(look, e.desktopA11y)
		}
		// Browser filmstrip (BROWSER-FILMSTRIP req.2): persist tool-produced
		// images (browser screenshots) to the served media plane so a still can
		// reach the workspace out-of-band instead of being discarded. Only wired
		// when a media dir exists; otherwise images summarize to a placeholder as
		// before (no filmstrip, no crash).
		if e.mediaDir != "" {
			e.tools.SetMediaPersist(e.persistToolImage)
		}
	}
	// Bound screenshot/media growth on the volume (BROWSER-FILMSTRIP req.7.2):
	// a one-shot GC sweep at boot + a periodic ticker delete media files older
	// than the retention window, parallel to the trace retain cap. Disabled when
	// retention is 0 or no media dir is configured.
	e.startMediaGC()
	// Production AUTOMATRIX governor (task 6.1): when a settings dir is provided
	// (serve.go derives it), wire the durable opt-in store + the Chronos
	// alarm_set/alarm_cancel lifecycle (issued through the MCP tool surface) as
	// the engine's authoritative AutomatrixGovernor. The wake handler re-reads
	// the opt-in here on every wake, and the control endpoints toggle it. Tests
	// that leave AutomatrixSettingsDir empty keep a nil governor (fail closed)
	// and may install their own seam via SetAutomatrixGovernor.
	if o.AutomatrixSettingsDir != "" {
		st := automatrixsettings.Open(o.AutomatrixSettingsDir)
		// Seal the settings sidecar at rest and (re)load state under the vault
		// (ORACLE task 2.2) before the governor reads the opt-in.
		st.SetVault(o.Vault, vaultUser)
		e.automatrixGov = newAutomatrixGovernor(st, newMCPAlarmController(e.tools), o.Config.AutomatrixBaseIntervalMinutes)
	}
	// Production morning-brief governor (ORACLE task 5.2): when a settings dir
	// is provided (serve.go derives it), wire the durable schedule sidecar +
	// the MORNING_BRIEF Chronos alarm lifecycle (issued through the MCP tool
	// surface). The wake handler re-reads the schedule here on every fire, and
	// the /brief/settings routes toggle/edit it. Tests that leave
	// BriefSettingsDir empty keep a nil governor (fail closed) and may install
	// their own via SetBriefGovernor.
	if o.BriefSettingsDir != "" {
		bs := briefsettings.Open(o.BriefSettingsDir)
		bs.SetVault(o.Vault, vaultUser)
		e.briefGov = newBriefGovernor(bs, newBriefMCPAlarmController(e.tools))
	}
	if o.TelegramSettingsDir != "" {
		ts := telegramsettings.Open(o.TelegramSettingsDir)
		ts.SetVault(o.Vault, vaultUser)
		e.telegram = newTelegramBridge(e, ts)
	}
	if o.MachineMailSettingsDir != "" {
		ms := machinemailsettings.Open(o.MachineMailSettingsDir)
		ms.SetVault(o.Vault, vaultUser)
		e.machineMail = newMachineMailBridge(ms, e.tools, e.channelGateway, vaultUser)
		_ = e.machineMail.Restore(context.Background())
	}
	if e.mediaDir != "" {
		e.mediaStudio, e.mediaStudioErr = mediastudio.Open(
			context.Background(),
			mediastudio.Config{
				MediaDir: e.mediaDir, APIKey: o.NovitaAPIKey, Tools: o.Tools,
				Vault: o.Vault, VaultUser: vaultUser,
			},
		)
	}
	return e
}

func (e *Engine) StartTelegram(ctx context.Context) {
	if e != nil && e.telegram != nil {
		e.telegram.Start(ctx)
	}
}

func (e *Engine) StartCloudChannels(ctx context.Context) {
	if e != nil && e.cloudChannels != nil {
		e.cloudChannels.Start(ctx)
	}
}

// SetBriefGovernor wires the morning-brief opt-in + alarm bookkeeping seam
// (task 5.2). Safe to leave unset: the control routes + wake handler fail closed
// without it.
func (e *Engine) SetBriefGovernor(g *briefGovernor) { e.briefGov = g }

// Vault returns the daemon's fail-closed data-at-rest encryption session, or nil
// in an explicitly-disabled plaintext dev/CLI run. Store wiring consults it to
// seal records/files before write and to open sealed data on read.
func (e *Engine) Vault() *vault.Session { return e.vault }

// ResumeOrphanedTasks re-dispatches every task left "running" in the durable
// ledger — tasks orphaned when the daemon was restarted or Fly-suspended
// mid-work. Each resumes on its own conversation with the catch-up prime, so
// the Task Durability Rule holds across a process death: a dropped task is
// picked back up and driven to completion rather than silently lost. Called
// once at boot (cmd/neo serve). Best-effort + idempotent — a task that actually
// finished is no longer "running" and is skipped, and the ledger's CAS-on-run-id
// keeps a stale record from clobbering a live one. Returns the count resumed.
func (e *Engine) ResumeOrphanedTasks() int {
	if e.tasks == nil || !e.tasks.Enabled() {
		return 0
	}
	orphans := e.tasks.Running()
	resumed := 0
	for _, t := range orphans {
		if rec, ok, _ := e.getRunRecord(t.RunID); ok && rec.Terminal() {
			status := task.StatusCeiling
			switch rec.Status {
			case runrecord.StatusCompleted:
				status = task.StatusDone
			case runrecord.StatusInterrupted:
				status = task.StatusInterrupted
			}
			e.tasks.Finish(t.ConvID, t.RunID, status)
			continue
		}
		e.sessions.get(t.ConvID).startResume(t.RunID, t.Objective)
		resumed++
	}
	return resumed
}

// Close flushes and stops the engine's durable sidecar stores (today: the
// workspace trace writer). Called on graceful shutdown so any queued workspace
// events are persisted before exit. Idempotent + safe on a disabled store.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	if e.telegram != nil {
		e.telegram.Stop()
	}
	if e.cloudChannels != nil {
		e.cloudChannels.Stop()
	}
	if e.buildWorker != nil {
		e.buildWorker.Stop()
	}
	if e.mediaStudio != nil {
		e.mediaStudio.Close()
	}
	// Stop the warm-on-open goroutine and wait it out BEFORE returning, so a
	// caller that closes the pager next never races an in-flight warm read.
	e.warmStopOnce.Do(func() { close(e.warmStop) })
	e.warmWG.Wait()
	// Drain in-flight autonomous Automatrix dispatch goroutines (bounded):
	// their terminal settle writes through the pager, which the caller closes
	// right after this. A run that outlives the bound stays durably
	// in_progress and resumes at next boot.
	waitBounded(&e.automatrixWG, 30*time.Second)
	// Drain in-flight morning-brief dispatch goroutines the same way (task 5.4):
	// a brief that outlives the bound stays durably running in the task ledger
	// and resumes on the restricted brief surface at next boot.
	waitBounded(&e.briefWG, 30*time.Second)
	// The observer can write proposals and route an approved proposal through
	// Neocortex or Capability Hub, so drain it before those owners close.
	waitBounded(&e.improvementWG, 30*time.Second)
	if e.runtime != nil {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		_ = e.runtime.Close(ctx)
		cancel()
	}
	e.trace.Close()
	if e.journal != nil {
		_ = e.journal.Close()
	}
	if e.capabilities != nil {
		_ = e.capabilities.Close()
	}
	if e.mcpControl != nil {
		_ = e.mcpControl.Close()
	}
	if e.channelGateway != nil {
		_ = e.channelGateway.Close()
	}
}

// applyMCPBound publishes a staged MCP snapshot only while the caller holds
// mcpBoundary and the global run registry is empty. A replacement is built and
// qualified before tools.Manager swaps it, so failure leaves the previous live
// pool intact and rolls the desired configuration back durably.
func (e *Engine) applyMCPBound(ctx context.Context, force bool) (bool, error) {
	if e == nil || e.mcpControl == nil || e.tools == nil {
		return false, e.mcpControlErr
	}
	if force {
		e.mcpDirty = true
	}
	e.mu.Lock()
	busy := len(e.runs) > 0
	e.mu.Unlock()
	if busy {
		return false, nil
	}
	pending := e.mcpControl.HasPending(ctx)
	refresh := e.mcpControl.OAuthRefreshDue(ctx)
	if !force && e.mcpLoaded && !e.mcpDirty && !pending && !refresh {
		return false, nil
	}
	entries, err := e.mcpControl.RuntimeEntries(ctx)
	if err == nil {
		err = e.tools.ReloadDynamicMCP(ctx, entries)
	}
	if err != nil {
		safeErr := mcpcontrol.SafeRuntimeError(err)
		if pending {
			_ = e.mcpControl.RollbackPending(ctx, safeErr)
		}
		e.mcpControlErr = safeErr
		return false, safeErr
	}
	if pending {
		if err := e.mcpControl.MarkApplied(ctx); err != nil {
			e.mcpControlErr = err
			return false, err
		}
	}
	e.mcpLoaded = true
	e.mcpDirty = false
	e.mcpControlErr = nil
	return true, nil
}

func (e *Engine) applyMCP(ctx context.Context) (bool, error) {
	if e == nil {
		return false, errors.New("Neo engine is unavailable")
	}
	e.mcpBoundary.Lock()
	defer e.mcpBoundary.Unlock()
	return e.applyMCPBound(ctx, false)
}

func (e *Engine) applyMCPForced(ctx context.Context) (bool, error) {
	if e == nil {
		return false, errors.New("Neo engine is unavailable")
	}
	e.mcpBoundary.Lock()
	defer e.mcpBoundary.Unlock()
	return e.applyMCPBound(ctx, true)
}

func (e *Engine) beginTurnBoundary(ctx context.Context) {
	e.mcpBoundary.Lock()
	_, _ = e.applyMCPBound(ctx, false)
}

func (e *Engine) endTurnBoundary() { e.mcpBoundary.Unlock() }

// waitBounded waits for wg up to d, returning early when it drains. Used only
// on the shutdown path, where a hung background goroutine must not wedge Close.
func waitBounded(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// warmScope is the sentinel conversation id used only for the warm-on-open
// retrieval. It never becomes a real session (the actor-scoped semantic lanes
// the warm exercises are conversation-independent).
const warmScope = "neo-warm-on-open"

// warmTimeout bounds the background warm so a slow/hung embedder or index load
// can't leak a goroutine for the process lifetime.
const warmTimeout = 30 * time.Second

// Warm is the Q2 warm-on-open hook: the FIRST inbound HTTP request kicks a
// one-shot, NON-BLOCKING warm of the embedder + HNSW semantic substrate so the
// first message's relevance recall (agent.cmRelevancePush) isn't paying the
// cold-start. It returns immediately; the actual warm runs on a background
// goroutine bounded by warmTimeout. Idempotent via warmOnce, and a no-op when
// WarmOnOpen is off, the pager is nil, or no embedder is running (the salience
// lane needs no warming).
func (e *Engine) Warm() {
	if e == nil || !e.cfg.WarmOnOpen {
		return
	}
	e.warmOnce.Do(func() {
		if e.pager == nil || !e.pager.HasEmbedder() {
			return
		}
		e.warmWG.Add(1)
		go func() {
			defer e.warmWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
			defer cancel()
			// Abort the warm the moment the engine closes: Close waits on
			// warmWG before returning, so the pager is never closed underneath
			// a warm read (the shutdown race the -race suite caught).
			go func() {
				select {
				case <-e.warmStop:
					cancel()
				case <-ctx.Done():
				}
			}()
			select {
			case <-e.warmStop:
				return
			default:
			}
			e.warmBody(ctx)
		}()
	})
}

// warmBody performs the actual substrate warm synchronously: one relevance
// retrieval that loads the embedding model + opens the HNSW index. The result
// is discarded — the point is the side effect of spinning up the cold path.
// Split out from Warm so tests can exercise it deterministically without the
// once/goroutine wrapper. Best-effort: any error is swallowed (a failed warm
// just means the first real recall pays what it would have anyway).
func (e *Engine) warmBody(ctx context.Context) {
	if e.pager == nil {
		return
	}
	_, _ = e.pager.Retrieve(ctx, warmScope)
	e.warmed.Store(true)
}

// Warmed reports whether the background warm has completed. Observability for
// tests + readiness checks; false until the warm goroutine finishes (or forever
// when warming is disabled / unavailable).
func (e *Engine) Warmed() bool {
	return e != nil && e.warmed.Load()
}

// neoSurfaceSink adapts the engine's per-run event broker to the construct
// transport EventSink interface, so the Construct emit logic stays
// single-source (transport.EmitSurface validates the surface + builds the
// event fields).
type neoSurfaceSink struct {
	e     *Engine
	runID string
}

func (s neoSurfaceSink) Event(typ, phase string, fields map[string]interface{}) {
	s.e.broker.publish(s.runID, typ, phase, fields)
}

// emitConstructSurface is the ACTIVE-tier Construct emitter wired into the tool
// manager: it streams an agent-authored surface onto the active run's event
// stream as a construct.surface event. Pure side-channel — it only publishes a
// transcript event, exactly like surfaceTool / notifyFor; it never signs,
// writes Neocortex, or touches the plan/walk.
func (e *Engine) emitConstructSurface(ctx context.Context, s *schema.Surface) error {
	r := runFromContext(ctx)
	if r == nil {
		return fmt.Errorf("construct: no active run on context")
	}
	return transport.EmitSurface(neoSurfaceSink{e: e, runID: r.id}, r.id, r.convID, s)
}

// emitTodo is the live task-list emitter wired into the tool manager
// (neo-smoothness req.3): it streams the agent's ordered checklist onto the
// active run's event stream as a tool.todo event. Pure side-channel — it only
// publishes a transcript event (like emitConstructSurface / surfaceTool); it
// never signs, writes Neocortex, or touches the plan/walk. The trace tap persists
// tool.todo (traceWorkspaceTypes) so the checklist survives reopen + respawn.
func (e *Engine) emitTodo(ctx context.Context, items []tools.TodoItem) error {
	r := runFromContext(ctx)
	if r == nil {
		return fmt.Errorf("todo: no active run on context")
	}
	list := make([]map[string]interface{}, len(items))
	for i, it := range items {
		list[i] = map[string]interface{}{"text": it.Text, "status": string(it.Status)}
	}
	e.broker.publish(r.id, "tool.todo", "neo", map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"items":           list,
	})
	return nil
}

// askWaitTimeout bounds how long a parked Ask waits for a human answer before
// it expires and the agent is told to proceed without it (still inside the
// run's own 20-minute ceiling). Long enough for a deliberate human decision
// (read a tx, pick an option), short enough not to wedge a run forever.
const askWaitTimeout = 10 * time.Minute

// respondAsk is the ACTIVE-tier Construct back-channel (invariant i5): it shows
// an Ask surface, PARKS the agent's construct_render tool call on a per-ask
// waiter keyed by the surface id, and returns the human's typed response once
// it is posted to /intents/{id}/asks/{ask_id}/answer. It mirrors approverFor
// (the in-walk gate) exactly — register the waiter FIRST so a fast answer can't
// race the park, then emit; on answer, patch the Ask surface to its settled
// state so the rendered control resolves. Pure side-channel: it publishes
// transcript events and returns the answer as a tool result (an agent INPUT);
// it never signs, writes Neocortex, or touches the plan/walk.
func (e *Engine) respondAsk(ctx context.Context, s *schema.Surface) (*primitives.AskResponse, error) {
	r := runFromContext(ctx)
	if r == nil {
		return nil, fmt.Errorf("construct: no active run on context")
	}
	askID := s.ID
	ch := r.sess.registerAsk(askID, s.Ask)
	sink := neoSurfaceSink{e: e, runID: r.id}

	// Show the question (the renderer paints the control), then announce the
	// run is parked awaiting a human answer.
	if err := transport.EmitSurface(sink, r.id, r.convID, s); err != nil {
		r.sess.clearAsk(askID)
		return nil, err
	}
	e.broker.publish(r.id, "ask.awaiting", transport.Phase, map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"ask_id":          askID,
		"ask_kind":        string(s.Ask.AskKind),
	})

	select {
	case <-ctx.Done():
		r.sess.clearAsk(askID)
		return nil, ctx.Err()
	case <-time.After(askWaitTimeout):
		r.sess.clearAsk(askID)
		e.broker.publish(r.id, "ask.expired", transport.Phase, map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"ask_id":          askID,
		})
		return nil, fmt.Errorf("timed out after %s waiting for an answer", askWaitTimeout)
	case resp := <-ch:
		// Settle the rendered control: patch the Ask with its response so the
		// client flips from the live control to the answered state (mirrors
		// gate.decided). A patch failure is non-fatal — the answer still flows.
		if answered, err := backchannel.Answered(s, resp); err == nil {
			_ = transport.PatchSurface(sink, r.id, r.convID, askID, answered)
		}
		e.broker.publish(r.id, "ask.answered", transport.Phase, map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"ask_id":          askID,
		})
		return resp, nil
	}
}

// coreExecute is the in-conversation bridge to the MCL pipeline. It reads the
// active run from ctx so it can surface the delegated run's approval gates and
// status onto that conversation's event stream, then delegates over HTTP to
// the daemon (the only thing that can move funds), servicing gates inline.
func (e *Engine) coreExecute(ctx context.Context, intent string) (string, error) {
	r := runFromContext(ctx)
	dele := delegate.New(delegate.Options{
		BaseURL:   e.backendURL,
		Token:     e.backendToken,
		CallerDID: e.cfg.ActorDID,
		Approver:  e.approverFor(r),
		Notify:    e.notifyFor(r),
		GateClaim: e.claimGate,
	})
	return dele.Run(ctx, intent)
}

// claimGate is the shared cross-attach gate guard handed to every per-call
// delegate client. The FIRST caller to claim (intentID, nodeID) wins and may
// answer the gate; later callers (a concurrent attach to the same in-flight
// intent) lose the claim and skip answering, so a single approval gate is
// serviced exactly once even when two core_execute delegations join the same
// run. Safe for concurrent use.
func (e *Engine) claimGate(intentID, nodeID string) bool {
	key := intentID + "\x00" + nodeID
	e.gateClaimsMu.Lock()
	defer e.gateClaimsMu.Unlock()
	if e.gateClaims[key] {
		return false
	}
	if e.gateClaims == nil {
		e.gateClaims = map[string]bool{}
	}
	e.gateClaims[key] = true
	return true
}

// approverFor surfaces a delegated MCL gate as a gate.invoked event on the
// conversation stream and blocks until the user answers via Neo's gate-answer
// endpoint (or the context is cancelled, which denies).
func (e *Engine) approverFor(r *run) delegate.Approver {
	return func(ctx context.Context, nodeID, question string, options []string) (bool, string) {
		if r == nil {
			return false, "" // no conversation context: deny rather than auto-spend
		}
		ans := r.sess.registerGate(nodeID)
		e.broker.publish(r.id, "gate.invoked", "neo", map[string]interface{}{
			"intent_id": r.id,
			"node_id":   nodeID,
			"question":  question,
			"options":   options,
		})
		select {
		case <-ctx.Done():
			r.sess.clearGate(nodeID)
			return false, ""
		case a := <-ans:
			decision := "denied"
			if a.approved {
				decision = "approved"
			}
			if e.journal != nil {
				if _, err := e.journal.Append(ctx, sessionjournal.Event{
					ConversationID: r.convID, TurnID: r.id,
					Kind: sessionjournal.KindApproval, DisplayContent: question,
					Approval: &sessionjournal.Approval{
						ApprovalID: nodeID, Action: question,
						Decision: decision, Authority: "user", Reason: a.answer,
					},
				}); err != nil {
					e.logLifecycle("journal.approval", r.id, r.convID, decision, time.Since(r.started), err)
					return false, ""
				}
			}
			e.broker.publish(r.id, "gate.decided", "neo", map[string]interface{}{
				"intent_id": r.id,
				"node_id":   nodeID,
				"approved":  a.approved,
			})
			return a.approved, a.answer
		}
	}
}

// publishAudit streams a Cassandra audit event onto the run's event stream as
// a cassandra.* event (cassandra.frozen.kvx [audit].events). Pure observability
// side-channel — it only publishes a transcript event, exactly like surfaceTool
// / notifyFor; the adjudicator signs nothing and writes no Neocortex (i_cass_4,
// i_cass_6).
func (e *Engine) publishAudit(r *run, ev agent.AuditEvent) {
	if r == nil {
		return
	}
	f := map[string]interface{}{"intent_id": r.id, "conversation_id": r.convID}
	for k, v := range ev.Fields {
		f[k] = v
	}
	e.broker.publish(r.id, ev.Type, "cassandra", f)
}

// publishMemory streams the turn's continuous-memory activation summary onto
// the run's event stream as a memory.activation event (continuous-memory task
// 7.1). Pure observability side-channel — like publishAudit / surfaceTool it
// only publishes a transcript event; Neocortex composed the bundle read-only and
// nothing here signs, writes Neocortex, or touches the plan/walk. It surfaces the
// RESULT (the durable story-so-far + a coarse memory timeline), never the
// protocol (no journal / MMR / rollup / snapshot jargon). Empty payloads are
// dropped so a turn with nothing to remember emits nothing.
func (e *Engine) publishMemory(r *run, ev agent.MemoryEvent) {
	if r == nil {
		return
	}
	if strings.TrimSpace(ev.StorySoFar) == "" && len(ev.Timeline) == 0 && len(ev.Excerpts) == 0 && strings.TrimSpace(ev.SelectionReason) == "" {
		return
	}
	e.broker.publish(r.id, "memory.activation", "neo", map[string]interface{}{
		"intent_id":         r.id,
		"conversation_id":   r.convID,
		"story_so_far":      ev.StorySoFar,
		"timeline":          ev.Timeline,
		"episodic_excerpts": ev.Excerpts,
		"trigger_class":     ev.TriggerClass,
		"selection_reason":  ev.SelectionReason,
	})
}

func (e *Engine) notifyFor(r *run) func(string) {
	return func(msg string) {
		if r == nil {
			return
		}
		e.broker.publish(r.id, "chat.assistant", "neo", map[string]interface{}{
			"role":            "assistant",
			"text":            msg,
			"conversation_id": r.convID,
			"intent_id":       r.id,
		})
	}
}

// registerRun / lookupRun / unregisterRun index active runs so the gate-answer
// route can find the waiting approver by run id.
func (e *Engine) registerRun(r *run) {
	e.mu.Lock()
	e.runs[r.id] = r
	e.mu.Unlock()
}

func (e *Engine) lookupRun(id string) *run {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[id]
}

func (e *Engine) getRunRecord(id string) (runrecord.Record, bool, error) {
	if e == nil || e.runRecords == nil {
		return runrecord.Record{}, false, nil
	}
	return e.runRecords.Get(id)
}

// activeRunForConv reports the in-flight run id for one conversation, or "" if
// nothing is live. Unlike broker.has(id) (which stays true for 2 minutes after
// settlement so late reconnects can replay), this consults the authoritative
// runs registry: a run is here iff registerRun has fired and unregisterRun has
// NOT — i.e. iff drive() is still executing. handleConversations uses it to
// stamp `live_run` on the GET /conversations/{id} response so a fresh tab /
// post-relogin client can subscribe(replay:true) without waiting for the poll.
func (e *Engine) activeRunForConv(convID string) string {
	if e == nil || convID == "" {
		return ""
	}
	if e.runRecords != nil && e.runRecords.Enabled() {
		if rec, ok := e.runRecords.ActiveForConversation(convID); ok {
			return rec.IntentID
		}
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, r := range e.runs {
		if r != nil && r.convID == convID {
			return id
		}
	}
	return ""
}

func (e *Engine) unregisterRun(id string) {
	e.mu.Lock()
	delete(e.runs, id)
	e.mu.Unlock()
	// Keep the event topic briefly for late reconnects, then reclaim it.
	go func() {
		time.Sleep(2 * time.Minute)
		e.broker.drop(id)
		e.logLifecycle("run.eviction", id, "", "evicted", 0, nil)
	}()
}

// haltAll interrupts EVERY live run this daemon is driving — the global kill
// switch behind the client's "Stop all" control. Each daemon is single-tenant,
// so this halts exactly the calling user's own in-flight Neos (their runaway
// respawns / parallel conversations) and nothing else. It mirrors the per-run
// POST /intents/{id}/stop path: interrupt() marks each run stopped + cancels its
// context, so drive() closes it quietly as StatusInterrupted and the reaper
// never resumes it. Returns the number of runs signalled. The run snapshot is
// taken under e.mu and released BEFORE interrupt() (which takes the session's
// actMu) to avoid any lock-ordering coupling with the dispatch path.
func (e *Engine) haltAll() int {
	e.mu.Lock()
	live := make([]*run, 0, len(e.runs))
	for _, r := range e.runs {
		if r != nil {
			live = append(live, r)
		}
	}
	e.mu.Unlock()
	for _, r := range live {
		r.sess.interrupt(r)
	}
	return len(live)
}

// traceWorkspaceTypes is the set of event types that make up the durable "Neo's
// Computer" workspace timeline (F3). Only these are persisted to the sidecar
// trace store; transient channels (chat.delta, chat.thinking), gate/ask control
// events, and terminals are NOT — they are either re-derived on reopen or
// irrelevant to the rebuilt workspace. chat.assistant is handled specially in
// recordTrace (only the non-final "thinking out loud" narration is persisted;
// the final answer + bubbles come from the durable conversation store).
var traceWorkspaceTypes = map[string]bool{
	"tool.step":               true,
	"tool.search":             true,
	"tool.media":              true,
	"tool.artifact":           true,
	"tool.finance":            true,
	"tool.todo":               true,
	"memory.activation":       true,
	"construct.surface":       true,
	"construct.surface.patch": true,
	"swarm.started":           true,
	"subagent.created":        true,
	"subagent.step":           true,
	"subagent.note":           true,
	"subagent.status":         true,
	"swarm.completed":         true,
	// Sandbox preview lifecycle (NEO-WORKBENCH req 7.2): durable so reopen
	// rebuilds the LAST honest preview state. The live-typing tool.delta
	// channel is deliberately NOT here — deltas are ephemeral by design.
	"preview.pending": true,
	"preview.ready":   true,
	"preview.failed":  true,
	"preview.expired": true,
	// Dojo desktop lifecycle (DOJO req 1.3): durable so reopen rebuilds the
	// LAST honest desktop state.
	"dojo.provisioning":   true,
	"dojo.ready":          true,
	"dojo.active":         true,
	"dojo.takeover":       true,
	"dojo.handback":       true,
	"dojo.shipping":       true,
	"dojo.destroyed":      true,
	"dojo.failed":         true,
	"voice.session.start": true,
	"voice.session.state": true,
	"voice.session.stop":  true,
}

// recordTrace is the broker tap (F3): it persists the workspace-relevant slice
// of the live event stream to the durable sidecar trace store so "Neo's
// Computer" can be rebuilt when a thread is reopened — after the run settles,
// scrolls past the 512-event broker buffer, or the topic is reclaimed. It runs
// on every publish, so it stays cheap: a map lookup, then a NON-BLOCKING
// enqueue (trace.Record drops rather than blocks). It never signs, never writes
// Neocortex, never touches plan/walk (pure sidecar — m9/i_trace).
func (e *Engine) recordTrace(id string, ev Event) {
	if e.trace == nil || !e.trace.Enabled() || id == "" {
		return
	}
	keep := traceWorkspaceTypes[ev.Type]
	if !keep && ev.Type == "chat.assistant" {
		// Persist only the non-final narration ("thinking out loud") turns as
		// workspace steps; the final answer is the conversation store's job.
		keep = ev.Fields == nil || ev.Fields["final"] != true
	}
	if !keep {
		return
	}
	e.trace.Record(id, trace.Event{
		Seq:    ev.Seq,
		Ts:     ev.Ts,
		Phase:  ev.Phase,
		Type:   ev.Type,
		Fields: ev.Fields,
	})
}

// automatrixCompleteEvent is the broker event type announcing that Neo finished
// an unprompted opportunity (req 6.1). It carries the RESULT, not the protocol
// (no Chronos/alarm/marker/Neocortex jargon — the consumer rule, req 6.5): the
// opportunity summary, a result summary, the conversation it belongs to, the
// created_at, the unread flag, and the durable record id. The client renders it
// as an Automatrix inbox item / unread badge.
const automatrixCompleteEvent = "automatrix.complete"

// emitAutomatrixComplete writes the durable in-app completion record and
// publishes the automatrix.complete broker event so a live client surfaces the
// surprise result immediately. The durable record is the canonical, replayable
// surface: it is written FIRST so the result is never lost even if no client is
// connected (and regardless of any out-of-app ntfy/Apprise ping). This is the
// clean writer seam the completion path (5.3) calls on a genuinely-gated pass;
// it never signs, writes Neocortex, or touches the plan/walk. No secrets are
// recorded — only the result-not-protocol fields.
func (e *Engine) emitAutomatrixComplete(convID, opportunitySummary, resultSummary string) automatrixlog.Record {
	rec, err := e.automatrix.Append(automatrixlog.Record{
		OpportunitySummary: opportunitySummary,
		ResultSummary:      resultSummary,
		ConversationID:     convID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/automatrix: record completion for %s: %v\n", convID, err)
	}
	e.broker.publish(convID, automatrixCompleteEvent, "neo", map[string]interface{}{
		"id":                  rec.ID,
		"conversation_id":     rec.ConversationID,
		"opportunity_summary": rec.OpportunitySummary,
		"result_summary":      rec.ResultSummary,
		"created_at":          rec.CreatedAt,
		"read":                rec.Read,
	})
	return rec
}

// AutomatrixInbox exposes the durable Automatrix completion store so the daemon
// routes can render the in-app inbox / unread badge and mark items read (req
// 7.4). Returns the (possibly disabled) store; never nil for a built engine.
func (e *Engine) AutomatrixInbox() *automatrixlog.Store { return e.automatrix }

// latestRunForConv resolves the run id whose workspace trace a reopened thread
// should rebuild: the in-flight run if one is live, else the most recent run on
// the conversation (the last persisted turn carrying an intent_id). Returns ""
// when the conversation has no run-bearing turn.
func (e *Engine) latestRunForConv(convID string) string {
	if live := e.activeRunForConv(convID); live != "" {
		return live
	}
	if e.conv == nil {
		return ""
	}
	rec := e.conv.Get(convID)
	if rec == nil {
		return ""
	}
	for i := len(rec.Turns) - 1; i >= 0; i-- {
		if rec.Turns[i].IntentID != "" {
			return rec.Turns[i].IntentID
		}
	}
	return ""
}

// surfaceTool turns a tool call into the live "Neo Workspace" — a single
// `tool.step` event the client renders as an ANIMATED VIEWPORT of the action
// itself: a terminal when Neo runs a command, a browser window when it
// browses, an editor when it reads/writes files. The same step id is updated
// from running→done across the start/end pair, so the viewport fills in place
// rather than appending a new card. Web-search results and generated media ALSO
// keep their dedicated rich events (cards / media grid) on completion.
func (e *Engine) surfaceTool(r *run, ev agent.ToolEvent) {
	if r == nil {
		return
	}
	// Live file-typing fragment (NEO-WORKBENCH): a mid-generation write_file
	// content delta. Published as its own ephemeral event type — deliberately
	// NOT in traceWorkspaceTypes, so the durable trace replays only final
	// states (the deltas are a live-view garnish; disk + the final tool.step
	// are the authoritative complete state).
	if ev.Phase == agent.ToolStream {
		e.broker.publish(r.id, "tool.delta", "neo", map[string]interface{}{
			"id":              ev.ID,
			"tool":            ev.Name,
			"path":            ev.StreamPath,
			"delta":           ev.StreamDelta,
			"offset":          ev.StreamOffset,
			"intent_id":       r.id,
			"conversation_id": r.convID,
		})
		return
	}
	// The animated workspace step (start paints "running"; end fills it in).
	step := describeStep(ev)
	step["intent_id"] = r.id
	step["conversation_id"] = r.convID
	e.broker.publish(r.id, "tool.step", "neo", step)

	if ev.Phase != agent.ToolEnd {
		return
	}
	// Finance tools return a compact, structured market payload. Publish it as
	// its own durable screen event so Neo's Computer can render the same quote,
	// chart and list components as /finance instead of a generic action chip.
	if payload, ok := financeResult(ev); ok {
		e.broker.publish(r.id, "tool.finance", "neo", map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"id":              ev.ID,
			"tool":            stripAlias(ev.Name),
			"ok":              !ev.IsErr,
			"payload":         payload,
		})
	}
	// On completion, web search/news also emit rich source+snippet cards.
	if isSearchTool(ev.Name) {
		if s, ok := parseSearch(ev.Result); ok {
			e.broker.publish(r.id, "tool.search", "neo", map[string]interface{}{
				"intent_id":       r.id,
				"conversation_id": r.convID,
				"tool":            ev.Name,
				"provider":        s.Provider,
				"query":           s.Query,
				"answer":          s.Answer,
				"results":         s.cards(),
			})
		}
	}
	// Generated/edited media → a rich media card the client renders inline
	// (image thumbnail / video player). Transcripts are plain text and flow
	// through the model's answer, so they don't get a media card.
	if m, ok := parseMedia(ev.Result); ok && (m.Kind == "image" || m.Kind == "video") {
		e.broker.publish(r.id, "tool.media", "neo", map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"tool":            ev.Name,
			"kind":            m.Kind,
			"url":             m.URL,
			"mime":            m.MIME,
			"prompt":          m.Prompt,
		})
	}
	// F6: a deliverable artifact (a downloadable file/archive, or a deployed
	// site URL) → a first-class `tool.artifact` event the client renders as an
	// in-app download card / open-and-preview card, instead of a raw bucket URL
	// pasted into the answer text. ADDITIVE + ignore-safe: older clients that
	// don't know the event simply skip it. A tool opts in by returning the
	// documented artifact shape (see parseArtifact); the blob itself stays in
	// object storage as a content-addressed ref (boundaries: never bytes in the
	// conversation store or Neocortex).
	if a, ok := parseArtifact(ev.Result); ok {
		if e.journal != nil {
			if _, err := e.journal.Append(context.Background(), sessionjournal.Event{
				ConversationID: r.convID, TurnID: r.id,
				Kind: sessionjournal.KindArtifact, DisplayContent: ev.Result,
				Artifact: &sessionjournal.Artifact{
					ArtifactID: ev.ID + ":" + a.URL, Kind: a.Kind,
					URI: a.URL,
				},
			}); err != nil {
				e.logLifecycle("journal.artifact", r.id, r.convID, a.Kind, time.Since(r.started), err)
			}
		}
		e.broker.publish(r.id, "tool.artifact", "neo", map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"tool":            ev.Name,
			"kind":            a.Kind,
			"url":             a.URL,
			"mime":            a.MIME,
			"name":            a.Name,
			"size":            a.Size,
			"preview":         a.Preview,
		})
	}
}

func isSearchTool(name string) bool {
	return strings.HasSuffix(name, "web_search") || strings.HasSuffix(name, "web_news") || hasAlias(name, "exa")
}

// searchPayload mirrors the web-search MCP tool's JSON result.
type searchPayload struct {
	Tool     string `json:"tool"`
	Provider string `json:"provider"`
	Query    string `json:"query"`
	Answer   string `json:"answer"`
	Results  []struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Snippet   string `json:"snippet"`
		Published string `json:"published"`
	} `json:"results"`
	OK *bool `json:"ok"` // present (false) only on a structured error
}

func parseSearch(raw string) (searchPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return searchPayload{}, false
	}
	var s searchPayload
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return searchPayload{}, false
	}
	if s.OK != nil && !*s.OK {
		return searchPayload{}, false // error result: not a card set
	}
	if len(s.Results) == 0 {
		return searchPayload{}, false
	}
	return s, true
}

// mediaPayload mirrors the media MCP tool's JSON result for image/video output.
type mediaPayload struct {
	OK     bool   `json:"ok"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	MIME   string `json:"mime"`
	Prompt string `json:"prompt"`
}

// parseMedia recognises a successful media-tool result ({ok:true, kind, url}).
func parseMedia(raw string) (mediaPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return mediaPayload{}, false
	}
	var m mediaPayload
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return mediaPayload{}, false
	}
	if !m.OK || m.URL == "" || m.Kind == "" {
		return mediaPayload{}, false
	}
	return m, true
}

// artifactPayload is a deliverable hand-off (F6): a downloadable file/archive
// or a deployed-site URL the client renders as an in-app card. Kind is one of
// "file" | "archive" | "site" (image/video stay on the media plane). URL is a
// content-addressed / public ref into object storage — never inline bytes.
type artifactPayload struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	MIME    string `json:"mime"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

// artifactEnvelope lets a tool advertise its artifact either at the top level
// ({ok,kind,url,...}) or nested under an "artifact" key ({ok:true,artifact:{…}}),
// matching the two shapes tools actually emit.
type artifactEnvelope struct {
	OK       bool             `json:"ok"`
	Artifact *artifactPayload `json:"artifact"`
}

// validArtifactKind gates the deliverable kinds so an artifact result never
// collides with the media plane (image/video) or a search payload.
func validArtifactKind(k string) bool {
	return k == "file" || k == "archive" || k == "site"
}

// parseArtifact recognises a successful artifact-tool result. It accepts both
// the nested ({ok:true,artifact:{kind,url,…}}) and the flat ({ok,kind,url,…})
// shapes, requires a deliverable kind + a URL ref, and never matches a media or
// search result (kind gating). Returns (payload, false) on any other shape so
// the additive event is emitted ONLY for genuine deliverables.
func parseArtifact(raw string) (artifactPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return artifactPayload{}, false
	}
	// Nested shape first ({ok:true, artifact:{…}}).
	var env artifactEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && env.Artifact != nil {
		a := *env.Artifact
		if validArtifactKind(a.Kind) && a.URL != "" {
			return a, true
		}
	}
	// Flat shape ({ok:true, kind, url, …}).
	var a artifactPayload
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return artifactPayload{}, false
	}
	if !a.OK || a.URL == "" || !validArtifactKind(a.Kind) {
		return artifactPayload{}, false
	}
	return a, true
}

func (s searchPayload) cards() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(s.Results))
	for _, r := range s.Results {
		out = append(out, map[string]interface{}{
			"title":     r.Title,
			"url":       r.URL,
			"snippet":   r.Snippet,
			"published": r.Published,
		})
	}
	return out
}

func humanizeTool(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return strings.ReplaceAll(name, "_", " ")
}

// profileRefreshTTL bounds how long a fetched profile is reused before a
// session rebuild re-fetches it. Short enough that a Settings edit (req 8.2)
// reflects on the next conversation/respawn; long enough that a burst of
// respawns doesn't hammer the daemon. A failed fetch also stamps the clock,
// so an unreachable daemon backs off instead of retrying every rebuild.
const profileRefreshTTL = 30 * time.Second

// fetchUserProfile fetches the onboarding profile from the daemon's
// /profile endpoint and caches it on the engine for injection into every
// agent's stable system prompt prefix (req 2.4/2.5). Best-effort: a
// missing daemon, absent profile, or any error falls back to the default
// "Neo" identity cleanly (empty preferredName + nil expertiseDomains).
// Thread-safe: writes the cached fields under profileMu.
func (e *Engine) fetchUserProfile() {
	// Stamp the attempt time up front so both success and failure back off
	// for the TTL (an unreachable daemon must not be polled every rebuild).
	e.profileMu.Lock()
	e.profileFetchedAt = time.Now()
	e.profileMu.Unlock()

	if e.profileStore != nil {
		stored, err := e.profileStore.Get(context.Background(), e.readLegacyProfile)
		if err == nil {
			e.cacheProfile(stored)
		}
		return
	}
	legacy, ok, err := e.readLegacyProfile(context.Background())
	if err != nil || !ok {
		return
	}
	e.cacheProfile(legacy)
}

func (e *Engine) readLegacyProfile(ctx context.Context) (profileStore.Profile, bool, error) {
	if e.backendURL == "" {
		return profileStore.Profile{}, false, nil
	}
	url := strings.TrimRight(e.backendURL, "/") + "/profile"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return profileStore.Profile{}, false, err
	}
	if e.backendToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.backendToken)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return profileStore.Profile{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return profileStore.Profile{}, false, nil
	}
	var pr struct {
		PreferredName    string   `json:"preferred_name"`
		AgentName        string   `json:"agent_name"`
		ExpertiseDomains []string `json:"expertise_domains"`
		URI              string   `json:"uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return profileStore.Profile{}, false, err
	}
	if strings.TrimSpace(pr.URI) == "" {
		return profileStore.Profile{}, false, nil
	}
	return profileStore.Profile{
		PreferredPersonName: strings.TrimSpace(pr.PreferredName),
		AgentName:           strings.TrimSpace(pr.AgentName),
		ExpertiseDomains:    append([]string(nil), pr.ExpertiseDomains...),
	}, true, nil
}

func (e *Engine) cacheProfile(pr profileStore.Profile) {
	e.profileMu.Lock()
	e.userPreferredName = strings.TrimSpace(pr.PreferredPersonName)
	e.userExpertiseDomains = append([]string(nil), pr.ExpertiseDomains...)
	if pr.AgentName != "" {
		e.userAgentName = pr.AgentName
	}
	e.profileMu.Unlock()
}

// maybeRefreshProfile re-fetches the onboarding profile if the cached copy
// is older than profileRefreshTTL. Called at session rebuild so a profile
// edited in Settings (req 8.2) takes effect on subsequent conversations/
// turns without a process restart.
func (e *Engine) maybeRefreshProfile() {
	e.profileMu.Lock()
	stale := e.profileFetchedAt.IsZero() || time.Since(e.profileFetchedAt) > profileRefreshTTL
	e.profileMu.Unlock()
	if stale {
		e.fetchUserProfile()
	}
}

// profileSnapshot returns the current cached profile under lock, for safe
// injection into a freshly rebuilt agent.
func (e *Engine) profileSnapshot() (agentName, preferredName string, expertiseDomains []string) {
	e.profileMu.Lock()
	defer e.profileMu.Unlock()
	return e.userAgentName, e.userPreferredName, e.userExpertiseDomains
}
