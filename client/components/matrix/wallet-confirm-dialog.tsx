'use client'

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { useTranslations } from 'next-intl'

export interface WalletConfirmPayload {
  action: 'fund' | 'sweep' | 'send' | 'sign'
  amount?: string
  symbol?: string
  destination?: string
  message?: string
}

interface WalletConfirmDialogProps {
  open: boolean
  payload: WalletConfirmPayload | null
  pending?: boolean
  onConfirm: () => void
  onCancel: () => void
}

/** Explicit confirmation before any wallet mutation or signing call. */
export function WalletConfirmDialog({
  open,
  payload,
  pending,
  onConfirm,
  onCancel,
}: WalletConfirmDialogProps) {
  const t = useTranslations('walletConfirm')

  if (!payload) return null

  const title =
    payload.action === 'fund'
      ? t('titleFund')
      : payload.action === 'sweep'
        ? t('titleSweep')
        : payload.action === 'sign'
          ? t('titleSign')
          : t('titleSend')

  return (
    <AlertDialog open={open} onOpenChange={(v) => !v && onCancel()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription asChild>
            <div className="text-muted-foreground space-y-2 text-sm">
              {payload.amount && payload.symbol && (
                <p>
                  <span className="text-foreground font-medium tabular-nums">
                    {payload.amount} {payload.symbol}
                  </span>
                </p>
              )}
              {payload.destination && (
                <p>
                  {t('destination')}{' '}
                  <code className="bg-muted rounded px-1 py-0.5 font-mono text-xs">
                    {payload.destination}
                  </code>
                </p>
              )}
              {payload.message && (
                <p>
                  {t('message')}{' '}
                  <code className="bg-muted block max-h-24 overflow-auto rounded p-2 font-mono text-xs whitespace-pre-wrap">
                    {payload.message}
                  </code>
                </p>
              )}
              <p>{t('warning')}</p>
            </div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending} onClick={onCancel}>
            {t('cancel')}
          </AlertDialogCancel>
          <AlertDialogAction disabled={pending} onClick={onConfirm}>
            {pending ? t('confirming') : t('confirm')}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
