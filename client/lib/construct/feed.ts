/**
 * Surface_Feed — the client consumer that drives the shared `SurfaceWorkspace`
 * from BOTH durable rehydration frames and live SSE frames through ONE reducer
 * (`applySurfaceEvent`), so a reopened "computer" is observationally equal to
 * the live one it left ("never vanishing").
 *
 * This EXTENDS the existing live feed (the construct.surface[.patch] handling
 * already wired through `useChat` + `store.ts`); it does NOT add a new
 * agent→client wire path (R14). Live frames keep riding the existing chat SSE
 * transport (`lib/api/events.ts`); the ONLY read path added is the read-only
 * `GET /construct/state` backfill (`lib/api/construct.ts`).
 *
 * The cold-open contract (design Component 3, R1.2/R1.3, R8.2/R8.3):
 *
 *   hydrate(conversationId):
 *     1. GET /construct/state → the conversation's durable frames (oldest-first).
 *     2. Replay each frame through the SAME `applySurfaceEvent` reducer the live
 *        feed uses, rebuilding the workspace.
 *     3. Capture `last_seq` and subscribe the live stream resuming
 *        `since_seq = lastSeq`, folding every catch-up/live frame via
 *        `applyEvent` (the reducer dedups by `seq`, so a frame already replayed
 *        is a no-op and frames apply oldest-first).
 *     4. Degraded path: if the read path fails, open an EMPTY workspace and
 *        subscribe the live stream anyway — rehydration is best-effort backfill,
 *        never a hard dependency, so the environment still comes up live.
 *
 *   applyEvent(event): parse one live construct.surface[.patch] SSE frame and
 *     fold it; non-construct frames are ignored.
 *
 * Reactivity: the shell wiring (task 8.1) passes an `onChange` callback that is
 * invoked with the new workspace whenever a frame is folded, so an adapter can
 * mirror it into its own reactive state (React `useState`, a store, etc.). The
 * feed itself stays framework-agnostic and testable.
 *
 * This module uses the client SSE transport + `apiFetch`, so it is
 * client-specific; the shared core it builds on (`workspace.ts`) is the faithful
 * copy mirrored into `apps/mobile`. The mobile feed is deferred to the mobile
 * adapter task (it wires the same reducer to the mobile transport).
 */
