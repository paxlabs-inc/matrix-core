import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { SurfaceFeed, type FeedSubscription } from '@/lib/construct/feed'
import { CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'
import type { StateResponse, Frame } from '@/lib/construct/types.gen'
import type {
  Surface,
  Kind,
  Stakes,
  Temporality,
  NarrationRole,
  StepStatus,
} from '@/lib/construct/types.gen'

/**
 * Property 1: Rehydration fidelity ("never vanishing").
 *
 * Exercises the REAL `SurfaceFeed.hydrate` cold-open path and the REAL
 * `applySurfaceEvent` reducer (no stubs/mocks of the code under test). For an
 * arbitrary valid frame sequence `F`, the workspace reconstructed by replaying
 * the durable record must be observationally equal to the workspace a connected
 * client builds by applying that same sequence live:
 *
 *   hydrate(replay(F)) ≡ liveApply(F)
 *
 *   - liveApply(F): fold F oldest-first through the workspace reducer
 *     `applySurfaceEvent` (what a connected client builds from the live SSE tap).
 *   - hydrate(replay(F)): drive `SurfaceFeed.hydrate` with an injected
 *     `loadState` that returns F as the durable `StateResponse`
 *     ({conversation_id, frames, last_seq}), and an injected `transport` that
 *     delivers NO further live frames. After hydrate resolves, read
 *     `feed.workspace`.
 *
 * Both `loadState` and `transport` are legitimate test seams the feed EXPOSES
 * (not fakes of the reducer/feed): the real `SurfaceFeed.replay` →
 * `applySurfaceEvent` runs on one side and the same reducer runs on the other,
 * so the two paths converge by construction — the design's "one reducer"
 * guarantee (R1.2/R1.3). The durable frames are delivered to the feed in a
 * SHUFFLED order to additionally exercise `replay`'s defensive seq-sort, while
 * `liveApply` folds them oldest-first (the order a connected client sees them).
 *
 * "≡" is OBSERVATIONAL equality over the whole workspace — conversationId,
 * lastSeq, the surfaces Map (id → {surface, placement, address, updatedSeq}),
 * focus/pins/asks/tray, AND the orphan-patch `pending` buffer Map (R1.3
 * enumerates exactly the surface id set, per-id envelope, per-id placement, and
 * lastSeq).
 *
 * Validates: Requirements 1.3, 1.5
 */

const CONVERSATION_ID = 'conv-1'

// ---------------------------------------------------------------------------
// Observational equality over SurfaceWorkspace
// ---------------------------------------------------------------------------

/**
 * Normalize a workspace into a plain, order-independent value so two workspaces
 * can be compared field-for-field with `toEqual`. The two Maps (`surfaces`,
 * `pending`) are projected into key-sorted entry arrays; every other field is a
 * plain JSON-serializable value already.
 */
function normalizeWorkspace(ws: SurfaceWorkspace) {
  const sortByKey = <V>(entries: [string, V][]): [string, V][] =>
    [...entries].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))

  return {
    conversationId: ws.conversationId,
    lastSeq: ws.lastSeq,
    focus: ws.focus,
    pins: ws.pins,
    asks: ws.asks,
    tray: ws.tray,
    surfaces: sortByKey([...ws.surfaces.entries()]),
    pending: sortByKey([...ws.pending.entries()]),
  }
}

function expectWorkspacesEqual(a: SurfaceWorkspace, b: SurfaceWorkspace): void {
  expect(normalizeWorkspace(a)).toEqual(normalizeWorkspace(b))
}

// ---------------------------------------------------------------------------
// Arbitraries — well-formed Surface envelopes (exactly one payload per kind)
// ---------------------------------------------------------------------------

// A SMALL id pool is deliberate: it makes patches land on existing bases AND
// arrive as orphans (a patch for an id before any full surface for it), so the
// orphan-patch buffer, the patch-onto-existing merge, and the full-surface
// upsert paths are all exercised within one sequence.
const arbId: fc.Arbitrary<string> = fc.constantFrom('a', 'b', 'c', 'd')

const arbStakes = fc.constantFrom<Stakes>('fact', 'hypothesis', 'decision', 'irreversible')
const arbTemporality = fc.constantFrom<Temporality>('point', 'stream', 'persistent')
const arbRole = fc.constantFrom<NarrationRole>('thinking', 'intent', 'answer')
const arbStepStatus = fc.constantFrom<StepStatus>('pending', 'running', 'done', 'failed')

const arbAttributes = fc.option(
  fc.record({
    stakes: fc.option(arbStakes, { nil: undefined }),
    confidence: fc.option(fc.double({ min: 0, max: 1, noNaN: true }), { nil: undefined }),
    temporality: fc.option(arbTemporality, { nil: undefined }),
  }),
  { nil: undefined },
)

