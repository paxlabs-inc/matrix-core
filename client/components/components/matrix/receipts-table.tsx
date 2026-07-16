'use client'

import { memo, useRef } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { BadgeCheck, ChevronRight } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { formatUsdFromDecimal } from '@/lib/paxeer/format'
import { relativeTime, type Receipt } from '@/lib/matrix-data'
import { cn } from '@/lib/utils'

const ROW_HEIGHT = 56
const VIRTUALIZE_THRESHOLD = 30

/**
 * One receipt as a single activatable row. Rendered as a native <button>
 * so it is focusable, Enter/Space-activatable, and announced as a button
 * without bespoke keydown handling or invalid ARIA grid roles. Memoized so
 * a parent re-render (or a sibling row scrolling into view under the
 * virtualizer) doesn't re-render every row.
 */
const ReceiptRow = memo(function ReceiptRow({
  receipt: r,
  onOpen,
  t,
}: {
  receipt: Receipt
  onOpen?: (receipt: Receipt) => void
  t: ReturnType<typeof useTranslations>
}) {
  return (
    <button
      type="button"
      onClick={() => onOpen?.(r)}
      aria-label={`${t('openReceipt')}: ${r.title}`}
      className="border-border hover:bg-muted/40 focus-visible:bg-muted/40 grid w-full cursor-pointer grid-cols-[1fr_auto] items-center gap-2 border-b px-4 py-3 text-left sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto] sm:gap-4"
    >
      <div className="min-w-0">
        <div className="text-foreground truncate text-sm font-medium">{r.title}</div>
        <div className="text-muted-foreground flex items-center gap-1.5 font-mono text-xs">
          <BadgeCheck className="text-primary size-3 shrink-0" />
          <span className="truncate">{r.id}</span>
        </div>
      </div>
      <div className="text-muted-foreground hidden font-mono text-sm tabular-nums sm:block">
        {r.stepCount}
      </div>
      <div className="text-muted-foreground hidden font-mono text-sm tabular-nums sm:block">
        {formatUsdFromDecimal(String(r.costUsd))}
      </div>
      <div className="text-muted-foreground font-mono text-xs">{relativeTime(r.signedAt)}</div>
      <span
        aria-hidden="true"
        className="text-muted-foreground flex size-7 items-center justify-center"
      >
        <ChevronRight />
      </span>
    </button>
  )
})

export function ReceiptsTable({
  receipts,
  onOpen,
}: {
  receipts: Receipt[]
  onOpen?: (receipt: Receipt) => void
}) {
  const t = useTranslations('receiptsTable')
  const parentRef = useRef<HTMLDivElement>(null)
  const virtualize = receipts.length > VIRTUALIZE_THRESHOLD

  const virtualizer = useVirtualizer({
    count: receipts.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 10,
    enabled: virtualize,
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t('title')}</CardTitle>
        <CardDescription>{t('description')}</CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        {receipts.length === 0 ? (
          <div className="px-6 pb-2">
            <Empty>
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <BadgeCheck />
                </EmptyMedia>
                <EmptyTitle>{t('emptyTitle')}</EmptyTitle>
                <EmptyDescription>{t('emptyDesc')}</EmptyDescription>
              </EmptyHeader>
            </Empty>
          </div>
        ) : (
          <>
            <div
              aria-hidden="true"
              className="text-muted-foreground hidden border-b px-4 py-2 text-xs font-medium sm:grid sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto] sm:gap-4"
            >
              <span>{t('columnTask')}</span>
              <span className="hidden sm:block">{t('columnSteps')}</span>
              <span className="hidden sm:block">{t('columnCost')}</span>
              <span>{t('columnSigned')}</span>
              <span>{/* actions */}</span>
            </div>
            <div
              ref={parentRef}
              className={cn(virtualize && 'max-h-[min(70vh,32rem)] overflow-auto')}
              role="group"
              aria-label={t('title')}
            >
              {virtualize ? (
                <div
                  className="relative w-full"
                  style={{ height: `${virtualizer.getTotalSize()}px` }}
                >
                  {virtualizer.getVirtualItems().map((vRow) => {
                    const r = receipts[vRow.index]
                    return (
                      <div
                        key={r.id}
                        className="absolute top-0 left-0 w-full"
                        style={{ transform: `translateY(${vRow.start}px)` }}
                      >
                        <ReceiptRow receipt={r} onOpen={onOpen} t={t} />
                      </div>
                    )
                  })}
                </div>
              ) : (
                receipts.map((r) => <ReceiptRow key={r.id} receipt={r} onOpen={onOpen} t={t} />)
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  )
}
