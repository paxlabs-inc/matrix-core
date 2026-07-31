'use client'

/**
 * Batch detail — one settlement window: the committed Merkle root, its
 * status, the window bounds and the Paxeer anchor transaction.
 */
import { useTranslations } from 'next-intl'
import { layerxConfigured, useLayerXBatch } from '@/hooks/api/useLayerXExplorer'
import {
  DetailRow,
  ExplorerState,
  HashChip,
  PaxscanTxLink,
  RowsSkeleton,
  SectionCard,
  StatusPill,
  fullTime,
  shortMiddle,
} from './explorer-bits'
import { statusLabel } from './explorer-rows'

export function ExplorerBatchDetail({ id }: { id: string }) {
  const t = useTranslations('layerxExplorer')
  const batch = useLayerXBatch(id)

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }
  if (batch.isLoading) {
    return (
      <SectionCard title={t('batchTitle')}>
        <RowsSkeleton rows={6} />
      </SectionCard>
    )
  }
  if (batch.isError || !batch.data) {
    return <ExplorerState title={t('batchNotFound')} body={t('batchNotFoundBody')} />
  }
  const b = batch.data

  return (
    <SectionCard
      title={t('batchTitle')}
      action={<StatusPill status={b.status} label={statusLabel(t, b.status)} />}
    >
      <div className="flex flex-col pb-2">
        <DetailRow label={t('batchId')}>
          <HashChip value={b.id} display={b.id} />
        </DetailRow>
        <DetailRow label={t('merkleRoot')}>
          <HashChip value={b.root} />
        </DetailRow>
        <DetailRow label={t('transferCount')}>
          <span className="text-foreground font-mono text-sm">{b.transfer_count}</span>
        </DetailRow>
        <DetailRow label={t('window')}>
          <span className="text-foreground text-sm">
            {fullTime(b.window_start)} — {fullTime(b.window_end)}
          </span>
        </DetailRow>
        <DetailRow label={t('created')}>
          <span className="text-foreground text-sm">{fullTime(b.created_at)}</span>
        </DetailRow>
        <DetailRow label={t('anchorTx')}>
          {b.anchor_tx ? (
            <PaxscanTxLink hash={b.anchor_tx} label={shortMiddle(b.anchor_tx, 14, 8)} />
          ) : (
            <span className="text-muted-foreground text-xs">{t('notAnchoredYet')}</span>
          )}
        </DetailRow>
      </div>
    </SectionCard>
  )
}
