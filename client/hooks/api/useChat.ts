'use client'

/**
 * useChat — drives the Liaison conversation surface.
 *
 * One thread of user/assistant turns. Each user message is POSTed to
 * /chat. The Liaison either replies directly (rendered immediately) or
 * dispatches a pipeline run, in which case we subscribe to
 * /events?intent_id=<id> and render every chat.assistant narration turn
 * as it arrives, until the run reaches a terminal event.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslations } from 'next-intl'
import { toast } from 'sonner'
import { nanoid } from 'nanoid'
import { sendChat, type ChatResponse } from '@/lib/api/chat'
import { subscribeEvents } from '@/lib/api/events'
import {
  listConversations,
  getConversation,
  getConversationTrace,
  type ConversationSummary,
  type TraceEvent,
} from '@/lib/api/conversations'
import {
  getAsyncJob,
  answerGate as apiAnswerGate,
  answerAsk as apiAnswerAsk,
  cancelRun,
} from '@/lib/api/runs'
import type { SSEUpdate } from '@/lib/realtime/sse'
import type { Surface, AskResponse } from '@/lib/construct/types.gen'
import {
  applySurfaceEvent,
  parseSurface,
  CONSTRUCT_SURFACE,
  CONSTRUCT_SURFACE_PATCH,
} from '@/lib/construct/store'

export type ChatRole = 'user' | 'assistant'

/** A generated/edited media artifact (image or video) surfaced from a
 *  tool.media event and rendered inline in the thread + surface. */
export interface ChatMedia {
  url: string
  kind: 'image' | 'video'
  mime?: string
  prompt?: string
}

/** A deliverable artifact (F6) surfaced from a tool.artifact event: a
 *  downloadable file/archive or a deployed-site URL, rendered in-app as a
 *  download card / open-and-preview card instead of a raw bucket link in the
 *  answer text. The url is a content-addressed/public ref (never inline bytes). */
export interface ChatArtifact {
  url: string
  kind: 'file' | 'archive' | 'site'
  name?: string
  mime?: string
  size?: number
  /** Optional preview image (e.g. a deployed-site screenshot). */
  preview?: string
}

export interface ChatMessage {
  id: string
  role: ChatRole
  text: string
  /** Media artifacts attached to this assistant turn (generated images/video). */
  media?: ChatMedia[]
  /** The run this assistant turn narrates (absent for direct replies). */
  intentId?: string
  /** Source event seq for narration turns — used to de-dupe when a run
   *  is resumed (replayed) after a refresh so bubbles aren't doubled. */
  seq?: number
  /** The model's reasoning (chain-of-thought) for this turn, surfaced as a
   *  separate collapsible channel — NEVER part of the answer text. */
  reasoning?: string
  ts: number
}

/** idle = composer ready; thinking = awaiting /chat; working = a run is
 *  executing and being narrated. */
export type ChatPhase = 'idle' | 'thinking' | 'working'

export interface PendingGate {
  intentId: string
  nodeId: string
  question: string
  options: string[]
}

/* -------------------------------------------------------------------------- */
/*  Neo Adaptive Surface — structured task channel                            */
/* -------------------------------------------------------------------------- */
/* The morphing surface shows ONE live task at a time. Alongside the flat
 * `messages` thread (kept for history + the classic chat view), we derive a
 * structured `NeoTask` from the same SSE stream: intermediate narration and
 * tool activity become a visible "thought process" trace, web-search results
 * become source cards, and the closing turn becomes the answer. This is the
 * show-the-work differentiator surfaced as UI. */

/** The surface a workspace step is rendered as — each is an animated viewport,
 *  not a label. `narration` is Neo thinking out loud between actions. */
export type NeoStepKind =
  | 'narration'
  | 'terminal'
  | 'browser'
  | 'editor'
  | 'search'
  | 'media'
  | 'action'

/** One step in the live "Neo Workspace" timeline. A discriminated payload by
 *  `kind`: the renderer paints a terminal / browser / editor viewport from the
 *  real command / url / file — never a generic card. */
export interface NeoStep {
  id: string
  kind: NeoStepKind
  /** True while the action is in flight (paints the "running" viewport). */
  running: boolean
  /** False when the tool reported an error. */
  ok: boolean
  /** Short, user-friendly verb for the viewport header (e.g. "Terminal"). */
  title: string
  /** narration — Neo's spoken preamble between actions. */
  text?: string
  /** terminal — the real command + its captured output. */
  command?: string
  cwd?: string
  output?: string
  /** browser — the action verb, page, element, typed text, screenshot. */
  action?: string
  url?: string
  pageTitle?: string
  target?: string
  typed?: string
  screenshotUrl?: string
  excerpt?: string
  /** editor — the file path, language, and a bounded content/diff preview. */
  path?: string
  language?: string
  preview?: string
  /** search — the query (rich cards render from `searches`). */
  query?: string
}

/** A web-search source card surfaced from a `tool.search` event. */
export interface NeoSearchCard {
  title: string
  url: string
  snippet: string
  published?: string
}

/** A web-search result set (answer + source cards). */
export interface NeoSearch {
  tool: string
  provider: string
  query: string
  answer: string
  results: NeoSearchCard[]
}

/** One step in the live task-list (neo-smoothness req.3), surfaced from a
 *  `tool.todo` event: a short description and its lifecycle status. The list is
 *  REPLACED on every tool.todo (the agent sends the full current list each
 *  time), so the surface always renders the latest plan. */
export type NeoTodoStatus = 'pending' | 'in_progress' | 'done'
export interface NeoTodoItem {
  text: string
  status: NeoTodoStatus
}

/** One task-scoped sub-agent in a concurrent Agent Swarm, surfaced from the
 *  swarm.* / subagent.* events. Each runs in its OWN context with the full
 *  reversible toolset, so it carries its own workspace timeline (`steps`) —
 *  the "Agent's Window" — that renders exactly like Neo's own. */
export interface NeoSubAgent {
  index: number
  name: string
  persona?: string
  task?: string
  status: 'running' | 'done' | 'failed'
  /** Distilled report / failure note once it settles. */
  summary?: string
  /** This sub-agent's own workspace steps (terminal / browser / editor / …). */
  steps: NeoStep[]
  /** Last activity line (a tool step title or narration note) for the row. */
  activity?: string
}

/** A live Agent Swarm: several sub-agents spawned from one spawn_subagents
 *  call, running concurrently and reporting back. */
export interface NeoSwarm {
  id: string
  count: number
  agents: NeoSubAgent[]
  done: boolean
}

