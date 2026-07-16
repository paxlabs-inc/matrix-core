'use client'

/**
 * ProvisioningWait — the brief, honest, progress-bearing wait shown when
 * the user finishes the final onboarding screen but the machine isn't
 * active yet. Polls GET /provision/status and enters the app once active.
 * Surfaces a failed state with retry (no infinite spin).
 */
import { useEffect, useState } from 'react'
import { AlertCircle, RotateCcw } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { MatrixLogo } from '@/components/matrix/matrix-logo'
import Loader from '@/components/ui/box-loader'
import { useProvisionStatus } from '@/hooks/api/useOnboarding'

const STATUS_LINES = [
  'Spinning up your private agent runtime…',
  'Provisioning secure, isolated compute…',
  'Loading your skills and tools…',
  'Connecting to the Paxeer network…',
  'Putting the finishing touches…',
]

export function ProvisioningWait({ onReady }: { onReady: () => void }) {
  const t = useTranslations('onboarding')
  const [line, setLine] = useState(0)
  const provision = useProvisionStatus(true)

  const state = provision.data?.state ?? 'provisioning'

  useEffect(() => {
    if (state === 'active') {
      onReady()
    }
  }, [state, onReady])

  useEffect(() => {
    const id = setInterval(() => setLine((i) => (i + 1) % STATUS_LINES.length), 2600)
    return () => clearInterval(id)
  }, [])

  if (state === 'failed') {
    return (
      <main className="bg-background fixed inset-0 z-50 flex flex-col items-center justify-center gap-6 px-6">
        <MatrixLogo size="lg" />
        <div className="flex max-w-sm flex-col items-center gap-3 text-center">
          <AlertCircle className="text-destructive size-8" />
          <p className="text-foreground text-sm font-medium">{t('provisionFailedTitle')}</p>
          <p className="text-muted-foreground text-xs text-pretty">{t('provisionFailedBody')}</p>
          <Button
            variant="secondary"
            size="sm"
            className="mt-2"
            onClick={() => provision.refetch()}
          >
            <RotateCcw className="size-3.5" />
            {t('retry')}
          </Button>
        </div>
      </main>
    )
  }

  return (
    <main className="bg-background fixed inset-0 z-50 flex flex-col items-center justify-center gap-8 px-6">
      <MatrixLogo size="lg" />
      <Loader size="lg" />
      <div className="mt-12 flex max-w-sm flex-col items-center gap-1 text-center">
        <p className="text-foreground text-sm font-medium">{t('provisionWaitTitle')}</p>
        <p className="text-muted-foreground text-xs text-pretty" aria-live="polite">
          {STATUS_LINES[line]}
        </p>
      </div>
    </main>
  )
}
