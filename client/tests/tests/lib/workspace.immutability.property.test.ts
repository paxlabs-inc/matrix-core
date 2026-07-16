import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH, validateSurface } from '@/lib/construct/store'
import type {
  Surface,
  Kind,
  Stakes,
  Temporality,
  NarrationRole,
  Trend,
  MetricDisplay,
  Shape,
  StepStatus,
  MediaKind,
  AskKind,
} from '@/lib/construct/types.gen'

/**
 * Property 4: Renderer reuse / envelope immutability.
 *
 * Exercises the REAL `applySurfaceEvent` reducer (no stubs/mocks) against
 * arbitrary schema-valid `Surface` envelopes spanning all 8 frozen kinds and
 * arbitrary apply sequences. The reducer uses clone-on-write (`cloneSurface` +
 * `applyPatch`), so two facets must hold:
 *
 *   1. Stored-equals-input (full frame): after a `construct.surface` frame is
 *      applied, the stored envelope deep-equals the validated input it wrapped —
 *      the shell stores an equal copy, never a transformed surface.
 *   2. Non-mutation of the caller's input (all frames): applying any frame —
 *      full surface or patch, orphan or folded onto an existing base — never
 *      mutates the input `Surface` object in place. The pre-call deep snapshot of
 *      every input equals that input after the apply.
 *
 * (A `construct.surface.patch` MERGES onto the base, so the stored result is a
 * new merged object NOT deep-equal to the patch input — facet 1 is asserted for
 * full-surface frames only, while facet 2 holds for every frame.)
 *
 * Validates: Requirements 6.1
 */

// ---- JSON-safe scalar generators (survive the wire round-trip the reducer mirrors) ----
const arbNum = fc.double({ noNaN: true, noDefaultInfinity: true, min: -1e6, max: 1e6 })
const arbId = fc.string({ minLength: 1 })
const opt = <T>(a: fc.Arbitrary<T>) => fc.option(a, { nil: undefined })

// ---- Envelope-level attribute generators ----
const arbStakes = fc.constantFrom<Stakes>('fact', 'hypothesis', 'decision', 'irreversible')
const arbTemporality = fc.constantFrom<Temporality>('point', 'stream', 'persistent')
const arbRole = fc.constantFrom<NarrationRole>('thinking', 'intent', 'answer')
const arbTrend = fc.constantFrom<Trend>('up', 'down', 'flat')
const arbDisplay = fc.constantFrom<MetricDisplay>('plain', 'bar', 'gauge')
const arbShape = fc.constantFrom<Shape>('list', 'table', 'tree')
const arbStepStatus = fc.constantFrom<StepStatus>('pending', 'running', 'done', 'failed')
const arbMediaKind = fc.constantFrom<MediaKind>('image', 'video', 'audio', 'page', 'chart')
const arbAskKind = fc.constantFrom<AskKind>('choose', 'input', 'confirm', 'sign', 'upload')

const arbAttributes = opt(
  fc.record({
    stakes: opt(arbStakes),
    confidence: opt(fc.double({ min: 0, max: 1, noNaN: true })),
    temporality: opt(arbTemporality),
  }),
)

// ---- Per-kind payload generators (each builds exactly one valid payload) ----
const payloadByKind: Record<Kind, fc.Arbitrary<unknown>> = {
  narration: fc.record({ text: fc.string(), role: opt(arbRole) }),
  metric: fc.record({
    label: fc.string(),
    value: fc.string(),
    unit: opt(fc.string()),
    magnitude: opt(arbNum),
    trend: opt(arbTrend),
    display: opt(arbDisplay),
  }),
  entity: fc.record({
    type: fc.string(),
    identity: fc.string(),
    label: opt(fc.string()),
    fields: opt(
      fc.array(fc.record({ key: fc.string(), value: fc.string(), ref: opt(fc.string()) })),
    ),
  }),
  structure: fc.record({
    shape: arbShape,
    columns: opt(fc.array(fc.string())),
    records: fc.array(
      fc.record({
        id: opt(fc.string()),
        label: opt(fc.string()),
        ref: opt(fc.string()),
      }),
    ),
  }),
  stream: fc.record({
    source: opt(fc.string()),
    title: opt(fc.string()),
    chunks: opt(
      fc.array(fc.record({ seq: fc.nat(), text: fc.string(), channel: opt(fc.string()) })),
    ),
    closed: opt(fc.boolean()),
  }),
  timeline: fc.record({
    title: opt(fc.string()),
    steps: fc.array(
      fc.record({
        id: arbId,
        label: fc.string(),
        status: arbStepStatus,
        detail: opt(fc.string()),
        ref: opt(fc.string()),
      }),
    ),
  }),
  canvas: fc.record({
    media: fc.record({
      kind: arbMediaKind,
      url: opt(fc.string()),
      mime: opt(fc.string()),
      alt: opt(fc.string()),
    }),
    caption: opt(fc.string()),
  }),
  ask: fc.record({
    ask_kind: arbAskKind,
    prompt: fc.string(),
    options: opt(fc.array(fc.record({ id: arbId, label: fc.string() }))),
    required: opt(fc.boolean()),
  }),
}