/** The active morphing-surface task. Null when the surface is at rest. */
export interface NeoTask {
  /** The run being narrated ('' for a direct reply with no pipeline run). */
  intentId: string
  /** The user prose that opened this task — shown as the card title so the
   *  morph paints meaningful content the instant it appears. */
  title?: string
  /** Ordered workspace steps — the visible, animated thought + action process. */
  steps: NeoStep[]
  /** Source-card payloads surfaced from web-search tools. */
  searches: NeoSearch[]
  /** Generated/edited media (images, video) surfaced during the run. */
  media: ChatMedia[]
  /** Deliverable artifacts (downloadable files/archives, deployed sites)
   *  surfaced during the run via tool.artifact (F6). */
  artifacts: ChatArtifact[]
  /** Latest glimpse of the model's chain-of-thought (the "thinking" channel).
   *  Secondary + dismissible — never the answer, never persisted. Streamed live
   *  via chat.delta (channel=reasoning) and reset at each new turn. */
  thinking?: string
  /** The answer text being typed out RIGHT NOW, streamed token-by-token via
   *  chat.delta (channel=content) before the turn settles. Transient: cleared
   *  when the segment ends (a narration/tool step) and replaced by the durable
   *  bubble when the closing answer (final) lands. */
  streamingAnswer?: string
  /** The agent-loop step index of the current streaming segment, so a delta
   *  from a NEW turn resets the live thinking + answer buffers. */
  streamTurn?: number
  /** A live Agent Swarm spawned during this run (absent until one starts). */
  swarm?: NeoSwarm
  /** The live task-list checklist (neo-smoothness req.3), replaced wholesale on
   *  each tool.todo event. Absent until the agent lays out a multi-step plan. */
  todos?: NeoTodoItem[]
  /** Construct surfaces projected for this run (construct.surface[.patch]
   *  events). The OPEN, typed projection protocol that supersedes the closed
   *  NeoStep union; rendered trustedly by the Construct renderers. Empty until
   *  the projection engine (Phase 4) emits them. */
  surfaces: Surface[]
  /** The final answer text (empty until the closing turn lands). */
  answer: string
  /** Optional chain-of-thought attached to the closing turn. */
  reasoning?: string
  /** True once the run settled (closing turn / terminal / direct reply). */
  done: boolean
  /** Set when the run ended without a clean answer. */
  failed?: boolean
  /** What Neo is carrying forward from its own memory for this run — a plain
   *  "story so far" recap plus a coarse, oldest-first timeline of what has
   *  happened. Surfaced once per turn via the memory.activation event and
   *  persisted to the durable trace, so it survives reopen. Result, not
   *  protocol: no journal / rollup / snapshot jargon. */
  memory?: NeoMemory
}

/** Neo's activated working memory for a run — the human-readable recap it is
 *  carrying forward, and the coarse timeline behind it. Read-only in the UI:
 *  the agent's memory is never editable by a human. */
export interface NeoMemory {
  /** A short prose recap of the story so far. */
  storySoFar: string
  /** Coarse, oldest-first beats of what has happened. */
  timeline: string[]
}

/** Mark any in-flight ('running') narration steps as done — a new step
 *  supersedes the seeded "Working on your request" placeholder. Tool steps
 *  settle by id on their own end event, so they are left untouched. */
function settleActive(steps: NeoStep[]): NeoStep[] {
  let changed = false
  const next = steps.map((s) => {
    if (s.kind === 'narration' && s.running) {
      changed = true
      return { ...s, running: false }
    }
    return s
  })
  return changed ? next : steps
}

/** Upsert a tool step by id: replace the matching step in place (running→done)
 *  so the viewport fills rather than appending a duplicate; otherwise append,
 *  settling any in-flight narration placeholder first. */
function upsertStep(steps: NeoStep[], step: NeoStep): NeoStep[] {
  const i = steps.findIndex((s) => s.id === step.id)
  if (i >= 0) {
    const next = steps.slice()
    next[i] = { ...next[i], ...step }
    return next
  }
  return [...settleActive(steps), step]
}

/** Build a NeoStep from a `tool.step` event's fields. */
function parseToolStep(fields: Record<string, unknown>): NeoStep | null {
  const id = asString(fields.id)
  const kind = asString(fields.surface) as NeoStepKind
  if (!id || !kind) return null
  return {
    id,
    kind,
    running: fields.running === true,
    ok: fields.ok !== false,
    title: asString(fields.title) || 'Working',
    command: asString(fields.command) || undefined,
    cwd: asString(fields.cwd) || undefined,
    output: asString(fields.output) || undefined,
    action: asString(fields.action) || undefined,
    url: asString(fields.url) || undefined,
    pageTitle: asString(fields.page_title) || undefined,
    target: asString(fields.target) || undefined,
    typed: asString(fields.text) || undefined,
    screenshotUrl: asString(fields.screenshot_url) || undefined,
    excerpt: asString(fields.excerpt) || undefined,
    path: asString(fields.path) || undefined,
    language: asString(fields.language) || undefined,
    preview: asString(fields.preview) || undefined,
    query: asString(fields.query) || undefined,
  }
}

/** Coerce the `results` field of a tool.search event into typed cards. */
function parseSearchCards(v: unknown): NeoSearchCard[] {
  if (!Array.isArray(v)) return []
  const cards: NeoSearchCard[] = []
  for (const row of v) {
    const o = (row ?? {}) as Record<string, unknown>
    const card: NeoSearchCard = {
      title: asString(o.title),
      url: asString(o.url),
      snippet: asString(o.snippet),
      published: asString(o.published) || undefined,
    }
    if (card.title || card.url || card.snippet) cards.push(card)
  }
  return cards
}

/** Coerce the `agents` field of a swarm.started event into typed sub-agents. */
function parseSwarmAgents(v: unknown): NeoSubAgent[] {
  if (!Array.isArray(v)) return []
  const agents: NeoSubAgent[] = []
  for (const row of v) {
    const o = (row ?? {}) as Record<string, unknown>
    const index = typeof o.index === 'number' ? o.index : Number(o.index) || agents.length + 1
    agents.push({
      index,
      name: asString(o.name) || `Agent ${String(index).padStart(2, '0')}`,
      persona: asString(o.persona) || undefined,
      task: asString(o.task) || undefined,
      status: 'running',
      steps: [],
    })
  }
  return agents.sort((a, b) => a.index - b.index)
}

/** Coerce the `items` field of a tool.todo event into typed checklist items.
 *  Unknown statuses fall back to 'pending'; entries with no text are dropped. */
function parseTodoItems(v: unknown): NeoTodoItem[] {
  if (!Array.isArray(v)) return []
  const items: NeoTodoItem[] = []
  for (const row of v) {
    const o = (row ?? {}) as Record<string, unknown>
    const text = asString(o.text)
    if (!text) continue
    const raw = asString(o.status)
    const status: NeoTodoStatus =
      raw === 'in_progress' ? 'in_progress' : raw === 'done' ? 'done' : 'pending'
    items.push({ text, status })
  }
  return items
}

/** Apply a per-agent update to the swarm by index, returning a new task. */
function patchAgent(
  prev: NeoTask | null,
  index: number,
  patch: (a: NeoSubAgent) => NeoSubAgent,
): NeoTask | null {
  if (!prev?.swarm) return prev
  const agents = prev.swarm.agents.map((a) => (a.index === index ? patch(a) : a))
  return { ...prev, swarm: { ...prev.swarm, agents } }
}

/** Fold one streamed `chat.delta` fragment into the live task. `turn` is the
 *  agent-loop step index: when it advances, the previous turn's live buffers
 *  are reset so each turn types fresh (the reasoning channel feeds the live
 *  "thinking" glimpse; the content channel feeds the answer being typed). */
