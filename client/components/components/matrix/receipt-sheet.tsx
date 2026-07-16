'use client'

import {
  BadgeCheck,
  FileText,
  MessageSquare,
  GitCommitHorizontal,
  Database,
  Link2,
  Download,
  Copy,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { type Receipt, type ReceiptArtifact } from '@/lib/matrix-data'

const ARTIFACT_ICON: Record<
  ReceiptArtifact['kind'],
  React.ComponentType<{ className?: string }>
> = {
  file: FileText,
  message: MessageSquare,
  commit: GitCommitHorizontal,
  record: Database,
  link: Link2,
}

export function ReceiptSheet({
  receipt,
  open,
  onOpenChange,
}: {
  receipt: Receipt | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const t = useTranslations('receiptSheet')

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full gap-0 overflow-y-auto p-0 sm:max-w-md">
        <SheetHeader className="border-border border-b">
          <div className="flex items-center gap-2">
            <BadgeCheck className="text-primary size-5" />
            <SheetTitle className="font-mono text-sm">{t('title')}</SheetTitle>
          </div>
          <SheetDescription className="text-pretty">
            {receipt?.title ?? t('noReceipt')}
          </SheetDescription>
        </SheetHeader>

        {receipt && (
          <div className="flex flex-col">
            <div className="px-4 py-5">
              <div className="receipt-edge bg-popover text-foreground rounded-t-md p-5 font-mono text-xs">
                <div className="flex flex-col items-center gap-1 pb-4 text-center">
                  <span className="text-sm font-semibold tracking-tight">{t('header')}</span>
                  <span className="text-muted-foreground">{receipt.id}</span>
                </div>

                <Separator className="bg-border my-3" />

                <dl className="flex flex-col gap-2">
                  <Row label={t('run')} value={receipt.runId} />
                  <Row label={t('signed')} value={new Date(receipt.signedAt).toLocaleString()} />
                  <Row label={t('steps')} value={String(receipt.stepCount)} />
                  <Row label={t('toolCalls')} value={String(receipt.toolCallCount)} />
                  <Row label={t('cost')} value={`$${receipt.costUsd.toFixed(2)}`} />
                </dl>

                <Separator className="bg-border my-3" />

                <dl className="flex flex-col gap-2">
                  <Row label={t('hash')} value={receipt.hash} truncate />
                  <Row label={t('signature')} value={receipt.signature} />
                  <div className="flex items-center justify-between pt-1">
                    <span className="text-muted-foreground">{t('replay')}</span>
                    <span className="text-primary inline-flex items-center gap-1">
                      <BadgeCheck className="size-3.5" />
                      {receipt.replayVerified ? t('verified') : t('unverified')}
                    </span>
                  </div>
                </dl>

                <div className="receipt-edge bg-popover mt-4 h-3" aria-hidden="true" />
              </div>
            </div>

            <div className="flex flex-col gap-3 px-4 pb-4">
              <h4 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
                {t('artifacts')}
              </h4>
              <ul className="flex flex-col gap-2">
                {receipt.artifacts.map((a) => {
                  const Icon = ARTIFACT_ICON[a.kind]
                  return (
                    <li
                      key={a.id}
                      className="border-border bg-card flex items-start gap-3 rounded-lg border p-3"
                    >
                      <span className="bg-primary/10 text-primary mt-0.5 flex size-7 items-center justify-center rounded-md">
                        <Icon className="size-4" />
                      </span>
                      <div className="flex min-w-0 flex-col">
                        <span className="text-foreground truncate text-sm font-medium">
                          {a.name}
                        </span>
                        <span className="text-muted-foreground text-xs text-pretty">
                          {a.summary}
                        </span>
                      </div>
                    </li>
                  )
                })}
              </ul>
            </div>

            <div className="border-border flex gap-2 border-t p-4">
              <Button className="flex-1">
                <Download data-icon="inline-start" />
                {t('export')}
              </Button>
              <Button variant="outline" className="flex-1">
                <Copy data-icon="inline-start" />
                {t('copyHash')}
              </Button>
            </div>
          </div>
        )}
      </SheetContent>
    </Sheet>
  )
}

function Row({ label, value, truncate }: { label: string; value: string; truncate?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground shrink-0">{label}</span>
      <span className={truncate ? 'truncate text-right' : 'text-right'}>{value}</span>
    </div>
  )
}
