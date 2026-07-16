import { describe, it, expect } from 'vitest'
import {
  validateSurface,
  parseSurface,
  applyPatch,
  applySurfaceEvent,
  CONSTRUCT_SURFACE,
  CONSTRUCT_SURFACE_PATCH,
} from '@/lib/construct/store'
import { neoTaskToSurfaces } from '@/lib/construct/adapter'
import type { Surface } from '@/lib/construct/types.gen'
import type { NeoStep, NeoTask } from '@/hooks/api/useChat'

function narration(id: string, text: string): Surface {
  return { kind: 'narration', id, narration: { text } }
}

describe('validateSurface', () => {
  it('accepts a well-formed surface', () => {
    expect(validateSurface(narration('n1', 'hi'))).toBe(true)
  })
  it('rejects an unknown kind', () => {
    expect(validateSurface({ kind: 'widget', id: 'x', narration: { text: 'a' } })).toBe(false)
  })
  it('rejects a missing id', () => {
    expect(validateSurface({ kind: 'narration', id: '', narration: { text: 'a' } })).toBe(false)
  })
  it('rejects more than one payload', () => {
    expect(
      validateSurface({
        kind: 'narration',
        id: 'x',
        narration: { text: 'a' },
        metric: { label: 'l', value: '1' },
      }),
    ).toBe(false)
  })
  it('rejects a payload that does not match the kind', () => {
    expect(validateSurface({ kind: 'metric', id: 'x', narration: { text: 'a' } })).toBe(false)
  })
})

describe('parseSurface', () => {
  it('extracts the surface from event fields', () => {
    const s = parseSurface({ surface: narration('n1', 'hi'), intent_id: 'i1' })
    expect(s?.id).toBe('n1')
  })
  it('returns null when the field is absent', () => {
    expect(parseSurface({ intent_id: 'i1' })).toBeNull()
  })
  it('drops a malformed surface (hostile payload)', () => {
    expect(parseSurface({ surface: { kind: 'evil', id: 'x' } })).toBeNull()
  })
})

describe('applyPatch — mirrors the Go ApplyPatch', () => {
  it('Stream appends chunks and dedups by seq (idempotent on replay)', () => {
    const base: Surface = {
      kind: 'stream',
      id: 's1',
      stream: { source: 'terminal', chunks: [{ seq: 0, text: 'a' }], closed: false },
    }
    const patch: Surface = {
      kind: 'stream',
      id: 's1',
      stream: {
        chunks: [
          { seq: 0, text: 'a' },
          { seq: 1, text: 'b' },
        ],
        closed: true,
      },
    }
    const out = applyPatch(base, patch)
    expect(out.stream?.chunks?.map((c) => c.seq)).toEqual([0, 1])
    expect(out.stream?.closed).toBe(true)
    // base untouched
    expect(base.stream?.chunks?.length).toBe(1)
    // replaying the same patch is a no-op
    expect(applyPatch(out, patch).stream?.chunks?.length).toBe(2)
  })

  it('Timeline upserts by step id and appends new ids', () => {
    const base: Surface = {
      kind: 'timeline',
      id: 't1',
      timeline: {
        title: 'Plan',
        steps: [
          { id: 'a', label: 'A', status: 'running' },
          { id: 'b', label: 'B', status: 'pending' },
        ],
      },
    }
    const patch: Surface = {
      kind: 'timeline',
      id: 't1',
      timeline: {
        steps: [
          { id: 'a', label: 'A', status: 'done' },
          { id: 'c', label: 'C', status: 'pending' },
        ],
      },
    }
    const out = applyPatch(base, patch)
    expect(out.timeline?.steps.map((s) => [s.id, s.status])).toEqual([
      ['a', 'done'],
      ['b', 'pending'],
      ['c', 'pending'],
    ])
    expect(out.timeline?.title).toBe('Plan')
  })

  it('non-stream/timeline kinds replace the payload wholesale', () => {
    const base: Surface = { kind: 'metric', id: 'm1', metric: { label: 'cost', value: '1' } }
    const patch: Surface = { kind: 'metric', id: 'm1', metric: { label: 'cost', value: '2' } }
    expect(applyPatch(base, patch).metric?.value).toBe('2')
  })

  it('a kind mismatch returns the base unchanged', () => {
    const base = narration('x', 'a')
    const patch: Surface = { kind: 'metric', id: 'x', metric: { label: 'l', value: '1' } }
    expect(applyPatch(base, patch)).toBe(base)
  })
})

