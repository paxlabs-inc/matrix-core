import { createServer, type Server } from 'node:http'
import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NextIntlClientProvider } from 'next-intl'
import {
  AppRouterContext,
  type AppRouterInstance,
} from 'next/dist/shared/lib/app-router-context.shared-runtime'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import messages from '@/messages/en.json'

type FinanceSurface = typeof import('@/components/matrix/finance/finance-surface')

const source = {
  provider: 'fmp',
  fetched_at: '2026-07-27T14:00:00Z',
}

let server: Server
let origin = ''
let surface: FinanceSurface
let requests: URL[] = []
let researchBodies: Array<Record<string, unknown>> = []
let originalRouterURL: string | undefined

function quote(symbol: string, name: string, price = 100) {
  return {
    symbol,
    name,
    exchange: 'NASDAQ',
    class: 'equity',
    price,
    change: 1,
    change_percent: 1,
    previous_close: price - 1,
    source,
  }
}

function homeResponse() {
  return {
    panels: {
      strip: {
        data: {
          quotes: [quote('^GSPC', 'S&P 500', 6_350)],
          source,
        },
      },
      gainers: {
        data: {
          kind: 'gainers',
          movers: [{ symbol: 'NVDA', name: 'NVIDIA', price: 190, change_percent: 4.5 }],
          source,
        },
      },
      losers: {
        data: {
          kind: 'losers',
          movers: [{ symbol: 'INTC', name: 'Intel', price: 22, change_percent: -3.5 }],
          source,
        },
      },
      active: {
        data: {
          kind: 'active',
          movers: [],
          source,
        },
      },
      sectors: {
        error: {
          kind: 'upstream',
          message: 'Sector performance is temporarily unavailable.',
          provider: 'fmp',
        },
      },
      status: {
        data: {
          sessions: [{ exchange: 'NASDAQ', region: 'United States', is_open: true }],
          source,
        },
      },
      news: {
        data: {
          items: [],
          source,
        },
      },
    },
  }
}

function boardResponse(assetClass: string): { status: number; body: unknown } {
  if (assetClass === 'forex') {
    return {
      status: 400,
      body: {
        error: {
          kind: 'not_configured',
          message: 'Forex quotes are not configured.',
          provider: 'fmp',
        },
      },
    }
  }
  const items: Record<string, ReturnType<typeof quote>[]> = {
    equity: [quote('SAP', 'SAP SE', 280)],
    index: [quote('^GDAXI', 'DAX Index', 24_500)],
    crypto: [quote('BTCUSD', 'Bitcoin', 118_000)],
    commodity: [],
  }
  return {
    status: 200,
    body: {
      quotes: items[assetClass] ?? [],
      source,
    },
  }
}

function symbolResponse(symbol: string): { status: number; body: unknown } {
  if (symbol === 'MISS') {
    return {
      status: 400,
      body: {
        error: {
          kind: 'not_configured',
          message: 'Market data is not configured for this symbol.',
          provider: 'fmp',
        },
      },
    }
  }
  return {
    status: 200,
    body: {
      symbol,
      range: '1D',
      panels: {
        quote: {
          data: {
            ...quote(symbol, 'Apple Inc.', 213.55),
            open: 210,
            day_low: 209,
            day_high: 215,
            volume: 45_000_000,
          },
        },
        series: {
          data: {
            symbol,
            interval: '5min',
            candles: [
              {
                t: '2026-07-27T13:30:00Z',
                o: 210,
                h: 215,
                l: 209,
                c: 213.55,
                v: 45_000_000,
              },
            ],
            source,
          },
        },
        profile: {
          data: {
            symbol,
            name: 'Apple Inc.',
            exchange: 'NASDAQ',
            sector: 'Technology',
            description: 'Consumer technology company.',
            source,
          },
        },
        fundamentals: {
          data: {
            symbol,
            market_cap: 3_200_000_000_000,
            pe_ratio: 31.2,
            analysts: {
              strong_buy: 4,
              buy: 3,
              hold: 1,
              consensus: 'Buy',
              target_mean: 240,
            },
            source,
          },
        },
        extended: {
          error: {
            kind: 'not_found',
            message: 'No extended-hours print is available.',
          },
        },
        change: {
          data: {
            symbol,
            windows: { '1D': 1 },
            source,
          },
        },
        news: {
          data: {
            items: [
              {
                title: 'Company files quarterly report',
                url: 'https://www.sec.gov/news-source?utm_source=feed',
                publisher: 'SEC',
              },
            ],
            source,
          },
        },
      },
    },
  }
}

