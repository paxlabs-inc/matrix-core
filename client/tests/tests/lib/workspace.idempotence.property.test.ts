import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'
import type {
  Surface,
  Kind,
  Stakes,
  Temporality,
  NarrationRole,
  StepStatus,
} from '@/lib/construct/types.gen'

/**
 * Property 2: Reducer idempotence on replay.
 *
 * Exercises the REAL `applySurfaceEvent` reducer (no stubs/mocks) against
 * arbitrary `SurfaceWorkspace`s and arbitrary single frames `f = (type, surface,
 * seq)`. The reducer dedups by `seq` (full-surface upsert, patch merge, and the
 * orphan-patch buffer all guard on the applied/buffered `seq`), so applying the
 * same frame twice must equal applying it once:
 *
 *   apply(apply(ws, f), f) ≡ apply(ws, f)
 *
 * "≡" here is OBSERVATIONAL equality over the whole workspace — conversationId,
 * lastSeq, the surfaces Map (id → {surface, placement, address, updatedSeq}),
 * focus/pins/asks/tray, AND the orphan-patch `pending` buffer Map.
 *
 * Validates: Requirements 1.4
 */

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

// A SMALL id pool is deliberate: it makes the single frame `f` collide with
// surfaces already present in `ws` (exercising the patch-onto-existing and
// full-surface-replace dedup paths) AND miss them (exercising the orphan-patch
// buffering path), instead of every id being unique.
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

// A frame is (type, surface, seq): both full-surface and patch types, with frame
// seqs drawn from a small range so they straddle surfaces' applied seqs (forcing
// the dedup `seq <= updatedSeq` and buffer-dedup branches to actually fire).
interface Frame {
  type: string
  surface: Surface
  seq: number
}

const arbFrame: fc.Arbitrary<Frame> = fc.record({
  type: fc.constantFrom(CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH),
  surface: arbSurface,
  seq: fc.integer({ min: 1, max: 20 }),
})

/**
 * An arbitrary starting workspace: fold a random sequence of frames through the
 * REAL reducer from an empty workspace. This produces realistic workspaces with
 * placed surfaces, advanced `lastSeq`, and (when patches arrive before their
 * base) a populated orphan-patch `pending` buffer.
 */
const arbWorkspace: fc.Arbitrary<SurfaceWorkspace> = fc
  .array(arbFrame, { maxLength: 14 })
  .map((frames) =>
    frames.reduce(
      (ws, f) => applySurfaceEvent(ws, f.type, f.surface, f.seq),
      emptyWorkspace('conv-1'),
    ),
  )

// ---------------------------------------------------------------------------
// Property
// ---------------------------------------------------------------------------

describe('applySurfaceEvent — Property 2: reducer idempotence on replay', () => {
  it('apply(apply(ws,f),f) ≡ apply(ws,f) — dedup by seq across full-surface, patch, and orphan-buffer paths (R1.4)', () => {
    fc.assert(
      fc.property(arbWorkspace, arbFrame, (ws, f) => {
        const once = applySurfaceEvent(ws, f.type, f.surface, f.seq)
        const twice = applySurfaceEvent(once, f.type, f.surface, f.seq)
        expectWorkspacesEqual(twice, once)
      }),
      { numRuns: 500 },
    )
  })

  it('idempotence holds specifically when the frame targets an id already present (patch/full replace dedup)', () => {
    // Seed an id, then replay a frame for that SAME id twice on top.
    const seeded = applySurfaceEvent(
      emptyWorkspace('conv-1'),
      CONSTRUCT_SURFACE,
      {
        kind: 'timeline',
        id: 'a',
        timeline: { steps: [{ id: 's1', label: 'x', status: 'running' }] },
      },
      1,
    )
    fc.assert(
      fc.property(
        arbFrame.filter((f) => f.surface.id === 'a'),
        (f) => {
          const once = applySurfaceEvent(seeded, f.type, f.surface, f.seq)
          const twice = applySurfaceEvent(once, f.type, f.surface, f.seq)
          expectWorkspacesEqual(twice, once)
        },
      ),
      { numRuns: 300 },
    )
  })

  it('idempotence holds specifically for orphan patches (base absent → buffered, replay deduped by seq)', () => {
    fc.assert(
      fc.property(arbSurface, fc.integer({ min: 1, max: 20 }), (surface, seq) => {
        // Empty workspace → a patch for an unseen id is buffered, not seeded.
        const ws = emptyWorkspace('conv-1')
        const once = applySurfaceEvent(ws, CONSTRUCT_SURFACE_PATCH, surface, seq)
        const twice = applySurfaceEvent(once, CONSTRUCT_SURFACE_PATCH, surface, seq)
        expectWorkspacesEqual(twice, once)
      }),
      { numRuns: 300 },
    )
  })
})
