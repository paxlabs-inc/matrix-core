'use client'

/**
 * DisclosureScreen — Screen 2 of onboarding. Beta disclosure acknowledgement
 * (POST /disclosure/ack) + training-consent toggle (PUT /consent). Proceeds
 * during provisioning (router-only, no daemon needed). Privacy-respecting
 * default (opt-out).
 */
import { useState } from 'react'
import { ArrowLeft, ArrowRight, Check, Loader2, ShieldAlert } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { toast } from 'sonner'
import { useAckDisclosure, usePutConsent } from '@/hooks/api/useOnboarding'

const DISCLOSURE_VERSION = '1'

interface Props {
  onNext: () => void
  onBack: () => void
}

export function DisclosureScreen({ onNext, onBack }: Props) {
  const t = useTranslations('onboarding')
  const [acknowledged, setAcknowledged] = useState(false)
  const [trainingOptIn, setTrainingOptIn] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const ackDisclosure = useAckDisclosure()
  const putConsent = usePutConsent()

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
        <h1 className="text-foreground text-2xl font-semibold tracking-tight">
          {t('disclosureTitle')}
        </h1>
        <p className="text-muted-foreground text-sm text-pretty">{t('disclosureDesc')}</p>
      </div>

      <div className="mt-6 space-y-4">
        <div className="bg-destructive/10 rounded-lg p-4">
          <div className="flex items-start gap-3">
            <ShieldAlert className="text-destructive mt-0.5 size-5 shrink-0" />
            <div className="space-y-2">
              <p className="text-foreground text-sm font-medium">{t('betaNoticeTitle')}</p>
              <p className="text-muted-foreground text-xs text-pretty">{t('betaNoticeBody')}</p>
            </div>
          </div>
        </div>

        <label className="bg-card flex cursor-pointer items-start gap-3 rounded-lg p-4">
          <button
            type="button"
            onClick={() => setAcknowledged(!acknowledged)}
            className={`mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-md transition-colors ${
              acknowledged ? 'bg-primary' : 'bg-muted hover:bg-muted/80'
            }`}
            aria-label={t('ackLabel')}
          >
            {acknowledged && <Check className="text-primary-foreground size-3.5" />}
          </button>
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('ackLabel')}</p>
            <p className="text-muted-foreground text-xs text-pretty">{t('ackHelp')}</p>
          </div>
        </label>

        <div className="bg-card rounded-lg p-4">
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
        </div>
      </div>

      <div className="mt-auto flex items-center gap-3 pt-8">
        <Button variant="ghost" size="lg" onClick={onBack}>
          <ArrowLeft data-icon="inline-start" />
          {t('back')}
        </Button>
        <Button
          size="lg"
          className="flex-1"
          disabled={submitting || !acknowledged}
          onClick={() => void handleSubmit()}
        >
          {submitting ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <ArrowRight data-icon="inline-end" />
          )}
          {t('continue')}
        </Button>
      </div>
    </div>
  )
}
