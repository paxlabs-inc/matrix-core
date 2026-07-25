'use client'

import { useMemo } from 'react'
import { ArrowLeft, ArrowRight, Check, Clock, Loader2 } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { toast } from 'sonner'
import { useClaimLaunchSubscription, useSubscription } from '@/hooks/api/useOnboarding'

interface Props {
  onNext: () => void
  onBack: () => void
}

export function SubscriptionScreen({ onNext, onBack }: Props) {
  const t = useTranslations('onboarding')
  const subscription = useSubscription()
  const claim = useClaimLaunchSubscription()
  const active = subscription.data?.status === 'active'

  const expiry = useMemo(() => {
    const value = subscription.data?.ends_at
    if (!value) return ''
    return new Intl.DateTimeFormat(undefined, {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(new Date(value))
  }, [subscription.data?.ends_at])

  const handleClaim = async () => {
    try {
      await claim.mutateAsync()
      toast.success(t('promoClaimed'))
    } catch {
      toast.error(t('promoClaimError'))
    }
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="space-y-2">
        <h1 className="text-foreground text-2xl font-semibold tracking-tight">
          {t('subscriptionTitle')}
        </h1>
        <p className="text-muted-foreground text-sm text-pretty">{t('subscriptionDesc')}</p>
      </div>

      <div className="bg-card mt-6 rounded-2xl p-5">
        <div className="flex items-start justify-between gap-4">
          <div>
            <p className="text-foreground text-lg font-semibold">{t('unlimitedPlan')}</p>
            <p className="text-muted-foreground mt-1 text-xs">{t('launchOffer')}</p>
          </div>
          <span className="rounded-full bg-[#d7ead7] px-3 py-1 text-xs font-semibold text-[#18231a]">
            {t('twoDaysFree')}
          </span>
        </div>

        <div className="mt-5 space-y-3">
          {(['unlimitedRequests', 'allTools', 'cancelFree'] as const).map((key) => (
            <div key={key} className="flex items-center gap-2.5">
              <span className="flex size-5 items-center justify-center rounded-full bg-[#334237]">
                <Check className="size-3 text-[#c9e3cb]" strokeWidth={2.5} />
              </span>
              <span className="text-foreground text-sm">{t(key)}</span>
            </div>
          ))}
        </div>

        {active ? (
          <div className="mt-5 rounded-xl bg-[#29332c] p-4">
            <div className="flex items-center gap-2">
              <Clock className="size-4 text-[#c9e3cb]" strokeWidth={2.25} />
              <p className="text-foreground text-sm font-medium">{t('promoActive')}</p>
            </div>
            {expiry && (
              <p className="text-muted-foreground mt-1 text-xs">{t('activeUntil', { expiry })}</p>
            )}
          </div>
        ) : (
          <Button
            size="lg"
            className="mt-5 w-full"
            disabled={claim.isPending || subscription.isLoading}
            onClick={() => void handleClaim()}
          >
            {claim.isPending ? <Loader2 className="size-4 animate-spin" /> : null}
            {t('claimPromo')}
          </Button>
        )}
      </div>

      <p className="text-muted-foreground mt-4 text-center text-xs text-pretty">
        {t('billingComingSoon')}
      </p>

      <div className="mt-auto flex items-center gap-3 pt-8">
        <Button variant="ghost" size="lg" onClick={onBack}>
          <ArrowLeft data-icon="inline-start" />
          {t('back')}
        </Button>
        <Button size="lg" className="flex-1" disabled={!active} onClick={onNext}>
          <ArrowRight data-icon="inline-end" />
          {t('enterMatrix')}
        </Button>
      </div>
    </div>
  )
}