function applyDelta(prev: NeoTask, turn: number, channel: string, text: string): NeoTask {
  const newSeg = prev.streamTurn !== turn
  if (channel === 'reasoning') {
    return {
      ...prev,
      thinking: (newSeg ? '' : (prev.thinking ?? '')) + text,
      streamingAnswer: newSeg ? '' : prev.streamingAnswer,
      streamTurn: turn,
    }
  }
  return {
    ...prev,
    streamingAnswer: (newSeg ? '' : (prev.streamingAnswer ?? '')) + text,
    thinking: newSeg ? '' : prev.thinking,
    streamTurn: turn,
  }
}

const EMPTY_TASK: NeoTask = {
  intentId: '',
  steps: [],
  searches: [],
  media: [],
  artifacts: [],
  surfaces: [],
  answer: '',
  done: false,
}

/** memoryFromFields parses a memory.activation event's fields into a NeoMemory
 *  (story-so-far recap + coarse oldest-first timeline), or null when the payload
 *  carries nothing to show. Shared by the live reducer and the trace rebuild so
 *  both render identically. Result, not protocol — no journal/rollup jargon. */
function memoryFromFields(fields: Record<string, unknown>): NeoMemory | null {
  const storySoFar = asString(fields.story_so_far).trim()
  const timeline = Array.isArray(fields.timeline)
    ? fields.timeline.map((t) => asString(t).trim()).filter(Boolean)
    : []
  if (!storySoFar && timeline.length === 0) return null
  return { storySoFar, timeline }
}

/** artifactFromFields parses a tool.artifact event's fields into a ChatArtifact
 *  (F6), or null when the payload isn't a deliverable artifact. Shared by the
 *  live reducer and the trace rebuild so both render identically. */
function artifactFromFields(fields: Record<string, unknown>): ChatArtifact | null {
  const kind = asString(fields.kind)
  const url = asString(fields.url)
  if ((kind !== 'file' && kind !== 'archive' && kind !== 'site') || !url) return null
  const size = typeof fields.size === 'number' ? fields.size : Number(fields.size) || undefined
  return {
    url,
    kind,
    name: asString(fields.name) || undefined,
    mime: asString(fields.mime) || undefined,
    size,
    preview: asString(fields.preview) || undefined,
  }
}

/**
 * buildTaskFromTrace rebuilds the settled "Neo's Computer" workspace from the
 * durable per-run trace (F3) — the slice of the live SSE stream the daemon
 * persists so a reopened thread shows the prior tool steps / source cards /
 * media / Construct surfaces / Agent-Swarm windows instead of an empty
 * computer. It folds the SAME workspace event families through the SAME helpers
 * the live reducer uses (upsertStep / parseSearchCards / parseSwarmAgents /
 * patchAgent / applySurfaceEvent), so a hydrated workspace is byte-identical to
 * the one the live stream produced.
 *
 * It is PURE (no React state) and never creates chat bubbles: the durable turn
 * log (rec.turns) owns the answer + narration bubbles, and the trace omits the
 * final answer + transient channels (chat.delta / chat.thinking), so there is
 * no double-render. Narration steps reuse the live id scheme (`${intentId}:
 * ${seq}`) so they dedupe identically if a live replay later folds the same
 * events.
 */
export function buildTaskFromTrace(
  events: TraceEvent[],
  intentId: string,
  title?: string,
): NeoTask {
  let task: NeoTask = { ...EMPTY_TASK, intentId, title, done: true }
  // Trace is persisted in publish order; sort defensively so an out-of-order
  // delivery never scrambles the timeline.
  const ordered = [...events].sort((a, b) => (a.seq ?? 0) - (b.seq ?? 0))
  for (const ev of ordered) {
    const fields = (ev.fields ?? {}) as Record<string, unknown>
    switch (ev.type) {
      case 'tool.step': {
        const step = parseToolStep(fields)
        if (step) task = { ...task, steps: upsertStep(task.steps, step) }
        break
      }
      case 'tool.search': {
        task = {
          ...task,
          searches: [
            ...task.searches,
            {
              tool: asString(fields.tool),
              provider: asString(fields.provider),
              query: asString(fields.query),
              answer: asString(fields.answer),
              results: parseSearchCards(fields.results),
            },
          ],
        }
        break
      }
      case 'tool.media': {
        const kind = asString(fields.kind)
        const url = asString(fields.url)
        if ((kind === 'image' || kind === 'video') && url) {
          task = {
            ...task,
            media: [
              ...task.media,
              {
                url,
                kind,
                mime: asString(fields.mime) || undefined,
                prompt: asString(fields.prompt) || undefined,
              },
            ],
          }
        }
        break
      }
      case 'tool.artifact': {
        const art = artifactFromFields(fields)
        if (art) task = { ...task, artifacts: [...task.artifacts, art] }
        break
      }
      case 'tool.todo': {
        // The checklist is replaced wholesale; the LAST tool.todo in the trace
        // is the final plan state, so it survives reopen (req.3.5).
        task = { ...task, todos: parseTodoItems(fields.items) }
        break
      }
      case 'memory.activation': {
        // Neo's activated working memory for the run — replaced wholesale (the
        // LAST activation is the settled story-so-far + timeline), so it
        // survives reopen. Read-only; never editable by a human.
        const mem = memoryFromFields(fields)
        if (mem) task = { ...task, memory: mem }
        break
      }
      case CONSTRUCT_SURFACE:
      case CONSTRUCT_SURFACE_PATCH: {
        const surface = parseSurface(fields)
        if (surface) {
          task = { ...task, surfaces: applySurfaceEvent(task.surfaces, ev.type, surface) }
        }
        break
      }
      case 'swarm.started': {
        const agents = parseSwarmAgents(fields.agents)
        const count = typeof fields.count === 'number' ? fields.count : agents.length
        task = { ...task, swarm: { id: asString(fields.swarm_id), count, agents, done: false } }
        break
      }
      case 'subagent.created': {
        const index = Number(fields.agent_index) || 0
        task =
          patchAgent(task, index, (a) => ({
            ...a,
            name: asString(fields.name) || a.name,
            persona: asString(fields.persona) || a.persona,
            task: asString(fields.task) || a.task,
          })) ?? task
        break
      }
      case 'subagent.step': {
        const index = Number(fields.agent_index) || 0
        const step = parseToolStep(fields)
        if (step) {
          task =
            patchAgent(task, index, (a) => ({
              ...a,
              steps: upsertStep(a.steps, step),
              activity: step.title || a.activity,
            })) ?? task
        }
        break
      }
      case 'subagent.note': {
        const index = Number(fields.agent_index) || 0
        const note = asString(fields.text)
        if (note) task = patchAgent(task, index, (a) => ({ ...a, activity: note })) ?? task
        break
      }
      case 'subagent.status': {
        const index = Number(fields.agent_index) || 0
        const raw = asString(fields.status)
        const status: NeoSubAgent['status'] =
          raw === 'done' ? 'done' : raw === 'failed' ? 'failed' : 'running'
        const summary = asString(fields.summary)
        task =
          patchAgent(task, index, (a) => ({ ...a, status, summary: summary || a.summary })) ?? task
        break
      }
      case 'swarm.completed': {
        if (task.swarm) task = { ...task, swarm: { ...task.swarm, done: true } }
        break
      }
      case 'chat.assistant': {
        // The trace persists only the non-final "thinking out loud" narration
        // (the final answer lives in the durable turn log). Each becomes a
        // narration step, keyed exactly as the live reducer keys it.
        if (fields.final === true) break
        const text = asString(fields.text)
        if (!text) break
        task = {
          ...task,
          steps: [
            ...settleActive(task.steps),
            {
              id: `${intentId}:${ev.seq}`,
              kind: 'narration',
              running: false,
              ok: true,
              title: '',
              text,
            },
          ],
        }
        break
      }
      default:
        break
    }
  }
  return { ...task, steps: settleActive(task.steps) }
}

