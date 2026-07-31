import { fireEvent, render as renderRTL, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NextIntlClientProvider } from 'next-intl'
import type { ReactNode } from 'react'
import { describe, expect, it } from 'vitest'

import { MarketChart } from '@/components/matrix/finance/market-chart'
import type { Range, Series } from '@/lib/api/finance'
import messages from '@/messages/en.json'

function render(ui: ReactNode) {
  return renderRTL(ui, {
    wrapper: ({ children }) => (
      <NextIntlClientProvider locale="en" messages={messages}>
        {children}
      </NextIntlClientProvider>
    ),
  })
}

const source = {
  provider: 'fmp' as const,
  fetched_at: '2026-07-27T14:00:00Z',
}

function series(candles: Series['candles']): Series {
  return {
    symbol: 'AAPL',
    interval: '5min',
    candles,
    source,
  }
}

const regular = series([
  { t: '2026-07-27T13:30:00Z', o: 100, h: 103, l: 99, c: 102, v: 1_000 },
  { t: '2026-07-27T13:35:00Z', o: 102, h: 106, l: 101, c: 105, v: 1_400 },
  { t: '2026-07-27T13:40:00Z', o: 105, h: 107, l: 103, c: 104, v: 900 },
])

describe('MarketChart', () => {
  it('switches every supported range and both chart forms', async () => {
    const user = userEvent.setup()
    const selected: Range[] = []
    const { container } = render(
      <MarketChart
        series={regular}
        range="1D"
        onRangeChange={(range) => selected.push(range)}
        previousClose={98}
        label="AAPL"
      />,
    )

    for (const range of ['1D', '5D', '1M', '6M', 'YTD', '1Y', '5Y', 'MAX']) {
      expect(screen.getByRole('button', { name: range })).toBeInTheDocument()
    }
    await user.click(screen.getByRole('button', { name: '5Y' }))
    expect(selected).toEqual(['5Y'])

    expect(screen.getByRole('button', { name: 'Line chart' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(container.querySelector('linearGradient')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Candlestick chart' }))
    expect(screen.getByRole('button', { name: 'Candlestick chart' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(container.querySelectorAll('svg g')).toHaveLength(3)
    expect(container.querySelector('[stroke-dasharray="3 4"]')).toBeInTheDocument()
  })

  it('moves the crosshair by pointer and keyboard with an OHLCV readout', () => {
    render(<MarketChart series={regular} range="1D" previousClose={98} label="AAPL" />)
    const chart = screen.getByRole('img', { name: /AAPL over 1D/ })

    expect(screen.getByText('O 105.00')).toBeInTheDocument()
    expect(screen.getByText('Vol 900')).toBeInTheDocument()

    fireEvent.pointerMove(chart, { clientX: 8 })
    expect(screen.getByText('O 100.00')).toBeInTheDocument()
    expect(screen.getByText('Vol 1K')).toBeInTheDocument()

    fireEvent.keyDown(chart, { key: 'Escape' })
    fireEvent.keyDown(chart, { key: 'ArrowLeft' })
    expect(screen.getByText('O 102.00')).toBeInTheDocument()
    expect(screen.getByText('Vol 1.4K')).toBeInTheDocument()
  })

  it('renders empty, sparse, and single-point series without invalid geometry', () => {
    const { container, rerender } = render(
      <MarketChart series={series([])} range="MAX" label="EMPTY" />,
    )
    expect(
      screen.getByRole('img', { name: 'No price history is available for this range.' }),
    ).toBeInTheDocument()
    expect(screen.getByText('No price history for this range')).toBeInTheDocument()
    expect(container.innerHTML).not.toContain('NaN')

    rerender(
      <MarketChart
        series={series([{ t: '2026-07-01T00:00:00Z', o: 42, h: 42, l: 42, c: 42 }])}
        range="1M"
        label="ONE"
      />,
    )
    expect(screen.getByRole('img', { name: /ONE over 1M/ })).toBeInTheDocument()
    expect(screen.getByText('O 42.00')).toBeInTheDocument()
    expect(container.innerHTML).not.toContain('NaN')
    expect(container.innerHTML).not.toContain('Infinity')

    rerender(
      <MarketChart
        series={series([
          { t: '2026-01-01T00:00:00Z', o: 40, h: 44, l: 39, c: 43 },
          { t: '2026-07-01T00:00:00Z', o: 43, h: 48, l: 41, c: 47 },
        ])}
        range="1Y"
        label="SPARSE"
      />,
    )
    expect(screen.getByRole('img', { name: /SPARSE over 1Y/ })).toBeInTheDocument()
    expect(container.innerHTML).not.toContain('NaN')
  })

  it('exposes the full series summary to assistive technology', () => {
    render(<MarketChart series={regular} range="1D" label="Apple" />)

    const summary =
      'Apple over 1D: opened at 100.00, last 104.00, up +4.00%. Range low 99.00, high 107.00, across 3 bars.'
    expect(screen.getByRole('img', { name: summary })).toBeInTheDocument()
    expect(screen.getByText(summary)).toHaveClass('sr-only')
  })
})