function responseFor(url: URL): { status: number; body: unknown } {
  if (url.pathname === '/finance/home') return { status: 200, body: homeResponse() }
  if (url.pathname === '/finance/board') {
    return boardResponse(url.searchParams.get('class') ?? '')
  }
  if (url.pathname === '/finance/symbol') {
    return symbolResponse(url.searchParams.get('symbol') ?? '')
  }
  if (url.pathname === '/finance/earnings') {
    return {
      status: 200,
      body: {
        symbol: 'AAPL',
        events: [{ symbol: 'AAPL', date: '2026-07-20', eps_actual: 2.1, eps_estimated: 2 }],
        source,
      },
    }
  }
  if (url.pathname === '/finance/dividends') {
    return {
      status: 200,
      body: {
        symbol: 'AAPL',
        events: [{ symbol: 'AAPL', date: '2026-07-15', amount: 0.25, frequency: 'Quarterly' }],
        source,
      },
    }
  }
  if (url.pathname.startsWith('/finance/research/')) {
    const id = url.pathname.split('/').at(-1) ?? 'run-1'
    return {
      status: 200,
      body: {
        run: {
          id,
          status: 'completed',
          output: {
            structured: { summary: 'Primary evidence', state: 'verified' },
            grounding: [
              {
                field: 'summary',
                confidence: 'high',
                citations: [{ url: 'https://www.sec.gov/example', title: 'SEC filing' }],
              },
            ],
          },
          costDollars: { total: 0.04 },
        },
        workflow: 'finance.equity_brief.v1',
        subject: 'AAPL',
        meta: { retrieved_at: '2026-07-31T12:00:00Z' },
      },
    }
  }
  return {
    status: 200,
    body: {
      query: url.searchParams.get('q') ?? '',
      matches: [],
      source,
    },
  }
}

const router: AppRouterInstance = {
  back: () => window.history.back(),
  forward: () => window.history.forward(),
  refresh: () => window.dispatchEvent(new Event('popstate')),
  push: (href) => window.history.pushState(null, '', href),
  replace: (href) => window.history.replaceState(null, '', href),
  prefetch: () => undefined,
}

function queryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { gcTime: Infinity },
    },
  })
}

function wrapper(client: QueryClient) {
  return function FinanceWrapper({ children }: { children: ReactNode }) {
    return (
      <AppRouterContext.Provider value={router}>
        <NextIntlClientProvider locale="en" messages={messages}>
          <QueryClientProvider client={client}>{children}</QueryClientProvider>
        </NextIntlClientProvider>
      </AppRouterContext.Provider>
    )
  }
}

