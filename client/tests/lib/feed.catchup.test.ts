import { describe, it, expect } from 'vitest'
import { SurfaceFeed, type SubscribeLive, type FeedSubscription } from '@/lib/construct/feed'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'
import type { SSEEvent } from '@/lib/api/types'
import type { Surface, Frame, StateResponse } from '@/lib/construct/types.gen'

/**
 * Task 5.3 — disconnect catch-up (R8.2, R8.3).
 *
 * Scenario: a user disconnects with the conversation persisted up to seq=K. A
 * long-running job kept running WHILE they were away and emitted more frames
 * (seq > K). On reconnect the feed must:
 *
 *   1. rehydrate the durable state (frames ≤ K), and
 *   2. resume the live stream from `since_seq = K` (R8.2), then
 *   3. apply the catch-up frames (seq > K) so the surfaces the job produced
 *      while disconnected are present after rehydration (R8.3), while
 *   4. NOT altering the model for any catch-up frame whose seq ≤ K (already
 *      applied — dedup by seq, R8.3).
 *
 * The REAL `SurfaceFeed` + the REAL `applySurfaceEvent` reducer run here; only
 * the feed's injectable `loadState` (durable read path) and `transport` (live
 * stream) seams are bound to deterministic test sources — these are the seams
 * the design exposes for exactly this, NOT fakes of the logic under test.
 */

const CONV = 'conv-catchup'

// --- frame/event builders (mirror the persisted Frame ↔ live SSEEvent shapes) ---

/** A persisted durable Frame: its `fields.surface` carries the envelope. */
function durableFrame(seq: number, type: string, surface: Surface): Frame {
  return { seq, ts: `t${seq}`, phase: 'construct', type, fields: { surface } }
}

/** A live SSE event: same {seq,ts,phase,type,fields} shape the live feed folds. */
function liveEvent(seq: number, type: string, surface: Surface): SSEEvent {
  return { seq, ts: `t${seq}`, phase: 'construct', type, fields: { surface } }
}

// --- surfaces describing a long-running "job" the user left behind ---

const home: Surface = {
  kind: 'entity',
  id: 'home',
  entity: { type: 'workspace', identity: 'home', label: 'Home' },
}

// The job, captured mid-flight at disconnect: step s1 still running.
const jobRunning: Surface = {
  kind: 'timeline',
  id: 'job',
  timeline: { title: 'Build report', steps: [{ id: 's1', label: 'gather', status: 'running' }] },
}

const jobLog: Surface = {
  kind: 'stream',
  id: 'job-log',
  stream: { source: 'job', chunks: [{ seq: 0, text: 'starting\n' }], closed: false },
}

// While disconnected, the job advanced: s1 finished + a new step s2 ran.
const jobAdvancedPatch: Surface = {
  kind: 'timeline',
  id: 'job',
  timeline: {
    steps: [
      { id: 's1', label: 'gather', status: 'done' },
      { id: 's2', label: 'write', status: 'done' },
    ],
  },
}

// ...and the job produced a deliverable the user has never seen.
const deliverable: Surface = {
  kind: 'entity',
  id: 'deliverable',
  entity: { type: 'file', identity: 'report.pdf', label: 'report.pdf' },
}

// Durable frames the client had at disconnect: seq 1..K (K = 3).
const K = 3
const durableFrames: Frame[] = [
  durableFrame(1, CONSTRUCT_SURFACE, home),
  durableFrame(2, CONSTRUCT_SURFACE, jobRunning),
  durableFrame(3, CONSTRUCT_SURFACE, jobLog),
]

// Catch-up frames emitted by the still-running job WHILE the user was away.
const catchUp: SSEEvent[] = [
  liveEvent(4, CONSTRUCT_SURFACE_PATCH, jobAdvancedPatch),
  liveEvent(5, CONSTRUCT_SURFACE, deliverable),
]

// --- observational workspace equality (Map-aware, order-independent) ---

function normalize(ws: SurfaceWorkspace) {
  const byKey = <V>(e: [string, V][]) => [...e].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  return {
    conversationId: ws.conversationId,
    lastSeq: ws.lastSeq,
    focus: ws.focus,
    pins: ws.pins,
    asks: ws.asks,
    tray: ws.tray,
    surfaces: byKey([...ws.surfaces.entries()]),
    pending: byKey([...ws.pending.entries()]),
  }
}

/** The reference model: the FULL durable+catch-up sequence folded live (R8.5). */
function liveApply(frames: { seq: number; type: string; surface: Surface }[]): SurfaceWorkspace {
  return frames.reduce(
    (ws, f) => applySurfaceEvent(ws, f.type, f.surface, f.seq),
    emptyWorkspace(CONV),
  )
}

