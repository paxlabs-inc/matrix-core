import { describe, it, expect } from 'vitest'
import fc from 'fast-check'
import {
  placeSurface,
  REGIONS,
  DEFAULT_REGION,
  type Placement,
  type Region,
  type Lifecycle,
} from '@/lib/construct/workspace'
import type { Surface, Kind, Stakes, Temporality, NarrationRole } from '@/lib/construct/types.gen'

/**
 * Property 3: Placement determinism.
 *
 * Exercises the REAL `placeSurface` policy (no stubs/mocks) against arbitrary
 * `Surface` envelopes — all 8 frozen kinds AND forged/unknown kinds — paired
 * with arbitrary prior `Placement`s.
 *
 * Validates: Requirements 2.4, 2.5, 2.6, 2.7, 3.3
 */

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

// Forged / out-of-vocabulary kinds the shell must tolerate without throwing.
const FORGED_KINDS = ['widget', 'evil', '', 'Narration', 'HOME', 'window', '__proto__'] as const

// A kind generator that mixes the 8 frozen primitives with hostile/unknown values.
const arbKind: fc.Arbitrary<string> = fc.oneof(
  fc.constantFrom<Kind>(...VALID_KINDS),
  fc.constantFrom(...FORGED_KINDS),
  fc.string(),
)

const arbStakes = fc.constantFrom<Stakes>('fact', 'hypothesis', 'decision', 'irreversible')
const arbTemporality = fc.constantFrom<Temporality>('point', 'stream', 'persistent')
const arbRole = fc.constantFrom<NarrationRole>('thinking', 'intent', 'answer')

const arbAttributes = fc.option(
  fc.record({
    stakes: fc.option(arbStakes, { nil: undefined }),
    confidence: fc.option(fc.double({ min: 0, max: 1, noNaN: true }), { nil: undefined }),
    temporality: fc.option(arbTemporality, { nil: undefined }),
  }),
  { nil: undefined },
)

const arbNarrationPayload = fc.option(
  fc.record({
    text: fc.string(),
    role: fc.option(arbRole, { nil: undefined }),
  }),
  { nil: undefined },
)

// An arbitrary Surface envelope with a (possibly forged) kind. Cast because the
// `kind` generator intentionally produces values outside the `Kind` union to
// exercise the unknown-kind branch (R2.7).
function buildSurface(
  kind: string,
  id: string,
  attributes: Surface['attributes'],
  narration: Surface['narration'],
  seq: number | undefined,
): Surface {
  return { kind: kind as Kind, id, attributes, narration, seq }
}

const arbSurface: fc.Arbitrary<Surface> = fc
  .tuple(
    arbKind,
    fc.string(),
    arbAttributes,
    arbNarrationPayload,
    fc.option(fc.nat(), { nil: undefined }),
  )
  .map(([kind, id, attributes, narration, seq]) =>
    buildSurface(kind, id, attributes, narration, seq),
  )

const arbRegion = fc.constantFrom<Region>(...(REGIONS as Region[]))
const arbLifecycle = fc.constantFrom<Lifecycle>('live', 'settled', 'archived')

const arbPlacement: fc.Arbitrary<Placement> = fc.record({
  region: arbRegion,
  pinned: fc.boolean(),
  lifecycle: arbLifecycle,
  zOrder: fc.option(fc.integer(), { nil: undefined }),
  gridSlot: fc.option(fc.integer(), { nil: undefined }),
})

const arbPrior: fc.Arbitrary<Placement | undefined> = fc.option(arbPlacement, { nil: undefined })

describe('placeSurface — Property 3: placement determinism', () => {
  it('is field-for-field deterministic for equal (surface, prior) inputs (R2.5)', () => {
    fc.assert(
      fc.property(arbSurface, arbPrior, (surface, prior) => {
        const a = placeSurface(surface, prior)
        const b = placeSurface(surface, prior)
        expect(a).toEqual(b)
      }),
    )
  })

  it('always assigns a region in the 7 regions and never throws, even for unknown kinds (R2.4, R2.7)', () => {
    fc.assert(
      fc.property(arbSurface, arbPrior, (surface, prior) => {
        const placement = placeSurface(surface, prior)
        expect(REGIONS).toContain(placement.region)
        // an unknown/forged kind must land in the deterministic default region
        if (!(VALID_KINDS as readonly string[]).includes(surface.kind)) {
          expect(placement.region).toBe(DEFAULT_REGION)
        }
      }),
    )
  })

  it('preserves a prior pinned=true on the re-projected placement (R2.6)', () => {
    fc.assert(
      fc.property(arbSurface, arbPlacement, (surface, prior) => {
        const pinnedPrior: Placement = { ...prior, pinned: true }
        expect(placeSurface(surface, pinnedPrior).pinned).toBe(true)
      }),
    )
  })

  it('routes any narration surface to the narration region regardless of attributes or prior (R3.3)', () => {
    const arbNarrationSurface: fc.Arbitrary<Surface> = fc
      .tuple(
        fc.string(),
        arbAttributes,
        arbNarrationPayload,
        fc.option(fc.nat(), { nil: undefined }),
      )
      .map(([id, attributes, narration, seq]) =>
        buildSurface('narration', id, attributes, narration, seq),
      )
    fc.assert(
      fc.property(arbNarrationSurface, arbPrior, (surface, prior) => {
        expect(placeSurface(surface, prior).region).toBe('narration')
      }),
    )
  })
})
