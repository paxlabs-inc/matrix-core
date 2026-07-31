'use client'

import { useLocale, useTranslations } from 'next-intl'
import type {
  FinanceFailureKind,
  FinanceSource,
  FundamentalSummary,
  Interval,
  Mover,
  NewsItem,
  Profile,
  Quote,
  Range,
  SectorPerformance,
  Series,
} from '@/lib/api/finance'
import { RANGES } from '@/lib/api/finance'
import type { NeoFinanceEvent } from '@/hooks/api/useChat'
import { FinanceInstrumentCore } from '@/components/matrix/finance/finance-instrument'
import {
  MarketAsOf,
  MarketPanel,
  MarketStat,
  MarketUnavailable,
} from '@/components/matrix/finance/market-panel'
import {
  MarketAnalysts,
  MarketProfileRail,
  MarketStatsBar,
} from '@/components/matrix/finance/market-quote-bar'
import {
  MarketMoverRow,
  MarketNewsRow,
  MarketQuoteRow,
  MarketSectorRows,
} from '@/components/matrix/finance/market-lists'
import { formatCompact, formatDate, formatPrice } from '@/lib/finance/format'

function record(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

function rows(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value)
    ? value.map(record).filter((item): item is Record<string, unknown> => item !== null)
    : []
}

function text(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined
}

