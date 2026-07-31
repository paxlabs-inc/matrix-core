import { render as renderRTL, screen, within } from '@testing-library/react'
import { NextIntlClientProvider } from 'next-intl'
import type { ReactNode } from 'react'
import { describe, expect, it } from 'vitest'

import {
  MarketAsOf,
  MarketPanel,
  MarketSkeleton,
  MarketUnavailable,
} from '@/components/matrix/finance/market-panel'
import {
  MarketAnalysts,
  MarketIdentity,
  MarketProfileRail,
  MarketQuoteBar,
  MarketStatsBar,
} from '@/components/matrix/finance/market-quote-bar'
import {
  MarketMoverRow,
  MarketNewsRow,
  MarketQuoteRow,
  MarketSectorRows,
  MarketStripCard,
} from '@/components/matrix/finance/market-lists'
import type { FundamentalSummary, Profile, Quote } from '@/lib/api/finance'
import messages from '@/messages/de.json'

function render(ui: ReactNode) {
  return renderRTL(ui, {
    wrapper: ({ children }) => (
      <NextIntlClientProvider locale="de" messages={messages}>
        {children}
      </NextIntlClientProvider>
    ),
  })
}

const source = {
  provider: 'fmp' as const,
  fetched_at: '2026-07-27T14:00:00Z',
}

const quote: Quote = {
  symbol: 'SAP',
  name: 'SAP SE',
  exchange: 'XETRA',
  currency: 'EUR',
  price: 1_234.5,
  change: 12.3,
  change_percent: 1.25,
  volume: 2_500_000,
  source,
}

describe('market panel states', () => {
  it('renders loading as skeleton rows, never numeric placeholders', () => {
    const { container } = render(
      <MarketPanel title="Movers">
        <MarketSkeleton rows={4} />
      </MarketPanel>,
    )

    expect(screen.getByRole('heading', { name: 'Movers' })).toBeInTheDocument()
    expect(container.querySelectorAll('[aria-hidden="true"] > div')).toHaveLength(4)
    expect(container).not.toHaveTextContent('0')
    expect(container).not.toHaveTextContent('—')
  })

  it('renders typed provider degradation and a plain fallback state', () => {
    const { rerender } = render(
      <MarketUnavailable
        failure={{
          kind: 'not_configured',
          message: 'Market quotes are unavailable until the provider is configured.',
          provider: 'fmp',
        }}
      />,
    )
    expect(screen.getByRole('status')).toHaveTextContent(
      'Market quotes are unavailable until the provider is configured.',
    )

    rerender(<MarketUnavailable what="Sector performance" />)
    expect(screen.getByRole('status')).toHaveTextContent(
      'Sector performance kann derzeit nicht geladen werden.',
    )
  })

  it('labels stale and fallback provenance instead of presenting it as live', () => {
    const { rerender } = render(
      <MarketAsOf source={{ ...source, stale: true, fetched_at: new Date().toISOString() }} />,
    )
    expect(screen.getByText(/Zuletzt verfügbar/)).toHaveTextContent(
      'der Anbieter antwortet derzeit nicht',
    )

    rerender(
      <MarketAsOf source={{ ...source, fallback: true, fetched_at: new Date().toISOString() }} />,
    )
    expect(screen.getByText(/Aktualisiert/)).toHaveTextContent('Ersatzquelle')
  })
})

describe('quote, stats, and company rails', () => {
  it('locale-formats real figures and renders absent stats as dashes, never zeros', () => {
    const { container } = render(
      <>
        <MarketIdentity quote={quote} />
        <MarketQuoteBar quote={quote} sessionOpen={false} />
        <MarketStatsBar quote={quote} />
      </>,
    )

    expect(screen.getByRole('heading', { name: 'SAP SE' })).toBeInTheDocument()
    expect(screen.getByText('SAP · XETRA')).toBeInTheDocument()
    expect(screen.getByText('1.234,50')).toBeInTheDocument()
    expect(screen.getByText('+12,30 +1,25%')).toBeInTheDocument()
    expect(screen.getByText('2,5 Mio.')).toBeInTheDocument()
    expect(screen.getByText('Schlusskurs')).toBeInTheDocument()
    expect(screen.queryByText('Nachbörslich')).not.toBeInTheDocument()
    expect(screen.getAllByText('—').length).toBeGreaterThan(5)
    expect(container).not.toHaveTextContent('0,00')
  })

  it('shows an extended-hours print only when it carries a real price', () => {
    render(
      <MarketQuoteBar
        quote={quote}
        sessionOpen={false}
        extended={{ price: 1_240, as_of: '2026-07-27T15:15:00Z' }}
      />,
    )

    expect(screen.getByText('Nachbörslich · 17:15')).toBeInTheDocument()
    expect(screen.getByText('1.240,00')).toBeInTheDocument()
    expect(screen.getByText('+5,50 +0,45%')).toBeInTheDocument()
  })

  it('keeps missing profile fields absent and renders real analyst totals and targets', () => {
    const profile: Profile = {
      symbol: 'SAP',
      name: 'SAP SE',
      sector: 'Technology',
      employees: 107_602,
      description: 'Enterprise software.',
      source,
    }
    const fundamentals: FundamentalSummary = {
      symbol: 'SAP',
      analysts: {
        strong_buy: 3,
        buy: 2,
        hold: 1,
        consensus: 'Buy',
        target_low: 1_100,
        target_mean: 1_300,
        target_high: 1_500,
      },
      source,
    }
    render(
      <>
        <MarketProfileRail profile={profile} />
        <MarketAnalysts fundamentals={fundamentals} />
      </>,
    )

    expect(screen.getByText('107.602')).toBeInTheDocument()
    expect(screen.getByText('Enterprise software.')).toBeInTheDocument()
    expect(screen.getAllByText('—')).toHaveLength(5)
    expect(screen.getByText('Buy')).toBeInTheDocument()
    expect(screen.getByText('6 Analysten')).toBeInTheDocument()
    expect(screen.getByText('1.300,00')).toBeInTheDocument()
  })
})

describe('market list components', () => {
  it('renders quotes, movers, sectors, and news with honest missing values', () => {
    const { container } = render(
      <>
        <MarketStripCard quote={quote} />
        <MarketMoverRow mover={{ symbol: 'BMW', change_percent: -2.5 }} />
        <MarketQuoteRow quote={{ symbol: 'DAX', source }} />
        <MarketSectorRows
          sectors={[
            { sector: 'Technology', change_percent: 1.5 },
            { sector: 'Energy', change_percent: -0.75 },
          ]}
        />
        <MarketNewsRow
          item={{
            title: 'Markets digest the latest data',
            url: 'https://example.com/markets',
            symbols: ['SAP', 'BMW'],
          }}
        />
      </>,
    )

    expect(screen.getByText('SAP SE')).toBeInTheDocument()
    const mover = screen.getByText('BMW').parentElement?.parentElement
    expect(mover).toBeTruthy()
    expect(within(mover as HTMLElement).getByText('−2,50%')).toBeInTheDocument()
    expect(screen.getByText('+1,50%')).toBeInTheDocument()
    expect(screen.getByText('−0,75%')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Markets digest the latest data/ })).toHaveAttribute(
      'href',
      'https://example.com/markets',
    )
    expect(screen.getByText('— · SAP, BMW')).toBeInTheDocument()
    expect(container.innerHTML).not.toContain('undefined')
    expect(container.innerHTML).not.toContain('NaN')
  })
})