import type { SSEEvent } from '@/lib/api/types'
import type { StateResponse } from '@/lib/construct/types.gen'
import { fetchConstructState } from '@/lib/api/construct'
import { subscribeEvents } from '@/lib/api/events'
import { parseSurface, CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'

/** The phase every construct surface frame is published under. */
const CONSTRUCT_PHASE = 'construct'

/** A handle to a live subscription; closing it tears the stream down. */
export interface FeedSubscription {
  close: () => void
}

/**
 * Connection status surfaced to a caller that wants to render a non-jargon
 * "connecting / live / reconnecting" indicator. Internal transport mechanics
 * (SSE, replay, gaps) never leak past this typed status (R12).
 */
export type FeedStatus = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'closed' | 'error'

/**
 * A live transport: given a resume cursor and a frame sink, open the stream and
 * return a close handle. Injectable so the shell wiring can bind the exact chat
 * transport it uses (intent-scoped vs conversation firehose) and tests can feed
 * frames deterministically — the feed adds NO transport of its own (R14).
 */
export type SubscribeLive = (opts: {
  /** Resume the live stream from this seq (frames with seq > sinceSeq). */
  sinceSeq: number
  /** Called for every raw SSE frame; the feed filters to construct frames. */
  onEvent: (event: SSEEvent) => void
  /** Optional lifecycle hook for a non-jargon connection indicator. */
  onStatus?: (status: FeedStatus) => void
}) => FeedSubscription

export interface SurfaceFeedOptions {
  /**
   * Invoked with the new workspace each time a frame is folded, so an adapter
   * can mirror it into reactive state. The feed stays framework-agnostic.
   */
  onChange?: (workspace: SurfaceWorkspace) => void
  /** Optional connection-status sink for a non-jargon indicator. */
  onStatus?: (status: FeedStatus) => void
  /**
   * The durable read path. Defaults to `fetchConstructState`
   * (`GET /construct/state`). Overridable for tests.
   */
  loadState?: (conversationId: string, sinceSeq: number) => Promise<StateResponse>
  /**
   * The live transport. Defaults to the existing chat SSE transport filtered to
   * construct frames. Overridable so the shell wiring binds its own scope and
   * tests inject frames directly.
   */
  transport?: SubscribeLive
  /**
   * Optional run scope for the DEFAULT transport: when set, the default
   * transport tails `/events?intent_id=…` (the run's chat stream); otherwise it
   * tails the conversation firehose filtered to construct frames. Ignored when a
   * custom `transport` is supplied.
   */
  intentId?: string
}

/**
 * The default live transport: a thin wrapper over the existing chat SSE
 * transport (`subscribeEvents`), filtered to the `construct` phase and resumed
 * from a seq cursor. This is the SAME pipe the live chat feed already uses — no
 * new agent→client wire path is introduced (R14).
 */
function chatTransport(intentId?: string): SubscribeLive {
  return ({ sinceSeq, onEvent, onStatus }) => {
    const handle = subscribeEvents({
      intentId,
      phase: CONSTRUCT_PHASE,
      sinceSeq: sinceSeq > 0 ? sinceSeq : undefined,
      onUpdate: (u) => {
        switch (u.kind) {
          case 'event':
            onEvent(u.event)
            break
          case 'open':
            onStatus?.('live')
            break
          case 'reconnecting':
            onStatus?.('reconnecting')
            break
          case 'error':
            onStatus?.('error')
            break
          case 'closed':
            onStatus?.('closed')
            break
          default:
            // 'gap' — a deeper store-backed catch-up (R8.4) is a later task; the
            // reducer's seq dedup keeps the live tail correct in the meantime.
            break
        }
      },
    })
    return { close: handle.close }
  }
}

/**
 * SurfaceFeed implements the design's Component 3 interface: it owns the shared
 * `SurfaceWorkspace`, rehydrates it from the durable read path, then folds live
 * frames through the same reducer.
 */
export class SurfaceFeed {
  private ws: SurfaceWorkspace
  private readonly opts: SurfaceFeedOptions
  private sub: FeedSubscription | null = null
  /**
   * The run scope the DEFAULT live transport currently tails. Initialized from
   * `opts.intentId` and re-bindable via `setLiveScope` as the active run changes
   * within a conversation, so the live tail follows Neo's current run WITHOUT a
   * full re-hydrate. Ignored when a custom `transport` is supplied.
   */
  private liveIntentId?: string

  constructor(conversationId: string, opts: SurfaceFeedOptions = {}) {
    this.ws = emptyWorkspace(conversationId)
    this.opts = opts
    this.liveIntentId = opts.intentId
  }

  /** The reactive shared model the shells read. */
  get workspace(): SurfaceWorkspace {
    return this.ws
  }

  /**
   * hydrate replays the conversation's durable frames through the live reducer,
   * then subscribes the live stream resuming from the newest seq. On a read-path
   * failure it opens an empty workspace and subscribes live anyway (degraded
   * backfill; the environment still comes up).
   */
  async hydrate(conversationId: string): Promise<void> {
    // A re-hydrate replaces any prior stream + workspace for this conversation.
    this.closeSubscription()
    this.ws = emptyWorkspace(conversationId)
    this.opts.onStatus?.('connecting')

    const load = this.opts.loadState ?? fetchConstructState
    // The live cursor: the newest seq we have backfilled. The durable read
    // path's `last_seq` is authoritative (it is the newest seq across the full
    // set even when the catch-up window is empty); fall back to the reducer's
    // own `lastSeq` so the cursor is correct even if the response omits it.
    let cursor = 0

    try {
      const state = await load(conversationId, 0)
      this.replay(state)
      cursor = Math.max(this.ws.lastSeq, state.last_seq ?? 0)
      this.notify()
    } catch {
      // Degraded: the durable backfill is best-effort. Keep the empty workspace
      // and still subscribe live so the environment comes up (R1.3-ish). No
      // jargon or error code leaks; the caller's status sink can show
      // "connecting" → "live" plainly.
      this.ws = emptyWorkspace(conversationId)
      cursor = 0
    }

    this.subscribeLive(cursor)
  }

  /**
   * applyEvent folds one live SSE frame into the workspace. Non-construct frames
   * are ignored; a malformed/forged payload is dropped by `parseSurface`. The
   * reducer dedups by `seq`, so a frame already applied (replay/reconnect) is a
   * no-op and frames are only applied oldest-first when `seq` advances (R8.3).
   */
  applyEvent(event: SSEEvent): void {
    if (event.type !== CONSTRUCT_SURFACE && event.type !== CONSTRUCT_SURFACE_PATCH) return
    const surface = parseSurface(event.fields)
    if (!surface) return
    const next = applySurfaceEvent(this.ws, event.type, surface, event.seq)
    if (next === this.ws) return // dedup/no-op: nothing changed
    this.ws = next
    this.notify()
  }

  /**
   * Re-bind the DEFAULT live transport to a new run scope (the active
   * `intentId`) WITHOUT re-reading the durable record, so the already-hydrated
   * workspace — and any open descent on it — is preserved as runs come and go
   * within one conversation. The live tail re-resumes from the workspace's
   * current `lastSeq`, so the reducer's seq dedup keeps the stream correct
   * across the re-subscribe. Riding the SAME chat SSE transport, this adds no
   * new wire path (R14). A no-op when the scope is unchanged, when a custom
   * `transport` is supplied, or before the live stream is up (the new scope is
   * then simply picked up when `hydrate` subscribes).
   */
  setLiveScope(intentId?: string): void {
    if (intentId === this.liveIntentId) return
    this.liveIntentId = intentId
    if (this.opts.transport) return // a custom transport owns its own scope
    if (this.sub) {
      this.closeSubscription()
      this.subscribeLive(this.ws.lastSeq)
    }
  }

  /**
   * Re-read the durable record and fold any frames it carries into the LIVE
   * workspace through the same reducer, returning the resulting workspace. The
   * reducer dedups by `seq`, so frames already applied are no-ops and only a
   * surface the live tail has not yet seen (e.g. a cold, older surface a descent
   * is re-entering by address, R4.4/R7.3) is added. Best-effort: a failed
   * re-read leaves the live workspace untouched. This reuses the ONE durable
   * read path (`GET /construct/state`); it adds no new wire path (R14).
   */
  async refresh(): Promise<SurfaceWorkspace> {
    const load = this.opts.loadState ?? fetchConstructState
    try {
      const state = await load(this.ws.conversationId, 0)
      this.replay(state)
      this.notify()
    } catch {
      // Best-effort backfill: keep the live workspace as-is on a read failure.
    }
    return this.ws
  }

  /** Tear down the live subscription. Idempotent; safe to call on unmount. */
  close(): void {
    this.closeSubscription()
    this.opts.onStatus?.('closed')
  }

  /**
   * replay folds every durable frame through the SAME reducer the live feed
   * uses, oldest-first. Each persisted `Frame` mirrors the live SSE event shape
   * ({seq,ts,phase,type,fields}), so a frame becomes the reducer's
   * `(type, surface, seq)` exactly as a live event does: the surface payload
   * lives under `fields.surface` and is parsed/validated by `parseSurface`.
   */
  private replay(state: StateResponse): void {
    const frames = state.frames ?? []
    // The read path returns frames oldest-first; sort defensively by seq so an
    // out-of-order response never scrambles the replay (the reducer also guards).
    const ordered = [...frames].sort((a, b) => a.seq - b.seq)
    for (const frame of ordered) {
      if (frame.type !== CONSTRUCT_SURFACE && frame.type !== CONSTRUCT_SURFACE_PATCH) continue
      const surface = parseSurface(frame.fields)
      if (!surface) continue
      this.ws = applySurfaceEvent(this.ws, frame.type, surface, frame.seq)
    }
  }

  /** Open the live stream resuming from `cursor`, routing frames to applyEvent. */
  private subscribeLive(cursor: number): void {
    const transport = this.opts.transport ?? chatTransport(this.liveIntentId)
    this.sub = transport({
      sinceSeq: cursor,
      onEvent: (event) => this.applyEvent(event),
      onStatus: (status) => this.opts.onStatus?.(status),
    })
  }

  private closeSubscription(): void {
    this.sub?.close()
    this.sub = null
  }

  private notify(): void {
    this.opts.onChange?.(this.ws)
  }
}
