import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import {
  applySurfaceEvent,
  emptyWorkspace,
  surfaceAddress,
  type SurfaceWorkspace,
  type PlacedSurface,
} from '@/lib/construct/workspace'
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
 * Property 7: Addressing re-enterability.
 *
 * Exercises the REAL `SurfaceFeed.hydrate` cold-open path, the REAL
 * `applySurfaceEvent` reducer, and the REAL `surfaceAddress` derivation (no
 * stubs/mocks of the code under test — `loadState`/`transport` are the feed's
 * own injectable seams). For an arbitrary valid frame sequence emitted in a
 * conversation `C`, after a cold rehydrate:
 *
 *   - Every surface the agent ever emitted in `C` (every DISTINCT id that
 *     received a full `construct.surface` frame — a base surface) is present in
 *     the rehydrated hot set, carries the stable address
 *     `construct://C/{id}`, and that address RESOLVES back to exactly that
 *     surface (R7.1, R7.4). The address is identical to the one a connected
 *     client builds live, i.e. stable across reload/rehydration (R7.1).
 *   - A pure orphan patch (a `construct.surface.patch` whose id never received a
 *     full surface) never instantiates a surface, so its id does NOT resolve —
 *     it was never "emitted" as a base surface (scoping the emitted set, R7.4).
 *   - An address is resolved ONLY against the conversation it names: an address
 *     for a different `conversationId`, or an id never emitted in `C`, does not
 *     resolve (R7.5).
 *
 * "Resolves" for this MVP task is the hot-set/rehydration resolution: the
 * address's `surfaceId` is present in the rehydrated `workspace.surfaces` and
 * the stored `PlacedSurface.address` equals the requested address, scoped to the
 * address's `conversationId` (the full cold rehydrate-on-open resolver is task
 * 11.3). `resolveAddress` below is that resolver — a pure lookup over the shared
 * model, NOT a fake of the code under test.
 *
 * Validates: Requirements 7.1, 7.4, 7.5
 */

const CONVERSATION_ID = 'conv-C'
const OTHER_CONVERSATION_ID = 'conv-OTHER'
// Ids that the generator never emits (outside the small surface-id pool), used
// to assert that an un-emitted id does not resolve.
const NEVER_EMITTED_IDS = ['z', 'never-emitted', ''] as const

// ---------------------------------------------------------------------------
// Resolve-by-address (MVP hot-set/rehydration resolution)
// ---------------------------------------------------------------------------

/**
 * resolveAddress resolves a `construct://{conversationId}/{surfaceId}` address
 * against a workspace's hot set, scoped strictly to the conversation named in
 * the address (R7.5). Returns the `PlacedSurface` whose stored address equals
 * the requested address, or `undefined` when the conversation does not match,
 * the id is absent, or the address is malformed. Pure read over the shared
 * model — the MVP "resolves after rehydration" path.
 */
function resolveAddress(ws: SurfaceWorkspace, address: string): PlacedSurface | undefined {
  const prefix = 'construct://'
  if (!address.startsWith(prefix)) return undefined
  const rest = address.slice(prefix.length)
  const slash = rest.indexOf('/')
  if (slash < 0) return undefined
  const conversationId = rest.slice(0, slash)
  const surfaceId = rest.slice(slash + 1)
  // R7.5: resolve only against the conversation the address names.
  if (conversationId !== ws.conversationId) return undefined
  const placed = ws.surfaces.get(surfaceId)
  if (!placed) return undefined
  // The stored stable address must match the one requested.
  if (placed.address !== address) return undefined
  return placed
}

// ---------------------------------------------------------------------------
// Arbitraries — well-formed Surface envelopes (exactly one payload per kind)
// ---------------------------------------------------------------------------

// A SMALL id pool makes patches land on existing bases AND arrive as orphans (a
// patch for an id before any full surface for it), so both the "emitted base"
// and "pure orphan patch" cases occur within one sequence.
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

/** Build a well-formed `Surface` payload of a given kind (exactly one field set). */
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

interface FrameSpec {
  type: string
  surface: Surface
}

const arbFrameSpec: fc.Arbitrary<FrameSpec> = fc.record({
  type: fc.constantFrom(CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH),
  surface: arbSurface,
})

interface SeqFrame {
  type: string
  surface: Surface
  seq: number
}

