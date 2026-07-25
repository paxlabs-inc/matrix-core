'use client'

/**
 * SettingsSheet — account, preferences, and the legal entrance.
 *
 * Rendered as a full-page overlay (mirroring the Timeline / Workspace pages),
 * NOT a slide-over drawer: sign-out, language, the notification toggles, and
 * links into the static legal & compliance site served from /public/legal.
 * Separation is by background TONE only (bg-background stage, bg-card panels) —
 * no border strokes for depth, no emojis / gradients / glow.
 */
import {
  ExternalLink,
  FileText,
  Globe,
  LogOut,
  RotateCcw,
  Bug,
  Settings,
  Wallet,
  MicIcon,
  X,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
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
import { BriefSection } from '@/components/matrix/brief-section'
import { TelegramSection } from '@/components/matrix/telegram-section'
import { MachineMailSection } from '@/components/matrix/machinemail-section'

const VOICE_OPTIONS = [
  'Mia',
  'Chloe',
  'Milo',
  'Dean',
  'mimo_default',
  '冰糖',
  '茉莉',
  '苏打',
  '白桦',
]

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
  onOpenWallet,
  onOpenConversation,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversationId?: string | null
  intentId?: string | null
  /** Opens the agent Wallet page (closes settings first). */
  onOpenWallet?: () => void
  /** Opens a conversation in the chat surface (closes settings first). */
  onOpenConversation?: (id: string) => void
}) {
  const t = useTranslations('settingsPanel')
  const tOnboard = useTranslations('onboarding')
  const tWallet = useTranslations('agentWallet')
  const tVoice = useTranslations('voiceSettings')
  const reduce = useReducedMotion()
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
    <AnimatePresence>
      {open ? (
        <motion.div
          initial={reduce ? false : { opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.2 }}
          className="bg-background fixed inset-0 z-50 flex flex-col"
          role="dialog"
          aria-modal="true"
          aria-label={t('settingsTitle')}
        >
          {/* header */}
          <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
            <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
              <Settings className="size-5" />
            </span>
            <div className="min-w-0 flex-1">
              <h1 className="text-foreground text-lg font-bold tracking-tight">
                {t('settingsTitle')}
              </h1>
              <p className="text-muted-foreground text-xs">{t('settingsDesc')}</p>
            </div>
            <button
              type="button"
              onClick={() => onOpenChange(false)}
              aria-label="Close settings"
              className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition"
            >
              <X className="size-5" />
            </button>
          </div>

          {/* body */}
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="mx-auto w-full max-w-2xl space-y-6 px-4 pb-10 sm:px-6">
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
                    <p className="text-muted-foreground truncate text-xs">
                      {email ?? t('noEmail')}
                    </p>
                  </div>
                </div>
                {onOpenWallet && (
                  <button
                    type="button"
                    onClick={() => {
                      onOpenChange(false)
                      onOpenWallet()
                    }}
                    className="bg-card hover:bg-accent flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition"
                  >
                    <Wallet className="text-primary size-4 shrink-0" />
                    <div className="min-w-0 flex-1">
                      <p className="text-foreground text-sm font-medium">{tWallet('title')}</p>
                      <p className="text-muted-foreground text-xs">{tWallet('desc')}</p>
                    </div>
                  </button>
                )}
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

              <section className="space-y-3">
                <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
                  {tVoice('title')}
                </h3>
                <div className="bg-card space-y-3 rounded-lg p-4">
                  <div className="flex items-center gap-3">
                    <MicIcon className="text-muted-foreground size-4 shrink-0" />
                    <div className="min-w-0 flex-1">
                      <label htmlFor="voice-choice" className="text-foreground text-sm font-medium">
                        {tVoice('choice')}
                      </label>
                      <p className="text-muted-foreground text-xs">{tVoice('choiceHelp')}</p>
                    </div>
                    <select
                      id="voice-choice"
                      value={prefs.voice.voice}
                      onChange={(event) => setPrefs({ voice: { voice: event.target.value } })}
                      className="bg-muted text-foreground rounded-lg px-3 py-2 text-sm outline-none"
                    >
                      {VOICE_OPTIONS.map((voice) => (
                        <option key={voice} value={voice}>
                          {voice}
                        </option>
                      ))}
                    </select>
                  </div>
                  <div className="space-y-1.5">
                    <label htmlFor="voice-style" className="text-foreground text-sm font-medium">
                      {tVoice('style')}
                    </label>
                    <p className="text-muted-foreground text-xs">{tVoice('styleHelp')}</p>
                    <textarea
                      id="voice-style"
                      value={prefs.voice.style}
                      maxLength={500}
                      rows={2}
                      onChange={(event) => setPrefs({ voice: { style: event.target.value } })}
                      className="bg-muted text-foreground placeholder:text-muted-foreground min-h-16 w-full resize-y rounded-lg px-3 py-2 text-sm outline-none"
                      placeholder={tVoice('stylePlaceholder')}
                    />
                  </div>
                </div>
              </section>

              <TelegramSection />

              <MachineMailSection />

              {/* Automatrix — proactive surprise tasks */}
              <AutomatrixSection />

              {/* Morning brief — opt-in personalization + schedule */}
              <BriefSection
                onOpenConversation={(id) => {
                  onOpenChange(false)
                  onOpenConversation?.(id)
                }}
              />

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
                      <p className="text-muted-foreground text-xs">
                        {tOnboard('settingsConsentHelp')}
                      </p>
                    </div>
                    <Switch
                      checked={consentQuery.data?.training_opt_in ?? false}
                      onCheckedChange={(v) =>
                        void putConsent.mutate({ optIn: v, policyVersion: '1' })
                      }
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
          </div>
        </motion.div>
      ) : null}
    </AnimatePresence>
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
