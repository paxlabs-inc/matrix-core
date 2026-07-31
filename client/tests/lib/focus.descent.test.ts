import { describe, it, expect, vi } from 'vitest'
import {
  resolveLink,
  topFocus,
  pushRawFocus,
  ascend,
  descendToLinkedStream,
  DESCENT_UNAVAILABLE_MESSAGE,
  type Linkable,
} from '@/lib/construct/focus'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE } from '@/lib/construct/store'
import type { Surface } from '@/lib/construct/types.gen'

/**
 * Unit test for one level of descent (task 6.4 / R4.3–R4.6, R17.3).
 *
 * Drives the REAL pure focus-stack operations from `lib/construct/focus.ts`
 * against the REAL `SurfaceWorkspace` model from `lib/construct/workspace.ts`.
 * The ONLY injected seam is `rehydrate` — a legitimate dependency the cold-link
 * descent path takes (it stands in for the feed-backed rehydration wired later),
 * NOT a fake of the logic under test. No stubs/mocks of focus.ts or workspace.ts.
 *
 * Concrete MVP path: tap a Timeline STEP → resolve its `ref` (else `parent`) to
 * a target `Stream` surface → push exactly ONE raw focus frame at the linked
 * Stream. Cold target rehydrates BY ADDRESS first, then pushes; ascend pops.
 */

// ---------------------------------------------------------------------------
// Helpers — build real surfaces and fold them into a real workspace
// ---------------------------------------------------------------------------

const CONV = 'conv-1'

/** A well-formed Stream surface envelope (the descend target kind). */
function streamSurface(id: string): Surface {
  return {
    kind: 'stream',
    id,
    stream: { source: 'terminal', chunks: [{ seq: 0, text: 'a' }], closed: false },
  }
}

/** Fold a full surface into a workspace through the real reducer. */
function withSurface(ws: SurfaceWorkspace, surface: Surface, seq = 1): SurfaceWorkspace {
  return applySurfaceEvent(ws, CONSTRUCT_SURFACE, surface, seq)
}

// ---------------------------------------------------------------------------
// resolveLink (R4.3): ref FIRST, else parent, else undefined
// ---------------------------------------------------------------------------

describe('resolveLink — ref resolves before parent (R4.3)', () => {
  it('resolves ref first when both ref and parent are present', () => {
    expect(resolveLink({ ref: 's1', parent: 'p1' })).toBe('s1')
  })
  it('falls back to parent when ref is absent', () => {
    expect(resolveLink({ parent: 'p1' })).toBe('p1')
  })
  it('returns undefined when neither ref nor parent is present', () => {
    expect(resolveLink({})).toBeUndefined()
    expect(resolveLink(undefined)).toBeUndefined()
  })
})

// ---------------------------------------------------------------------------
// pushRawFocus / topFocus (pure stack primitives)
// ---------------------------------------------------------------------------

describe('pushRawFocus — pushes one raw frame without mutating the input', () => {
  it('appends exactly one raw frame targeting the surface id', () => {
    const ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    const next = pushRawFocus(ws, 's1')
    expect(next.focus.stack).toHaveLength(1)
    expect(topFocus(next)).toEqual({ surfaceId: 's1', level: 'raw' })
    // input workspace is left unchanged (immutable update)
    expect(ws.focus.stack).toHaveLength(0)
  })
})

// ---------------------------------------------------------------------------
// Case 1 — tap a Timeline step whose ref points to a hot Stream (R4.3/R17.3)
// ---------------------------------------------------------------------------

describe('descendToLinkedStream — hot link (target already in the workspace)', () => {
  it('pushes exactly one raw focus frame targeting the linked Stream id', async () => {
    const ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    const step: Linkable = { ref: 's1' }

    const result = await descendToLinkedStream(ws, step)

    expect(result.status).toBe('descended')
    expect(result.workspace.focus.stack).toHaveLength(1)
    expect(topFocus(result.workspace)).toEqual({ surfaceId: 's1', level: 'raw' })
    // original workspace untouched
    expect(ws.focus.stack).toHaveLength(0)
  })

  it('resolves ref before parent when descending (R4.3)', async () => {
    const ws = withSurface(
      withSurface(emptyWorkspace(CONV), streamSurface('s1')),
      streamSurface('p1'),
      2,
    )
    const step: Linkable = { ref: 's1', parent: 'p1' }

    const result = await descendToLinkedStream(ws, step)

    expect(result.status).toBe('descended')
    expect(topFocus(result.workspace)).toEqual({ surfaceId: 's1', level: 'raw' })
  })

  it('does NOT invoke rehydrate when the target is hot', async () => {
    const ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    const rehydrate = vi.fn(async (_id: string) => ws)

    const result = await descendToLinkedStream(ws, { ref: 's1' }, { rehydrate })

    expect(result.status).toBe('descended')
    expect(rehydrate).not.toHaveBeenCalled()
  })
})

// ---------------------------------------------------------------------------
// Case 2 — cold link: rehydrate BY ADDRESS first, then push (R4.4)
// ---------------------------------------------------------------------------

