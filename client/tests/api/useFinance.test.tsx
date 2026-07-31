import { createServer, type Server } from 'node:http'
import { createElement, type ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { qk } from '@/lib/query/keys'

type FinanceHooks = typeof import('@/hooks/api/useFinance')

let server: Server
let origin = ''
let financeHooks: FinanceHooks
let requests: URL[] = []
let originalRouterURL: string | undefined

const requestWaiters: Array<{
  matches: (url: URL) => boolean
  resolve: (url: URL) => void
}> = []

function json(body: unknown): string {
  return JSON.stringify(body)
}

function nextRequest(matches: (url: URL) => boolean): Promise<URL> {
  return new Promise((resolve) => {
    requestWaiters.push({ matches, resolve })
  })
}

function responseFor(url: URL): { status: number; body: unknown } {
  if (url.pathname === '/finance/quote' && url.searchParams.get('symbol') === 'MISS') {
    return {
      status: 400,
      body: {
        error: {
          kind: 'not_configured',
          message: 'FMP is not configured.',
          provider: 'fmp',
        },
      },
    }
  }
  if (url.pathname === '/finance/quote') {
    return {
      status: 200,
      body: {
        symbol: url.searchParams.get('symbol'),
        price: 213.55,
        source: { provider: 'fmp', fetched_at: '2026-07-27T12:00:00Z' },
      },
    }
  }
  return {
    status: 200,
    body: {
      symbol: url.searchParams.get('symbol') ?? 'AAPL',
      interval: '1day',
      candles: [],
      items: [],
      events: [],
      source: { provider: 'fmp', fetched_at: '2026-07-27T12:00:00Z' },
    },
  }
}

function wrapper(client: QueryClient) {
  return function QueryWrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client }, children)
  }
}

function queryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { gcTime: Infinity },
    },
  })
}

function setVisibility(state: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    value: state,
  })
  window.dispatchEvent(new Event('visibilitychange'))
}

async function settleRequest(request: Promise<URL>): Promise<void> {
  await act(async () => {
    await request
    await Promise.resolve()
    await Promise.resolve()
  })
}

beforeAll(async () => {
  originalRouterURL = process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL
  server = createServer((req, res) => {
    const url = new URL(req.url ?? '/', origin)
    requests.push(url)
    const waiterIndex = requestWaiters.findIndex(({ matches }) => matches(url))
    const waiter = waiterIndex >= 0 ? requestWaiters.splice(waiterIndex, 1)[0] : undefined
    const response = responseFor(url)
    const body = json(response.body)
    res.writeHead(response.status, {
      'content-type': 'application/json',
      'content-length': Buffer.byteLength(body),
    })
    res.end(body, () => waiter?.resolve(url))
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('finance test server did not bind')
  origin = `http://127.0.0.1:${address.port}`
  process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL = origin
  financeHooks = await import('@/hooks/api/useFinance')
})

afterEach(() => {
  vi.useRealTimers()
  requests = []
  requestWaiters.splice(0)
  setVisibility('visible')
})

afterAll(async () => {
  if (originalRouterURL === undefined) {
    delete process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL
  } else {
    process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL = originalRouterURL
  }
  await new Promise<void>((resolve, reject) => {
    server.close((error) => (error ? reject(error) : resolve()))
  })
})

describe('finance query cadence', () => {
  it('assigns the documented beat to each freshness class', () => {
    const client = queryClient()
    const queryWrapper = wrapper(client)

    const quote = renderHook(() => financeHooks.useQuote('aapl', false), {
      wrapper: queryWrapper,
    })
    const intraday = renderHook(() => financeHooks.useSeries('aapl', '1D', false), {
      wrapper: queryWrapper,
    })
    const daily = renderHook(() => financeHooks.useSeries('aapl', '1Y', false), {
      wrapper: queryWrapper,
    })
    const news = renderHook(() => financeHooks.useNews('market', undefined, false), {
      wrapper: queryWrapper,
    })
    const earnings = renderHook(() => financeHooks.useEarnings('aapl', false), {
      wrapper: queryWrapper,
    })

    expect(
      client.getQueryCache().find({ queryKey: qk.financeQuote('aapl') })?.options,
    ).toMatchObject({
      enabled: false,
      refetchInterval: 15_000,
      refetchIntervalInBackground: false,
      staleTime: 7_500,
    })
    expect(
      client.getQueryCache().find({ queryKey: qk.financeSeries('aapl', '1D') })?.options,
    ).toMatchObject({
      refetchInterval: 60_000,
      staleTime: 30_000,
    })
    expect(
      client.getQueryCache().find({ queryKey: qk.financeSeries('aapl', '1Y') })?.options,
    ).toMatchObject({
      refetchInterval: 300_000,
      staleTime: 150_000,
    })
    expect(
      client.getQueryCache().find({ queryKey: qk.financeNews('market') })?.options,
    ).toMatchObject({
      refetchInterval: 180_000,
      staleTime: 90_000,
    })
    expect(
      client.getQueryCache().find({ queryKey: qk.financeEarnings('aapl') })?.options,
    ).toMatchObject({
      refetchInterval: 1_800_000,
      staleTime: 900_000,
    })

    quote.unmount()
    intraday.unmount()
    daily.unmount()
    news.unmount()
    earnings.unmount()
    client.clear()
  })

  it('pauses polling while hidden and resumes when visible', async () => {
    const client = queryClient()
    const initial = nextRequest(
      (url) => url.pathname === '/finance/quote' && url.searchParams.get('symbol') === 'AAPL',
    )
    const view = renderHook(() => financeHooks.useQuote('AAPL'), {
      wrapper: wrapper(client),
    })
    await settleRequest(initial)
    await waitFor(() => expect(view.result.current.data?.price).toBe(213.55))
    vi.useFakeTimers()
    view.rerender()

    act(() => setVisibility('hidden'))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(45_000)
    })
    expect(requests).toHaveLength(1)

    const resumed = nextRequest((url) => url.pathname === '/finance/quote')
    await act(async () => {
      setVisibility('visible')
      await vi.advanceTimersByTimeAsync(15_000)
    })
    await settleRequest(resumed)
    expect(requests).toHaveLength(2)

    view.unmount()
    client.clear()
  })

  it('stops polling when the hook unmounts', async () => {
    const client = queryClient()
    const initial = nextRequest((url) => url.pathname === '/finance/quote')
    const view = renderHook(() => financeHooks.useQuote('AAPL'), {
      wrapper: wrapper(client),
    })
    await settleRequest(initial)
    await waitFor(() => expect(view.result.current.isSuccess).toBe(true))
    vi.useFakeTimers()
    view.rerender()
    view.unmount()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000)
    })
    expect(requests).toHaveLength(1)
    client.clear()
  })
})

describe('finance failures', () => {
  it('surfaces a missing provider key as typed query state', async () => {
    const client = queryClient()
    const view = renderHook(() => financeHooks.useQuote('MISS'), {
      wrapper: wrapper(client),
    })

    await waitFor(() => expect(view.result.current.isError).toBe(true))
    expect(financeHooks.failureOf(view.result.current.error)).toEqual({
      kind: 'not_configured',
      message: 'FMP is not configured.',
      provider: 'fmp',
    })
    expect(requests.map((request) => request.pathname + request.search)).toEqual([
      '/finance/quote?symbol=MISS',
    ])

    view.unmount()
    client.clear()
  })
})