export interface UseChatResult {
  messages: ChatMessage[]
  phase: ChatPhase
  activeIntentId: string | null
  conversationId: string | null
  error: string | null
  pendingGate: PendingGate | null
  /** F2 — durable live-run resume, visible half. True between the moment the
   *  F1 resume branch attaches to a still-running run (GET /conversations/{id}
   *  carried a non-empty `live_run`) and the first event arrival on the
   *  reconnected stream. The surface renders this as "Reconnecting to your
   *  task…" so the thread is never read as idle/done during the resume window. */
  resuming: boolean
  /** F2 — visible "connection lost — retrying" state during a non-terminal
   *  stream drop. True while the bounded re-subscribe loop is in flight (up to
   *  3 attempts with exponential backoff 1s/2s/4s) AND while the underlying SSE
   *  client is in its own `reconnecting` window. Cleared on the next event
   *  arrival, or replaced by the existing terminal toast on exhaustion. */
  connectionRetrying: boolean
  /** The active morphing-surface task (null when the surface is idle). */
  task: NeoTask | null
  /** Collapse the active surface back to the idle pill (returns to idle).
   *  No-op while a money/approval gate is pending — consent is required. */
  dismissTask: () => void
  /** Every persisted thread (newest-first) for the history list. */
  conversations: ConversationSummary[]
  /** True while the conversation list or a selected thread is loading. */
  loadingThread: boolean
  send: (text: string) => void
  /** Start a fresh thread (clears the surface; does not delete history). */
  reset: () => void
  /** Reopen a past thread by id, loading its turns from the DB. */
  selectConversation: (id: string) => void
  /** Re-fetch the conversation list from the DB. */
  refreshConversations: () => void
  answerGate: (approved: boolean, answer?: string) => void
  /** Answer a parked Construct Ask (the bidirectional back-channel): POST the
   *  typed response for the surface id; the run resumes with it. */
  respondAsk: (surfaceId: string, response: AskResponse) => void
}