const PAYLOAD_KEY: Record<Kind, keyof Surface> = {
  narration: 'narration',
  metric: 'metric',
  entity: 'entity',
  structure: 'structure',
  stream: 'stream',
  timeline: 'timeline',
  canvas: 'canvas',
  ask: 'ask',
}

const VALID_KINDS: readonly Kind[] = [
  'narration',
  'metric',
  'entity',
  'structure',
  'stream',
  'timeline',
  'canvas',
  'ask',
] as const

/** Build a valid single-payload Surface envelope of a given kind and id. */
function buildSurface(
  kind: Kind,
  id: string,
  payload: unknown,
  ref: string | undefined,
  parent: string | undefined,
  seq: number | undefined,
  attributes: Surface['attributes'],
): Surface {
  return {
    kind,
    id,
    ref,
    parent,
    seq,
    attributes,
    [PAYLOAD_KEY[kind]]: payload,
  } as Surface
}

/** An arbitrary schema-valid Surface across all 8 kinds, with an id from a pool. */
function arbSurfaceWithId(idArb: fc.Arbitrary<string>): fc.Arbitrary<Surface> {
  return fc
    .constantFrom<Kind>(...VALID_KINDS)
    .chain((kind) =>
      fc
        .tuple(
          idArb,
          payloadByKind[kind],
          opt(fc.string()),
          opt(fc.string()),
          opt(fc.nat()),
          arbAttributes,
        )
        .map(([id, payload, ref, parent, seq, attributes]) =>
          buildSurface(kind, id, payload, ref, parent, seq, attributes),
        ),
    )
}

const arbSurface = arbSurfaceWithId(arbId)

/** Faithful deep snapshot that preserves explicit `undefined` properties. */
function snapshot<T>(v: T): T {
  return structuredClone(v)
}

describe('applySurfaceEvent — Property 4: envelope immutability (R6.1)', () => {
  it('stores a full-surface envelope deep-equal to its validated input (facet 1)', () => {
    fc.assert(
      fc.property(arbSurface, fc.nat(), (surface, seq) => {
        // sanity: the generator only produces envelopes the transport admits
        expect(validateSurface(surface)).toBe(true)

        const ws = emptyWorkspace('conv-immut')
        const next = applySurfaceEvent(ws, CONSTRUCT_SURFACE, surface, seq)

        const stored = next.surfaces.get(surface.id)
        expect(stored).toBeDefined()
        // the wrapped envelope is an equal copy of the input (wrap, never transform)
        expect(stored!.surface).toEqual(surface)
      }),
    )
  })

  it('never mutates the caller input across an arbitrary apply sequence (facet 2)', () => {
    // ids drawn from a small pool so some patches fold onto an existing base and
    // some arrive as orphans — both paths must leave the input object untouched.
    const idPool = fc.constantFrom('a', 'b', 'c')
    const arbFrame = fc.record({
      type: fc.constantFrom(CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH),
      surface: arbSurfaceWithId(idPool),
      seq: fc.nat(),
    })

    fc.assert(
      fc.property(fc.array(arbFrame, { minLength: 1, maxLength: 12 }), (frames) => {
        const snaps = frames.map((f) => snapshot(f.surface))

        let ws: SurfaceWorkspace = emptyWorkspace('conv-immut')
        for (const f of frames) {
          ws = applySurfaceEvent(ws, f.type, f.surface, f.seq)
        }

        // every input envelope is byte-for-byte unchanged after being applied
        frames.forEach((f, i) => {
          expect(f.surface).toEqual(snaps[i])
        })
      }),
    )
  })

  it('mutates neither the base input nor the patch input when a patch folds onto a base (facet 2)', () => {
    // same kind + id so the patch genuinely merges onto the stored base
    const arbBaseAndPatch = fc.constantFrom<Kind>(...VALID_KINDS).chain((kind) =>
      fc
        .tuple(arbId, payloadByKind[kind], payloadByKind[kind], arbAttributes, arbAttributes)
        .map(([id, basePayload, patchPayload, baseAttrs, patchAttrs]) => ({
          base: buildSurface(kind, id, basePayload, undefined, undefined, undefined, baseAttrs),
          patch: buildSurface(kind, id, patchPayload, undefined, undefined, undefined, patchAttrs),
        })),
    )

    fc.assert(
      fc.property(arbBaseAndPatch, ({ base, patch }) => {
        const baseSnap = snapshot(base)
        const patchSnap = snapshot(patch)

        let ws = emptyWorkspace('conv-immut')
        ws = applySurfaceEvent(ws, CONSTRUCT_SURFACE, base, 1)
        const merged = applySurfaceEvent(ws, CONSTRUCT_SURFACE_PATCH, patch, 2)

        // neither caller-supplied object was mutated by store-then-fold
        expect(base).toEqual(baseSnap)
        expect(patch).toEqual(patchSnap)

        // and the stored result is a distinct object from both inputs
        const stored = merged.surfaces.get(base.id)!.surface
        expect(stored).not.toBe(base)
        expect(stored).not.toBe(patch)
      }),
    )
  })
})
