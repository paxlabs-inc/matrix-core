'use client'

/**
 * Account view — one agent DID: balance, escrow, mapped payout address and
 * full transfer history (both directions, keyset-paginated).
 */
import { useTranslations } from 'next-intl'
import {
  layerxConfigured,
  useLayerXAccountView,
  useLayerXTransfers,
} from '@/hooks/api/useLayerXExplorer'
import {
  ExplorerState,
  HashChip,
  RowsSkeleton,
  SectionCard,
  StatCard,
  shortMiddle,
} from './explorer-bits'
import { TransferRow } from './explorer-rows'

export function ExplorerAccount({ did }: { did: string }) {
  const t = useTranslations('layerxExplorer')
  const account = useLayerXAccountView(did)
  const history = useLayerXTransfers(did, 50)

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }
  if (account.isError) {
    return <ExplorerState title={t('unavailable')} body={t('unavailableBody')} />
  }

  const a = account.data
  const rows = history.data?.pages.flatMap((p) => p.transfers) ?? []

  return (
    <div className="flex flex-col gap-4">
      <div className="bg-card flex flex-col gap-3 rounded-xl p-4">
        <span className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
          {t('account')}
        </span>
        <HashChip value={did} display={shortMiddle(did, 34, 10)} className="w-fit" />
        {a?.evm_address && (
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground text-xs">{t('payoutAddress')}</span>
            <HashChip value={a.evm_address} />
          </div>
        )}
      </div>

      <div className="grid grid-cols-2 gap-3">
        <StatCard
          label={t('balance')}
          value={a ? a.balance_usdx : '—'}
          hint={a ? 'USDX' : undefined}
          accent
        />
        <StatCard
          label={t('escrow')}
          value={a ? a.escrow_usdx : '—'}
          hint={a ? 'USDX' : undefined}
        />
      </div>

      <SectionCard title={t('accountHistory')}>
        {history.isLoading ? (
          <RowsSkeleton rows={6} />
        ) : rows.length === 0 ? (
          <p className="text-muted-foreground px-4 pt-2 pb-6 text-center text-xs">
            {t('noTransfers')}
          </p>
        ) : (
          <>
            <ul className="flex flex-col gap-0.5 p-2">
              {rows.map((tr) => (
                <TransferRow key={tr.seq} transfer={tr} />
              ))}
            </ul>
            {history.hasNextPage && (
              <div className="px-4 pb-4">
                <button
                  type="button"
                  disabled={history.isFetchingNextPage}
                  onClick={() => void history.fetchNextPage()}
                  className="bg-accent text-muted-foreground hover:bg-surface-hover hover:text-foreground w-full rounded-lg px-3 py-2 text-xs font-medium transition-colors disabled:opacity-60"
                >
                  {history.isFetchingNextPage ? t('loading') : t('loadOlder')}
                </button>
              </div>
            )}
          </>
        )}
      </SectionCard>
    </div>
  )
}
