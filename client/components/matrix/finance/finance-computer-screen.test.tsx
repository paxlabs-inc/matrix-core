import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NextIntlClientProvider } from 'next-intl'
import { describe, expect, it } from 'vitest'

import { NeoComputer } from '@/components/matrix/neo/neo-computer'
import { buildTaskFromTrace } from '@/hooks/api/useChat'
import type { TraceEvent } from '@/lib/api/conversations'
import messages from '@/messages/en.json'

function renderComputer(events: TraceEvent[]) {
  const task = buildTaskFromTrace(events, 'finance-run', 'Check Apple')
  const view = render(
    <NextIntlClientProvider locale="en" messages={messages}>
      <NeoComputer task={task} phase="idle" reduce showMedia={false} />
    </NextIntlClientProvider>,
  )
  return { task, ...view }
}

describe('finance work on Neo’s Computer', () => {
  it('updates a live screen in place instead of remounting the Computer', () => {
    const quoteEvent = (seq: number, price: number): TraceEvent => ({
      seq,
      ts: `2026-07-27T14:0${seq}:00Z`,
      type: 'tool.finance',
      fields: {
        id: 'quote-live',
        tool: 'market_quote',
        ok: true,
        payload: {
          tool: 'market_quote',
          quote: {
            symbol: 'AAPL',
            name: 'Apple Inc.',
            exchange: 'NASDAQ',
            price,
            change: price - 209,
            change_percent: 1,
            previous_close: 209,
            source: 'fmp',
            as_of: `2026-07-27T14:0${seq}:00Z`,
          },
        },
      },
    })
    const events = [quoteEvent(1, 210)]
    const { container, rerender } = renderComputer(events)
    const computer = container.querySelector('[data-neo-computer]')
    const liveScreen = container.querySelector('[data-computer-screen="finance-quote-live"]')

    expect(computer).not.toBeNull()
    expect(liveScreen).not.toBeNull()
    expect(screen.getByText('210.00')).toBeInTheDocument()

    const updatedTask = buildTaskFromTrace([...events, quoteEvent(2, 213.55)], 'finance-run')
    rerender(
      <NextIntlClientProvider locale="en" messages={messages}>
        <NeoComputer task={updatedTask} phase="idle" reduce showMedia={false} />
      </NextIntlClientProvider>,
    )

    expect(container.querySelector('[data-neo-computer]')).toBe(computer)
    expect(container.querySelector('[data-computer-screen="finance-quote-live"]')).toBe(liveScreen)
    expect(screen.getByText('213.55')).toBeInTheDocument()
  })

  it('folds durable tool events and renders quote and chart screens through the shared spine', async () => {
    const user = userEvent.setup()
    const events: TraceEvent[] = [
      {
        seq: 1,
        ts: '2026-07-27T14:00:00Z',
        type: 'tool.finance',
        fields: {
          id: 'quote-1',
          tool: 'market_quote',
          ok: true,
          payload: {
            tool: 'market_quote',
            quote: {
              symbol: 'AAPL',
              name: 'Apple Inc.',
              exchange: 'NASDAQ',
              price: 210,
              change: 1,
              change_percent: 0.48,
              previous_close: 209,
              source: 'fmp',
              as_of: '2026-07-27T14:00:00Z',
            },
          },
        },
      },
      {
        seq: 2,
        ts: '2026-07-27T14:01:00Z',
        type: 'tool.finance',
        fields: {
          id: 'quote-1',
          tool: 'market_quote',
          ok: true,
          payload: {
            tool: 'market_quote',
            quote: {
              symbol: 'AAPL',
              name: 'Apple Inc.',
              exchange: 'NASDAQ',
              price: 213.55,
              change: 4.55,
              change_percent: 2.18,
              previous_close: 209,
              source: 'fmp',
              as_of: '2026-07-27T14:01:00Z',
            },
          },
        },
      },
      {
        seq: 3,
        ts: '2026-07-27T14:02:00Z',
        type: 'tool.finance',
        fields: {
          id: 'series-1',
          tool: 'market_series',
          ok: true,
          payload: {
            tool: 'market_series',
            range: '1M',
            series: {
              symbol: 'AAPL',
              interval: '1day',
              points: [
                { t: '2026-06-27T00:00:00Z', c: 200 },
                { t: '2026-07-27T00:00:00Z', c: 213.55 },
              ],
              source: 'fmp',
              as_of: '2026-07-27T14:02:00Z',
            },
          },
        },
      },
    ]
    const { task, container } = renderComputer(events)

    expect(task.finance).toHaveLength(2)
    expect(task.finance?.[0].payload).toMatchObject({
      quote: { price: 213.55 },
    })
    expect(screen.getByRole('img', { name: /AAPL over 1M/ })).toBeInTheDocument()
    expect(container.querySelector('[data-finance-instrument]')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'AAPL quote' }))
    expect(screen.getByRole('heading', { name: 'Apple Inc.' })).toBeInTheDocument()
    expect(screen.getByText('213.55')).toBeInTheDocument()
    expect(container.querySelector('[data-finance-instrument]')).toBeInTheDocument()
  })

  it('renders a structured provider failure as an honest finance screen', () => {
    renderComputer([
      {
        seq: 1,
        type: 'tool.finance',
        fields: {
          id: 'quote-failed',
          tool: 'market_quote',
          ok: false,
          payload: {
            ok: false,
            tool: 'market_quote',
            kind: 'not_configured',
            error: 'Market data is not configured.',
          },
        },
      },
    ])

    expect(screen.getByRole('status')).toHaveTextContent('Market data is not configured.')
  })

  it('renders generated finance research with its terminal grounding', () => {
    renderComputer([
      {
        seq: 1,
        type: 'tool.finance',
        fields: {
          id: 'research-1',
          tool: 'market_research_get',
          ok: true,
          payload: {
            tool: 'market_research_get',
            research: {
              id: 'agent_run_1',
              status: 'completed',
              subject: 'AAPL',
              output: {
                text: 'Demand remains the central debate.',
                structured: { ticker: 'AAPL', key_debates: ['Demand'] },
                grounding: [
                  {
                    field: 'key_debates[0]',
                    citations: [{ url: 'https://www.sec.gov/aapl', title: 'Apple filing' }],
                  },
                ],
              },
            },
          },
        },
      },
    ])

    expect(
      screen.getByText('Generated synthesis. Verify factual claims against the attached evidence.'),
    ).toBeInTheDocument()
    expect(screen.getByText('Demand remains the central debate.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Apple filing' })).toHaveAttribute(
      'href',
      'https://www.sec.gov/aapl',
    )
  })
})