describe('applySurfaceEvent', () => {
  it('appends a new full surface and replaces by id', () => {
    let list: Surface[] = []
    list = applySurfaceEvent(list, CONSTRUCT_SURFACE, narration('n1', 'a'))
    list = applySurfaceEvent(list, CONSTRUCT_SURFACE, narration('n2', 'b'))
    expect(list.map((s) => s.id)).toEqual(['n1', 'n2'])
    list = applySurfaceEvent(list, CONSTRUCT_SURFACE, narration('n1', 'a2'))
    expect(list).toHaveLength(2)
    expect(list[0].narration?.text).toBe('a2')
  })

  it('merges a patch onto an existing base in place', () => {
    let list: Surface[] = [
      { kind: 'stream', id: 's1', stream: { chunks: [{ seq: 0, text: 'a' }] } },
    ]
    list = applySurfaceEvent(list, CONSTRUCT_SURFACE_PATCH, {
      kind: 'stream',
      id: 's1',
      stream: { chunks: [{ seq: 1, text: 'b' }] },
    })
    expect(list[0].stream?.chunks?.length).toBe(2)
  })

  it('seeds from a patch that arrives before its base (reconnect)', () => {
    const list = applySurfaceEvent([], CONSTRUCT_SURFACE_PATCH, narration('n1', 'a'))
    expect(list.map((s) => s.id)).toEqual(['n1'])
  })
})

/* ----------------------- adapter (wrap-first) ----------------------- */

function step(partial: Partial<NeoStep> & Pick<NeoStep, 'id' | 'kind'>): NeoStep {
  return { running: false, ok: true, title: '', ...partial }
}

function task(partial: Partial<NeoTask>): NeoTask {
  return {
    intentId: 'i1',
    steps: [],
    searches: [],
    media: [],
    artifacts: [],
    surfaces: [],
    answer: '',
    done: false,
    ...partial,
  }
}

describe('neoTaskToSurfaces', () => {
  it('maps each NeoStep kind onto a Construct primitive', () => {
    const out = neoTaskToSurfaces(
      task({
        steps: [
          step({ id: 'n', kind: 'narration', text: 'thinking…' }),
          step({ id: 't', kind: 'terminal', command: 'ls', output: 'a\nb' }),
          step({ id: 'b', kind: 'browser', url: 'https://x.dev', pageTitle: 'X' }),
          step({ id: 'e', kind: 'editor', path: '/a/b.go', language: 'go' }),
          step({ id: 'a', kind: 'action', title: 'Did a thing' }),
        ],
      }),
    )
    expect(out.map((s) => [s.id, s.kind])).toEqual([
      ['n', 'narration'],
      ['t', 'stream'],
      ['b', 'canvas'],
      ['e', 'entity'],
      ['a', 'entity'],
    ])
    // every surface validates
    for (const s of out) expect(validateSurface(s)).toBe(true)
  })

  it('projects searches, media, swarm and the settled answer', () => {
    const out = neoTaskToSurfaces(
      task({
        searches: [
          {
            tool: 'web_search',
            provider: 'x',
            query: 'q',
            answer: 'the answer',
            results: [{ title: 'T', url: 'https://t.dev', snippet: 's' }],
          },
        ],
        media: [{ url: 'https://m.dev/a.png', kind: 'image' }],
        swarm: {
          id: 'sw',
          count: 1,
          done: true,
          agents: [{ index: 1, name: 'Agent 01', status: 'done', steps: [] }],
        },
        answer: 'final answer',
        done: true,
      }),
    )
    const kinds = out.map((s) => s.kind)
    expect(kinds).toContain('structure') // search sources
    expect(kinds).toContain('canvas') // media
    expect(kinds).toContain('timeline') // swarm
    // the settled answer is the last surface
    expect(out[out.length - 1]).toMatchObject({ kind: 'narration' })
    expect(out[out.length - 1].narration?.role).toBe('answer')
    for (const s of out) expect(validateSurface(s)).toBe(true)
  })

  it('is deterministic (same task -> same surfaces)', () => {
    const t = task({ steps: [step({ id: 'n', kind: 'narration', text: 'x' })] })
    expect(neoTaskToSurfaces(t)).toEqual(neoTaskToSurfaces(t))
  })
})