describe('descendToLinkedStream — cold link (target not in the workspace)', () => {
  it('calls rehydrate by the linked surface id FIRST, then pushes the raw frame', async () => {
    const ws = emptyWorkspace(CONV) // 's1' is NOT present (cold)
    const order: string[] = []

    const rehydrate = vi.fn(async (id: string) => {
      order.push('rehydrate')
      // rehydration lands the target into the (returned) workspace
      return withSurface(ws, streamSurface(id))
    })

    const result = await descendToLinkedStream(ws, { ref: 's1' }, { rehydrate })

    // rehydrated by address first
    expect(rehydrate).toHaveBeenCalledTimes(1)
    expect(rehydrate).toHaveBeenCalledWith('s1')
    // then the raw frame was pushed onto the rehydrated workspace
    expect(result.status).toBe('descended')
    expect(topFocus(result.workspace)).toEqual({ surfaceId: 's1', level: 'raw' })
    // the pushed frame sits atop the workspace that now contains the target
    expect(result.workspace.surfaces.has('s1')).toBe(true)
    expect(order).toEqual(['rehydrate'])
  })

  it('is unavailable (non-jargon) when no rehydration path is wired', async () => {
    const ws = emptyWorkspace(CONV)

    const result = await descendToLinkedStream(ws, { ref: 's1' })

    expect(result.status).toBe('unavailable')
    expect(result.message).toBe(DESCENT_UNAVAILABLE_MESSAGE)
    // focus stack left unchanged
    expect(result.workspace).toBe(ws)
    expect(result.workspace.focus.stack).toHaveLength(0)
  })
})

// ---------------------------------------------------------------------------
// Case 3 — rehydration failure / timeout / no-show leaves focus UNCHANGED (R4.5)
// ---------------------------------------------------------------------------

describe('descendToLinkedStream — cold link failure leaves the focus stack unchanged (R4.5)', () => {
  it('rejecting rehydrate → unavailable, no focus frame pushed, non-jargon message', async () => {
    const ws = emptyWorkspace(CONV)
    const rehydrate = vi.fn(async (_id: string) => {
      throw new Error('cold-store-miss')
    })

    const result = await descendToLinkedStream(ws, { ref: 's1' }, { rehydrate })

    expect(result.status).toBe('unavailable')
    expect(result.message).toBe(DESCENT_UNAVAILABLE_MESSAGE)
    expect(result.workspace).toBe(ws) // unchanged input
    expect(result.workspace.focus.stack).toHaveLength(0)
    // message carries no protocol jargon
    expect(result.message).not.toMatch(/cortex|merkle|sse|replay|mcl/i)
  })

  it('rehydrate exceeding the budget → unavailable within the timeout, focus unchanged', async () => {
    const ws = emptyWorkspace(CONV)
    // never resolves; bounded by a small injected timeout (exercises the 5s branch fast)
    const rehydrate = vi.fn(() => new Promise<SurfaceWorkspace>(() => {}))

    const result = await descendToLinkedStream(ws, { ref: 's1' }, { rehydrate, timeoutMs: 20 })

    expect(result.status).toBe('unavailable')
    expect(result.message).toBe(DESCENT_UNAVAILABLE_MESSAGE)
    expect(result.workspace).toBe(ws)
    expect(result.workspace.focus.stack).toHaveLength(0)
  })

  it('rehydrate resolves WITHOUT the target present → unavailable, focus unchanged', async () => {
    const ws = emptyWorkspace(CONV)
    // returns a workspace that still does not contain 's1'
    const rehydrate = vi.fn(async () => withSurface(emptyWorkspace(CONV), streamSurface('other')))

    const result = await descendToLinkedStream(ws, { ref: 's1' }, { rehydrate })

    expect(rehydrate).toHaveBeenCalledWith('s1')
    expect(result.status).toBe('unavailable')
    expect(result.workspace).toBe(ws)
    expect(result.workspace.focus.stack).toHaveLength(0)
  })
})

// ---------------------------------------------------------------------------
// Case 5 — no-link step is a no-op (R4.6)
// ---------------------------------------------------------------------------

describe('descendToLinkedStream — no-link step is a no-op (R4.6)', () => {
  it('a step with neither ref nor parent leaves the workspace unchanged and never rehydrates', async () => {
    const ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    const rehydrate = vi.fn(async (_id: string) => ws)

    const result = await descendToLinkedStream(ws, {}, { rehydrate })

    expect(result.status).toBe('noop')
    expect(result.workspace).toBe(ws) // unchanged input
    expect(result.workspace.focus.stack).toHaveLength(0)
    expect(rehydrate).not.toHaveBeenCalled()
  })
})

// ---------------------------------------------------------------------------
// Case 4 — ascend pops exactly the top frame; no-op at the base (R4.7/R4.8)
// ---------------------------------------------------------------------------

describe('ascend — pops the top focus frame; no-op at the base (R4.7/R4.8)', () => {
  it('pops exactly the top frame after a descend', async () => {
    const ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    const descended = (await descendToLinkedStream(ws, { ref: 's1' })).workspace
    expect(descended.focus.stack).toHaveLength(1)

    const ascended = ascend(descended)
    expect(ascended.focus.stack).toHaveLength(0)
    expect(topFocus(ascended)).toBeUndefined()
  })

  it('pops only the top frame when several are stacked', () => {
    let ws = withSurface(emptyWorkspace(CONV), streamSurface('s1'))
    ws = pushRawFocus(ws, 's1')
    ws = pushRawFocus(ws, 's2')
    expect(ws.focus.stack).toHaveLength(2)

    const ascended = ascend(ws)
    expect(ascended.focus.stack).toHaveLength(1)
    expect(topFocus(ascended)).toEqual({ surfaceId: 's1', level: 'raw' })
  })

  it('ascend at the base is a no-op (returns the same workspace)', () => {
    const ws = emptyWorkspace(CONV)
    const ascended = ascend(ws)
    expect(ascended).toBe(ws)
    expect(ascended.focus.stack).toHaveLength(0)
  })
})
