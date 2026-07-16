'use client'

/**
 * Shared primitives for the LayerX explorer pages. House rules: separation
 * by background tone only (no border strokes), sage as the single accent,
 * mono for hashes/DIDs/amounts.
 */
import { useState, type ReactNode } from 'react'
import { useTranslations } from 'next-intl'
import { Check, Copy, ExternalLink } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'

type T = ReturnType<typeof useTranslations>

/** Web explorer base for anchor-tx links (Paxscan). Env-overridable. */
export function paxscanWebBase(): string {
  const raw = process.env.NEXT_PUBLIC_PAXSCAN_WEB
  if (raw && raw.trim() !== '') return raw.trim().replace(/\/+$/, '')
  return 'https://paxscan.io'
}

/** Middle-ellipsis for hashes, DIDs and addresses. */
export function shortMiddle(value: string, head = 10, tail = 6): string {
  if (value.length <= head + tail + 3) return value
  return `${value.slice(0, head)}…${value.slice(-tail)}`
}

/** Terse relative timestamp for feed rows. */
export function timeAgo(iso: string, nowLabel: string): string {
  const then = new Date(iso).getTime()
  if (!Number.isFinite(then)) return iso
  const secs = Math.floor((Date.now() - then) / 1000)
  if (secs < 5) return nowLabel
  if (secs < 60) return `${secs}s`
  const mins = Math.floor(secs / 60)
  if (mins < 60) return `${mins}m`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h`
  return `${Math.floor(hrs / 24)}d`
}

/** Full locale timestamp for detail pages. */
export function fullTime(iso: string): string {
  const d = new Date(iso)
  return Number.isFinite(d.getTime()) ? d.toLocaleString() : iso
}

/** Copyable mono chip for a hash / DID / address. */
export function HashChip({
  value,
  display,
  className,
}: {
  value: string
  display?: string
  className?: string
}) {
  const t = useTranslations('layerxExplorer')
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      title={value}
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        })
      }}
      className={cn(
        'bg-accent text-muted-foreground hover:bg-surface-hover hover:text-foreground inline-flex max-w-full items-center gap-1.5 rounded-lg px-2.5 py-1 font-mono text-xs transition-colors',
        className,
      )}
    >
      <span className="truncate">{display ?? shortMiddle(value)}</span>
      {copied ? (
        <Check className="text-pax size-3 shrink-0" />
      ) : (
        <Copy className="size-3 shrink-0 opacity-60" />
      )}
      <span className="sr-only">{copied ? t('copied') : t('copy')}</span>
    </button>
  )
}

/** Settlement status pill (tone-only, sage for anchored/settled). */
export function StatusPill({ status, label }: { status: string; label: string }) {
  const good = status === 'anchored' || status === 'settled'
  const bad = status === 'failed'
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium',
        good && 'bg-pax-soft text-pax',
        bad && 'bg-destructive/15 text-destructive',
        !good && !bad && 'bg-accent text-muted-foreground',
      )}
    >
      <span
        className={cn(
          'size-1.5 rounded-full',
          good && 'bg-pax',
          bad && 'bg-destructive',
          !good && !bad && 'bg-muted-foreground',
        )}
      />
      {label}
    </span>
  )
}

/** External link to a Paxscan transaction. */
export function PaxscanTxLink({ hash, label }: { hash: string; label: string }) {
  return (
    <a
      href={`${paxscanWebBase()}/tx/${hash}`}
      target="_blank"
      rel="noreferrer"
      className="bg-accent text-muted-foreground hover:bg-surface-hover hover:text-foreground inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1 font-mono text-xs transition-colors"
    >
      <span className="truncate">{label}</span>
      <ExternalLink className="size-3 shrink-0 opacity-60" />
    </a>
  )
}

/** Top-line metric card. */
export function StatCard({
  label,
  value,
  hint,
  accent = false,
}: {
  label: string
  value: ReactNode
  hint?: string
  accent?: boolean
}) {
  return (
    <div className="bg-card flex flex-col gap-1 rounded-xl p-4">
      <span className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
        {label}
      </span>
      <span
        className={cn('font-mono text-xl font-semibold', accent ? 'text-pax' : 'text-foreground')}
      >
        {value}
      </span>
      {hint && <span className="text-muted-foreground/70 text-xs">{hint}</span>}
    </div>
  )
}

/** Section container: a card with a heading row. */
export function SectionCard({
  title,
  action,
  children,
}: {
  title: string
  action?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="bg-card flex flex-col rounded-xl">
      <div className="flex items-center justify-between gap-3 px-4 pt-4 pb-2">
        <h2 className="text-foreground text-sm font-semibold">{title}</h2>
        {action}
      </div>
      {children}
    </section>
  )
}

/** Labeled row on a detail page. */
export function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-col gap-1 px-4 py-2.5 sm:flex-row sm:items-center sm:gap-4">
      <span className="text-muted-foreground w-44 shrink-0 text-xs font-medium tracking-wide uppercase">
        {label}
      </span>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  )
}

/** Neutral empty / error / not-configured state. */
export function ExplorerState({ title, body }: { title: string; body?: string }) {
  return (
    <div className="bg-card flex flex-col items-center gap-2 rounded-xl px-6 py-14 text-center">
      <p className="text-foreground text-sm font-medium">{title}</p>
      {body && <p className="text-muted-foreground max-w-md text-xs">{body}</p>}
    </div>
  )
}

/** Loading shimmer rows. */
export function RowsSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-2 p-4">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="bg-accent h-9 animate-pulse rounded-lg" />
      ))}
    </div>
  )
}

export type { T }
