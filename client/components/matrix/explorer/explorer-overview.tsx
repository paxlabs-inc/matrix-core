'use client'

/**
 * Explorer overview — reserve-proof stat cards, the live transfer feed
 * (SSE-fed, newest first) and the latest settlement batches.
 */
import { useCallback, useState } from 'react'
import { useTranslations } from 'next-intl'
import { Link } from '@/i18n/navigation'
import { useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import type { LayerXTransfer } from '@/lib/layerx/client'
import {
  layerxConfigured,
  useLayerXBatches,
  useLayerXStream,
  useLayerXSupply,
  useLayerXTransfers,
} from '@/hooks/api/useLayerXExplorer'
import { ExplorerState, RowsSkeleton, SectionCard, StatCard } from './explorer-bits'
import { BatchRow, TransferRow } from './explorer-rows'

const LIVE_MAX = 25

export function ExplorerOverview() {
  const t = useTranslations('layerxExplorer')
  const qc = useQueryClient()
  const supply = useLayerXSupply()
  const transfers = useLayerXTransfers(undefined, LIVE_MAX)
  const batches = useLayerXBatches(8)
  const [live, setLive] = useState<LayerXTransfer[]>([])

  const onTransfer = useCallback((tr: LayerXTransfer) => {
    setLive((prev) =>
      prev.some((p) => p.seq === tr.seq) ? prev : [tr, ...prev].slice(0, LIVE_MAX),
    )
  }, [])
  const onAnchor = useCallback(() => {
    void qc.invalidateQueries({ queryKey: qk.layerxBatches() })
    void qc.invalidateQueries({ queryKey: qk.layerxSupply() })
  }, [qc])
  useLayerXStream({ onTransfer, onAnchor, enabled: layerxConfigured() })

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }

  const loaded = transfers.data?.pages[0]?.transfers ?? []
  const liveSeqs = new Set(live.map((l) => l.seq))
  const feed = [...live, ...loaded.filter((l) => !liveSeqs.has(l.seq))].slice(0, LIVE_MAX)
  const recentBatches = batches.data?.pages[0]?.batches ?? []
  const s = supply.data

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatCard
          label={t('circulating')}
          value={s ? `${s.circulating_usdx}` : '—'}
          hint={s ? 'USDX' : undefined}
        />
        <StatCard
          label={t('reserve')}
          value={s?.reserve_known ? `${s.reserve_usdl}` : t('reserveUnknown')}
          hint={s?.reserve_known ? 'USDL' : undefined}
          accent={Boolean(s?.fully_reserved)}
        />
        <StatCard label={t('accounts')} value={s ? s.accounts.toLocaleString() : '—'} />
        <StatCard label={t('totalTransfers')} value={s ? s.transfers.toLocaleString() : '—'} />
      </div>

      {s?.reserve_known && (
        <div className="bg-card flex items-center gap-2 rounded-xl px-4 py-2.5">
          <span
            className={
              s.fully_reserved
                ? 'bg-pax size-1.5 rounded-full'
                : 'bg-text-warning size-1.5 rounded-full'
            }
          />
          <p className="text-muted-foreground text-xs">
            {s.fully_reserved
              ? t('fullyReserved')
              : t('reserveDrift', { drift: s.drift_usdx ?? '' })}
          </p>
        </div>
      )}

      <SectionCard
        title={t('liveTransfers')}
        action={
          <span className="flex items-center gap-3">
            <span className="text-pax flex items-center gap-1.5 text-xs">
              <span className="bg-pax size-1.5 animate-pulse rounded-full" />
              {t('live')}
            </span>
            <Link
              href="/explorer/transfers"
              className="text-muted-foreground hover:text-foreground text-xs font-medium transition-colors"
            >
              {t('viewAll')}
            </Link>
          </span>
        }
      >
        {transfers.isLoading ? (
          <RowsSkeleton />
        ) : feed.length === 0 ? (
          <p className="text-muted-foreground px-4 pt-2 pb-6 text-center text-xs">
            {t('noTransfers')}
          </p>
        ) : (
          <ul className="flex flex-col gap-0.5 p-2">
            {feed.map((tr) => (
              <TransferRow key={tr.seq} transfer={tr} fresh={liveSeqs.has(tr.seq)} />
            ))}
          </ul>
        )}
      </SectionCard>

      <SectionCard
        title={t('recentBatches')}
        action={
          <Link
            href="/explorer/batches"
            className="text-muted-foreground hover:text-foreground text-xs font-medium transition-colors"
          >
            {t('viewAll')}
          </Link>
        }
      >
        {batches.isLoading ? (
          <RowsSkeleton rows={4} />
        ) : recentBatches.length === 0 ? (
          <p className="text-muted-foreground px-4 pt-2 pb-6 text-center text-xs">
            {t('noBatches')}
          </p>
        ) : (
          <ul className="flex flex-col gap-0.5 p-2">
            {recentBatches.map((b) => (
              <BatchRow key={b.id} batch={b} />
            ))}
          </ul>
        )}
      </SectionCard>
    </div>
  )
}