/**
 * An arbitrary VALID frame sequence emitted in `C`: frame specs assigned
 * strictly increasing frame `seq`s (1, 2, 3, …), the faithful oldest-first
 * record the durable store persists. The full/patch mix over a small id pool
 * naturally produces emitted base surfaces, folds, re-projections, and pure
 * orphan patches.
 */
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
function toStateResponse(frames: SeqFrame[]): StateResponse {
  const persisted: Frame[] = frames.map((f) => ({
    seq: f.seq,
    ts: '',
    phase: 'construct',
    type: f.type,
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
 * rehydrate(F): drive the REAL `SurfaceFeed.hydrate` with F as the durable
 * record and a silent live transport, then read the rehydrated workspace.
 */
async function rehydrate(frames: SeqFrame[]): Promise<SurfaceWorkspace> {
  const state = toStateResponse(frames)
  const feed = new SurfaceFeed(CONVERSATION_ID, {
    loadState: async () => state,
    transport: silentTransport(),
  })
  await feed.hydrate(CONVERSATION_ID)
  const ws = feed.workspace
  feed.close()
  return ws
}

/** The set of ids the agent emitted as base surfaces (received a full surface). */
function emittedBaseIds(frames: SeqFrame[]): Set<string> {
  const ids = new Set<string>()
  for (const f of frames) {
    if (f.type === CONSTRUCT_SURFACE) ids.add(f.surface.id)
  }
  return ids
}

/** Ids that received ONLY patches and never a full surface (pure orphans). */
function patchOnlyIds(frames: SeqFrame[]): Set<string> {
  const emitted = emittedBaseIds(frames)
  const patched = new Set<string>()
  for (const f of frames) {
    if (f.type === CONSTRUCT_SURFACE_PATCH && !emitted.has(f.surface.id)) {
      patched.add(f.surface.id)
    }
  }
  return patched
}

// ---------------------------------------------------------------------------
// Property
// ---------------------------------------------------------------------------

describe('Property 7: addressing re-enterability — every emitted surface\u2019s address resolves after rehydration', () => {
  it('every emitted base surface in C resolves by its stable construct://C/{id} address after rehydration (R7.1, R7.4)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const ws = await rehydrate(frames)
        const emitted = emittedBaseIds(frames)
        for (const id of emitted) {
          const expectedAddress = surfaceAddress(CONVERSATION_ID, id)
          // Present in the rehydrated hot set (R7.4).
          const placed = ws.surfaces.get(id)
          expect(placed).toBeDefined()
          // Carries the stable address construct://C/{id} (R7.1).
          expect(placed?.address).toBe(expectedAddress)
          // Resolving that address returns exactly this surface (R7.4).
          const resolved = resolveAddress(ws, expectedAddress)
          expect(resolved).toBe(placed)
          expect(resolved?.surface.id).toBe(id)
        }
      }),
      { numRuns: 300 },
    )
  })

  it('the rehydrated address equals the address a connected client builds live — stable across reload/rehydration (R7.1)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const live = liveApply(frames)
        const ws = await rehydrate(frames)
        for (const id of emittedBaseIds(frames)) {
          expect(ws.surfaces.get(id)?.address).toBe(live.surfaces.get(id)?.address)
          expect(ws.surfaces.get(id)?.address).toBe(surfaceAddress(CONVERSATION_ID, id))
        }
      }),
      { numRuns: 300 },
    )
  })

  it('a pure orphan patch never instantiates a surface, so its id does not resolve (R7.4 emitted-set scoping)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const ws = await rehydrate(frames)
        for (const id of patchOnlyIds(frames)) {
          expect(ws.surfaces.has(id)).toBe(false)
          expect(resolveAddress(ws, surfaceAddress(CONVERSATION_ID, id))).toBeUndefined()
        }
      }),
      { numRuns: 300 },
    )
  })

  it('an address is resolved only against the conversation it names — a foreign conversationId never resolves (R7.5)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const ws = await rehydrate(frames)
        for (const id of emittedBaseIds(frames)) {
          // Same id, WRONG conversation: must not resolve against C's surfaces.
          const foreign = surfaceAddress(OTHER_CONVERSATION_ID, id)
          expect(resolveAddress(ws, foreign)).toBeUndefined()
        }
      }),
      { numRuns: 300 },
    )
  })

  it('an id never emitted in C does not resolve (R7.4/R7.5)', async () => {
    await fc.assert(
      fc.asyncProperty(arbFrameSequence, async (frames) => {
        const ws = await rehydrate(frames)
        const emitted = emittedBaseIds(frames)
        for (const id of NEVER_EMITTED_IDS) {
          if (emitted.has(id)) continue // (the pool never produces these, but guard anyway)
          expect(resolveAddress(ws, surfaceAddress(CONVERSATION_ID, id))).toBeUndefined()
        }
      }),
      { numRuns: 300 },
    )
  })
})