/** Wire a feed to deterministic seams, capturing the live resume cursor + sink. */
function makeFeed(state: StateResponse) {
  let capturedSinceSeq = -1
  let deliver: ((e: SSEEvent) => void) | undefined
  let closed = false

  const transport: SubscribeLive = ({ sinceSeq, onEvent }): FeedSubscription => {
    capturedSinceSeq = sinceSeq
    deliver = onEvent
    return {
      close: () => {
        closed = true
      },
    }
  }

  const feed = new SurfaceFeed(CONV, {
    loadState: async () => state,
    transport,
  })

  return {
    feed,
    get sinceSeq() {
      return capturedSinceSeq
    },
    get closed() {
      return closed
    },
    emit(event: SSEEvent) {
      if (!deliver) throw new Error('live stream not subscribed yet')
      deliver(event)
    },
  }
}

describe('SurfaceFeed disconnect catch-up (R8.2, R8.3)', () => {
  it('resumes the live stream from since_seq = lastSeq (K) after rehydration (R8.2)', async () => {
    const state: StateResponse = { conversation_id: CONV, frames: durableFrames, last_seq: K }
    const h = makeFeed(state)

    await h.feed.hydrate(CONV)

    // The durable replay set lastSeq to K; the live subscription MUST resume there.
    expect(h.feed.workspace.lastSeq).toBe(K)
    expect(h.sinceSeq).toBe(K)
  })

  it('applies catch-up frames (seq > K) so work done while disconnected is present (R8.3)', async () => {
    const state: StateResponse = { conversation_id: CONV, frames: durableFrames, last_seq: K }
    const h = makeFeed(state)
    await h.feed.hydrate(CONV)

    // Before catch-up: the job is still mid-flight and the deliverable is unseen.
    const job0 = h.feed.workspace.surfaces.get('job')!
    expect(job0.surface.timeline?.steps.map((s) => s.status)).toEqual(['running'])
    expect(h.feed.workspace.surfaces.has('deliverable')).toBe(false)

    // The still-running job's frames arrive on the resumed live stream.
    for (const e of catchUp) h.emit(e)

    const ws = h.feed.workspace
    // The deliverable the job produced while away is now present and re-enterable.
    const placed = ws.surfaces.get('deliverable')
    expect(placed).toBeDefined()
    expect(placed!.surface.entity?.identity).toBe('report.pdf')
    expect(placed!.address).toBe(`construct://${CONV}/deliverable`)

    // The job advanced to done with the step that ran while disconnected.
    const job = ws.surfaces.get('job')!
    expect(job.surface.timeline?.steps.map((s) => [s.id, s.status])).toEqual([
      ['s1', 'done'],
      ['s2', 'done'],
    ])

    expect(ws.lastSeq).toBe(5)

    // Convergence (R8.5): catch-up result equals folding the full sequence live.
    const reference = liveApply([
      { seq: 1, type: CONSTRUCT_SURFACE, surface: home },
      { seq: 2, type: CONSTRUCT_SURFACE, surface: jobRunning },
      { seq: 3, type: CONSTRUCT_SURFACE, surface: jobLog },
      { seq: 4, type: CONSTRUCT_SURFACE_PATCH, surface: jobAdvancedPatch },
      { seq: 5, type: CONSTRUCT_SURFACE, surface: deliverable },
    ])
    expect(normalize(ws)).toEqual(normalize(reference))
  })

  it('does NOT alter the model for a live frame whose seq ≤ K (already applied — dedup, R8.3)', async () => {
    const state: StateResponse = { conversation_id: CONV, frames: durableFrames, last_seq: K }
    const h = makeFeed(state)
    await h.feed.hydrate(CONV)

    const before = normalize(h.feed.workspace)

    // A live re-delivery of an already-applied full surface (seq 2 ≤ K) ...
    h.emit(liveEvent(2, CONSTRUCT_SURFACE, jobRunning))
    // ... and a stale patch for the job at seq 1 (≤ K) that, if applied, would
    // wrongly mutate the timeline. Both must be no-ops.
    h.emit(liveEvent(1, CONSTRUCT_SURFACE_PATCH, jobAdvancedPatch))

    expect(normalize(h.feed.workspace)).toEqual(before)
    expect(h.feed.workspace.lastSeq).toBe(K)
  })

  it('applies catch-up oldest-first and still converges when frames arrive interleaved with stale ones', async () => {
    const state: StateResponse = { conversation_id: CONV, frames: durableFrames, last_seq: K }
    const h = makeFeed(state)
    await h.feed.hydrate(CONV)

    // Interleave a stale frame (seq 3 ≤ K) between the two catch-up frames; it
    // must not perturb the converged result.
    h.emit(catchUp[0]) // seq 4
    h.emit(liveEvent(3, CONSTRUCT_SURFACE, jobLog)) // stale, no-op
    h.emit(catchUp[1]) // seq 5

    const reference = liveApply([
      { seq: 1, type: CONSTRUCT_SURFACE, surface: home },
      { seq: 2, type: CONSTRUCT_SURFACE, surface: jobRunning },
      { seq: 3, type: CONSTRUCT_SURFACE, surface: jobLog },
      { seq: 4, type: CONSTRUCT_SURFACE_PATCH, surface: jobAdvancedPatch },
      { seq: 5, type: CONSTRUCT_SURFACE, surface: deliverable },
    ])
    expect(normalize(h.feed.workspace)).toEqual(normalize(reference))
  })
})
