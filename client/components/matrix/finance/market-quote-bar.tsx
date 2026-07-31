'use client'

/**
 * The live quote and stats bar — the top of a symbol page.
 *
 * It states three things a market surface must never blur: what the price IS,
 * WHEN it was true, and whether the session was open when it was. A close is
 * labelled a close, an extended-hours print is labelled as such, and a stale
 * value says so rather than posing as live.
 */
import { cn } from '@/lib/utils'
import { useLocale, useTranslations } from 'next-intl'
import type { ExtendedQuote, FundamentalSummary, Profile, Quote } from '@/lib/api/finance'
import {
  ABSENT,
  directionOf,
  formatChange,
  formatCompact,
  formatCount,
  formatFractionAsPercent,
  formatPercent,
  formatPrice,
  formatRange,
  formatRatio,
  formatTime,
} from '@/lib/finance/format'
import { directionColor } from '@/components/matrix/finance/market-chart'
import { MarketAsOf, MarketStat } from '@/components/matrix/finance/market-panel'

/** The identity header: what this instrument is. */
export function MarketIdentity({
  quote,
  profile,
  className,
}: {
  quote: Quote
  profile?: Profile
  className?: string
}) {
  const name = profile?.name || quote.name || quote.symbol
  const exchange = profile?.exchange || quote.exchange
  return (
    <div className={cn('flex min-w-0 items-center gap-3', className)}>
      {profile?.image_url ? (
        // eslint-disable-next-line @next/next/no-img-element -- vendor logo host is not in the image config allow-list
        <img
          src={profile.image_url}
          alt=""
          className="bg-muted size-9 shrink-0 rounded-full object-contain"
          loading="lazy"
        />
      ) : (
        <span className="bg-muted text-muted-foreground grid size-9 shrink-0 place-items-center rounded-full font-mono text-[0.7rem] font-semibold">
          {quote.symbol.replace('^', '').slice(0, 3)}
        </span>
      )}
      <div className="min-w-0">
        <h1 className="text-foreground truncate text-lg leading-tight font-semibold">{name}</h1>
        <p className="text-muted-foreground truncate font-mono text-[0.7rem]">
          {quote.symbol}
          {exchange ? ` · ${exchange}` : ''}
        </p>
      </div>
    </div>
  )
}

/** One price block: the figure, its change, and what moment it belongs to. */
function PriceBlock({
  label,
  price,
  change,
  changePercent,
  at,
  large,
  locale,
}: {
  label: string
  price?: number
  change?: number
  changePercent?: number
  at?: string
  large?: boolean
  locale: string
}) {
  const tone = directionColor(directionOf(change ?? changePercent))
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span
          className={cn(
            'text-foreground font-mono font-semibold',
            large ? 'text-2xl' : 'text-base',
          )}
        >
          {formatPrice(price, undefined, locale)}
        </span>
        <span className="font-mono text-[0.8rem] font-medium" style={{ color: tone }}>
          {formatChange(change, locale)} {formatPercent(changePercent, locale)}
        </span>
      </div>
      <p className="text-muted-foreground text-[0.7rem]">
        {label}
        {at ? ` · ${formatTime(at, locale)}` : ''}
      </p>
    </div>
  )
}

/**
 * The quote header. `sessionOpen` decides whether the main figure is called a
 * live price or a close — the one thing a market page must not guess at.
 */
