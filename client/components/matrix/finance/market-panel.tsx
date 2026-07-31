'use client'

/**
 * Panel primitives for the market surface.
 *
 * Three states, and no fourth: loading is a skeleton, an outcome the provider
 * could not give is a plain-language line, and data is data. There is no state
 * where a panel shows zeros because nothing arrived — that is the failure mode
 * this file exists to prevent.
 *
 * Separation is by background tone (`bg-card` on `bg-background`), never a
 * border stroke; there are no emojis, gradients, or glows.
 */
import type { ReactNode } from 'react'
import { useLocale, useTranslations } from 'next-intl'
import { Card } from '@astryxdesign/core/Card'
import { Heading } from '@astryxdesign/core/Text'
import { cn } from '@/lib/utils'
import type { FinanceFailure, FinanceSource } from '@/lib/api/finance'
import { formatAgo } from '@/lib/finance/format'

/** The section shell every market panel sits in. */
export function MarketPanel({
  title,
  action,
  children,
  className,
}: {
  title?: ReactNode
  action?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <Card variant="muted" padding={4} className={cn('flex flex-col gap-3', className)}>
      {title || action ? (
        <header className="flex items-center gap-3">
          {title ? (
            <Heading level={2} className="text-[0.9rem] leading-tight">
              {title}
            </Heading>
          ) : null}
          {action ? <div className="ml-auto">{action}</div> : null}
        </header>
      ) : null}
      {children}
    </Card>
  )
}

/** Loading. Never a zero standing in for a number that has not arrived. */
export function MarketSkeleton({ rows = 3, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn('flex flex-col gap-2', className)} aria-hidden>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="bg-muted h-8 animate-pulse rounded-lg" />
      ))}
    </div>
  )
}

/**
 * What a panel says when the provider could not answer. The router already
 * phrased it for a person; this renders that phrasing rather than a code.
 */
export function MarketUnavailable({
  failure,
  what,
  className,
}: {
  failure?: FinanceFailure | null
  /** What was being loaded, for the fallback sentence. */
  what?: string
  className?: string
}) {
  const t = useTranslations('finance')
  const message = failure?.message ?? (what ? t('unavailableWhat', { what }) : t('unavailable'))
  return (
    <div className={cn('bg-muted/40 rounded-xl px-3 py-6 text-center', className)} role="status">
      <p className="text-muted-foreground text-xs">{message}</p>
    </div>
  )
}

/**
 * The provenance line: when the figures were fetched, who answered, and — the
 * part that matters — whether they are stale or came from the backup provider.
 */
export function MarketAsOf({
  source,
  className,
}: {
  source?: FinanceSource | null
  className?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  if (!source) return null
  return (
    <p className={cn('text-muted-foreground/80 font-mono text-[0.65rem]', className)}>
      {source.stale ? `${t('lastAvailable')} · ` : `${t('updated')} `}
      {formatAgo(source.fetched_at, locale)}
      {source.stale ? ` · ${t('providerUnavailable')}` : null}
      {source.fallback && !source.stale ? ` · ${t('backupSource')}` : null}
    </p>
  )
}

/** One label/value stat. An absent value renders as a dash, never as zero. */
export function MarketStat({
  label,
  value,
  tone,
  className,
}: {
  label: string
  value: ReactNode
  tone?: string
  className?: string
}) {
  return (
    <div className={cn('flex min-w-0 flex-col gap-0.5', className)}>
      <span className="text-muted-foreground truncate text-[0.65rem] tracking-wide uppercase">
        {label}
      </span>
      <span
        className="text-foreground truncate font-mono text-[0.8rem] font-medium"
        style={tone ? { color: tone } : undefined}
      >
        {value}
      </span>
    </div>
  )
}
