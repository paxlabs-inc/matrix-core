import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { startSSE } from '@/lib/realtime/sse'

const ORIGIN = 'https://router.test'

beforeEach(() => {
  process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL = ORIGIN
})

afterEach(() => {
  vi.restoreAllMocks()
})

function streamFromChunks(chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder()
  let i = 0
  return new ReadableStream<Uint8Array>({
    pull(ctrl) {
      if (i < chunks.length) {
        ctrl.enqueue(enc.encode(chunks[i++]))
      } else {
        ctrl.close()
      }
    },
  })
}

describe('startSSE', () => {
  it('parses data: frames and dispatches events', async () => {
    const chunks = [
      `data: ${JSON.stringify({ seq: 1, ts: 't1', phase: 'p', type: 'a', fields: {} })}\n\n`,
      `: ping\n\n`,
      `data: ${JSON.stringify({ seq: 2, ts: 't2', phase: 'p', type: 'b', fields: {} })}\n\n`,
    ]
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(streamFromChunks(chunks), {
        status: 200,
        headers: { 'content-type': 'text/event-stream' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    const updates: string[] = []
    const events: number[] = []
    const handle = startSSE({
      path: '/events',
      onUpdate: (u) => {
        updates.push(u.kind)
        if (u.kind === 'event') events.push(u.event.seq)
      },
    })

    // Wait for the rAF flush + stream completion. jsdom doesn't tick
    // rAF automatically; pump it.
    await new Promise((r) => setTimeout(r, 100))
    if (typeof requestAnimationFrame !== 'undefined') {
      // Force a flush.
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    }
    handle.close()

    expect(updates).toContain('open')
    expect(events).toEqual(expect.arrayContaining([1, 2]))
  })

  it('reports a gap when sequence jumps', async () => {
    const chunks = [
      `data: ${JSON.stringify({ seq: 1, ts: 't1', phase: 'p', type: 'a', fields: {} })}\n\n`,
      `data: ${JSON.stringify({ seq: 5, ts: 't5', phase: 'p', type: 'a', fields: {} })}\n\n`,
    ]
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(streamFromChunks(chunks), {
        status: 200,
        headers: { 'content-type': 'text/event-stream' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    const seenGaps: Array<{ from: number; to: number }> = []
    const handle = startSSE({
      path: '/events',
      onUpdate: (u) => {
        if (u.kind === 'gap') seenGaps.push({ from: u.from, to: u.to })
      },
    })
    await new Promise((r) => setTimeout(r, 80))
    if (typeof requestAnimationFrame !== 'undefined') {
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    }
    handle.close()
    expect(seenGaps[0]).toEqual({ from: 1, to: 5 })
  })
})
