'use client'

/**
 * The market lists: the index/top-asset strip, the ranked movers, the sector
 * board, and the news stream.
 *
 * All four are the same idea — a row that states an instrument or a story, its
 * move, and nothing it does not know. Rows separate from the panel by tone, and
 * from each other by spacing; there are no rules or strokes anywhere.
 */
import Link from 'next/link'
import { useLocale } from 'next-intl'
import { cn } from '@/lib/utils'
import type { Mover, NewsItem, Quote, SectorPerformance } from '@/lib/api/finance'
import {
  ABSENT,
  directionOf,
  formatAgo,
  formatCompact,
  formatPercent,
  formatPrice,
} from '@/lib/finance/format'
import { directionColor } from '@/components/matrix/finance/market-chart'

/** A compact sparkline-free strip card: an index or top asset. */
export function MarketStripCard({
  quote,
  href,
  className,
}: {
  quote: Quote
  href?: string
  className?: string
}) {
  const locale = useLocale()
  const tone = directionColor(directionOf(quote.change ?? quote.change_percent))
  const body = (
    <div
      className={cn('bg-muted/40 flex min-w-0 flex-col gap-1 rounded-xl px-3 py-2.5', className)}
    >
      <span className="text-muted-foreground truncate text-[0.7rem] font-medium">
        {quote.name || quote.symbol}
      </span>
      <span className="text-foreground truncate font-mono text-[0.95rem] font-semibold">
        {formatPrice(quote.price, undefined, locale)}
      </span>
      <span className="truncate font-mono text-[0.7rem]" style={{ color: tone }}>
        {formatPercent(quote.change_percent, locale)}
      </span>
    </div>
  )
  if (!href) return body
  return (
    <Link href={href} className="min-w-0 transition-opacity hover:opacity-80">
      {body}
    </Link>
  )
}

/** One ranked row: symbol, name, price, move. */
export function MarketMoverRow({
  mover,
  href,
  className,
}: {
  mover: Mover
  href?: string
  className?: string
}) {
  const locale = useLocale()
  const tone = directionColor(directionOf(mover.change ?? mover.change_percent))
  const body = (
    <div className={cn('flex min-w-0 items-center gap-3 rounded-lg px-2 py-2', className)}>
      <div className="min-w-0 flex-1">
        <p className="text-foreground truncate font-mono text-[0.78rem] font-semibold">
          {mover.symbol}
        </p>
        {mover.name ? (
          <p className="text-muted-foreground truncate text-[0.68rem]">{mover.name}</p>
        ) : null}
      </div>
      <span className="text-foreground shrink-0 font-mono text-[0.75rem]">
        {formatPrice(mover.price, undefined, locale)}
      </span>
      <span className="w-16 shrink-0 text-right font-mono text-[0.75rem]" style={{ color: tone }}>
        {formatPercent(mover.change_percent, locale)}
      </span>
    </div>
  )
  if (!href) return body
  return (
    <Link href={href} className="hover:bg-muted/50 block rounded-lg transition-colors">
      {body}
    </Link>
  )
}

/** The sector board: a proportional read of where the market moved. */
export function MarketSectorRows({
  sectors,
  className,
}: {
  sectors: SectorPerformance[]
  className?: string
}) {
  const locale = useLocale()
  const widest = sectors.reduce((max, s) => Math.max(max, Math.abs(s.change_percent)), 0) || 1
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      {sectors.map((s) => {
        const tone = directionColor(directionOf(s.change_percent))
        const width = (Math.abs(s.change_percent) / widest) * 100
        return (
          <div key={s.sector} className="flex items-center gap-3">
            <span className="text-foreground w-32 shrink-0 truncate text-[0.72rem]">
              {s.sector}
            </span>
            <span className="bg-muted/50 relative h-1.5 min-w-0 flex-1 overflow-hidden rounded-full">
              <span
                className="absolute inset-y-0 left-0 rounded-full"
                style={{ width: `${width}%`, background: tone }}
              />
            </span>
            <span
              className="w-14 shrink-0 text-right font-mono text-[0.7rem]"
              style={{ color: tone }}
            >
              {formatPercent(s.change_percent, locale)}
            </span>
          </div>
        )
      })}
    </div>
  )
}

/** One story: headline, source, when. */
export function MarketNewsRow({ item, className }: { item: NewsItem; className?: string }) {
  const locale = useLocale()
  return (
    <a
      href={item.url}
      target="_blank"
      rel="noreferrer noopener"
      className={cn(
        'hover:bg-muted/50 flex min-w-0 items-start gap-3 rounded-xl p-2 transition-colors',
        className,
      )}
    >
      {item.image_url ? (
        // eslint-disable-next-line @next/next/no-img-element -- vendor image hosts are not in the image config allow-list
        <img
          src={item.image_url}
          alt=""
          className="bg-muted size-14 shrink-0 rounded-lg object-cover"
          loading="lazy"
        />
      ) : null}
      <div className="min-w-0 flex-1">
        <p className="text-foreground line-clamp-2 text-[0.78rem] leading-snug font-medium [overflow-wrap:anywhere]">
          {item.title}
        </p>
        <p className="text-muted-foreground mt-1 truncate text-[0.66rem]">
          {item.publisher || item.site || ABSENT}
          {item.published_at ? ` · ${formatAgo(item.published_at, locale)}` : ''}
          {item.symbols?.length ? ` · ${item.symbols.slice(0, 3).join(', ')}` : ''}
        </p>
      </div>
    </a>
  )
}

/** A watchlist / per-market table row with volume. */
export function MarketQuoteRow({
  quote,
  href,
  className,
}: {
  quote: Quote
  href?: string
  className?: string
}) {
  const locale = useLocale()
  const tone = directionColor(directionOf(quote.change ?? quote.change_percent))
  const body = (
    <div className={cn('flex min-w-0 items-center gap-3 rounded-lg px-2 py-2', className)}>
      <div className="min-w-0 flex-1">
        <p className="text-foreground truncate font-mono text-[0.78rem] font-semibold">
          {quote.symbol}
        </p>
        {quote.name ? (
          <p className="text-muted-foreground truncate text-[0.68rem]">{quote.name}</p>
        ) : null}
      </div>
      <span className="text-muted-foreground hidden w-20 shrink-0 text-right font-mono text-[0.72rem] sm:block">
        {formatCompact(quote.volume, locale)}
      </span>
      <span className="text-foreground w-24 shrink-0 text-right font-mono text-[0.75rem]">
        {formatPrice(quote.price, undefined, locale)}
      </span>
      <span className="w-20 shrink-0 text-right font-mono text-[0.75rem]" style={{ color: tone }}>
        {formatPercent(quote.change_percent, locale)}
      </span>
    </div>
  )
  if (!href) return body
  return (
    <Link href={href} className="hover:bg-muted/50 block rounded-lg transition-colors">
      {body}
    </Link>
  )
}
