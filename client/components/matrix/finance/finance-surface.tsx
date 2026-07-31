'use client'

/**
 * The market surface at /finance.
 *
 * Two views through one component: the markets home (index strip, ranked lists,
 * sector board, news) and a symbol page (identity, quote, chart, stats, profile,
 * analysts, news). Both dock the composer at the bottom, so a user reading a
 * chart can ask Neo about exactly what is on screen without leaving.
 *
 * Every panel loads and degrades independently: a vendor gap greys one panel
 * with a plain-language line while the rest of the page keeps working. Nothing
 * renders a zero for a figure that did not arrive.
 */
import Link from 'next/link'
import { useMemo, useState } from 'react'
import { useLocale, useTranslations } from 'next-intl'
import { TextInput } from '@astryxdesign/core/TextInput'
import { ArrowLeft, Search } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { AssetClass, Panel, Range } from '@/lib/api/finance'
import {
  failureOf,
  useBoard,
  useDividends,
  useEarnings,
  useMarketsHome,
  useSymbolPage,
  useSymbolSearch,
} from '@/hooks/api/useFinance'
import { FinanceComposer } from '@/components/matrix/finance/finance-composer'
import { FinanceInstrumentCore } from '@/components/matrix/finance/finance-instrument'
import { GroundedResearch } from '@/components/matrix/finance/grounded-research'
import {
  MarketAsOf,
  MarketPanel,
  MarketSkeleton,
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
  MarketStripCard,
} from '@/components/matrix/finance/market-lists'
import { formatCompact, formatDate, formatPrice } from '@/lib/finance/format'

/** Render a panel's three states without ever inventing a fourth. */
function PanelBody<T>({
  panel,
  loading,
  what,
  children,
  skeletonRows = 3,
}: {
  panel: Panel<T> | undefined
  loading: boolean
  what: string
  children: (data: T) => React.ReactNode
  skeletonRows?: number
}) {
  if (loading && !panel) return <MarketSkeleton rows={skeletonRows} />
  if (!panel) return <MarketUnavailable what={what} />
  if (panel.error) return <MarketUnavailable failure={panel.error} what={what} />
  return <>{children(panel.data)}</>
}

/* ── search ───────────────────────────────────────────────────────────────── */

function SymbolSearch({
  label,
  placeholder,
  emptyLabel,
}: {
  label: string
  placeholder: string
  emptyLabel: string
}) {
  const [query, setQuery] = useState('')
  const { data, isFetching } = useSymbolSearch(query)
  const open = query.trim().length >= 2

  return (
    <div className="relative w-full max-w-md">
      <TextInput
        label={label}
        isLabelHidden
        value={query}
        onChange={setQuery}
        placeholder={placeholder}
        startIcon={<Search className="size-4" />}
        width="100%"
        hasClear
      />
      {open ? (
        <div className="bg-popover absolute top-full right-0 left-0 z-30 mt-1 max-h-72 overflow-y-auto rounded-xl p-1">
          {isFetching && !data ? (
            <MarketSkeleton rows={3} className="p-2" />
          ) : data && data.matches.length > 0 ? (
            data.matches.map((m) => (
              <Link
                key={`${m.symbol}-${m.exchange ?? ''}`}
                href={`/finance/${encodeURIComponent(m.symbol)}`}
                onClick={() => setQuery('')}
                className="hover:bg-muted/60 flex items-center gap-3 rounded-lg px-3 py-2 transition-colors"
              >
                <span className="text-foreground font-mono text-[0.78rem] font-semibold">
                  {m.symbol}
                </span>
                <span className="text-muted-foreground min-w-0 flex-1 truncate text-[0.72rem]">
                  {m.name}
                </span>
                <span className="text-muted-foreground shrink-0 text-[0.66rem]">{m.exchange}</span>
              </Link>
            ))
          ) : (
            <p className="text-muted-foreground px-3 py-4 text-center text-xs">{emptyLabel}</p>
          )}
        </div>
      ) : null}
    </div>
  )
}

