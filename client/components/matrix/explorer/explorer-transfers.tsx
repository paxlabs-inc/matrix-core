'use client'

/**
 * Full transfer feed — keyset-paginated history with a live SSE prepend.
 */
import { useCallback, useState } from 'react'
import { useTranslations } from 'next-intl'
import type { LayerXTransfer } from '@/lib/layerx/client'
import {
  layerxConfigured,
  useLayerXStream,
  useLayerXTransfers,
} from '@/hooks/api/useLayerXExplorer'
import { ExplorerState, RowsSkeleton, SectionCard } from './explorer-bits'
import { TransferRow } from './explorer-rows'

export function ExplorerTransfers() {
  const t = useTranslations('layerxExplorer')
  const transfers = useLayerXTransfers(undefined, 50)
  const [live, setLive] = useState<LayerXTransfer[]>([])

  const onTransfer = useCallback((tr: LayerXTransfer) => {
    setLive((prev) => (prev.some((p) => p.seq === tr.seq) ? prev : [tr, ...prev].slice(0, 100)))
  }, [])
  useLayerXStream({ onTransfer, enabled: layerxConfigured() })

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }
  if (transfers.isError) {
    return <ExplorerState title={t('unavailable')} body={t('unavailableBody')} />
  }

  const loaded = transfers.data?.pages.flatMap((p) => p.transfers) ?? []
  const liveSeqs = new Set(live.map((l) => l.seq))
  const feed = [...live, ...loaded.filter((l) => !liveSeqs.has(l.seq))]

  return (
    <SectionCard
      title={t('tabTransfers')}
      action={
        <span className="text-pax flex items-center gap-1.5 text-xs">
          <span className="bg-pax size-1.5 animate-pulse rounded-full" />
          {t('live')}
        </span>
      }
    >
      {transfers.isLoading ? (
        <RowsSkeleton rows={10} />
      ) : feed.length === 0 ? (
        <p className="text-muted-foreground px-4 pt-2 pb-6 text-center text-xs">
          {t('noTransfers')}
        </p>
      ) : (
        <>
          <ul className="flex flex-col gap-0.5 p-2">
            {feed.map((tr) => (
              <TransferRow key={tr.seq} transfer={tr} fresh={liveSeqs.has(tr.seq)} />
            ))}
          </ul>
          {transfers.hasNextPage && (
            <div className="px-4 pb-4">
              <button
                type="button"
                disabled={transfers.isFetchingNextPage}
                onClick={() => void transfers.fetchNextPage()}
                className="bg-accent text-muted-foreground hover:bg-surface-hover hover:text-foreground w-full rounded-lg px-3 py-2 text-xs font-medium transition-colors disabled:opacity-60"
              >
                {transfers.isFetchingNextPage ? t('loading') : t('loadOlder')}
              </button>
            </div>
          )}
        </>
      )}
    </SectionCard>
  )
}