export function MarketQuoteBar({
  quote,
  extended,
  sessionOpen,
  className,
}: {
  quote: Quote
  extended?: ExtendedQuote
  /** From the market-status board; undefined when it is not known. */
  sessionOpen?: boolean
  className?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const mainLabel =
    sessionOpen === undefined ? t('lastTrade') : sessionOpen ? t('livePrice') : t('atClose')

  // The extended block is only meaningful when it actually carries a price;
  // an aftermarket book with no print is not a second quote.
  const extPrice = extended?.price
  const extChange =
    typeof extPrice === 'number' && typeof quote.price === 'number'
      ? extPrice - quote.price
      : undefined
  const extPercent =
    typeof extChange === 'number' && typeof quote.price === 'number' && quote.price !== 0
      ? (extChange / quote.price) * 100
      : undefined

  return (
    <div className={cn('flex flex-wrap items-start gap-x-10 gap-y-3', className)}>
      <PriceBlock
        label={mainLabel}
        price={quote.price}
        change={quote.change}
        changePercent={quote.change_percent}
        at={quote.as_of}
        large
        locale={locale}
      />
      {typeof extPrice === 'number' ? (
        <PriceBlock
          label={sessionOpen ? t('extendedHours') : t('afterHours')}
          price={extPrice}
          change={extChange}
          changePercent={extPercent}
          at={extended?.as_of}
          locale={locale}
        />
      ) : null}
      <MarketAsOf source={quote.source} className="w-full" />
    </div>
  )
}

/**
 * The stats bar. Every cell is a real figure or a dash — the vendor's silence is
 * never rendered as a number.
 */
export function MarketStatsBar({
  quote,
  fundamentals,
  className,
}: {
  quote: Quote
  fundamentals?: FundamentalSummary
  className?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const stats: { label: string; value: string }[] = [
    { label: t('openValue'), value: formatPrice(quote.open, undefined, locale) },
    { label: t('previousClose'), value: formatPrice(quote.previous_close, undefined, locale) },
    { label: t('dayRange'), value: formatRange(quote.day_low, quote.day_high, locale) },
    { label: t('yearRange'), value: formatRange(quote.year_low, quote.year_high, locale) },
    { label: t('volume'), value: formatCompact(quote.volume, locale) },
    {
      label: t('marketCap'),
      value: formatCompact(fundamentals?.market_cap ?? quote.market_cap, locale),
    },
    { label: t('peRatio'), value: formatRatio(fundamentals?.pe_ratio, locale) },
    { label: t('eps'), value: formatPrice(fundamentals?.eps, undefined, locale) },
    {
      label: t('dividendYield'),
      value:
        fundamentals?.dividend_yield === undefined
          ? ABSENT
          : formatFractionAsPercent(fundamentals.dividend_yield, locale),
    },
    { label: t('priceBook'), value: formatRatio(fundamentals?.price_to_book, locale) },
  ]

  return (
    <div
      className={cn('grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-3 lg:grid-cols-5', className)}
    >
      {stats.map((s) => (
        <MarketStat key={s.label} label={s.label} value={s.value} />
      ))}
    </div>
  )
}

/** The company rail: reference facts, and the description last. */
export function MarketProfileRail({
  profile,
  className,
}: {
  profile: Profile
  className?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const rows: { label: string; value: string }[] = [
    { label: t('symbol'), value: profile.symbol },
    { label: t('exchange'), value: profile.exchange_name || profile.exchange || ABSENT },
    { label: t('sector'), value: profile.sector || ABSENT },
    { label: t('industry'), value: profile.industry || ABSENT },
    { label: t('country'), value: profile.country || ABSENT },
    { label: t('ceo'), value: profile.ceo || ABSENT },
    { label: t('employees'), value: formatCount(profile.employees, locale) },
    { label: t('ipoDate'), value: profile.ipo_date || ABSENT },
  ]
  return (
    <div className={cn('flex flex-col gap-3', className)}>
      <dl className="flex flex-col gap-2">
        {rows.map((r) => (
          <div key={r.label} className="flex items-baseline justify-between gap-3">
            <dt className="text-muted-foreground text-[0.72rem]">{r.label}</dt>
            <dd className="text-foreground truncate font-mono text-[0.72rem]">{r.value}</dd>
          </div>
        ))}
      </dl>
      {profile.description ? (
        <p className="text-muted-foreground text-[0.75rem] leading-relaxed [overflow-wrap:anywhere]">
          {profile.description}
        </p>
      ) : null}
      {profile.website ? (
        <a
          href={profile.website}
          target="_blank"
          rel="noreferrer noopener"
          className="text-pax text-[0.72rem] font-medium hover:underline"
        >
          {profile.website.replace(/^https?:\/\//, '')}
        </a>
      ) : null}
    </div>
  )
}

/** The analyst rail: the grade split as a proportional bar, plus the targets. */
export function MarketAnalysts({
  fundamentals,
  className,
}: {
  fundamentals: FundamentalSummary
  className?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const a = fundamentals.analysts
  if (!a) return null
  const buckets = [
    { key: t('strongBuy'), value: a.strong_buy ?? 0, tone: 'oklch(0.72 0.14 155)' },
    { key: t('buy'), value: a.buy ?? 0, tone: 'oklch(0.75 0.11 155)' },
    { key: t('hold'), value: a.hold ?? 0, tone: 'oklch(0.68 0.02 250)' },
    { key: t('sell'), value: a.sell ?? 0, tone: 'oklch(0.66 0.16 25)' },
    { key: t('strongSell'), value: a.strong_sell ?? 0, tone: 'oklch(0.62 0.2 25)' },
  ]
  const total = buckets.reduce((sum, b) => sum + b.value, 0)

  return (
    <div className={cn('flex flex-col gap-3', className)}>
      <div className="flex items-baseline gap-2">
        <span className="text-foreground text-[0.85rem] font-semibold">
          {a.consensus || t('noConsensus')}
        </span>
        {total > 0 ? (
          <span className="text-muted-foreground text-[0.7rem]">
            {t('analystCount', { count: total })}
          </span>
        ) : null}
      </div>
      {total > 0 ? (
        <>
          <div className="flex h-1.5 w-full overflow-hidden rounded-full">
            {buckets.map((b) =>
              b.value > 0 ? (
                <span
                  key={b.key}
                  style={{ width: `${(b.value / total) * 100}%`, background: b.tone }}
                  title={`${b.key}: ${b.value}`}
                />
              ) : null,
            )}
          </div>
          <div className="text-muted-foreground flex flex-wrap gap-x-4 gap-y-1 font-mono text-[0.65rem]">
            {buckets
              .filter((b) => b.value > 0)
              .map((b) => (
                <span key={b.key}>
                  {b.key} {b.value}
                </span>
              ))}
          </div>
        </>
      ) : null}
      {a.target_mean !== undefined || a.target_high !== undefined ? (
        <div className="grid grid-cols-3 gap-3">
          <MarketStat label={t('targetLow')} value={formatPrice(a.target_low, undefined, locale)} />
          <MarketStat
            label={t('targetMean')}
            value={formatPrice(a.target_mean, undefined, locale)}
          />
          <MarketStat
            label={t('targetHigh')}
            value={formatPrice(a.target_high, undefined, locale)}
          />
        </div>
      ) : null}
    </div>
  )
}
