'use client'

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
import type { MatrixConsentChoice } from '@/lib/consent/matrix-consent'

export function CookiePreferencesDialog({
  open,
  gpc,
  functional,
  analytics,
  onFunctionalChange,
  onAnalyticsChange,
  onClose,
  onPersist,
}: {
  open: boolean
  gpc: boolean
  functional: boolean
  analytics: boolean
  onFunctionalChange: (checked: boolean) => void
  onAnalyticsChange: (checked: boolean) => void
  onClose: () => void
  onPersist: (choice: MatrixConsentChoice) => void
}) {
  return (
    <Dialog open={open} onOpenChange={(nextOpen) => !nextOpen && onClose()}>
      <DialogContent className="max-w-lg gap-0 p-0 sm:max-w-lg" showCloseButton={false}>
        <DialogHeader className="space-y-2 px-6 pt-6">
          <p className="text-primary bg-primary/10 w-fit rounded-full px-3 py-1 text-xs font-semibold tracking-wider uppercase">
            Your privacy
          </p>
          <DialogTitle>Cookie preferences</DialogTitle>
          <DialogDescription>
            Choose which optional cookies Centra AI may use. Strictly necessary cookies are always
            enabled. See our{' '}
            <Link
              href="/cookies"
              prefetch={false}
              className="text-primary underline-offset-2 hover:underline"
            >
              Cookie Policy
            </Link>
            .
          </DialogDescription>
        </DialogHeader>

        {gpc ? (
          <p className="bg-destructive/10 text-muted-foreground mx-6 mt-4 rounded-lg px-3 py-2 text-xs">
            We detected a Global Privacy Control / Do Not Track signal, so analytics is switched off
            by default.
          </p>
        ) : null}

        <div className="bg-muted mx-6 mt-4 rounded-lg">
          <div className="flex items-start justify-between gap-4 p-4">
            <div className="space-y-1">
              <Label className="text-sm font-medium">Strictly necessary</Label>
              <p className="text-muted-foreground text-xs leading-relaxed">
                Required for security, sessions, and connecting your wallet. Always on.
              </p>
            </div>
            <Switch checked disabled aria-readonly />
          </div>
          <div className="bg-card flex items-start justify-between gap-4 p-4">
            <div className="space-y-1">
              <Label htmlFor="cookie-functional" className="text-sm font-medium">
                Functional
              </Label>
              <p className="text-muted-foreground text-xs leading-relaxed">
                Remembers preferences such as language and interface settings.
              </p>
            </div>
            <Switch
              id="cookie-functional"
              checked={functional}
              onCheckedChange={onFunctionalChange}
            />
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
            <Switch id="cookie-analytics" checked={analytics} onCheckedChange={onAnalyticsChange} />
          </div>
        </div>

        <DialogFooter className="flex-col gap-2 px-6 pt-4 pb-6 sm:flex-row sm:justify-end">
          <Button
            type="button"
            variant="outline"
            onClick={() => onPersist({ functional: false, analytics: false })}
          >
            Reject non-essential
          </Button>
          <Button type="button" onClick={() => onPersist({ functional, analytics })}>
            Save my choices
          </Button>
          <Button
            type="button"
            variant="secondary"
            onClick={() => onPersist({ functional: true, analytics: true })}
          >
            Accept all
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
