'use client'

import { useEffect, useState } from 'react'
import { useTranslations } from 'next-intl'
import { WifiOff } from '@/lib/matrix-icons'

/** Offline banner — surfaces connectivity loss without blocking the UI. */
export function NetworkStatus() {
  const t = useTranslations('network')
  const [offline, setOffline] = useState(false)

  useEffect(() => {
    const sync = () => setOffline(!navigator.onLine)
    sync()
    window.addEventListener('online', sync)
    window.addEventListener('offline', sync)
    return () => {
      window.removeEventListener('online', sync)
      window.removeEventListener('offline', sync)
    }
  }, [])

  if (!offline) return null

  return (
    <div
      role="status"
      aria-live="polite"
      className="bg-destructive text-destructive-foreground flex items-center justify-center gap-2 px-4 py-2 text-xs font-medium"
    >
      <WifiOff className="size-3.5" aria-hidden />
      {t('offline')}
    </div>
  )
}
