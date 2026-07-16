import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { apiFetch, apiSend, ApiError } from '@/lib/api/client'

const ORIGIN = 'https://router.test'

beforeEach(() => {
  process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL = ORIGIN
  vi.resetModules()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('apiFetch', () => {
  it('throws ApiError with parsed message on 4xx', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: 'boom' }), {
        status: 400,
        headers: { 'content-type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    await expect(apiFetch('/x', { retries: 0 })).rejects.toMatchObject({
      status: 400,
      message: 'boom',
      retryable: false,
    })
  })

  it('retries transient 503 then succeeds', async () => {
    let calls = 0
    const fetchMock = vi.fn().mockImplementation(async () => {
      calls++
      if (calls < 3) {
        return new Response('busy', { status: 503 })
      }
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    })
    vi.stubGlobal('fetch', fetchMock)
    const r = await apiFetch<{ ok: boolean }>('/x', { retries: 3 })
    expect(r.ok).toBe(true)
    expect(calls).toBe(3)
  })

  it('does not retry POST by default', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('busy', { status: 503 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(apiSend('/x', { a: 1 })).rejects.toBeInstanceOf(ApiError)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
