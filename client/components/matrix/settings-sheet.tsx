'use client'

import { useEffect, useState, type ComponentType, type ReactNode, type SVGProps } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useTranslations } from 'next-intl'
import { motion, useReducedMotion } from 'motion/react'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Dialog } from '@astryxdesign/core/Dialog'
import { SideNav, SideNavItem } from '@astryxdesign/core/SideNav'
import { Card } from '@astryxdesign/core/Card'
import { ClickableCard } from '@astryxdesign/core/ClickableCard'
import {
  Activity,
  ArrowLeft,
  Bell,
  Bot,
  BrainIcon,
  Bug,
  ChevronRight,
  Cpu,
  Database,
  ExternalLink,
  FileText,
  Globe,
  Lock,
  LogOut,
  MicIcon,
  Monitor,
  RotateCcw,
  Settings,
  ShieldCheck,
  Wallet,
  Wrench,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { motionTransition } from '@/lib/motion'
import { env } from '@/lib/env'
import { qk } from '@/lib/query/keys'
import { getSkills } from '@/lib/api/agents'
import { getMemoryConsent, setMemoryConsent } from '@/lib/api/memory'
import { useSettings, useDaemonHealth } from '@/hooks/api/useSettings'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/matrix/astryx-switch'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { LocaleSwitcher } from '@/components/matrix/locale-switcher'
import { useAuth, userDisplayName } from '@/lib/auth/AuthProvider'
import { usePrefs } from '@/lib/prefs'
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

const LEGAL_HUB = '/legal/index.html'
const LEGAL_QUICK_LINKS: { key: string; href: string }[] = [
  { key: 'legalTerms', href: '/legal/terms-of-service.html' },
  { key: 'legalPrivacy', href: '/legal/privacy-policy.html' },
  { key: 'legalCookies', href: '/legal/cookie-policy.html' },
  { key: 'legalAcceptableUse', href: '/legal/acceptable-use-policy.html' },
  { key: 'legalRisk', href: '/legal/risk-disclosure.html' },
  { key: 'legalAiUse', href: '/legal/ai-agent-responsible-use-policy.html' },
]

type CategoryId =
  | 'account'
  | 'preferences'
  | 'personalization'
  | 'memory'
  | 'notifications'
  | 'computer'
  | 'configuration'
  | 'connectors'
  | 'skills'
  | 'credentials'
  | 'other'

type SettingsIcon = ComponentType<SVGProps<SVGSVGElement>>

const CATEGORIES: Array<{
  id: CategoryId
  label: string
  description: string
  icon: SettingsIcon
}> = [
  { id: 'account', label: 'account', description: 'accountDesc', icon: ShieldCheck },
  { id: 'preferences', label: 'preferences', description: 'preferencesDesc', icon: Globe },
  {
    id: 'personalization',
    label: 'personalization',
    description: 'personalizationDesc',
    icon: Bot,
  },
  { id: 'memory', label: 'memory', description: 'memoryDesc', icon: BrainIcon },
  {
    id: 'notifications',
    label: 'notifications',
    description: 'notificationsDesc',
    icon: Bell,
  },
  { id: 'computer', label: 'computer', description: 'computerDesc', icon: Monitor },
  {
    id: 'configuration',
    label: 'configuration',
    description: 'configurationDesc',
    icon: Cpu,
  },
  { id: 'connectors', label: 'connectors', description: 'connectorsDesc', icon: Activity },
  { id: 'skills', label: 'skills', description: 'skillsDesc', icon: Wrench },
  {
    id: 'credentials',
    label: 'credentialVault',
    description: 'credentialVaultDesc',
    icon: Lock,
  },
  { id: 'other', label: 'other', description: 'otherDesc', icon: Database },
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
  onOpenTimeline,
  onOpenSelfModel,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversationId?: string | null
  intentId?: string | null
  onOpenWallet?: () => void
  onOpenConversation?: (id: string) => void
  onOpenTimeline?: () => void
  onOpenSelfModel?: () => void
}) {
  const t = useTranslations('settingsPanel')
  const tOnboard = useTranslations('onboarding')
  const tWallet = useTranslations('agentWallet')
  const tVoice = useTranslations('voiceSettings')
  const reduce = useReducedMotion()
  const { user, enabled: authEnabled, signOut } = useAuth()
  const [prefs, setPrefs] = usePrefs()
  const consentQuery = useConsent()
  const putConsent = usePutConsent()
  const settingsQuery = useSettings()
  const healthQuery = useDaemonHealth()
  const queryClient = useQueryClient()
  const [activeCategory, setActiveCategory] = useState<CategoryId>('account')
  const [mobileMenu, setMobileMenu] = useState(true)

  const skillsQuery = useQuery({
    queryKey: qk.skills(),
    queryFn: ({ signal }) => getSkills(signal),
    enabled: open,
    staleTime: 5 * 60_000,
  })
  const memoryConsentQuery = useQuery({
    queryKey: qk.memoryConsent(),
    queryFn: ({ signal }) => getMemoryConsent(signal),
    enabled: open,
    staleTime: 30_000,
  })
  const memoryConsentMutation = useMutation({
    mutationFn: setMemoryConsent,
    onSuccess: (next) => queryClient.setQueryData(qk.memoryConsent(), next),
  })

  useEffect(() => {
    if (!open) return
    setActiveCategory('account')
    setMobileMenu(true)
  }, [open])

  const userLabel = userDisplayName(user) ?? 'Centra'
  const email = user?.email ?? null
  const selectedCategory =
    CATEGORIES.find((category) => category.id === activeCategory) ?? CATEGORIES[0]

  const onSignOut = async () => {
    await signOut()
    onOpenChange(false)
    window.location.assign('/login')
  }

  const openRelated = (action?: () => void) => {
    onOpenChange(false)
    action?.()
  }

  return (
    <Dialog
      isOpen={open}
      onOpenChange={onOpenChange}
      variant="standard"
      purpose="info"
      width={760}
      maxHeight="calc(100dvh - 3rem)"
      padding={0}
      aria-label={t('settingsTitle')}
      className="bg-card overflow-hidden rounded-xl border-0"
    >
      <motion.div
        initial={reduce ? false : { opacity: 0, y: 12, scale: 0.99 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        exit={reduce ? { opacity: 0 } : { opacity: 0, y: 8, scale: 0.99 }}
        transition={motionTransition.content}
        className="bg-card flex h-[min(680px,calc(100dvh-3rem))] min-h-0 w-full flex-col overflow-hidden"
      >
        <header className="flex shrink-0 items-center gap-3 px-4 py-3 sm:px-5">
          <button
            type="button"
            onClick={() => setMobileMenu(true)}
            aria-label={t('backToSettings')}
            className={cn(
              'text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition md:hidden',
              mobileMenu && 'hidden',
            )}
          >
            <ArrowLeft className="size-5" />
          </button>
          <span
            className={cn(
              'bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl',
              !mobileMenu && 'max-md:hidden',
            )}
          >
            <Settings className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <Heading level={1} className="text-lg tracking-tight">
              {mobileMenu ? t('settingsTitle') : t(selectedCategory.label)}
            </Heading>
            <Text type="supporting" display="block" maxLines={1}>
              {mobileMenu ? t('settingsDesc') : t(selectedCategory.description)}
            </Text>
          </div>
          <button
            type="button"
            onClick={() => onOpenChange(false)}
            aria-label={t('closeSettings')}
            className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition"
          >
            <X className="size-5" />
          </button>
        </header>

        <div className="flex min-h-0 flex-1">
          <SideNav
            className={cn(
              'min-h-0 flex-1 overflow-y-auto px-3 pb-5 md:flex md:w-56 md:flex-none md:flex-col',
              mobileMenu ? 'flex flex-col' : 'hidden',
            )}
            role="tablist"
            aria-label={t('settingsCategories')}
          >
            {CATEGORIES.map((category) => {
              const Icon = category.icon
              const active = category.id === activeCategory
              return (
                <SideNavItem
                  key={category.id}
                  label={t(category.label)}
                  icon={<Icon className="size-4" aria-hidden />}
                  isSelected={active}
                  endContent={<ChevronRight className="size-4 opacity-50 md:hidden" />}
                  onClick={() => {
                    setActiveCategory(category.id)
                    setMobileMenu(false)
                  }}
                />
              )
            })}
          </SideNav>

          <main
            role="tabpanel"
            className={cn(
              'min-h-0 min-w-0 flex-1 overflow-y-auto px-4 pb-8 sm:px-6 md:flex md:flex-col',
              mobileMenu ? 'hidden' : 'flex flex-col',
            )}
          >
            <div className="mx-auto w-full max-w-3xl pb-4">
              <div className="mb-6 hidden md:block">
                <Heading level={2}>{t(selectedCategory.label)}</Heading>
                <Text type="supporting" display="block" className="mt-1">
                  {t(selectedCategory.description)}
                </Text>
              </div>

              {activeCategory === 'account' ? (
                <SettingsGroup title={t('account')}>
                  <div className="flex items-center gap-3">
                    <Avatar className="size-11 rounded-xl">
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
                  {onOpenWallet ? (
                    <SettingsLink
                      icon={Wallet}
                      title={tWallet('title')}
                      description={tWallet('desc')}
                      onClick={() => openRelated(onOpenWallet)}
                    />
                  ) : null}
                  {authEnabled ? (
                    <Button
                      variant="secondary"
                      size="sm"
                      className="self-start"
                      onClick={() => void onSignOut()}
                    >
                      <LogOut data-icon="inline-start" />
                      {t('signOut')}
                    </Button>
                  ) : null}
                </SettingsGroup>
              ) : null}

              {activeCategory === 'preferences' ? (
                <div className="space-y-5">
                  <SettingsGroup title={t('language')}>
                    <SettingsRow icon={Globe} title={t('language')} description={t('languageHelp')}>
                      <LocaleSwitcher />
                    </SettingsRow>
                  </SettingsGroup>
                  <SettingsGroup title={tVoice('title')}>
                    <SettingsRow
                      icon={MicIcon}
                      title={tVoice('choice')}
                      description={tVoice('choiceHelp')}
                    >
                      <select
                        id="voice-choice"
                        value={prefs.voice.voice}
                        onChange={(event) => setPrefs({ voice: { voice: event.target.value } })}
                        aria-label={tVoice('choice')}
                        className="bg-muted text-foreground rounded-lg px-3 py-2 text-sm outline-none"
                      >
                        {VOICE_OPTIONS.map((voice) => (
                          <option key={voice} value={voice}>
                            {voice}
                          </option>
                        ))}
                      </select>
                    </SettingsRow>
                    <div className="space-y-1.5">
                      <label htmlFor="voice-style" className="text-foreground text-sm font-medium">
                        {tVoice('style')}
                      </label>
                      <p className="text-muted-foreground text-xs">{tVoice('styleHelp')}</p>
                      <textarea
                        id="voice-style"
                        value={prefs.voice.style}
                        maxLength={500}
                        rows={3}
                        onChange={(event) => setPrefs({ voice: { style: event.target.value } })}
                        className="bg-muted text-foreground placeholder:text-muted-foreground min-h-20 w-full resize-y rounded-xl px-3 py-2 text-sm outline-none"
                        placeholder={tVoice('stylePlaceholder')}
                      />
                    </div>
                  </SettingsGroup>
                </div>
              ) : null}

              {activeCategory === 'personalization' ? (
                <div className="space-y-5">
                  <AutomatrixSection />
                  <BriefSection
                    onOpenConversation={(id) => {
                      onOpenChange(false)
                      onOpenConversation?.(id)
                    }}
                  />
                </div>
              ) : null}

              {activeCategory === 'memory' ? (
                <SettingsGroup title={t('memory')}>
                  <ToggleRow
                    label={t('memoryEnabled')}
                    help={
                      memoryConsentQuery.data?.notice ??
                      (memoryConsentQuery.isError ? t('memoryUnavailable') : t('memoryEnabledHelp'))
                    }
                    checked={memoryConsentQuery.data?.consent.enabled ?? false}
                    disabled={memoryConsentQuery.isLoading || memoryConsentMutation.isPending}
                    onChange={(enabled) => memoryConsentMutation.mutate(enabled)}
                  />
                  {onOpenTimeline ? (
                    <Button
                      variant="secondary"
                      size="sm"
                      className="self-start"
                      onClick={() => openRelated(onOpenTimeline)}
                    >
                      <BrainIcon className="size-4" />
                      {t('openTimeline')}
                    </Button>
                  ) : null}
                </SettingsGroup>
              ) : null}

              {activeCategory === 'notifications' ? (
                <SettingsGroup title={t('notifications')}>
                  <ToggleRow
                    label={t('notifyCompleted')}
                    help={t('notifyCompletedHelp')}
                    checked={prefs.notif.completed}
                    onChange={(completed) => setPrefs({ notif: { completed } })}
                  />
                  <ToggleRow
                    label={t('notifyNeedsInput')}
                    help={t('notifyNeedsInputHelp')}
                    checked={prefs.notif.needsInput}
                    onChange={(needsInput) => setPrefs({ notif: { needsInput } })}
                  />
                  <ToggleRow
                    label={t('notifyFailed')}
                    help={t('notifyFailedHelp')}
                    checked={prefs.notif.failed}
                    onChange={(failed) => setPrefs({ notif: { failed } })}
                  />
                </SettingsGroup>
              ) : null}

              {activeCategory === 'computer' ? (
                <SettingsGroup title={t('computer')}>
                  <InformationalState
                    icon={Monitor}
                    title={t('computerControlsTitle')}
                    description={t('computerControlsHelp')}
                  />
                  {onOpenSelfModel ? (
                    <SettingsLink
                      icon={Cpu}
                      title={t('selfModel')}
                      description={t('selfModelHelp')}
                      onClick={() => openRelated(onOpenSelfModel)}
                    />
                  ) : null}
                </SettingsGroup>
              ) : null}

              {activeCategory === 'configuration' ? (
                <SettingsGroup title={t('configuration')}>
                  <DefinitionRow
                    label={t('connection')}
                    value={
                      healthQuery.isSuccess && healthQuery.data?.status === 'ok'
                        ? t('online')
                        : t('offlineShort')
                    }
                  />
                  <DefinitionRow
                    label={t('version')}
                    value={healthQuery.data?.version ?? env.release}
                    mono
                  />
                  <DefinitionRow
                    label={t('runtimeSkill')}
                    value={settingsQuery.data?.skill || t('notReported')}
                  />
                  <DefinitionRow
                    label={t('compilerModel')}
                    value={settingsQuery.data?.compiler_model || t('notReported')}
                  />
                  <DefinitionRow
                    label={t('executorModel')}
                    value={settingsQuery.data?.executor_model || t('notReported')}
                  />
                </SettingsGroup>
              ) : null}

              {activeCategory === 'connectors' ? (
                <div className="space-y-5">
                  <TelegramSection />
                  <MachineMailSection />
                </div>
              ) : null}

              {activeCategory === 'skills' ? (
                <SettingsGroup title={t('skills')}>
                  {skillsQuery.isLoading ? (
                    <p className="text-muted-foreground text-sm">{t('loadingSkills')}</p>
                  ) : skillsQuery.data && skillsQuery.data.length > 0 ? (
                    <ul className="grid gap-2 sm:grid-cols-2">
                      {skillsQuery.data.map((skill) => (
                        <li key={skill.uri} className="bg-muted/60 rounded-xl p-3">
                          <p className="text-foreground text-sm font-medium">
                            {skill.display || skill.slug}
                          </p>
                          <p className="text-muted-foreground mt-0.5 line-clamp-2 text-xs">
                            {skill.description || skill.uri}
                          </p>
                          <p className="text-muted-foreground/70 mt-2 font-mono text-[0.65rem]">
                            {skill.version}
                          </p>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <InformationalState
                      icon={Wrench}
                      title={t('noSkills')}
                      description={t('noSkillsHelp')}
                    />
                  )}
                </SettingsGroup>
              ) : null}

              {activeCategory === 'credentials' ? (
                <SettingsGroup title={t('credentialVault')}>
                  <InformationalState
                    icon={Lock}
                    title={t('credentialControlsTitle')}
                    description={t('credentialControlsHelp')}
                  />
                </SettingsGroup>
              ) : null}

              {activeCategory === 'other' ? (
                <div className="space-y-5">
                  <SettingsGroup title={tOnboard('settingsPrivacy')}>
                    <ToggleRow
                      label={tOnboard('settingsConsent')}
                      help={tOnboard('settingsConsentHelp')}
                      checked={consentQuery.data?.training_opt_in ?? false}
                      disabled={consentQuery.isLoading || putConsent.isPending}
                      onChange={(optIn) => void putConsent.mutate({ optIn, policyVersion: '1' })}
                    />
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
                  </SettingsGroup>

                  <SettingsGroup title={t('legal')}>
                    <a
                      href={LEGAL_HUB}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="bg-muted/60 hover:bg-muted flex items-center gap-3 rounded-xl px-3 py-3 transition"
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
                            className="text-muted-foreground hover:bg-muted hover:text-foreground flex items-center justify-between gap-2 rounded-lg px-3 py-2 text-sm transition"
                          >
                            {t(key)}
                            <ExternalLink className="size-3 shrink-0 opacity-60" />
                          </a>
                        </li>
                      ))}
                    </ul>
                  </SettingsGroup>
                </div>
              ) : null}
            </div>
          </main>
        </div>
      </motion.div>
    </Dialog>
  )
}

function SettingsGroup({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card variant="muted" padding={4} className="flex flex-col gap-4 sm:p-5">
      <Heading level={3}>{title}</Heading>
      {children}
    </Card>
  )
}

function SettingsRow({
  icon: Icon,
  title,
  description,
  children,
}: {
  icon: SettingsIcon
  title: string
  description: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
      <Icon className="text-muted-foreground size-4 shrink-0 max-sm:hidden" />
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">{title}</p>
        <p className="text-muted-foreground text-xs">{description}</p>
      </div>
      <div className="shrink-0">{children}</div>
    </div>
  )
}

function SettingsLink({
  icon: Icon,
  title,
  description,
  onClick,
}: {
  icon: SettingsIcon
  title: string
  description: string
  onClick: () => void
}) {
  return (
    <ClickableCard label={title} onClick={onClick} variant="transparent" padding={3} width="100%">
      <div className="flex items-center gap-3">
        <Icon className="text-primary size-4 shrink-0" />
        <span className="min-w-0 flex-1">
          <span className="text-foreground block text-sm font-medium">{title}</span>
          <span className="text-muted-foreground block text-xs">{description}</span>
        </span>
        <ChevronRight className="text-muted-foreground size-4 shrink-0" />
      </div>
    </ClickableCard>
  )
}

function ToggleRow({
  label,
  help,
  checked,
  disabled = false,
  onChange,
}: {
  label: string
  help: string
  checked: boolean
  disabled?: boolean
  onChange: (value: boolean) => void
}) {
  return (
    <div className="flex items-center gap-3">
      <span className="min-w-0 flex-1">
        <span className="text-foreground block text-sm font-medium">{label}</span>
        <span className="text-muted-foreground block text-xs">{help}</span>
      </span>
      <Switch checked={checked} onCheckedChange={onChange} disabled={disabled} aria-label={label} />
    </div>
  )
}

function DefinitionRow({
  label,
  value,
  mono = false,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <p className="text-muted-foreground text-sm">{label}</p>
      <p className={cn('text-foreground text-right text-sm', mono && 'font-mono text-xs')}>
        {value}
      </p>
    </div>
  )
}

function InformationalState({
  icon: Icon,
  title,
  description,
}: {
  icon: SettingsIcon
  title: string
  description: string
}) {
  return (
    <div className="bg-muted/60 flex items-start gap-3 rounded-xl p-4">
      <span className="bg-background text-primary grid size-9 shrink-0 place-items-center rounded-xl">
        <Icon className="size-4" />
      </span>
      <div className="min-w-0">
        <p className="text-foreground text-sm font-medium">{title}</p>
        <p className="text-muted-foreground mt-0.5 text-xs leading-relaxed">{description}</p>
      </div>
    </div>
  )
}