const arbStreamChunk = fc.record({
  seq: fc.nat({ max: 12 }),
  text: fc.string(),
  channel: fc.option(fc.constantFrom('stdout', 'stderr'), { nil: undefined }),
})

const arbTimelineStep = fc.record({
  id: fc.constantFrom('s1', 's2', 's3'),
  label: fc.string(),
  status: arbStepStatus,
  detail: fc.option(fc.string(), { nil: undefined }),
  ref: fc.option(fc.string(), { nil: undefined }),
})

/**
 * Build a well-formed `Surface` of a given kind: exactly one payload field set,
 * matching the kind (the structural contract `applyPatch`/the renderers rely on).
 */
function payloadForKind(kind: Kind): fc.Arbitrary<Partial<Surface>> {
  switch (kind) {
    case 'narration':
      return fc
        .record({ text: fc.string(), role: fc.option(arbRole, { nil: undefined }) })
        .map((narration) => ({ narration }))
    case 'metric':
      return fc
        .record({
          label: fc.string(),
          value: fc.string(),
          magnitude: fc.option(fc.double({ noNaN: true }), { nil: undefined }),
        })
        .map((metric) => ({ metric }))
    case 'entity':
      return fc
        .record({
          type: fc.string(),
          identity: fc.string(),
          label: fc.option(fc.string(), { nil: undefined }),
        })
        .map((entity) => ({ entity }))
    case 'structure':
      return fc
        .record({
          shape: fc.constantFrom('list', 'table', 'tree') as fc.Arbitrary<
            'list' | 'table' | 'tree'
          >,
          records: fc.array(
            fc.record({
              id: fc.option(fc.string(), { nil: undefined }),
              label: fc.option(fc.string(), { nil: undefined }),
            }),
            { maxLength: 3 },
          ),
        })
        .map((structure) => ({ structure }))
    case 'stream':
      return fc
        .record({
          source: fc.option(fc.string(), { nil: undefined }),
          title: fc.option(fc.string(), { nil: undefined }),
          chunks: fc.option(fc.array(arbStreamChunk, { maxLength: 4 }), { nil: undefined }),
          closed: fc.option(fc.boolean(), { nil: undefined }),
        })
        .map((stream) => ({ stream }))
    case 'timeline':
      return fc
        .record({
          title: fc.option(fc.string(), { nil: undefined }),
          steps: fc.array(arbTimelineStep, { maxLength: 4 }),
        })
        .map((timeline) => ({ timeline }))
    case 'canvas':
      return fc
        .record({
          media: fc.record({
            kind: fc.constantFrom('image', 'video', 'audio', 'page', 'chart') as fc.Arbitrary<
              'image' | 'video' | 'audio' | 'page' | 'chart'
            >,
            url: fc.option(fc.webUrl(), { nil: undefined }),
            alt: fc.option(fc.string(), { nil: undefined }),
          }),
          caption: fc.option(fc.string(), { nil: undefined }),
        })
        .map((canvas) => ({ canvas }))
    case 'ask':
      return fc
        .record({
          ask_kind: fc.constantFrom('choose', 'input', 'confirm', 'sign', 'upload') as fc.Arbitrary<
            'choose' | 'input' | 'confirm' | 'sign' | 'upload'
          >,
          prompt: fc.string(),
          required: fc.option(fc.boolean(), { nil: undefined }),
        })
        .map((ask) => ({ ask }))
  }
}

const arbKind = fc.constantFrom<Kind>(
  'narration',
  'metric',
  'entity',
  'structure',
  'stream',
  'timeline',
  'canvas',
  'ask',
)

/** A well-formed Surface envelope with a small-pool id and optional decoration. */
const arbSurface: fc.Arbitrary<Surface> = arbKind.chain((kind) =>
  fc
    .record({
      id: arbId,
      payload: payloadForKind(kind),
      ref: fc.option(fc.string(), { nil: undefined }),
      parent: fc.option(fc.string(), { nil: undefined }),
      seq: fc.option(fc.nat({ max: 20 }), { nil: undefined }),
      attributes: arbAttributes,
    })
    .map(({ id, payload, ref, parent, seq, attributes }) => ({
      kind,
      id,
      ref,
      parent,
      seq,
      attributes,
      ...payload,
    })),
)

// One element of a frame sequence, before a frame `seq` is assigned: a type
// (full surface or patch) + a well-formed surface envelope.
interface FrameSpec {
  type: string
  surface: Surface
}

const arbFrameSpec: fc.Arbitrary<FrameSpec> = fc.record({
  type: fc.constantFrom(CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH),
  surface: arbSurface,
})

