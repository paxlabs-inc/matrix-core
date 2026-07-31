'use client'

/**
 * Feed rows shared by the LayerX explorer pages — transfer rows (overview
 * live feed, transfers page, account history) and batch rows.
 */
import { useTranslations } from 'next-intl'
import { Link } from '@/i18n/navigation'
import { ArrowRight } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { LayerXTransfer } from '@/lib/layerx/client'
import type { LayerXBatch } from '@/lib/layerx/explorer'
import { shortMiddle, timeAgo, StatusPill } from './explorer-bits'

/** Compact DID rendering: keep the method tail meaningful. */
export function shortDID(did: string): string {
  return shortMiddle(did, 18, 6)
}

export function TransferRow({
  transfer,
  fresh = false,
}: {
  transfer: LayerXTransfer
  fresh?: boolean
}) {
  const t = useTranslations('layerxExplorer')
  return (
    <li>
      <Link
        href={`/explorer/tx/${transfer.seq}`}
        className={cn(
          'hover:bg-surface-hover flex items-center gap-3 rounded-lg px-4 py-2.5 transition-colors',
          fresh && 'bg-pax-softer',
        )}
      >
        <span className="text-muted-foreground w-16 shrink-0 font-mono text-xs">
          #{transfer.seq}
        </span>
        <span className="hidden min-w-0 flex-1 items-center gap-2 sm:flex">
          <span className="text-foreground truncate font-mono text-xs" title={transfer.from_did}>
            {shortDID(transfer.from_did)}
          </span>
          <ArrowRight className="text-muted-foreground size-3 shrink-0" />
          <span className="text-foreground truncate font-mono text-xs" title={transfer.to_did}>
            {shortDID(transfer.to_did)}
          </span>
        </span>
        <span className="text-foreground ml-auto shrink-0 font-mono text-xs font-medium">
          {transfer.amount_usdx} <span className="text-muted-foreground font-normal">USDX</span>
        </span>
        <span className="hidden w-20 shrink-0 justify-end sm:flex">
          <StatusPill
            status={transfer.settled ? 'settled' : 'pending'}
            label={transfer.settled ? t('settled') : t('pending')}
          />
        </span>
        <span className="text-muted-foreground w-10 shrink-0 text-right text-xs">
          {timeAgo(transfer.ts, t('justNow'))}
        </span>
      </Link>
    </li>
  )
}

export function BatchRow({ batch }: { batch: LayerXBatch }) {
  const t = useTranslations('layerxExplorer')
  return (
    <li>
      <Link
        href={`/explorer/batch/${batch.id}`}
        className="hover:bg-surface-hover flex items-center gap-3 rounded-lg px-4 py-2.5 transition-colors"
      >
        <span
          className="text-foreground min-w-0 flex-1 truncate font-mono text-xs"
          title={batch.root}
        >
          {shortMiddle(batch.root, 14, 8)}
        </span>
        <span className="text-muted-foreground hidden w-24 shrink-0 text-right font-mono text-xs sm:block">
          {t('transferCountShort', { count: batch.transfer_count })}
        </span>
        <span className="w-24 shrink-0 text-right">
          <StatusPill status={batch.status} label={statusLabel(t, batch.status)} />
        </span>
        <span className="text-muted-foreground w-10 shrink-0 text-right text-xs">
          {timeAgo(batch.created_at, t('justNow'))}
        </span>
      </Link>
    </li>
  )
}

/** Localized label for a layerxd batch status (unknown -> raw passthrough). */
export function statusLabel(t: ReturnType<typeof useTranslations>, status: string): string {
  switch (status) {
    case 'sealed':
      return t('statusSealed')
    case 'submitted':
      return t('statusSubmitted')
    case 'anchored':
      return t('statusAnchored')
    case 'failed':
      return t('statusFailed')
    default:
      return status
  }
}
