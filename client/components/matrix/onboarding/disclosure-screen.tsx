'use client'

/**
 * DisclosureScreen — Screen 2 of onboarding. Beta disclosure acknowledgement
 * (POST /disclosure/ack) + training-consent toggle (PUT /consent). Proceeds
 * during provisioning (router-only, no daemon needed). Privacy-respecting
 * default (opt-out).
 */
import { useState } from 'react'
import { ArrowLeft, ArrowRight, ShieldAlert } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@astryxdesign/core/Button'
import { Card } from '@astryxdesign/core/Card'
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Switch } from '@/components/matrix/astryx-switch'
import { toast } from 'sonner'
import { useAckDisclosure, usePutConsent, useStartOnboarding } from '@/hooks/api/useOnboarding'

const DISCLOSURE_VERSION = 'public-launch-1'

interface Props {
  onNext: () => void
  onBack?: () => void
}

export function DisclosureScreen({ onNext, onBack }: Props) {
  const t = useTranslations('onboarding')
  const [acknowledged, setAcknowledged] = useState(false)
  const [trainingOptIn, setTrainingOptIn] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const ackDisclosure = useAckDisclosure()
  const putConsent = usePutConsent()
  const startOnboarding = useStartOnboarding()

  const handleSubmit = async () => {
    if (!acknowledged) {
      toast.error(t('ackRequired'))
      return
    }
    setSubmitting(true)
    try {
      // The acknowledgement + consent MUST be recorded before proceeding
      // (req 4.1). On failure, stay on the screen and surface a retry —
      // do NOT advance, or the decision would be silently lost. These hit
      // only the central router, so they don't depend on daemon readiness.
      await Promise.all([
        ackDisclosure.mutateAsync(DISCLOSURE_VERSION),
        putConsent.mutateAsync({ optIn: trainingOptIn, policyVersion: DISCLOSURE_VERSION }),
      ])
      await startOnboarding.mutateAsync()
      onNext()
    } catch {
      toast.error(t('consentError'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="space-y-2">
        <Heading level={1}>{t('disclosureTitle')}</Heading>
        <Text type="supporting" color="secondary" display="block">
          {t('disclosureDesc')}
        </Text>
      </div>

      <div className="mt-6 space-y-4">
        <Card variant="red" padding={4}>
          <div className="flex items-start gap-3">
            <ShieldAlert className="text-destructive mt-0.5 size-5 shrink-0" />
            <div className="space-y-2">
              <Text type="label" weight="bold" display="block">
                {t('betaNoticeTitle')}
              </Text>
              <Text type="supporting" color="secondary" display="block">
                {t('betaNoticeBody')}
              </Text>
            </div>
          </div>
        </Card>

        <Card variant="transparent" padding={4}>
          <CheckboxInput
            label={t('ackLabel')}
            description={t('ackHelp')}
            value={acknowledged}
            onChange={setAcknowledged}
            isRequired
            width="100%"
          />
        </Card>

        <Card variant="transparent" padding={4}>
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0 flex-1">
              <p className="text-foreground text-sm font-medium">{t('consentTitle')}</p>
              <p className="text-muted-foreground mt-1 text-xs text-pretty">{t('consentBody')}</p>
            </div>
            <Switch
              checked={trainingOptIn}
              onCheckedChange={setTrainingOptIn}
              aria-label={t('consentTitle')}
            />
          </div>
          <p className="text-muted-foreground/70 mt-3 text-[11px] text-pretty">
            {t('consentNote')}
          </p>
        </Card>
      </div>

      <div className="mt-auto flex items-center gap-3 pt-8">
        {onBack && (
          <Button
            label={t('back')}
            variant="ghost"
            size="lg"
            icon={<ArrowLeft />}
            onClick={onBack}
          />
        )}
        <Button
          label={t('approveAndContinue')}
          size="lg"
          width="100%"
          isDisabled={submitting || !acknowledged}
          isLoading={submitting}
          endContent={<ArrowRight />}
          onClick={() => void handleSubmit()}
        />
      </div>
    </div>
  )
}
