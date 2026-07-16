'use client'

import { useEffect } from 'react'
import { captureException } from '@/lib/observability/sentry'
import { Link } from '@/i18n/navigation'
import { AlertTriangle } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

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
    <div className="bg-background flex min-h-screen flex-col items-center justify-center gap-6 p-8 text-center">
      <MatrixLogo size="lg" />

      <div className="flex flex-col items-center gap-3">
        <div className="bg-destructive/10 rounded-full p-3">
          <AlertTriangle className="text-destructive size-6" aria-hidden="true" />
        </div>
        <h1 className="text-foreground text-lg font-semibold">{t('title')}</h1>
        <p className="text-muted-foreground max-w-sm text-sm">{t('message')}</p>
        {error.digest && (
          <p className="text-muted-foreground/60 font-mono text-xs">
            {t('id')}
            {error.digest}
          </p>
        )}
      </div>

      <div className="flex gap-3">
        <Button variant="outline" asChild>
          <Link href="/">{t('goHome')}</Link>
        </Button>
        <Button onClick={reset}>{t('tryAgain')}</Button>
      </div>
    </div>
  )
}