const MARKET_CLASSES: AssetClass[] = ['equity', 'index', 'crypto', 'forex', 'commodity']

function BackToChat({ label }: { label: string }) {
  return (
    <Link
      href="/"
      className="bg-muted/60 text-muted-foreground hover:bg-muted hover:text-foreground inline-flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors"
    >
      <ArrowLeft className="size-3.5" aria-hidden="true" />
      {label}
    </Link>
  )
}

function MarketClassBoard() {
  const t = useTranslations('finance')
  const [assetClass, setAssetClass] = useState<AssetClass>('equity')
  const { data, isLoading, error } = useBoard(assetClass)
  const failure = failureOf(error)

  return (
    <MarketPanel
      title={t('marketLists')}
      action={
        <div className="flex flex-wrap items-center justify-end gap-0.5" role="tablist">
          {MARKET_CLASSES.map((value) => (
            <button
              key={value}
              type="button"
              role="tab"
              aria-selected={value === assetClass}
              onClick={() => setAssetClass(value)}
              className={cn(
                'rounded-md px-2 py-1 text-[0.68rem] font-medium transition-colors',
                value === assetClass
                  ? 'bg-muted text-foreground'
                  : 'text-muted-foreground hover:bg-muted/50',
              )}
            >
              {t(value)}
            </button>
          ))}
        </div>
      }
    >
      {isLoading && !data ? (
        <MarketSkeleton rows={5} />
      ) : failure ? (
        <MarketUnavailable failure={failure} what={t(assetClass)} />
      ) : data && data.quotes.length > 0 ? (
        <>
          <div className="flex flex-col">
            {data.quotes.slice(0, 12).map((item) => (
              <MarketQuoteRow
                key={item.symbol}
                quote={item}
                href={`/finance/${encodeURIComponent(item.symbol)}`}
              />
            ))}
          </div>
          <MarketAsOf source={data.source} />
        </>
      ) : (
        <MarketUnavailable what={t(assetClass)} />
      )}
    </MarketPanel>
  )
}

/* ── markets home ─────────────────────────────────────────────────────────── */