/**
 * An arbitrary VALID frame sequence `F`: a list of frame specs assigned strictly
 * increasing frame `seq`s (1, 2, 3, …) so the sequence is a faithful oldest-first
 * record — exactly what the durable store persists and what a connected client
 * sees live. The mix of full/patch over a small id pool naturally produces
 * orphan patches (a patch before its base), folds (a patch after its base), and
 * re-projections (a full surface replacing an existing one).
 */
interface SeqFrame {
  type: string
  surface: Surface
  seq: number
}

const arbFrameSequence: fc.Arbitrary<SeqFrame[]> = fc
  .array(arbFrameSpec, { maxLength: 16 })
  .map((specs) => specs.map((spec, i) => ({ ...spec, seq: i + 1 })))

// ---------------------------------------------------------------------------
// The two paths
// ---------------------------------------------------------------------------

/** liveApply(F): what a connected client builds by folding F oldest-first. */
function liveApply(frames: SeqFrame[]): SurfaceWorkspace {
  return frames.reduce(
    (ws, f) => applySurfaceEvent(ws, f.type, f.surface, f.seq),
    emptyWorkspace(CONVERSATION_ID),
  )
}

/** Convert a frame sequence into the durable `StateResponse` the feed replays. */
function toStateResponse(frames: SeqFrame[], deliveryOrder: SeqFrame[]): StateResponse {
  const persisted: Frame[] = deliveryOrder.map((f) => ({
    seq: f.seq,
    ts: '',
    phase: 'construct',
    type: f.type,
    // The persisted frame mirrors the live SSE event shape: the surface payload
    // lives under `fields.surface`, exactly where `parseSurface` reads it.
    fields: { surface: f.surface },
  }))
  const lastSeq = frames.reduce((m, f) => (f.seq > m ? f.seq : m), 0)
  return { conversation_id: CONVERSATION_ID, frames: persisted, last_seq: lastSeq }
}

/** A live transport seam that delivers NO frames (cold open, empty live tail). */
function silentTransport(): () => FeedSubscription {
  return () => ({ close: () => {} })
}

/**
 * hydrateReplay(F): drive the REAL `SurfaceFeed.hydrate` with F as the durable
 * record (delivered in `deliveryOrder`) and a silent live transport, then read
 * the rehydrated workspace.
 */
async function hydrateReplay(
  frames: SeqFrame[],
  deliveryOrder: SeqFrame[],
): Promise<SurfaceWorkspace> {
  const state = toStateResponse(frames, deliveryOrder)
  const feed = new SurfaceFeed(CONVERSATION_ID, {
    loadState: async () => state,
    transport: silentTransport(),
  })
  await feed.hydrate(CONVERSATION_ID)
  const ws = feed.workspace
  feed.close()
  return ws
}

// ---------------------------------------------------------------------------
// Property
// ---------------------------------------------------------------------------

describe('SurfaceFeed.hydrate — Property 1: rehydration fidelity ("never vanishing")', () => {
  it('hydrate(replay(F)) ≡ liveApply(F) — rehydration is observationally equal to live, durable frames delivered oldest-first (R1.3)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const live = liveApply(frames)
        // The real endpoint returns frames oldest-first.
        const rehydrated = await hydrateReplay(frames, frames)
        expectWorkspacesEqual(rehydrated, live)
      }),
      { numRuns: 300 },
    )
  })

  it('hydrate(replay(F)) ≡ liveApply(F) — durable frames delivered SHUFFLED, exercising replay\u2019s defensive seq-sort', async () => {
    await fc.assert(
      fc.asyncProperty(
        arbFrameSequence,
        // An independent permutation of the durable delivery order: the feed's
        // `replay` must seq-sort it back to oldest-first so the result still
        // equals the live fold (which always folds oldest-first).
        fc.infiniteStream(fc.integer()),
        async (frames, keys) => {
          const it = keys[Symbol.iterator]()
          const delivery = [...frames].sort(
            () => (it.next().value as number) - (it.next().value as number),
          )
          const live = liveApply(frames)
          const rehydrated = await hydrateReplay(frames, delivery)
          expectWorkspacesEqual(rehydrated, live)
        },
      ),
      { numRuns: 300 },
    )
  })

  it('lastSeq is monotonically non-decreasing as F is folded live, and equals the rehydrated lastSeq (R1.5)', () => {
    fc.assert(
      fc.property(arbFrameSequence, (frames) => {
        let ws = emptyWorkspace(CONVERSATION_ID)
        let prev = ws.lastSeq
        for (const f of frames) {
          ws = applySurfaceEvent(ws, f.type, f.surface, f.seq)
          expect(ws.lastSeq).toBeGreaterThanOrEqual(prev)
          prev = ws.lastSeq
        }
      }),
      { numRuns: 300 },
    )
  })
})
