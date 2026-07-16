'use client'

/**
 * Settlement batches — every sealed window, newest first, with its status
 * on the road to a Paxeer anchor.
 */
import { useTranslations } from 'next-intl'
import { layerxConfigured, useLayerXBatches } from '@/hooks/api/useLayerXExplorer'
import { ExplorerState, RowsSkeleton, SectionCard } from './explorer-bits'
import { BatchRow } from './explorer-rows'

export function ExplorerBatches() {
  const t = useTranslations('layerxExplorer')
  const batches = useLayerXBatches(25)

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }
  if (batches.isError) {
    return <ExplorerState title={t('unavailable')} body={t('unavailableBody')} />
  }

  const rows = batches.data?.pages.flatMap((p) => p.batches) ?? []

  return (
    <SectionCard title={t('tabBatches')}>
      {batches.isLoading ? (
        <RowsSkeleton rows={8} />
      ) : rows.length === 0 ? (
        <p className="text-muted-foreground px-4 pt-2 pb-6 text-center text-xs">{t('noBatches')}</p>
      ) : (
        <>
          <ul className="flex flex-col gap-0.5 p-2">
            {rows.map((b) => (
              <BatchRow key={b.id} batch={b} />
            ))}
          </ul>
          {batches.hasNextPage && (
            <div className="px-4 pb-4">
              <button
                type="button"
                disabled={batches.isFetchingNextPage}
                onClick={() => void batches.fetchNextPage()}
                className="bg-accent text-muted-foreground hover:bg-surface-hover hover:text-foreground w-full rounded-lg px-3 py-2 text-xs font-medium transition-colors disabled:opacity-60"
              >
                {batches.isFetchingNextPage ? t('loading') : t('loadOlder')}
              </button>
            </div>
          )}
        </>
      )}
    </SectionCard>
  )
}
