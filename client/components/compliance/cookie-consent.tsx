'use client'

import { lazy, Suspense, useCallback, useEffect, useState } from 'react'
import { Link } from '@/i18n/navigation'
import {
  bindCookieSettingsTriggers,
  clearConsentCookie,
  installMatrixConsentApi,
  privacySignal,
  readStoredConsent,
  registerOpenPreferences,
  saveConsent,
  unregisterOpenPreferences,
  type MatrixConsentChoice,
} from '@/lib/consent/matrix-consent'

const CookiePreferencesDialog = lazy(async () => {
  const preferences = await import('./cookie-preferences-dialog')
  return { default: preferences.CookiePreferencesDialog }
})

export function CookieConsent() {
  const [visible, setVisible] = useState(false)
  const [showOptions, setShowOptions] = useState(false)
  const [functional, setFunctional] = useState(true)
  const [analytics, setAnalytics] = useState(() => !privacySignal())

  const openPreferences = useCallback(() => {
    const existing = readStoredConsent()
    setFunctional(existing ? existing.functional : true)
    setAnalytics(existing ? existing.analytics : !privacySignal())
    setShowOptions(true)
    setVisible(true)
  }, [])

  useEffect(() => {
    installMatrixConsentApi()
    registerOpenPreferences(openPreferences)

    const existing = readStoredConsent()
    if (existing) {
      setVisible(false)
    } else {
      const stale = document.cookie.includes('mx_consent=')
      if (stale) clearConsentCookie()
      setVisible(true)
    }

    const unbindTriggers = bindCookieSettingsTriggers(openPreferences)

    return () => {
      unregisterOpenPreferences()
      unbindTriggers()
    }
  }, [openPreferences])

  const close = useCallback(() => {
    setVisible(false)
    setShowOptions(false)
  }, [])

  const persist = useCallback(
    (choice: MatrixConsentChoice) => {
      saveConsent(choice)
      close()
    },
    [close],
  )

  if (!visible) return null

  const gpc = privacySignal()

  if (!showOptions) {
    return (
      <div
        role="dialog"
        aria-labelledby="cookie-consent-title"
        className="bg-card fixed inset-x-4 bottom-4 z-50 mx-auto max-w-2xl rounded-xl p-4 sm:inset-x-auto sm:right-6 sm:bottom-6"
      >
        <p id="cookie-consent-title" className="text-foreground text-sm font-medium">
          We use cookies
        </p>
        <p className="text-muted-foreground mt-1 text-xs text-pretty">
          Centra AI uses strictly necessary cookies to run, and — with your consent — functional and
          analytics cookies to improve the experience. See our{' '}
          <Link
            href="/cookies"
            prefetch={false}
            className="text-primary underline-offset-2 hover:underline"
          >
            Cookie Policy
          </Link>{' '}
          and{' '}
          <Link
            href="/privacy"
            prefetch={false}
            className="text-primary underline-offset-2 hover:underline"
          >
            Privacy Policy
          </Link>
          .
        </p>
        {gpc ? (
          <p className="bg-destructive/10 text-muted-foreground mt-3 rounded-lg px-3 py-2 text-xs">
            We detected a Global Privacy Control / Do Not Track signal, so analytics is switched off
            by default. You can change this in customize.
          </p>
        ) : null}
        <div className="mt-3 flex flex-wrap justify-end gap-2">
          <button
            type="button"
            className="bg-muted text-foreground hover:bg-accent rounded-lg px-3 py-2 text-xs font-medium transition-colors"
            onClick={() => persist({ functional: false, analytics: false })}
          >
            Reject non-essential
          </button>
          <button
            type="button"
            className="bg-muted text-foreground hover:bg-accent rounded-lg px-3 py-2 text-xs font-medium transition-colors"
            onClick={() => setShowOptions(true)}
          >
            Customize
          </button>
          <button
            type="button"
            className="bg-primary text-primary-foreground hover:bg-primary/90 rounded-lg px-3 py-2 text-xs font-medium transition-colors"
            onClick={() => persist({ functional: true, analytics: true })}
          >
            Accept all
          </button>
        </div>
      </div>
    )
  }

  return (
    <Suspense fallback={null}>
      <CookiePreferencesDialog
        open={visible}
        gpc={gpc}
        functional={functional}
        analytics={analytics}
        onFunctionalChange={setFunctional}
        onAnalyticsChange={setAnalytics}
        onClose={() => {
          if (!readStoredConsent()) persist({ functional: false, analytics: false })
          else close()
        }}
        onPersist={persist}
      />
    </Suspense>
  )
}