function number(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

function groundingLinks(value: unknown): Array<{ url: string; title: string }> {
  const seen = new Set<string>()
  return rows(value).flatMap((group) =>
    rows(group.citations).flatMap((citation) => {
      const url = text(citation.url)
      if (!url || seen.has(url)) return []
      seen.add(url)
      return [{ url, title: text(citation.title) ?? url }]
    }),
  )
}

function GroundedResearchPanel({ payload }: { payload: Record<string, unknown> }) {
  const t = useTranslations('finance')
  const research = record(payload.research)
  const verification = record(payload.verification)
  const runOutput = record(record(research?.output)?.structured)
  const output = record(research?.output) ?? record(verification?.output)
  const grounding = groundingLinks(output?.grounding)
  const status = text(research?.status)
  const subject = text(research?.subject)
  const narrative = text(output?.text)
  const structured = runOutput ?? record(output?.content)

  if (status === 'queued' || status === 'running') {
    return (
      <MarketPanel title={subject ? `${subject} ${t('groundedResearch')}` : t('groundedResearch')}>
        <p className="text-muted-foreground text-sm">
          {status === 'queued' ? t('researchQueued') : t('researchRunning')}
        </p>
      </MarketPanel>
    )
  }

  return (
    <MarketPanel title={subject ? `${subject} ${t('groundedResearch')}` : t('verifiedFacts')}>
      <div className="flex flex-col gap-3">
        <p className="text-muted-foreground text-xs">{t('generatedSynthesis')}</p>
        {narrative ? (
          <p className="text-foreground text-sm whitespace-pre-wrap">{narrative}</p>
        ) : null}
        {structured ? (
          <pre className="bg-muted/40 text-foreground max-h-80 overflow-auto rounded-lg p-3 text-xs whitespace-pre-wrap">
            {JSON.stringify(structured, null, 2)}
          </pre>
        ) : null}
        {grounding.length > 0 ? (
          <div className="flex flex-col gap-2">
            <p className="text-muted-foreground text-xs font-medium">{t('evidenceSources')}</p>
            {grounding.map((citation) => (
              <a
                key={citation.url}
                href={citation.url}
                target="_blank"
                rel="noreferrer"
                className="bg-muted/40 text-foreground rounded-lg px-3 py-2 text-xs underline-offset-4 hover:underline"
              >
                {citation.title}
              </a>
            ))}
          </div>
        ) : null}
      </div>
    </MarketPanel>
  )
}

function sourceOf(value: Record<string, unknown>): FinanceSource {
  return {
    provider: value.source === 'alphavantage' ? 'alphavantage' : 'fmp',
    fetched_at: text(value.as_of) ?? new Date(0).toISOString(),
    stale: typeof value.note === 'string' ? true : undefined,
    fallback: typeof value.provider_note === 'string' ? true : undefined,
  }
}

function quoteOf(value: unknown): Quote | undefined {
  const item = record(value)
  const symbol = text(item?.symbol)
  if (!item || !symbol) return undefined
  return {
    symbol,
    name: text(item.name),
    exchange: text(item.exchange),
    class:
      item.asset_class === 'equity' ||
      item.asset_class === 'index' ||
      item.asset_class === 'crypto' ||
      item.asset_class === 'forex' ||
      item.asset_class === 'commodity'
        ? item.asset_class
        : undefined,
    price: number(item.price),
    change: number(item.change),
    change_percent: number(item.change_percent),
    open: number(item.open),
    previous_close: number(item.previous_close),
    volume: number(item.volume),
    market_cap: number(item.market_cap),
    as_of: text(item.quote_time),
    extended: record(item.extended_hours)
      ? {
          price: number(record(item.extended_hours)?.price),
          bid_price: number(record(item.extended_hours)?.bid),
          ask_price: number(record(item.extended_hours)?.ask),
          as_of: text(record(item.extended_hours)?.as_of),
        }
      : undefined,
    source: sourceOf(item),
  }
}

function seriesOf(value: unknown): Series | undefined {
  const item = record(value)
  const symbol = text(item?.symbol)
  if (!item || !symbol) return undefined
  const points = rows(item.points)
    .map((point) => ({ t: text(point.t), c: number(point.c) }))
    .filter(
      (point): point is { t: string; c: number } => point.t !== undefined && point.c !== undefined,
    )
  if (points.length === 0) return undefined
  const validIntervals: Interval[] = [
    '1min',
    '5min',
    '15min',
    '30min',
    '1hour',
    '4hour',
    '1day',
    '1week',
    '1month',
  ]
  const interval = validIntervals.includes(item.interval as Interval)
    ? (item.interval as Interval)
    : '1day'
  return {
    symbol,
    interval,
    candles: points.map((point) => ({
      t: point.t,
      o: point.c,
      h: point.c,
      l: point.c,
      c: point.c,
    })),
    source: sourceOf(item),
  }
}

function profileOf(value: unknown): Profile | undefined {
  const item = record(value)
  const symbol = text(item?.symbol)
  if (!item || !symbol) return undefined
  return {
    symbol,
    name: text(item.name),
    exchange_name: text(item.exchange),
    sector: text(item.sector),
    industry: text(item.industry),
    country: text(item.country),
    ceo: text(item.ceo),
    employees: number(item.employees),
    ipo_date: text(item.ipo_date),
    website: text(item.website),
    currency: text(item.currency),
    market_cap: number(item.market_cap),
    beta: number(item.beta),
    description: text(item.description),
    source: sourceOf(item),
  }
}

function fundamentalsOf(value: unknown): FundamentalSummary | undefined {
  const item = record(value)
  const symbol = text(item?.symbol)
  if (!item || !symbol) return undefined
  const analysts = record(item.analysts)
  return {
    symbol,
    market_cap: number(item.market_cap),
    pe_ratio: number(item.pe_ratio),
    price_to_book: number(item.price_to_book),
    price_to_sales: number(item.price_to_sales),
    dividend_yield:
      number(item.dividend_yield_percent) === undefined
        ? undefined
        : number(item.dividend_yield_percent)! / 100,
    eps: number(item.eps),
    return_on_equity:
      number(item.return_on_equity_percent) === undefined
        ? undefined
        : number(item.return_on_equity_percent)! / 100,
    net_profit_margin:
      number(item.net_profit_margin_percent) === undefined
        ? undefined
        : number(item.net_profit_margin_percent)! / 100,
    debt_to_equity: number(item.debt_to_equity),
    current_ratio: number(item.current_ratio),
    enterprise_value: number(item.enterprise_value),
    analysts: analysts
      ? {
          strong_buy: number(analysts.strong_buy),
          buy: number(analysts.buy),
          hold: number(analysts.hold),
          sell: number(analysts.sell),
          strong_sell: number(analysts.strong_sell),
          consensus: text(analysts.consensus),
          target_low: number(analysts.price_target_low),
          target_mean: number(analysts.price_target_mean),
          target_high: number(analysts.price_target_high),
        }
      : undefined,
    source: sourceOf(item),
  }
}

function rangeOf(value: unknown): Range {
  return RANGES.includes(value as Range) ? (value as Range) : '1M'
}

export interface FinanceEventLabels {
  quote: string
  chart: string
  movers: string
  sectors: string
  news: string
  status: string
  economicData: string
  markets: string
}

const defaultEventLabels: FinanceEventLabels = {
  quote: 'quote',
  chart: 'chart',
  movers: 'Movers',
  sectors: 'Sectors',
  news: 'Market news',
  status: 'Market status',
  economicData: 'Economic data',
  markets: 'Markets',
}

export function financeEventLabel(
  event: NeoFinanceEvent,
  labels: FinanceEventLabels = defaultEventLabels,
): string {
  const payload = event.payload
  const nested =
    record(payload.quote) ??
    record(payload.series) ??
    record(payload.profile) ??
    record(payload.fundamentals)
  const symbol = text(nested?.symbol) ?? text(payload.symbol)
  if (symbol) {
    if (event.tool === 'market_quote') return `${symbol} ${labels.quote}`
    if (event.tool === 'market_series') return `${symbol} ${labels.chart}`
    return symbol
  }
  switch (event.tool) {
    case 'market_research_start':
    case 'market_research_get':
      return text(record(payload.research)?.subject) ?? labels.markets
    case 'market_verify_facts':
      return text(payload.symbol) ?? labels.markets
    case 'market_movers':
      return labels.movers
    case 'market_sectors':
      return labels.sectors
    case 'market_news':
      return labels.news
    case 'market_status':
      return labels.status
    case 'market_macro':
      return text(payload.series) ?? labels.economicData
    default:
      return labels.markets
  }
}

export function FinanceComputerScreen({ event }: { event: NeoFinanceEvent }) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const eventLabels: FinanceEventLabels = {
    quote: t('quoteLabel'),
    chart: t('chartLabel'),
    movers: t('movers'),
    sectors: t('sectors'),
    news: t('news'),
    status: t('status'),
    economicData: t('economicData'),
    markets: t('title'),
  }
  const payload = event.payload
  if (!event.ok || payload.ok === false) {
    return (
      <MarketUnavailable
        failure={{
          kind: (text(payload.kind) as FinanceFailureKind) ?? 'upstream',
          message: text(payload.error) ?? t('marketDataUnavailable'),
        }}
      />
    )
  }

  if (
    event.tool === 'market_research_start' ||
    event.tool === 'market_research_get' ||
    event.tool === 'market_verify_facts'
  ) {
    return <GroundedResearchPanel payload={payload} />
  }

  if (event.tool === 'market_quote') {
    const quote = quoteOf(payload.quote)
    if (quote) {
      return (
        <FinanceInstrumentCore
          quote={quote}
          extended={quote.extended}
          range="1D"
          chartLabel={t('chart')}
        />
      )
    }
  }

  if (event.tool === 'market_series') {
    const series = seriesOf(payload.series)
    if (series) {
      return (
        <FinanceInstrumentCore
          series={series}
          range={rangeOf(payload.range)}
          chartLabel={t('chart')}
        />
      )
    }
  }

  if (event.tool === 'market_quotes') {
    const quotes = rows(payload.quotes)
      .map(quoteOf)
      .filter((item): item is Quote => !!item)
    if (quotes.length > 0) {
      return (
        <MarketPanel title={t('marketLists')}>
          <div className="flex flex-col">
            {quotes.map((item) => (
              <MarketQuoteRow key={item.symbol} quote={item} />
            ))}
          </div>
          <MarketAsOf source={quotes[0].source} />
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_movers') {
    const movers: Mover[] = rows(payload.movers).flatMap((item) => {
      const symbol = text(item.symbol)
      return symbol
        ? [
            {
              symbol,
              name: text(item.name),
              price: number(item.price),
              change_percent: number(item.change_percent),
              volume: number(item.volume),
            },
          ]
        : []
    })
    if (movers.length > 0) {
      return (
        <MarketPanel title={t('movers')}>
          {movers.map((item) => (
            <MarketMoverRow key={item.symbol} mover={item} />
          ))}
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_sectors') {
    const sectors: SectorPerformance[] = rows(payload.sectors).flatMap((item) => {
      const sector = text(item.sector)
      const change = number(item.change_percent)
      return sector && change !== undefined ? [{ sector, change_percent: change }] : []
    })
    if (sectors.length > 0) {
      return (
        <MarketPanel title={t('sectors')}>
          <MarketSectorRows sectors={sectors} />
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_news') {
    const items: NewsItem[] = rows(payload.items).flatMap((item) => {
      const title = text(item.title)
      const url = text(item.url)
      return title && url
        ? [
            {
              title,
              url,
              publisher: text(item.publisher),
              summary: text(item.summary),
              published_at: text(item.published),
              symbols: Array.isArray(item.symbols)
                ? item.symbols.map(text).filter((symbol): symbol is string => !!symbol)
                : undefined,
            },
          ]
        : []
    })
    if (items.length > 0) {
      return (
        <MarketPanel title={t('news')}>
          {items.map((item) => (
            <MarketNewsRow key={item.url} item={item} />
          ))}
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_profile') {
    const profile = profileOf(payload.profile)
    if (profile) {
      return (
        <MarketPanel title={t('about')}>
          <MarketProfileRail profile={profile} />
          <MarketAsOf source={profile.source} />
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_fundamentals') {
    const fundamentals = fundamentalsOf(payload.fundamentals)
    if (fundamentals) {
      const quote: Quote = { symbol: fundamentals.symbol, source: fundamentals.source }
      return (
        <div className="flex flex-col gap-3">
          <MarketPanel title={t('fundamentals')}>
            <MarketStatsBar quote={quote} fundamentals={fundamentals} />
          </MarketPanel>
          {fundamentals.analysts ? (
            <MarketPanel title={t('analysts')}>
              <MarketAnalysts fundamentals={fundamentals} />
            </MarketPanel>
          ) : null}
        </div>
      )
    }
  }

  if (event.tool === 'market_earnings' || event.tool === 'market_dividends') {
    const items = rows(payload.events)
    if (items.length > 0) {
      return (
        <MarketPanel title={event.tool === 'market_earnings' ? t('earnings') : t('dividends')}>
          <div className="flex flex-col gap-2">
            {items.map((item, index) => (
              <div
                key={`${text(item.date) ?? index}`}
                className="bg-muted/40 grid grid-cols-3 gap-3 rounded-lg px-3 py-2"
              >
                <MarketStat label={t('date')} value={formatDate(text(item.date), locale)} />
                <MarketStat
                  label={event.tool === 'market_earnings' ? t('epsActual') : t('amount')}
                  value={formatPrice(
                    event.tool === 'market_earnings'
                      ? number(item.eps_actual)
                      : number(item.amount),
                    undefined,
                    locale,
                  )}
                />
                <MarketStat
                  label={event.tool === 'market_earnings' ? t('epsEstimate') : t('frequency')}
                  value={
                    event.tool === 'market_earnings'
                      ? formatPrice(number(item.eps_estimated), undefined, locale)
                      : (text(item.frequency) ?? '—')
                  }
                />
              </div>
            ))}
          </div>
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_status') {
    const sessions = rows(payload.sessions)
    if (sessions.length > 0) {
      return (
        <MarketPanel title={t('status')}>
          <div className="flex flex-col gap-2">
            {sessions.map((session, index) => (
              <div
                key={`${text(session.exchange) ?? index}`}
                className="bg-muted/40 flex items-center gap-3 rounded-lg px-3 py-2"
              >
                <span className="text-foreground flex-1 text-sm">
                  {text(session.region) ?? text(session.exchange)}
                </span>
                <span className="text-muted-foreground text-xs">
                  {session.is_open === true ? t('open') : t('closed')}
                </span>
              </div>
            ))}
          </div>
        </MarketPanel>
      )
    }
  }

  if (event.tool === 'market_macro') {
    const points = rows(payload.points).slice(-12)
    if (points.length > 0) {
      return (
        <MarketPanel title={text(payload.series) ?? t('economicData')}>
          <div className="flex flex-col gap-2">
            {points.map((point, index) => (
              <div
                key={`${text(point.date) ?? index}`}
                className="bg-muted/40 grid grid-cols-2 gap-3 rounded-lg px-3 py-2"
              >
                <MarketStat label={t('date')} value={formatDate(text(point.date), locale)} />
                <MarketStat
                  label={text(payload.unit) ?? t('amount')}
                  value={formatCompact(number(point.value), locale)}
                />
              </div>
            ))}
          </div>
        </MarketPanel>
      )
    }
  }

  return <MarketUnavailable what={financeEventLabel(event, eventLabels)} />
}
