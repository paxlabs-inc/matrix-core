'use client'

/**
 * Shared primitives for the LayerX explorer pages. House rules: separation
 * by background tone only (no border strokes), sage as the single accent,
 * mono for hashes/DIDs/amounts.
 */
import { useState, type ReactNode } from 'react'
import { useTranslations } from 'next-intl'
import { Card, VStack } from '@astryxdesign/core/Layout'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Badge } from '@astryxdesign/core/Badge'
import { Skeleton } from '@astryxdesign/core/Skeleton'
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
    <Badge
      variant={good ? 'success' : bad ? 'error' : 'neutral'}
      label={
        <span className="inline-flex items-center gap-1.5">
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
      }
    />
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
    <Card variant="muted" padding={4} elevation="none">
      <VStack gap={1}>
        <Text type="supporting" color="secondary" weight="medium">
          {label}
        </Text>
        <Text type="large" weight="semibold" className={cn('font-mono', accent && 'text-pax')}>
          {value}
        </Text>
        {hint && (
          <Text type="supporting" color="secondary">
            {hint}
          </Text>
        )}
      </VStack>
    </Card>
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
    <Card variant="muted" padding={0} elevation="none" className="flex flex-col">
      <div className="flex items-center justify-between gap-3 px-4 pt-4 pb-2">
        <Heading level={2} type="display-3" style={{ fontSize: '0.875rem' }}>
          {title}
        </Heading>
        {action}
      </div>
      {children}
    </Card>
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
    <Card variant="muted" padding={8} elevation="none">
      <VStack gap={2} align="center">
        <Text type="body" weight="medium">
          {title}
        </Text>
        {body && (
          <Text type="supporting" color="secondary" justify="center">
            {body}
          </Text>
        )}
      </VStack>
    </Card>
  )
}

/** Loading shimmer rows. */
export function RowsSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div className="flex flex-col gap-2 p-4">
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} height={36} radius={2} index={i} />
      ))}
    </div>
  )
}

export type { T }
