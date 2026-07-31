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
import { Dialog } from '@astryxdesign/core/Dialog'
import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { EmptyState } from '@astryxdesign/core/EmptyState'
import { Button } from '@astryxdesign/core/Button'
import { Spinner } from '@astryxdesign/core/Spinner'
import { Text } from '@astryxdesign/core/Text'
import { MatrixLogo } from '@/components/matrix/matrix-logo'
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
      <Dialog isOpen onOpenChange={() => undefined} variant="fullscreen" purpose="required">
        <Layout
          height="fill"
          content={
            <Center>
              <EmptyState
                headingLevel={1}
                title={t('provisionFailedTitle')}
                description={t('provisionFailedBody')}
                icon={
                  <div className="flex flex-col items-center gap-4">
                    <MatrixLogo size="lg" />
                    <AlertCircle className="text-destructive size-8" />
                  </div>
                }
                actions={
                  <Button
                    label={t('retry')}
                    variant="secondary"
                    icon={<RotateCcw />}
                    onClick={() => void provision.refetch()}
                  />
                }
              />
            </Center>
          }
        />
      </Dialog>
    )
  }

  return (
    <Dialog isOpen onOpenChange={() => undefined} variant="fullscreen" purpose="required">
      <Layout
        height="fill"
        content={
          <Center>
            <div className="flex max-w-sm flex-col items-center gap-5 text-center">
              <MatrixLogo size="lg" />
              <Spinner size="xl" label={t('provisionWaitTitle')} />
              <Text type="supporting" color="secondary" display="block" aria-live="polite">
                {STATUS_LINES[line]}
              </Text>
            </div>
          </Center>
        }
      />
    </Dialog>
  )
}
