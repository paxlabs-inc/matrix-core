'use client'

import { useCallback, useEffect, useState } from 'react'
import { Link } from '@/i18n/navigation'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { cn } from '@/lib/utils'
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
        className="border-border bg-card/95 fixed inset-x-4 bottom-4 z-50 mx-auto max-w-2xl rounded-xl border p-4 shadow-lg backdrop-blur sm:inset-x-auto sm:right-6 sm:bottom-6"
      >
        <p id="cookie-consent-title" className="text-foreground text-sm font-medium">
          We use cookies
        </p>
        <p className="text-muted-foreground mt-1 text-xs text-pretty">
          Matrix uses strictly necessary cookies to run, and — with your consent — functional and
          analytics cookies to improve the experience. See our{' '}
          <Link href="/cookies" className="text-primary underline-offset-2 hover:underline">
            Cookie Policy
          </Link>{' '}
          and{' '}
          <Link href="/privacy" className="text-primary underline-offset-2 hover:underline">
            Privacy Policy
          </Link>
          .
        </p>
        {gpc ? (
          <p className="border-destructive/30 bg-destructive/10 text-muted-foreground mt-3 rounded-lg border px-3 py-2 text-xs">
            We detected a Global Privacy Control / Do Not Track signal, so analytics is switched off
            by default. You can change this in customize.
          </p>
        ) : null}
        <div className="mt-3 flex flex-wrap justify-end gap-2">
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => persist({ functional: false, analytics: false })}
          >
            Reject non-essential
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={() => setShowOptions(true)}>
            Customize
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={() => persist({ functional: true, analytics: true })}
          >
            Accept all
          </Button>
        </div>
      </div>
    )
  }

  return (
    <Dialog
      open={visible}
      onOpenChange={(open) => {
        if (!open) {
          if (!readStoredConsent()) persist({ functional: false, analytics: false })
          else close()
        }
      }}
    >
      <DialogContent className="max-w-lg gap-0 p-0 sm:max-w-lg" showCloseButton={false}>
        <DialogHeader className="space-y-2 px-6 pt-6">
          <p className="text-primary bg-primary/10 w-fit rounded-full px-3 py-1 text-xs font-semibold tracking-wider uppercase">
            Your privacy
          </p>
          <DialogTitle>Cookie preferences</DialogTitle>
          <DialogDescription>
            Choose which optional cookies Matrix may use. Strictly necessary cookies are always
            enabled. See our{' '}
            <Link href="/cookies" className="text-primary underline-offset-2 hover:underline">
              Cookie Policy
            </Link>
            .
          </DialogDescription>
        </DialogHeader>

        {gpc ? (
          <p className="border-destructive/30 bg-destructive/10 text-muted-foreground mx-6 mt-4 rounded-lg border px-3 py-2 text-xs">
            We detected a Global Privacy Control / Do Not Track signal, so analytics is switched off
            by default.
          </p>
        ) : null}

        <div className="divide-border mx-6 mt-4 divide-y rounded-lg border">
          <div className="flex items-start justify-between gap-4 p-4">
            <div className="space-y-1">
              <Label className="text-sm font-medium">Strictly necessary</Label>
              <p className="text-muted-foreground text-xs leading-relaxed">
                Required for security, sessions, and connecting your wallet. Always on.
              </p>
            </div>
            <Switch checked disabled aria-readonly />
          </div>
          <div className="flex items-start justify-between gap-4 p-4">
            <div className="space-y-1">
              <Label htmlFor="cookie-functional" className="text-sm font-medium">
                Functional
              </Label>
              <p className="text-muted-foreground text-xs leading-relaxed">
                Remembers preferences such as language and interface settings.
              </p>
            </div>
            <Switch id="cookie-functional" checked={functional} onCheckedChange={setFunctional} />
          </div>
          <div className="flex items-start justify-between gap-4 p-4">
            <div className="space-y-1">
              <Label htmlFor="cookie-analytics" className="text-sm font-medium">
                Analytics
              </Label>
              <p className="text-muted-foreground text-xs leading-relaxed">
                Aggregated, privacy-respecting usage measurement to help us improve.
              </p>
            </div>
            <Switch id="cookie-analytics" checked={analytics} onCheckedChange={setAnalytics} />
          </div>
        </div>

        <DialogFooter className={cn('flex-col gap-2 px-6 pt-4 pb-6 sm:flex-row sm:justify-end')}>
          <Button
            type="button"
            variant="outline"
            onClick={() => persist({ functional: false, analytics: false })}
          >
            Reject non-essential
          </Button>
          <Button type="button" onClick={() => persist({ functional, analytics })}>
            Save my choices
          </Button>
          <Button
            type="button"
            variant="secondary"
            onClick={() => persist({ functional: true, analytics: true })}
          >
            Accept all
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