function asString(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

// How long to keep the stream open after the run's terminal event,
// waiting for the Liaison's closing turn (the final answer / failure
// explanation), which is emitted as a chat.assistant event AFTER
// message.complete. Mirrors the daemon's liaisonFinalWait (30s) + slack.
const CLOSING_TURN_GRACE_MS = 40_000
// Fallback close window: after the terminal event the deterministic close
// is the closing turn marked final:true. If that marker never arrives (an
// older daemon, or a failed final-answer compose), close a short beat after
// the LAST post-terminal narration turn settles, still bounded by
// CLOSING_TURN_GRACE_MS.
const SETTLE_AFTER_LAST_TURN_MS = 8_000

export function useChat(): UseChatResult {
  // User-facing copy is translated. Because the closing/error strings are
  // produced inside long-lived SSE callbacks (stable across renders), we
  // read the translator through a ref so those callbacks never close over
  // a stale `t` and never need to be re-created when the locale changes.
  const t = useTranslations('agentChat')
  const tRef = useRef(t)
  useEffect(() => {
    tRef.current = t
  }, [t])

  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [phase, setPhase] = useState<ChatPhase>('idle')
  const [activeIntentId, setActiveIntentId] = useState<string | null>(null)
  const [conversationId, setConversationId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pendingGate, setPendingGate] = useState<PendingGate | null>(null)
  const [task, setTask] = useState<NeoTask | null>(null)
  // F2 — distinct UI states that live ALONGSIDE `phase` (kept byte-compatible).
  // `resuming` while the F1 reattach is mid-replay; `connectionRetrying` while
  // a non-terminal stream drop is being re-subscribed. The surface renders
  // these as plain-language strings (no protocol jargon, ux_truth).
  const [resuming, setResuming] = useState(false)
  const [connectionRetrying, setConnectionRetrying] = useState(false)
  const [conversations, setConversations] = useState<ConversationSummary[]>([])
  const [loadingThread, setLoadingThread] = useState(false)

  const convRef = useRef<string | null>(null)
  const subRef = useRef<{ close: () => void } | null>(null)
  const seenRef = useRef<Set<string>>(new Set())
  const hydratedRef = useRef(false)

  // Mirror the live run id + phase into refs so the abort/interrupt
  // primitives (dismiss, redirect) can read them without re-creating the
  // long-lived `send`/`dismissTask` callbacks on every state change.
  const activeIntentRef = useRef<string | null>(null)
  const phaseRef = useRef<ChatPhase>('idle')
  useEffect(() => {
    activeIntentRef.current = activeIntentId
  }, [activeIntentId])
  useEffect(() => {
    phaseRef.current = phase
  }, [phase])

  const closeSub = useCallback(() => {
    subRef.current?.close()
    subRef.current = null
  }, [])

  const answerGate = useCallback(
    (approved: boolean, answer?: string) => {
      if (!pendingGate) return
      apiAnswerGate(pendingGate.intentId, pendingGate.nodeId, { approved, answer }).catch(
        (e: Error) => setError(e.message || 'Failed to submit answer'),
      )
      // Optimistically clear the gate from the UI.
      setPendingGate(null)
    },
    [pendingGate],
  )

  // Answer a parked Construct Ask: POST the typed response for the active run +
  // surface id. The daemon delivers it to the blocked agent and emits an
  // answered patch that settles the rendered control (so no optimistic update
  // is needed here). The active run is the one the Ask was rendered under.
  const respondAsk = useCallback((surfaceId: string, response: AskResponse) => {
    const intentId = activeIntentRef.current
    if (!intentId) return
    apiAnswerAsk(intentId, surfaceId, response).catch((e: Error) =>
      setError(e.message || 'Failed to submit answer'),
    )
  }, [])

  const refreshConversations = useCallback(() => {
    listConversations()
      .then(setConversations)
      .catch(() => {
        /* non-fatal: the history list just stays as-is */
      })
  }, [])

  const pushAssistant = useCallback(
    (text: string, intentId?: string, seq?: number, reasoning?: string, media?: ChatMedia[]) => {
      setMessages((m) => [
        ...m,
        {
          id: nanoid(),
          role: 'assistant',
          text,
          intentId,
          seq,
          reasoning,
          media: media && media.length ? media : undefined,
          ts: Date.now(),
        },
      ])
    },
    [],
  )

  // Media artifacts generated during the in-flight run, attached to the
  // closing assistant turn when it lands (and rendered live on the surface).
  const runMediaRef = useRef<ChatMedia[]>([])

  // F2 — bounded re-subscribe scheduler for non-terminal stream drops. The
  // delay timer + the latest retry token live here so a new external subscribe
  // (a fresh send / a fresh selectConversation) cancels any pending retry.
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const cancelRetryTimer = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current)
      retryTimerRef.current = null
    }
  }, [])
  // F2 — break the self-reference cycle between subscribe and its bounded
  // retry: the retry path calls `subscribeRef.current(...)` so the useCallback
  // body never needs to reference itself by name (TDZ + lint-safe).
  type SubscribeFn = (
    intentId: string,
    opts?: {
      replay?: boolean
      seedSeen?: string[]
      resuming?: boolean
      retryAttempt?: number
    },
  ) => void
  const subscribeRef = useRef<SubscribeFn | null>(null)

  const subscribe: SubscribeFn = useCallback(
    (
      intentId: string,
      opts?: {
        replay?: boolean
        seedSeen?: string[]
        /** F2 — set by selectConversation's F1 resume branch so the surface
         *  renders "Reconnecting to your task…" until the first event arrives. */
        resuming?: boolean
        /** F2 — internal: bounded auto-retry attempt index (0 = fresh call). */
        retryAttempt?: number
      },
    ) => {
      closeSub()
      cancelRetryTimer()
      seenRef.current = new Set(opts?.seedSeen ?? [])
      runMediaRef.current = []
      setActiveIntentId(intentId)
      setPhase('working')
      // F2 — `resuming` is asserted only on the F1 reattach path; any retry
      // call also keeps the previously-cleared resuming state untouched.
      if (opts?.resuming) setResuming(true)
      // A retry call preserves `connectionRetrying=true` (set on the error
      // that triggered the retry); a fresh subscribe always starts clean.
      if (!opts?.retryAttempt) setConnectionRetrying(false)
      // Bind the live surface task to this run (create one if a direct
      // dispatch skipped the thinking phase, e.g. on replay reconnect).
      setTask((prev) =>
        prev ? { ...prev, intentId, done: false, failed: false } : { ...EMPTY_TASK, intentId },
      )

      let terminalSeen = false
      let terminalStatus = ''
      let failReason = ''
      let closingTurnSeen = false
      // Did this run commit any DURABLE thread content (a narration bubble or a
      // rendered Construct surface)? The task_complete completion summary is
      // normally hidden as a redundant recap — but if the run produced no
      // durable content, hiding it would leave an EMPTY thread (only the user's
      // question). So when nothing durable landed, we SHOW the summary instead:
      // the thread always keeps the answer, never nothing.
      let durableProduced = false
      // The chain-of-thought streamed for the CURRENT turn (chat.delta
      // channel=reasoning), accumulated so it can be attached to that turn's
      // durable assistant bubble when it lands. Without this the reasoning
      // lives only in the transient live-turn glimpse and vanishes the moment
      // the run settles; attaching it makes the thinking persist in the
      // bubble's collapsible subfield. Reset when the turn index advances.
      let liveReasoning = ''
      let liveReasoningTurn = -1
      let graceTimer: ReturnType<typeof setTimeout> | null = null
      let settleTimer: ReturnType<typeof setTimeout> | null = null
      const attempt = opts?.retryAttempt ?? 0

      const finish = () => {
        if (graceTimer) {
          clearTimeout(graceTimer)
          graceTimer = null
        }
        if (settleTimer) {
          clearTimeout(settleTimer)
          settleTimer = null
        }
        cancelRetryTimer()
        setPhase('idle')
        setActiveIntentId(null)
        setResuming(false)
        setConnectionRetrying(false)
        closeSub()
        // The surface stays mounted showing the result until the user
        // dismisses it; just mark the task settled.
        setTask((prev) => (prev ? { ...prev, done: true } : prev))
        // The closing answer is now persisted server-side; refresh the
        // history list so titles/previews/ordering reflect this thread.
        refreshConversations()
      }

      const armGrace = () => {
        if (graceTimer) return
        graceTimer = setTimeout(() => {
          // The closing turn never arrived. If the run didn't succeed,
          // tell the user plainly instead of silently stopping.
          if (!closingTurnSeen && terminalStatus && terminalStatus !== 'completed') {
            const reason = failReason
              ? tRef.current('taskFailed', { reason: failReason })
              : tRef.current('taskStopped')
            pushAssistant(reason)
            setTask((prev) =>
              prev ? { ...prev, answer: prev.answer || reason, done: true, failed: true } : prev,
            )
          }
          finish()
        }, CLOSING_TURN_GRACE_MS)
      }

      const armSettle = () => {
        if (settleTimer) clearTimeout(settleTimer)
        settleTimer = setTimeout(finish, SETTLE_AFTER_LAST_TURN_MS)
      }

      subRef.current = subscribeEvents({
        intentId,
        replay: opts?.replay,
        onUpdate: (u: SSEUpdate) => {
          if (u.kind === 'reconnecting') {
            // F2 — the resilient SSE client is mid-reconnect (transient
            // drop, still under its own attempt budget). Surface it as the
            // visible "Connection lost — retrying…" state instead of letting
            // the surface read as a normal working stream.
            if (!terminalSeen) setConnectionRetrying(true)
            return
          }
          if (u.kind === 'error') {
            // F2 — non-terminal stream drop (the SSE client's reconnect budget
            // exhausted). Do NOT silently finish; the agent may still be
            // working in the background. Keep the visible retrying state,
            // tear down the dead subscription, and re-subscribe with
            // replay:true after a bounded backoff. After 3 exhausted retries
            // (1s, 2s, 4s), fall back to the legacy toast + finish.
            if (terminalSeen) {
              finish()
              return
            }
            const nextAttempt = attempt + 1
            const MAX_RETRY = 3
            if (nextAttempt > MAX_RETRY) {
              toast.error(tRef.current('connectionLost'))
              finish()
              return
            }
            // Show the visible retrying state; tear down the current sub but
            // do NOT call finish() (that would flip phase to idle and clear
            // the task). Schedule a replay re-subscribe after backoff.
            setConnectionRetrying(true)
            subRef.current?.close()
            subRef.current = null
            cancelRetryTimer()
            const delay = 1000 * Math.pow(2, nextAttempt - 1)
            retryTimerRef.current = setTimeout(() => {
              retryTimerRef.current = null
              subscribeRef.current?.(intentId, {
                replay: true,
                seedSeen: Array.from(seenRef.current),
                resuming: opts?.resuming,
                retryAttempt: nextAttempt,
              })
            }, delay)
            return
          }
          if (u.kind === 'closed') {
            // Transport ended; nothing more will arrive, so wrap up.
            finish()
            return
          }
          if (u.kind !== 'event') return
          // F2 — first event after a reattach proves the wire is alive
          // again: drop the visible resuming/retrying state and let the
          // normal working surface take over.
          setResuming(false)
          setConnectionRetrying(false)
          const ev = u.event

          // Thinking channel → the secondary, dismissible "thinking" glimpse.
          // Never a bubble, never persisted; just the latest thought on the
          // surface so the user sees how Neo is reasoning.
          if (ev.type === 'chat.thinking') {
            const text = asString(ev.fields?.text)
            if (text) setTask((prev) => (prev ? { ...prev, thinking: text } : prev))
            return
          }

          // Live "typing" channel → the answer being typed (channel=content)
          // and the streaming thinking (channel=reasoning), folded fragment by
          // fragment so the user sees Neo work in real time instead of a blank
          // wait. Transient: the content stream is replaced by the durable
          // bubble when the closing answer lands; thinking is never persisted.
          if (ev.type === 'chat.delta') {
            const channel = asString(ev.fields?.channel)
            const text = asString(ev.fields?.text)
            const turn =
              typeof ev.fields?.turn === 'number' ? ev.fields.turn : Number(ev.fields?.turn) || 0
            if (text && (channel === 'content' || channel === 'reasoning')) {
              if (channel === 'reasoning') {
                if (turn !== liveReasoningTurn) {
                  liveReasoningTurn = turn
                  liveReasoning = text
                } else {
                  liveReasoning += text
                }
              }
              setTask((prev) => (prev ? applyDelta(prev, turn, channel, text) : prev))
            }
            return
          }

          // Construct surfaces → the OPEN, typed projection protocol. A full
          // surface or a progressive patch is validated and folded into the
          // run's ordered surface list (mirroring the canonical Go ApplyPatch),
          // then rendered trustedly by the Construct renderers. Malformed /
          // unknown payloads are dropped by parseSurface (invariant i2).
          if (ev.type === CONSTRUCT_SURFACE || ev.type === CONSTRUCT_SURFACE_PATCH) {
            const surface = parseSurface(ev.fields)
            if (surface) {
              // A rendered surface is durable thread content (it shows on the
              // centerpiece), so the closing summary may be safely hidden.
              durableProduced = true
              setTask((prev) =>
                prev
                  ? { ...prev, surfaces: applySurfaceEvent(prev.surfaces, ev.type, surface) }
                  : prev,
              )
            }
            return
          }

          // Narrator turns → assistant bubbles.
          if (ev.type.startsWith('chat.')) {
            const role = asString(ev.fields?.role)
            if (role && role !== 'assistant') return
            const key = `${intentId}:${ev.seq}`
            if (seenRef.current.has(key)) return
            seenRef.current.add(key)
            const text = asString(ev.fields?.text)
            // Prefer an explicit reasoning field; otherwise use the chain-of-
            // thought streamed live for this turn (chat.delta channel=reasoning),
            // so the thinking persists in the bubble's collapsible subfield
            // instead of vanishing with the live turn.
            const reasoning = asString(ev.fields?.reasoning) || liveReasoning
            const isFinal = ev.fields?.final === true
            // The validated task_complete summary is flagged `completion:true` —
            // a redundant recap layered on top of the narration the user already
            // read and the rendered Construct centerpiece. It is HIDDEN from the
            // thread (still sent on the wire so Cassandra's accepted completion
            // closes the task). Everything else becomes a PERMANENT assistant
            // bubble: Neo's narration as it works (non-final turns) AND a real
            // conversational / ceiling final answer (final without the
            // completion flag) — those ARE the content the user reads.
            const isCompletionSummary = isFinal && ev.fields?.completion === true
            // The synthetic narrate-before-act intent stub (e.g. "Layerx
            // deposit.") is flagged `ephemeral:true`: it is shown live so the
            // user can follow along, but it is NOT durable thread content and
            // must NEVER count toward durableProduced. Letting it count was the
            // bug where a straight-to-task_complete turn looked "already
            // narrated" and the real summary got hidden — the user saw only the
            // stub. The server never persists it either, so it drops away on
            // reopen while the real answer remains.
            const isEphemeral = ev.fields?.ephemeral === true
            if (text && !isCompletionSummary) {
              pushAssistant(text, intentId, ev.seq, reasoning || undefined)
              // A non-final GENUINE narration turn is durable thread content;
              // once one lands, the closing summary is a safe-to-hide redundant
              // recap. An ephemeral stub is not narration and does not qualify.
              if (!isFinal && !isEphemeral) durableProduced = true
            } else if (text && isCompletionSummary && !durableProduced) {
              // Safety net: the run produced no durable narration or surface, so
              // hiding the summary would empty the thread. Show it — it is the
              // only answer the user has — and treat it as a normal bubble.
              pushAssistant(text, intentId, ev.seq, reasoning || undefined)
            }
            if (text) {
              // Retire the transient live "typing" buffer either way: a settled
              // narration turn is now its durable bubble (pushed above), and the
              // hidden final reply must not linger as a streaming ghost. The
              // final reply still settles the task (answer/done) so the surface
              // and the work centerpiece flip to their finished state.
              setTask((prev) =>
                prev
                  ? isFinal
                    ? {
                        ...prev,
                        answer: text,
                        reasoning: reasoning || prev.reasoning,
                        done: true,
                        streamingAnswer: '',
                      }
                    : { ...prev, streamingAnswer: '' }
                  : prev,
              )
              // The turn's reasoning has been committed to its durable bubble;
              // clear the accumulator so the next turn starts fresh and this
              // thinking never re-attaches to a later bubble.
              liveReasoning = ''
              liveReasoningTurn = -1
            }
            // The Liaison's TRUE closing turn (final answer / failure
            // explanation) is composed AFTER the terminal event and marked
            // final:true. Earlier post-terminal turns can be stale trailing
            // progress narration still in flight when the run ended, so we
            // must NOT close on the first one (the old bug that hid the real
            // answer). Close deterministically on final:true; otherwise keep
            // reading and let a short settle timer close after the last turn.
            if (terminalSeen && text) {
              closingTurnSeen = true
              if (ev.fields?.final === true) finish()
              else armSettle()
            }
            return
          }

          // In-walk human gates → the wallet/approval surface.
          if (ev.type === 'gate.invoked') {
            const nodeId = asString(ev.fields?.node_id)
            const question = asString(ev.fields?.question)
            const options = Array.isArray(ev.fields?.options) ? ev.fields.options.map(asString) : []
            if (nodeId && question) {
              setPendingGate({ intentId, nodeId, question, options })
              setTask((prev) =>
                prev
                  ? {
                      ...prev,
                      steps: [
                        ...settleActive(prev.steps),
                        {
                          id: `${intentId}:gate:${nodeId}`,
                          kind: 'narration',
                          running: true,
                          ok: true,
                          title: '',
                          text: 'Ready for your approval',
                        },
                      ],
                    }
                  : prev,
              )
            }
            return
          }
          if (ev.type === 'gate.decided' || ev.type === 'gate.timeout') {
            setPendingGate((prev) => (prev?.nodeId === ev.fields?.node_id ? null : prev))
            setTask((prev) => (prev ? { ...prev, steps: settleActive(prev.steps) } : prev))
            return
          }

          // Show-the-work: the live animated workspace step (terminal /
          // browser / editor / action). Same id upserts running→done so the
          // viewport fills in place instead of stacking duplicate cards.
          if (ev.type === 'tool.step') {
            const step = parseToolStep(ev.fields ?? {})
            if (step) {
              // A tool call means this turn's streamed content was a preamble,
              // not the answer — clear the live buffer as work takes over.
              setTask((prev) =>
                prev ? { ...prev, steps: upsertStep(prev.steps, step), streamingAnswer: '' } : prev,
              )
            }
            return
          }

          // Show-the-work: web-search SOURCE CARDS. The running/done indicator
          // is the `search` workspace step (tool.step); this carries the rich
          // answer + source cards rendered beneath the timeline.
          if (ev.type === 'tool.search') {
            const search: NeoSearch = {
              tool: asString(ev.fields?.tool),
              provider: asString(ev.fields?.provider),
              query: asString(ev.fields?.query),
              answer: asString(ev.fields?.answer),
              results: parseSearchCards(ev.fields?.results),
            }
            setTask((prev) => (prev ? { ...prev, searches: [...prev.searches, search] } : prev))
            return
          }
          // Generated/edited media → the live media grid + (on the closing
          // turn) the answer bubble. The `media` workspace step (tool.step)
          // is the running indicator.
          if (ev.type === 'tool.media') {
            const kind = asString(ev.fields?.kind)
            const url = asString(ev.fields?.url)
            if ((kind === 'image' || kind === 'video') && url) {
              const item: ChatMedia = {
                url,
                kind,
                mime: asString(ev.fields?.mime) || undefined,
                prompt: asString(ev.fields?.prompt) || undefined,
              }
              runMediaRef.current = [...runMediaRef.current, item]
              setTask((prev) => (prev ? { ...prev, media: [...prev.media, item] } : prev))
            }
            return
          }
          // F6: a deliverable artifact (file/archive download, or a deployed
          // site) → the in-app artifact card, instead of a raw bucket URL in
          // the answer text.
          if (ev.type === 'tool.artifact') {
            const art = artifactFromFields(ev.fields ?? {})
            if (art) {
              setTask((prev) => (prev ? { ...prev, artifacts: [...prev.artifacts, art] } : prev))
            }
            return
          }

          // Live task-list (neo-smoothness req.3): the ordered checklist that
          // ticks off in real time. Replaced wholesale on each update (the agent
          // sends the full current list every time).
          if (ev.type === 'tool.todo') {
            const todos = parseTodoItems(ev.fields?.items)
            setTask((prev) => (prev ? { ...prev, todos } : prev))
            return
          }

          // Neo's activated working memory: a plain "story so far" recap plus a
          // coarse, oldest-first timeline of what has happened, emitted once per
          // turn. Replaced wholesale on each activation. Read-only — the agent's
          // memory is never editable by a human.
          if (ev.type === 'memory.activation') {
            const mem = memoryFromFields(ev.fields ?? {})
            if (mem) setTask((prev) => (prev ? { ...prev, memory: mem } : prev))
            return
          }

          // Agent Swarm: task-scoped sub-agents fanned out to run concurrently.
          // Each sub-agent reports its OWN workspace steps (same shape as Neo's
          // tool.step), so it renders its own animated "Agent's Window".
          if (ev.type === 'swarm.started') {
            const agents = parseSwarmAgents(ev.fields?.agents)
            const count = typeof ev.fields?.count === 'number' ? ev.fields.count : agents.length
            setTask((prev) =>
              prev
                ? {
                    ...prev,
                    swarm: { id: asString(ev.fields?.swarm_id), count, agents, done: false },
                  }
                : prev,
            )
            return
          }
          if (ev.type === 'subagent.created') {
            const index = Number(ev.fields?.agent_index) || 0
            setTask((prev) =>
              patchAgent(prev, index, (a) => ({
                ...a,
                name: asString(ev.fields?.name) || a.name,
                persona: asString(ev.fields?.persona) || a.persona,
                task: asString(ev.fields?.task) || a.task,
              })),
            )
            return
          }
          if (ev.type === 'subagent.step') {
            const index = Number(ev.fields?.agent_index) || 0
            const step = parseToolStep(ev.fields ?? {})
            if (step) {
              setTask((prev) =>
                patchAgent(prev, index, (a) => ({
                  ...a,
                  steps: upsertStep(a.steps, step),
                  activity: step.title || a.activity,
                })),
              )
            }
            return
          }
          if (ev.type === 'subagent.note') {
            const index = Number(ev.fields?.agent_index) || 0
            const note = asString(ev.fields?.text)
            if (note) {
              setTask((prev) => patchAgent(prev, index, (a) => ({ ...a, activity: note })))
            }
            return
          }
          if (ev.type === 'subagent.status') {
            const index = Number(ev.fields?.agent_index) || 0
            const raw = asString(ev.fields?.status)
            const status: NeoSubAgent['status'] =
              raw === 'done' ? 'done' : raw === 'failed' ? 'failed' : 'running'
            const summary = asString(ev.fields?.summary)
            setTask((prev) =>
              patchAgent(prev, index, (a) => ({ ...a, status, summary: summary || a.summary })),
            )
            return
          }
          if (ev.type === 'swarm.completed') {
            setTask((prev) =>
              prev?.swarm ? { ...prev, swarm: { ...prev.swarm, done: true } } : prev,
            )
            return
          }

          // Terminal detection — do NOT tear down here (the old bug): the
          // closing narration lands a beat later. Arm a bounded grace
          // window and keep reading until it arrives.
          const to = asString(ev.fields?.to)
          const status = asString(ev.fields?.status)
          if (ev.type === 'lifecycle.transition' && (to === 'failed' || to === 'cancelled')) {
            terminalSeen = true
            terminalStatus = to
            if (!failReason) failReason = asString(ev.fields?.error)
            armGrace()
          } else if (ev.type === 'message.complete') {
            terminalSeen = true
            terminalStatus = status || 'completed'
            armGrace()
          }
        },
      })
    },
    [closeSub, pushAssistant, refreshConversations, cancelRetryTimer],
  )
  // F2 — keep the ref in sync so the bounded-retry timeout can re-enter.
  useEffect(() => {
    subscribeRef.current = subscribe
  }, [subscribe])

  // Reopen a past thread by id: load its durable turns from the DB and,
  // if its most recent run is still in flight, reconnect the live stream
  // (which replays narration + any pending approval gate).
  const selectConversation = useCallback(
    (id: string) => {
      if (!id) return
      closeSub()
      setPendingGate(null)
      setError(null)
      setActiveIntentId(null)
      setPhase('idle')
      setLoadingThread(true)
      convRef.current = id
      setConversationId(id)
      getConversation(id)
        .then((rec) => {
          // When the most recent run is still live (rec.live_run), its
          // narration was persisted as it streamed AND will be replayed on the
          // reconnect below. Drop the live run's persisted ASSISTANT turns from
          // the initial load so replay (deduped by seq) is their single source —
          // otherwise the narration would double. The live USER turn is kept
          // (replay carries only agent events, never the user's own message),
          // and a settled thread (no live_run) loads every turn untouched.
          const loaded: ChatMessage[] = rec.turns
            .filter(
              (turn) =>
                !(
                  rec.live_run &&
                  turn.role === 'assistant' &&
                  (turn.intent_id ?? '') === rec.live_run
                ),
            )
            .map((turn) => ({
              id: nanoid(),
              role: turn.role,
              text: turn.text,
              intentId: turn.intent_id,
              ts: turn.ts ? Date.parse(turn.ts) || Date.now() : Date.now(),
            }))
          setMessages(loaded)
          setLoadingThread(false)
          // F1 — durable live-run resume. The server stamps the authoritative
          // in-flight run id on the conversation record's `live_run` field
          // (GET /conversations/{id}). When it is non-empty the run is still
          // executing on the daemon, so we subscribe(replay:true) IMMEDIATELY
          // — no /messages/async/<id> poll required. Replay reconnects
          // narration AND re-emits gate.invoked, so a pending approval
          // reappears after a refresh / hard-refresh / wipe-cookies-and-
          // relogin. The trailing-user-turn + poll branch below is kept as a
          // backstop for older daemons that omit `live_run`.
          if (rec.live_run) {
            // F2 — assert the visible "Reconnecting to your task…" state on
            // the F1 resume path so the surface never reads as idle/done
            // while the reattach is mid-replay.
            subscribe(rec.live_run, { replay: true, resuming: true })
            return
          }
          // F3 — settled thread: rebuild the durable "Neo's Computer" workspace
          // from the persisted per-run trace, so reopening shows the prior tool
          // steps / source cards / media / Construct surfaces / Agent-Swarm
          // windows instead of an empty computer. Best-effort + additive: an
          // older daemon (no trace route) or a text-only thread just leaves the
          // surface idle. The convRef guard drops a stale fetch if the user
          // navigated to another thread while it was in flight.
          const title = rec.title || rec.turns.find((turn) => turn.role === 'user')?.text
          getConversationTrace(id)
            .then((tr) => {
              if (convRef.current !== id) return
              if (tr.events.length > 0) {
                setTask(buildTaskFromTrace(tr.events, tr.intent_id || id, title))
              }
            })
            .catch(() => {
              /* no durable workspace (older daemon / text-only thread) */
            })
          const last = rec.turns[rec.turns.length - 1]
          if (last && last.role === 'user' && last.intent_id) {
            const liveIntent = last.intent_id
            getAsyncJob(liveIntent)
              .then((job) => {
                if (job.status === 'running' || job.status === 'queued') {
                  subscribe(liveIntent, { replay: true })
                }
              })
              .catch(() => {
                /* job lookup failed — treat the thread as settled */
              })
          }
        })
        .catch((e: Error) => {
          setLoadingThread(false)
          setError(e.message)
        })
    },
    [closeSub, subscribe],
  )

  // On first mount, load the conversation list from the DB and reopen
  // the most recent thread so the user lands back where they left off.
  useEffect(() => {
    if (hydratedRef.current) return
    hydratedRef.current = true
    listConversations()
      .then((items) => {
        setConversations(items)
        if (items.length > 0) selectConversation(items[0].conversation_id)
      })
      .catch(() => {
        /* no history yet (or daemon offline) — start with a blank thread */
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const send = useCallback(
    (text: string) => {
      const trimmed = text.trim()
      if (!trimmed) return
      setError(null)
      const midRun = !!activeIntentRef.current && phaseRef.current === 'working'
      setMessages((m) => [...m, { id: nanoid(), role: 'user', text: trimmed, ts: Date.now() }])
      // F5: a send mid-run is NOT an interrupt. The daemon folds the message
      // into the live agent at its next tool-call boundary (POST /chat returns
      // the SAME active run id), so the existing live stream + workspace keep
      // going — we never cancel the run, reset the surface, or re-subscribe.
      // The explicit Stop control is the only interrupt.
      if (midRun) {
        sendChat({ message: trimmed, conversationId: convRef.current ?? undefined })
          .then((res: ChatResponse) => {
            convRef.current = res.conversation_id
            setConversationId(res.conversation_id)
          })
          .catch((e: Error) => {
            setError(e.message)
            toast.error(tRef.current('sendError'))
          })
        return
      }
      setPhase('thinking')
      // Open a fresh surface immediately, seeded with the prose as the card
      // title and a first active step, so the pill→card morph paints
      // meaningful content in the same frame (no blank beat before the first
      // SSE event).
      setTask({
        ...EMPTY_TASK,
        title: trimmed,
        steps: [
          {
            id: 'seed',
            kind: 'narration',
            running: true,
            ok: true,
            title: '',
            text: 'Working on your request',
          },
        ],
      })
      sendChat({ message: trimmed, conversationId: convRef.current ?? undefined })
        .then((res: ChatResponse) => {
          convRef.current = res.conversation_id
          setConversationId(res.conversation_id)
          if (res.kind === 'reply') {
            pushAssistant(res.text)
            setPhase('idle')
            // Direct reply: settle the seeded step and the surface straight
            // to the answer.
            setTask((prev) =>
              prev
                ? { ...prev, steps: settleActive(prev.steps), answer: res.text, done: true }
                : { ...EMPTY_TASK, answer: res.text, done: true },
            )
            refreshConversations()
          } else {
            subscribe(res.intent_id)
            // Surface the new (or updated) thread in the history list.
            refreshConversations()
          }
        })
        .catch((e: Error) => {
          setError(e.message)
          toast.error(tRef.current('sendError'))
          pushAssistant(tRef.current('sendError'))
          setPhase('idle')
          setTask((prev) =>
            prev ? { ...prev, answer: tRef.current('sendError'), done: true, failed: true } : prev,
          )
        })
    },
    [pushAssistant, subscribe, refreshConversations],
  )

  // Start a fresh thread. History is durable server-side; this only
  // clears the live surface so the next message opens a new conversation.
  const reset = useCallback(() => {
    closeSub()
    cancelRetryTimer()
    convRef.current = null
    seenRef.current = new Set()
    setMessages([])
    setPhase('idle')
    setActiveIntentId(null)
    setConversationId(null)
    setPendingGate(null)
    setTask(null)
    setError(null)
    setResuming(false)
    setConnectionRetrying(false)
  }, [closeSub, cancelRetryTimer])

  // Collapse the active surface back to the idle pill, aborting the run.
  // Blocked while a money/approval gate is pending — the consent gate is the
  // one thing the user MUST act on, so it can't be swiped away.
  const dismissTask = useCallback(() => {
    if (pendingGate) return
    // Swipe-to-dismiss is an abort: cancel the in-flight run server-side
    // (best-effort) so it stops metering, not just close the client stream.
    if (activeIntentRef.current && phaseRef.current === 'working') {
      cancelRun(activeIntentRef.current, 'user_dismissed').catch(() => {})
    }
    closeSub()
    cancelRetryTimer()
    setTask(null)
    setPhase('idle')
    setActiveIntentId(null)
    setResuming(false)
    setConnectionRetrying(false)
  }, [pendingGate, closeSub, cancelRetryTimer])

  // Tear down the SSE stream if the surface unmounts mid-run.
  useEffect(
    () => () => {
      closeSub()
      cancelRetryTimer()
    },
    [closeSub, cancelRetryTimer],
  )

  return {
    messages,
    phase,
    activeIntentId,
    conversationId,
    error,
    pendingGate,
    resuming,
    connectionRetrying,
    task,
    dismissTask,
    conversations,
    loadingThread,
    send,
    reset,
    selectConversation,
    refreshConversations,
    answerGate,
    respondAsk,
  }
}
