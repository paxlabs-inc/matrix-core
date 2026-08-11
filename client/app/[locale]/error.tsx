'use client'

import { useEffect } from 'react'
import { captureException } from '@/lib/observability/sentry'
import { AlertTriangle } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { EmptyState } from '@astryxdesign/core/EmptyState'
import { Button } from '@astryxdesign/core/Button'
import { CentraLogo } from '@/components/brand/centra-logo'

export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string }
  reset: () => void
}) {
  const t = useTranslations('error')

  useEffect(() => {
    console.error('[matrix:error]', error)
    void captureException(error, { digest: error.digest })
  }, [error])

  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-screen"
      content={
        <Center>
          <EmptyState
            headingLevel={1}
            title={t('title')}
            description={error.digest ? `${t('message')} ${t('id')}${error.digest}` : t('message')}
            icon={
              <div className="flex flex-col items-center gap-4">
                <CentraLogo size="lg" />
                <AlertTriangle className="text-destructive size-6" aria-hidden="true" />
              </div>
            }
            actions={
              <>
                <Button label={t('goHome')} href="/" variant="secondary" />
                <Button label={t('tryAgain')} onClick={reset} variant="primary" />
              </>
            }
          />
        </Center>
      }
    />
  )
}