beforeAll(async () => {
  originalRouterURL = process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL
  server = createServer(async (req, res) => {
    const url = new URL(req.url ?? '/', origin)
    requests.push(url)
    let response: { status: number; body: unknown }
    if (req.method === 'POST' && url.pathname === '/finance/research/start') {
      const chunks: Buffer[] = []
      for await (const chunk of req) chunks.push(Buffer.from(chunk))
      const input = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
      researchBodies.push(input)
      response = {
        status: 200,
        body: {
          run: { id: `run-${researchBodies.length}`, status: 'queued' },
          workflow: `finance.${String(input.kind)}.v1`,
          subject: input.symbol,
          meta: {},
        },
      }
    } else if (req.method === 'POST' && url.pathname === '/finance/research/verify') {
      const chunks: Buffer[] = []
      for await (const chunk of req) chunks.push(Buffer.from(chunk))
      const input = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
      researchBodies.push(input)
      response = {
        status: 200,
        body: {
          data: {
            requestId: 'verify-1',
            results: [{ title: 'Issuer filing', url: 'https://www.sec.gov/verify' }],
            output: {
              content: { pe_ratio: { value: 31.2, state: 'verified' } },
              grounding: [
                {
                  field: 'pe_ratio',
                  confidence: 'high',
                  citations: [{ url: 'https://www.sec.gov/verify', title: 'Issuer filing' }],
                },
              ],
            },
          },
          meta: {},
        },
      }
    } else if (req.method === 'POST' && url.pathname === '/finance/research/news') {
      const chunks: Buffer[] = []
      for await (const chunk of req) chunks.push(Buffer.from(chunk))
      const input = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
      researchBodies.push(input)
      response = {
        status: 200,
        body: {
          data: {
            requestId: 'news-evidence-1',
            results: [
              {
                title: 'Company filing',
                url: 'https://www.sec.gov/news-source',
                highlights: ['Reported revenue increased year over year.'],
              },
            ],
            statuses: [
              { id: 'https://www.sec.gov/news-source', status: 'success' },
              {
                id: 'https://example.com/unavailable',
                status: 'error',
                error: { tag: 'CRAWL_TIMEOUT' },
              },
            ],
            costDollars: { total: 0.003 },
          },
          meta: { retrieved_at: '2026-07-31T12:00:00Z' },
          error: { kind: 'partial', message: 'One source could not be extracted.' },
        },
      }
    } else {
      response = responseFor(url)
    }
    const body = JSON.stringify(response.body)
    res.writeHead(response.status, {
      'content-type': 'application/json',
      'content-length': Buffer.byteLength(body),
    })
    res.end(body)
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('finance route server did not bind')
  origin = `http://127.0.0.1:${address.port}`
  process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL = origin
  surface = await import('@/components/matrix/finance/finance-surface')
})

afterEach(() => {
  requests = []
  researchBodies = []
  window.history.replaceState(null, '', '/')
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

describe('MarketsHomeView', () => {
  it('renders every market class through one board and keeps empty/degraded panels honest', async () => {
    const user = userEvent.setup()
    const client = queryClient()
    render(<surface.MarketsHomeView />, { wrapper: wrapper(client) })

    expect(await screen.findByRole('heading', { name: 'Markets' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to chat' })).toHaveAttribute('href', '/')
    expect(await screen.findByText('SAP SE')).toBeInTheDocument()
    expect(
      await screen.findByText('Sector performance is temporarily unavailable.'),
    ).toBeInTheDocument()
    expect(
      await screen.findByText('Market news could not be loaded right now.'),
    ).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Indexes' }))
    expect(await screen.findByText('DAX Index')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Crypto' }))
    expect(await screen.findByText('Bitcoin')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Forex' }))
    expect(await screen.findByText('Forex quotes are not configured.')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Commodities' }))
    expect(
      await screen.findByText('Commodities could not be loaded right now.'),
    ).toBeInTheDocument()

    expect(
      requests
        .filter((request) => request.pathname === '/finance/board')
        .map((request) => request.searchParams.get('class')),
    ).toEqual(['equity', 'index', 'crypto', 'forex', 'commodity'])
    client.clear()
  })

  it('shows an honest empty state when a ranked list has no rows', async () => {
    const user = userEvent.setup()
    const client = queryClient()
    render(<surface.MarketsHomeView />, { wrapper: wrapper(client) })
    await screen.findByText('NVIDIA')

    await user.click(screen.getByRole('button', { name: 'Active' }))
    expect(await screen.findByText('Movers could not be loaded right now.')).toBeInTheDocument()
    client.clear()
  })
})

describe('SymbolView', () => {
  it('renders every required depth tab and loads tab-only market data on demand', async () => {
    const user = userEvent.setup()
    const client = queryClient()
    render(<surface.SymbolView symbol="AAPL" />, { wrapper: wrapper(client) })

    expect(await screen.findByRole('heading', { name: 'Apple Inc.' })).toBeInTheDocument()
    expect(screen.getByRole('img', { name: /AAPL over 1D/ })).toBeInTheDocument()
    expect(document.querySelector('[data-finance-instrument]')).toBeInTheDocument()
    const tablist = screen.getByRole('tablist', { name: 'Markets' })
    expect(tablist).toHaveClass('w-full', 'max-w-full', 'overflow-x-auto')
    for (const tab of [
      'Overview',
      'Financials',
      'Earnings',
      'Holders',
      'Historical data',
      'Analysis',
      'Market news',
    ]) {
      expect(within(tablist).getByRole('tab', { name: tab })).toHaveClass(
        'shrink-0',
        'whitespace-nowrap',
      )
    }
    expect(requests.some((request) => request.pathname === '/finance/earnings')).toBe(false)
    expect(requests.some((request) => request.pathname === '/finance/dividends')).toBe(false)

    await user.click(screen.getByRole('tab', { name: 'Earnings' }))
    expect(await screen.findByText('Jul 20, 2026')).toBeInTheDocument()
    expect(await screen.findByText('Quarterly')).toBeInTheDocument()
    expect(requests.filter((request) => request.pathname === '/finance/earnings')).toHaveLength(1)
    expect(requests.filter((request) => request.pathname === '/finance/dividends')).toHaveLength(1)

    await user.click(screen.getByRole('tab', { name: 'Holders' }))
    expect(await screen.findByText('Ownership and filing research')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Historical data' }))
    expect(await screen.findByText('45M')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Analysis' }))
    expect(await screen.findByText('8 analysts')).toBeInTheDocument()
    expect(screen.getByText('Buy')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Market news' }))
    expect(await screen.findByText('Company files quarterly report')).toBeInTheDocument()
    client.clear()
  })

  it('starts every grounded workflow only after an explicit action and renders terminal evidence', async () => {
    const user = userEvent.setup()
    const client = queryClient()
    render(<surface.SymbolView symbol="AAPL" />, { wrapper: wrapper(client) })

    await screen.findByRole('heading', { name: 'Apple Inc.' })
    expect(requests.some((request) => request.pathname.startsWith('/finance/research'))).toBe(false)

    await user.click(screen.getByRole('button', { name: 'Research company' }))
    expect(await screen.findByText('Primary evidence')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'SEC filing' })).toHaveAttribute(
      'href',
      'https://www.sec.gov/example',
    )
    expect(researchBodies.at(-1)).toMatchObject({ kind: 'equity_brief', symbol: 'AAPL' })

    await user.click(screen.getByRole('tab', { name: 'Financials' }))
    expect(
      requests.filter((request) => request.pathname === '/finance/research/verify'),
    ).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: 'Verify facts' }))
    expect(await screen.findByText('31.2')).toBeInTheDocument()
    expect(researchBodies.at(-1)).toMatchObject({ symbol: 'AAPL' })
    expect((researchBodies.at(-1)?.fields as string[]) ?? []).toContain('pe_ratio')

    await user.click(screen.getByRole('tab', { name: 'Earnings' }))
    expect(await screen.findByRole('button', { name: 'Research earnings' })).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Holders' }))
    await user.click(screen.getByRole('button', { name: 'Research holders' }))
    await waitFor(() =>
      expect(researchBodies.at(-1)).toMatchObject({ kind: 'enrichment', symbol: 'AAPL' }),
    )

    await user.click(screen.getByRole('tab', { name: 'Analysis' }))
    expect(screen.getByRole('button', { name: 'Research analysis' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Run risk rubric' }))
    await waitFor(() =>
      expect(researchBodies.at(-1)).toMatchObject({ kind: 'risk_rubric', symbol: 'AAPL' }),
    )

    await user.click(screen.getByRole('tab', { name: 'Market news' }))
    await user.click(await screen.findByRole('button', { name: 'Extract evidence' }))
    expect(
      await screen.findByText('Reported revenue increased year over year.'),
    ).toBeInTheDocument()
    expect(screen.getByText(/CRAWL_TIMEOUT/)).toBeInTheDocument()
    expect(researchBodies.at(-1)).toMatchObject({ symbol: 'AAPL' })
    expect((researchBodies.at(-1)?.urls as string[]) ?? []).toEqual([
      'https://www.sec.gov/news-source?utm_source=feed',
    ])
    client.clear()
  })

  it('renders a whole-route typed failure without an empty page shell', async () => {
    const client = queryClient()
    render(<surface.SymbolView symbol="MISS" />, { wrapper: wrapper(client) })

    expect(
      await screen.findByText('Market data is not configured for this symbol.'),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Back to markets' })).toHaveAttribute(
      'href',
      '/finance',
    )
    await waitFor(() =>
      expect(requests.filter((request) => request.pathname === '/finance/symbol')).toHaveLength(1),
    )
    client.clear()
  })
})
