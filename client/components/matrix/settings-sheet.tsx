'use client'

/**
 * SettingsSheet — account, preferences, and the legal entrance.
 *
 * The Adaptive Surface has no pages or sidebars, so settings live in a
 * slide-over (mirroring ChatHistorySheet): sign-out, language, the
 * notification toggles, and links into the static legal & compliance
 * site served from /public/legal.
 */
import { ExternalLink, FileText, Globe, LogOut, RotateCcw, Bug } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from '@/components/ui/sheet'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { LocaleSwitcher } from '@/components/matrix/locale-switcher'
import { useAuth, userDisplayName } from '@/lib/auth/AuthProvider'
import { usePrefs } from '@/lib/prefs'
import { useRouter } from '@/i18n/navigation'
import { BugReportDialog } from '@/components/matrix/bug-report-dialog'
import { useConsent, usePutConsent } from '@/hooks/api/useOnboarding'
import { ONBOARDED_KEY, STORAGE_KEY } from '@/components/matrix/onboarding/onboarding-flow'
import { AutomatrixSection } from '@/components/matrix/automatrix-section'

/** The static legal & compliance site under /public/legal — served as plain
 *  HTML with its own styling, so links open in a new tab. The hub page links
 *  to every document; the quick links cover the ones users actually look for. */
const LEGAL_HUB = '/legal/index.html'
const LEGAL_QUICK_LINKS: { key: string; href: string }[] = [
  { key: 'legalTerms', href: '/legal/terms-of-service.html' },
  { key: 'legalPrivacy', href: '/legal/privacy-policy.html' },
  { key: 'legalCookies', href: '/legal/cookie-policy.html' },
  { key: 'legalAcceptableUse', href: '/legal/acceptable-use-policy.html' },
  { key: 'legalRisk', href: '/legal/risk-disclosure.html' },
  { key: 'legalAiUse', href: '/legal/ai-agent-responsible-use-policy.html' },
]

function deriveInitials(label: string): string {
  const words = label.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return 'MX'
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase()
  return (words[0][0] + words[words.length - 1][0]).toUpperCase()
}

