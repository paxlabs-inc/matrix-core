'use client'

/**
 * Transfer detail — the signed, Merkle-anchored receipt for one transfer:
 * parties + amount, then the full verification chain (leaf hash, sequencer
 * signature, batch root, inclusion path, Paxeer anchor tx).
 */
import { useTranslations } from 'next-intl'
import { Link } from '@/i18n/navigation'
import { layerxConfigured, useLayerXReceipt } from '@/hooks/api/useLayerXExplorer'
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

export function ExplorerTransferDetail({ seq }: { seq: number }) {
  const t = useTranslations('layerxExplorer')
  const receipt = useLayerXReceipt(seq)

  if (!layerxConfigured()) {
    return <ExplorerState title={t('notConfigured')} body={t('notConfiguredBody')} />
  }
  if (receipt.isLoading) {
    return (
      <SectionCard title={t('transferTitle', { seq })}>
        <RowsSkeleton rows={8} />
      </SectionCard>
    )
  }
  if (receipt.isError || !receipt.data) {
    return <ExplorerState title={t('transferNotFound')} body={t('transferNotFoundBody')} />
  }
  const r = receipt.data

  return (
    <div className="flex flex-col gap-4">
      <SectionCard
        title={t('transferTitle', { seq: r.seq })}
        action={
          <StatusPill
            status={r.settled ? 'settled' : 'pending'}
            label={r.settled ? t('settled') : t('pending')}
          />
        }
      >
        <div className="flex flex-col pb-2">
          <DetailRow label={t('amount')}>
            <span className="text-foreground font-mono text-sm font-semibold">
              {r.amount_usdx} <span className="text-muted-foreground font-normal">USDX</span>
            </span>
          </DetailRow>
          <DetailRow label={t('from')}>
            <Link href={`/explorer/account/${encodeURIComponent(r.from_did)}`}>
              <HashChip value={r.from_did} display={shortMiddle(r.from_did, 28, 8)} />
            </Link>
          </DetailRow>
          <DetailRow label={t('to')}>
            <Link href={`/explorer/account/${encodeURIComponent(r.to_did)}`}>
              <HashChip value={r.to_did} display={shortMiddle(r.to_did, 28, 8)} />
            </Link>
          </DetailRow>
          <DetailRow label={t('tier')}>
            <span className="text-foreground text-sm">{r.tier}</span>
          </DetailRow>
          <DetailRow label={t('time')}>
            <span className="text-foreground text-sm">{fullTime(r.ts)}</span>
          </DetailRow>
        </div>
      </SectionCard>

      <SectionCard title={t('verification')}>
        <p className="text-muted-foreground px-4 pb-2 text-xs">{t('verificationBody')}</p>
        <div className="flex flex-col pb-2">
          <DetailRow label={t('leafHash')}>
            <HashChip value={r.leaf_hash} />
          </DetailRow>
          <DetailRow label={t('sequencerSig')}>
            <HashChip value={r.sequencer_sig} />
          </DetailRow>
          <DetailRow label={t('sequencerKey')}>
            <HashChip value={r.sequencer_pubkey} />
          </DetailRow>
          {r.batch_root && (
            <DetailRow label={t('batchRoot')}>
              <HashChip value={r.batch_root} />
            </DetailRow>
          )}
          {r.batch_id && (
            <DetailRow label={t('batch')}>
              <Link
                href={`/explorer/batch/${r.batch_id}`}
                className="text-pax font-mono text-xs hover:opacity-80"
              >
                {r.batch_id}
              </Link>
            </DetailRow>
          )}
          {r.inclusion_path && r.inclusion_path.length > 0 && (
            <DetailRow label={t('inclusionPath')}>
              <div className="flex flex-col gap-1">
                {r.inclusion_path.map((step, i) => (
                  <span
                    key={i}
                    className="text-muted-foreground truncate font-mono text-xs"
                    title={step}
                  >
                    {i + 1}. {step}
                  </span>
                ))}
              </div>
            </DetailRow>
          )}
          <DetailRow label={t('anchorTx')}>
            {r.anchor_tx ? (
              <PaxscanTxLink hash={r.anchor_tx} label={shortMiddle(r.anchor_tx, 14, 8)} />
            ) : (
              <span className="text-muted-foreground text-xs">{t('notAnchoredYet')}</span>
            )}
          </DetailRow>
        </div>
      </SectionCard>
    </div>
  )
}