export function MarketsHomeView() {
  const t = useTranslations('finance')
  const { data, isLoading, error } = useMarketsHome()
  const failure = failureOf(error)
  const [moverTab, setMoverTab] = useState<'gainers' | 'losers' | 'active'>('gainers')

  // A whole-request failure (auth, the lane being down) is one honest line
  // rather than eight identical panels each saying the same thing.
  if (failure) {
    return (
      <div className="mx-auto flex w-full max-w-6xl flex-col items-start gap-3 p-4">
        <BackToChat label={t('backToChat')} />
        <MarketPanel title={t('title')}>
          <MarketUnavailable failure={failure} what={t('title')} />
        </MarketPanel>
      </div>
    )
  }

  const moverPanel =
    moverTab === 'gainers' ? data?.gainers : moverTab === 'losers' ? data?.losers : data?.active

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-3 p-3 sm:p-4">
      <header className="flex flex-wrap items-center gap-3">
        <BackToChat label={t('backToChat')} />
        <h1 className="text-foreground text-lg font-semibold">{t('title')}</h1>
        <div className="ml-auto">
          <SymbolSearch
            label={t('searchLabel')}
            placeholder={t('searchPlaceholder')}
            emptyLabel={t('noSearchMatches')}
          />
        </div>
      </header>

      {/* The strip: the benchmarks, at a glance. */}
      <MarketPanel>
        <PanelBody panel={data?.strip} loading={isLoading} what={t('strip')} skeletonRows={1}>
          {(board) =>
            board.quotes.length > 0 ? (
              <>
                <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                  {board.quotes.map((q) => (
                    <MarketStripCard
                      key={q.symbol}
                      quote={q}
                      href={`/finance/${encodeURIComponent(q.symbol)}`}
                    />
                  ))}
                </div>
                <MarketAsOf source={board.source} />
              </>
            ) : (
              <MarketUnavailable what={t('strip')} />
            )
          }
        </PanelBody>
      </MarketPanel>

      <MarketClassBoard />

      <div className="grid gap-3 lg:grid-cols-[1.4fr_1fr]">
        <div className="flex min-w-0 flex-col gap-3">
          {/* News — the market summary. */}
          <MarketPanel title={t('news')}>
            <PanelBody panel={data?.news} loading={isLoading} what={t('news')} skeletonRows={4}>
              {(feed) =>
                feed.items.length > 0 ? (
                  <>
                    <div className="flex flex-col gap-1">
                      {feed.items.slice(0, 8).map((item) => (
                        <MarketNewsRow key={item.url} item={item} />
                      ))}
                    </div>
                    <MarketAsOf source={feed.source} />
                  </>
                ) : (
                  <MarketUnavailable what={t('news')} />
                )
              }
            </PanelBody>
          </MarketPanel>

          {/* Sectors. */}
          <MarketPanel title={t('sectors')}>
            <PanelBody
              panel={data?.sectors}
              loading={isLoading}
              what={t('sectors')}
              skeletonRows={6}
            >
              {(board) =>
                board.sectors.length > 0 ? (
                  <>
                    <MarketSectorRows sectors={board.sectors} />
                    <MarketAsOf source={board.source} />
                  </>
                ) : (
                  <MarketUnavailable what={t('sectors')} />
                )
              }
            </PanelBody>
          </MarketPanel>
        </div>

        <div className="flex min-w-0 flex-col gap-3">
          {/* Ranked lists. */}
          <MarketPanel
            title={t('movers')}
            action={
              <div className="flex items-center gap-0.5" role="group" aria-label={t('movers')}>
                {(['gainers', 'losers', 'active'] as const).map((kind) => (
                  <button
                    key={kind}
                    type="button"
                    onClick={() => setMoverTab(kind)}
                    aria-pressed={kind === moverTab}
                    className={cn(
                      'rounded-md px-2 py-1 text-[0.68rem] font-medium transition-colors',
                      kind === moverTab
                        ? 'bg-muted text-foreground'
                        : 'text-muted-foreground hover:bg-muted/50',
                    )}
                  >
                    {t(kind)}
                  </button>
                ))}
              </div>
            }
          >
            <PanelBody panel={moverPanel} loading={isLoading} what={t('movers')} skeletonRows={6}>
              {(list) =>
                list.movers.length > 0 ? (
                  <>
                    <div className="flex flex-col">
                      {list.movers.slice(0, 10).map((m) => (
                        <MarketMoverRow
                          key={m.symbol}
                          mover={m}
                          href={`/finance/${encodeURIComponent(m.symbol)}`}
                        />
                      ))}
                    </div>
                    <MarketAsOf source={list.source} />
                  </>
                ) : (
                  <MarketUnavailable what={t('movers')} />
                )
              }
            </PanelBody>
          </MarketPanel>

          {/* Whether the market is even open. */}
          <MarketPanel title={t('status')}>
            <PanelBody panel={data?.status} loading={isLoading} what={t('status')} skeletonRows={3}>
              {(status) =>
                status.sessions.length > 0 ? (
                  <div className="flex flex-col gap-1.5">
                    {status.sessions.slice(0, 6).map((s, i) => (
                      <div key={`${s.exchange}-${i}`} className="flex items-center gap-2">
                        <span
                          className="size-1.5 shrink-0 rounded-full"
                          style={{
                            background: s.is_open ? 'oklch(0.72 0.14 155)' : 'oklch(0.62 0 0)',
                          }}
                        />
                        <span className="text-foreground min-w-0 flex-1 truncate text-[0.72rem]">
                          {s.region || s.exchange}
                        </span>
                        <span className="text-muted-foreground shrink-0 text-[0.68rem]">
                          {s.is_open ? t('open') : t('closed')}
                        </span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <MarketUnavailable what={t('status')} />
                )
              }
            </PanelBody>
          </MarketPanel>
        </div>
      </div>

      <FinanceComposer context={t('title')} placeholder={t('askPlaceholder')} />
    </div>
  )
}

/* ── symbol page ──────────────────────────────────────────────────────────── */

type SymbolTab =
  | 'overview'
  | 'financials'
  | 'earnings'
  | 'holders'
  | 'historical'
  | 'analysis'
  | 'news'

export function SymbolView({ symbol }: { symbol: string }) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const [range, setRange] = useState<Range>('1D')
  const [tab, setTab] = useState<SymbolTab>('overview')
  const { data, isLoading, error } = useSymbolPage(symbol, range)
  const earnings = useEarnings(symbol, tab === 'earnings')
  const dividends = useDividends(symbol, tab === 'earnings')
  const failure = failureOf(error)

  const quote = data?.quote.data
  const profile = data?.profile.data
  const fundamentals = data?.fundamentals.data

  const context = useMemo(() => {
    const name = profile?.name || quote?.name
    return name ? `${symbol.toUpperCase()} — ${name}` : symbol.toUpperCase()
  }, [profile?.name, quote?.name, symbol])

  if (failure) {
    return (
      <div className="mx-auto w-full max-w-6xl p-4">
        <MarketPanel title={symbol.toUpperCase()}>
          <MarketUnavailable failure={failure} what={symbol.toUpperCase()} />
          <Link href="/finance" className="text-pax text-xs font-medium hover:underline">
            {t('backToMarkets')}
          </Link>
        </MarketPanel>
      </div>
    )
  }

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-3 p-3 sm:p-4">
      <div className="flex flex-wrap items-center gap-3">
        <Link href="/finance" className="text-muted-foreground hover:text-foreground text-xs">
          {t('backToMarkets')}
        </Link>
        <div className="ml-auto">
          <SymbolSearch
            label={t('searchLabel')}
            placeholder={t('searchPlaceholder')}
            emptyLabel={t('noSearchMatches')}
          />
        </div>
      </div>

      {/* Identity + quote + chart: the spine of the page. */}
      <MarketPanel>
        {isLoading && !quote ? (
          <MarketSkeleton rows={4} />
        ) : quote ? (
          <FinanceInstrumentCore
            quote={quote}
            profile={profile}
            extended={data?.extended.data}
            series={data?.series.data}
            range={range}
            onRangeChange={setRange}
            seriesFailure={data?.series.error}
            chartLabel={t('chart')}
          />
        ) : (
          <MarketUnavailable failure={data?.quote.error} what={symbol.toUpperCase()} />
        )}
      </MarketPanel>

      {/* Tabs — depth loaded on demand, each degrading on its own. */}
      <div
        className="flex w-full max-w-full items-center gap-1 overflow-x-auto overscroll-x-contain pb-1"
        role="tablist"
        aria-label={t('title')}
      >
        {(
          [
            'overview',
            'financials',
            'earnings',
            'holders',
            'historical',
            'analysis',
            'news',
          ] as const
        ).map((key) => (
          <button
            key={key}
            type="button"
            role="tab"
            aria-selected={tab === key}
            onClick={() => setTab(key)}
            className={cn(
              'shrink-0 rounded-lg px-3 py-1.5 text-xs font-medium whitespace-nowrap transition-colors',
              tab === key
                ? 'bg-muted text-foreground'
                : 'text-muted-foreground hover:bg-muted/50 hover:text-foreground',
            )}
          >
            {t(key)}
          </button>
        ))}
      </div>

      {tab === 'overview' ? (
        <div className="flex flex-col gap-3">
          <div className="grid gap-3 lg:grid-cols-[1.4fr_1fr]">
            <MarketPanel title={t('keyStats')} className="min-w-0">
              {quote ? (
                <MarketStatsBar quote={quote} fundamentals={fundamentals} />
              ) : (
                <MarketSkeleton rows={2} />
              )}
            </MarketPanel>
            <MarketPanel title={t('about')} className="min-w-0">
              <PanelBody
                panel={data?.profile}
                loading={isLoading}
                what={t('about')}
                skeletonRows={6}
              >
                {(p) => <MarketProfileRail profile={p} />}
              </PanelBody>
            </MarketPanel>
          </div>
          <GroundedResearch
            symbol={symbol}
            title={t('researchOverviewTitle')}
            description={t('researchOverviewDescription')}
            actionLabel={t('researchOverviewAction')}
            mode={{ kind: 'equity_brief' }}
          />
        </div>
      ) : null}

      {tab === 'financials' ? (
        <div className="flex flex-col gap-3">
          <MarketPanel title={t('fundamentals')} className="min-w-0">
            <PanelBody
              panel={data?.fundamentals}
              loading={isLoading}
              what={t('fundamentals')}
              skeletonRows={5}
            >
              {(f) => (
                <>
                  {quote ? <MarketStatsBar quote={quote} fundamentals={f} /> : null}
                  <MarketAsOf source={f.source} />
                </>
              )}
            </PanelBody>
          </MarketPanel>
          <GroundedResearch
            symbol={symbol}
            title={t('researchFinancialsTitle')}
            description={t('researchFinancialsDescription')}
            actionLabel={t('researchFinancialsAction')}
            mode={{
              fields: [
                'market_cap',
                'pe_ratio',
                'eps',
                'dividend_yield',
                'debt_to_equity',
                'free_cash_flow_yield',
              ],
            }}
          />
        </div>
      ) : null}

      {tab === 'earnings' ? (
        <div className="grid gap-3 lg:grid-cols-2">
          <MarketPanel title={t('earnings')}>
            {earnings.isLoading ? (
              <MarketSkeleton rows={5} />
            ) : failureOf(earnings.error) ? (
              <MarketUnavailable failure={failureOf(earnings.error)} what={t('earnings')} />
            ) : earnings.data && earnings.data.events.length > 0 ? (
              <div className="flex flex-col gap-2">
                {earnings.data.events.map((event) => (
                  <div
                    key={event.date}
                    className="bg-muted/40 grid grid-cols-3 gap-3 rounded-lg px-3 py-2"
                  >
                    <MarketStat label={t('date')} value={formatDate(event.date, locale)} />
                    <MarketStat
                      label={t('epsActual')}
                      value={formatPrice(event.eps_actual, undefined, locale)}
                    />
                    <MarketStat
                      label={t('epsEstimate')}
                      value={formatPrice(event.eps_estimated, undefined, locale)}
                    />
                  </div>
                ))}
                <MarketAsOf source={earnings.data.source} />
              </div>
            ) : (
              <MarketUnavailable what={t('earnings')} />
            )}
          </MarketPanel>
          <MarketPanel title={t('dividends')}>
            {dividends.isLoading ? (
              <MarketSkeleton rows={5} />
            ) : failureOf(dividends.error) ? (
              <MarketUnavailable failure={failureOf(dividends.error)} what={t('dividends')} />
            ) : dividends.data && dividends.data.events.length > 0 ? (
              <div className="flex flex-col gap-2">
                {dividends.data.events.map((event) => (
                  <div
                    key={`${event.date}-${event.payment_date ?? ''}`}
                    className="bg-muted/40 grid grid-cols-3 gap-3 rounded-lg px-3 py-2"
                  >
                    <MarketStat label={t('date')} value={formatDate(event.date, locale)} />
                    <MarketStat
                      label={t('amount')}
                      value={formatPrice(event.amount, undefined, locale)}
                    />
                    <MarketStat label={t('frequency')} value={event.frequency ?? '—'} />
                  </div>
                ))}
                <MarketAsOf source={dividends.data.source} />
              </div>
            ) : (
              <MarketUnavailable what={t('dividends')} />
            )}
          </MarketPanel>
          <div className="lg:col-span-2">
            <GroundedResearch
              symbol={symbol}
              title={t('researchEarningsTitle')}
              description={t('researchEarningsDescription')}
              actionLabel={t('researchEarningsAction')}
              mode={{ kind: 'equity_brief' }}
            />
          </div>
        </div>
      ) : null}

      {tab === 'holders' ? (
        <GroundedResearch
          symbol={symbol}
          title={t('researchHoldersTitle')}
          description={t('researchHoldersDescription')}
          actionLabel={t('researchHoldersAction')}
          mode={{ kind: 'enrichment' }}
        />
      ) : null}

      {tab === 'historical' ? (
        <MarketPanel title={t('historical')}>
          {data?.series.error ? (
            <MarketUnavailable failure={data.series.error} what={t('historical')} />
          ) : data?.series.data && data.series.data.candles.length > 0 ? (
            <div className="flex flex-col gap-1">
              {data.series.data.candles
                .slice(-20)
                .reverse()
                .map((candle) => (
                  <div
                    key={candle.t}
                    className="bg-muted/40 grid grid-cols-3 gap-3 rounded-lg px-3 py-2 sm:grid-cols-6"
                  >
                    <MarketStat label={t('date')} value={formatDate(candle.t, locale)} />
                    <MarketStat
                      label={t('openValue')}
                      value={formatPrice(candle.o, undefined, locale)}
                    />
                    <MarketStat
                      label={t('high')}
                      value={formatPrice(candle.h, undefined, locale)}
                    />
                    <MarketStat label={t('low')} value={formatPrice(candle.l, undefined, locale)} />
                    <MarketStat
                      label={t('closeValue')}
                      value={formatPrice(candle.c, undefined, locale)}
                    />
                    <MarketStat label={t('volume')} value={formatCompact(candle.v, locale)} />
                  </div>
                ))}
              <MarketAsOf source={data.series.data.source} />
            </div>
          ) : (
            <MarketUnavailable what={t('historical')} />
          )}
        </MarketPanel>
      ) : null}

      {tab === 'analysis' ? (
        <div className="flex flex-col gap-3">
          <MarketPanel title={t('analysts')} className="min-w-0">
            <PanelBody
              panel={data?.fundamentals}
              loading={isLoading}
              what={t('analysts')}
              skeletonRows={4}
            >
              {(f) =>
                f.analysts ? (
                  <MarketAnalysts fundamentals={f} />
                ) : (
                  <MarketUnavailable what={t('analysts')} />
                )
              }
            </PanelBody>
          </MarketPanel>
          <GroundedResearch
            symbol={symbol}
            title={t('researchAnalysisTitle')}
            description={t('researchAnalysisDescription')}
            actionLabel={t('researchAnalysisAction')}
            mode={{ kind: 'equity_brief' }}
          />
          <GroundedResearch
            symbol={symbol}
            title={t('researchRiskTitle')}
            description={t('researchRiskDescription')}
            actionLabel={t('researchRiskAction')}
            mode={{ kind: 'risk_rubric' }}
          />
        </div>
      ) : null}

      {tab === 'news' ? (
        <div className="flex flex-col gap-3">
          <MarketPanel title={t('news')}>
            <PanelBody panel={data?.news} loading={isLoading} what={t('news')} skeletonRows={5}>
              {(feed) =>
                feed.items.length > 0 ? (
                  <>
                    <div className="flex flex-col gap-1">
                      {feed.items.map((item) => (
                        <MarketNewsRow key={item.url} item={item} />
                      ))}
                    </div>
                    <MarketAsOf source={feed.source} />
                  </>
                ) : (
                  <MarketUnavailable what={t('news')} />
                )
              }
            </PanelBody>
          </MarketPanel>
          <GroundedResearch
            symbol={symbol}
            title={t('researchNewsTitle')}
            description={t('researchNewsDescription')}
            actionLabel={t('researchNewsAction')}
            mode={{
              urls: [...new Set(data?.news.data?.items.map((item) => item.url) ?? [])].slice(0, 10),
            }}
          />
        </div>
      ) : null}

      <FinanceComposer
        context={context}
        placeholder={t('askAboutPlaceholder', { symbol: symbol.toUpperCase() })}
      />
    </div>
  )
}
