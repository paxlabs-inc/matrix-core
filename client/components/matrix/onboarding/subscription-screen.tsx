'use client'

import { useMemo } from 'react'
import { ArrowLeft, ArrowRight, Check, Clock } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@astryxdesign/core/Button'
import { Card } from '@astryxdesign/core/Card'
import { Badge } from '@astryxdesign/core/Badge'
import { Heading, Text } from '@astryxdesign/core/Text'
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
        <Heading level={1}>{t('subscriptionTitle')}</Heading>
        <Text type="supporting" color="secondary" display="block">
          {t('subscriptionDesc')}
        </Text>
      </div>

      <Card variant="transparent" padding={5} className="mt-6">
        <div className="flex items-start justify-between gap-4">
          <div>
            <Heading level={2}>{t('unlimitedPlan')}</Heading>
            <Text type="supporting" color="secondary" display="block">
              {t('launchOffer')}
            </Text>
          </div>
          <Badge variant="success" label={t('twoDaysFree')} />
        </div>

        <div className="mt-5 space-y-3">
          {(['unlimitedRequests', 'allTools', 'cancelFree'] as const).map((key) => (
            <div key={key} className="flex items-center gap-2.5">
              <span className="bg-primary/15 text-primary flex size-5 items-center justify-center rounded-full">
                <Check className="size-3" strokeWidth={2.5} />
              </span>
              <span className="text-foreground text-sm">{t(key)}</span>
            </div>
          ))}
        </div>

        {active ? (
          <Card variant="muted" padding={4} className="mt-5">
            <div className="flex items-center gap-2">
              <Clock className="text-primary size-4" strokeWidth={2.25} />
              <p className="text-foreground text-sm font-medium">{t('promoActive')}</p>
            </div>
            {expiry && (
              <p className="text-muted-foreground mt-1 text-xs">{t('activeUntil', { expiry })}</p>
            )}
          </Card>
        ) : (
          <Button
            label={t('claimPromo')}
            size="lg"
            width="100%"
            className="mt-5"
            isDisabled={claim.isPending || subscription.isLoading}
            isLoading={claim.isPending}
            onClick={() => void handleClaim()}
          />
        )}
      </Card>

      <p className="text-muted-foreground mt-4 text-center text-xs text-pretty">
        {t('billingComingSoon')}
      </p>

      <div className="mt-auto flex items-center gap-3 pt-8">
        <Button label={t('back')} variant="ghost" size="lg" icon={<ArrowLeft />} onClick={onBack} />
        <Button
          label={t('enterMatrix')}
          size="lg"
          width="100%"
          isDisabled={!active}
          endContent={<ArrowRight />}
          onClick={onNext}
        />
      </div>
    </div>
  )
}