export function SettingsSheet({
  open,
  onOpenChange,
  conversationId,
  intentId,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversationId?: string | null
  intentId?: string | null
}) {
  const t = useTranslations('settingsPanel')
  const tOnboard = useTranslations('onboarding')
  const { user, enabled: authEnabled, signOut } = useAuth()
  const [prefs, setPrefs] = usePrefs()
  const router = useRouter()
  const consentQuery = useConsent()
  const putConsent = usePutConsent()

  const userLabel = userDisplayName(user) ?? 'Matrix'
  const email = user?.email ?? null

  const onSignOut = async () => {
    await signOut()
    onOpenChange(false)
    router.replace('/login')
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-80 gap-0 p-0 sm:max-w-sm">
        <SheetHeader className="gap-1">
          <SheetTitle>{t('settingsTitle')}</SheetTitle>
          <SheetDescription>{t('settingsDesc')}</SheetDescription>
        </SheetHeader>

        <div className="min-h-0 flex-1 space-y-6 overflow-y-auto px-4 pb-6">
          {/* Account */}
          <section className="space-y-3">
            <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {t('account')}
            </h3>
            <div className="flex items-center gap-3">
              <Avatar className="size-10 rounded-xl">
                <AvatarFallback className="bg-primary/15 text-primary rounded-xl text-sm font-medium">
                  {deriveInitials(userLabel)}
                </AvatarFallback>
              </Avatar>
              <div className="min-w-0 flex-1">
                <p className="text-foreground truncate text-sm font-medium">{userLabel}</p>
                <p className="text-muted-foreground truncate text-xs">{email ?? t('noEmail')}</p>
              </div>
            </div>
            {authEnabled && (
              <Button variant="secondary" size="sm" onClick={() => void onSignOut()}>
                <LogOut data-icon="inline-start" />
                {t('signOut')}
              </Button>
            )}
          </section>

          {/* Preferences */}
          <section className="space-y-3">
            <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {t('preferences')}
            </h3>
            <div className="flex items-center gap-3">
              <Globe className="text-muted-foreground size-4 shrink-0" />
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">{t('language')}</p>
                <p className="text-muted-foreground text-xs">{t('languageHelp')}</p>
              </div>
              <LocaleSwitcher />
            </div>
          </section>

          {/* Automatrix — proactive surprise tasks */}
          <AutomatrixSection />

          {/* Notifications */}
          <section className="space-y-3">
            <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {t('notifications')}
            </h3>
            <ToggleRow
              label={t('notifyCompleted')}
              help={t('notifyCompletedHelp')}
              checked={prefs.notif.completed}
              onChange={(v) => setPrefs({ notif: { completed: v } })}
            />
            <ToggleRow
              label={t('notifyNeedsInput')}
              help={t('notifyNeedsInputHelp')}
              checked={prefs.notif.needsInput}
              onChange={(v) => setPrefs({ notif: { needsInput: v } })}
            />
            <ToggleRow
              label={t('notifyFailed')}
              help={t('notifyFailedHelp')}
              checked={prefs.notif.failed}
              onChange={(v) => setPrefs({ notif: { failed: v } })}
            />
          </section>

          {/* Privacy & Data */}
          <section className="space-y-3">
            <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {tOnboard('settingsPrivacy')}
            </h3>
            <div className="bg-card rounded-lg p-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0 flex-1">
                  <p className="text-foreground text-sm font-medium">
                    {tOnboard('settingsConsent')}
                  </p>
                  <p className="text-muted-foreground text-xs">{tOnboard('settingsConsentHelp')}</p>
                </div>
                <Switch
                  checked={consentQuery.data?.training_opt_in ?? false}
                  onCheckedChange={(v) => void putConsent.mutate({ optIn: v, policyVersion: '1' })}
                  disabled={consentQuery.isLoading || putConsent.isPending}
                  aria-label={tOnboard('settingsConsent')}
                />
              </div>
            </div>
            <Button
              variant="ghost"
              size="sm"
              className="w-full justify-start"
              onClick={() => {
                if (typeof window !== 'undefined') {
                  window.localStorage.removeItem(ONBOARDED_KEY)
                  window.localStorage.removeItem(STORAGE_KEY)
                  window.location.reload()
                }
              }}
            >
              <RotateCcw className="size-4" />
              {tOnboard('settingsRerunOnboarding')}
            </Button>
            <BugReportDialog conversationId={conversationId} intentId={intentId}>
              <Button variant="ghost" size="sm" className="w-full justify-start">
                <Bug className="size-4" />
                {tOnboard('reportBug')}
              </Button>
            </BugReportDialog>
          </section>

          {/* Legal & compliance */}
          <section className="space-y-3">
            <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {t('legal')}
            </h3>
            <a
              href={LEGAL_HUB}
              target="_blank"
              rel="noopener noreferrer"
              className="bg-card hover:bg-accent flex items-center gap-3 rounded-lg px-3 py-2.5 transition"
            >
              <FileText className="text-primary size-4 shrink-0" />
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">{t('legalAllDocs')}</p>
                <p className="text-muted-foreground text-xs">{t('legalAllDocsHelp')}</p>
              </div>
              <ExternalLink className="text-muted-foreground size-3.5 shrink-0" />
            </a>
            <ul className="flex flex-col">
              {LEGAL_QUICK_LINKS.map(({ key, href }) => (
                <li key={key}>
                  <a
                    href={href}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-muted-foreground hover:bg-card hover:text-foreground flex items-center justify-between gap-2 rounded-md px-3 py-2 text-sm transition"
                  >
                    {t(key)}
                    <ExternalLink className="size-3 shrink-0 opacity-60" />
                  </a>
                </li>
              ))}
            </ul>
          </section>
        </div>
      </SheetContent>
    </Sheet>
  )
}

function ToggleRow({
  label,
  help,
  checked,
  onChange,
}: {
  label: string
  help: string
  checked: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <label className="flex cursor-pointer items-center gap-3">
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">{label}</p>
        <p className="text-muted-foreground text-xs">{help}</p>
      </div>
      <Switch checked={checked} onCheckedChange={onChange} aria-label={label} />
    </label>
  )
}
