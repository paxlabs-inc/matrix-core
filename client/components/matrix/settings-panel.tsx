'use client'

/**
 * SettingsPanel — the Settings view. Surfaces the live account + runtime the
 * daemon reports (read-only, since /settings has no write route in v1) and
 * the controls that genuinely work client-side: language, the default model,
 * and which notifications to show. Sign-out runs through Supabase auth.
 */
import { type ReactNode } from 'react'
import { Cpu, Globe, LogOut, ShieldCheck, Wifi, WifiOff, type LucideIcon } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { LocaleSwitcher } from '@/components/matrix/locale-switcher'
import { ModelSelector } from '@/components/matrix/model-selector'
import { cn } from '@/lib/utils'
import { useAuth } from '@/lib/auth/AuthProvider'
import { useSettings, useDaemonHealth } from '@/hooks/api/useSettings'
import { usePrefs } from '@/lib/prefs'
import { env } from '@/lib/env'
function deriveInitials(label: string): string {
  const words = label.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return 'MX'
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase()
  return (words[0][0] + words[words.length - 1][0]).toUpperCase()
}

export function SettingsPanel({
  modelId,
  onModelChange,
  userLabel,
}: {
  modelId: string
  onModelChange: (id: string) => void
  userLabel: string
}) {
  const t = useTranslations('settingsPanel')
  const { user, enabled: authEnabled, signOut } = useAuth()
  const settings = useSettings()
  const health = useDaemonHealth()
  const [prefs, setPrefs] = usePrefs()

  const email = user?.email ?? null
  const plan = settings.data?.free_tier === false ? t('planCloud') : t('planFree')
  const connected = health.isSuccess && health.data?.status === 'ok'
  const version = health.data?.version ?? env.release

  return (
    <div className="flex max-w-2xl flex-col gap-6">
      <Section title={t('account')}>
        <div className="flex items-center gap-3">
          <Avatar className="size-11 rounded-xl">
            <AvatarFallback className="bg-primary/15 text-primary rounded-xl text-sm font-medium">
              {deriveInitials(userLabel)}
            </AvatarFallback>
          </Avatar>
          <div className="min-w-0 flex-1">
            <p className="text-foreground truncate text-sm font-medium">{userLabel}</p>
            <p className="text-muted-foreground truncate text-xs">{email ?? t('noEmail')}</p>
          </div>
          <span className="bg-primary/12 text-primary inline-flex shrink-0 items-center gap-1 rounded-full px-2.5 py-1 text-xs font-medium">
            <ShieldCheck className="size-3.5" />
            {plan}
          </span>
        </div>
        {authEnabled && (
          <Button variant="outline" size="sm" onClick={() => void signOut()} className="self-start">
            <LogOut className="size-4" />
            {t('signOut')}
          </Button>
        )}
      </Section>

      <Section title={t('preferences')}>
        <Row icon={Globe} label={t('language')} help={t('languageHelp')}>
          <LocaleSwitcher />
        </Row>
        <Row icon={Cpu} label={t('defaultModel')} help={t('defaultModelHelp')}>
          <ModelSelector value={modelId} onChange={onModelChange} />
        </Row>
      </Section>

      <Section title={t('notifications')}>
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
      </Section>

      <Section title={t('about')}>
        <Row
          icon={connected ? Wifi : WifiOff}
          label={t('connection')}
          help={connected ? t('connected') : t('offline')}
        >
          <span
            className={cn(
              'inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium',
              connected ? 'bg-primary/12 text-primary' : 'bg-muted text-muted-foreground',
            )}
          >
            <span
              className={cn(
                'size-1.5 rounded-full',
                connected ? 'bg-primary' : 'bg-muted-foreground',
              )}
            />
            {connected ? t('online') : t('offlineShort')}
          </span>
        </Row>
        <div className="flex items-center justify-between gap-3">
          <p className="text-muted-foreground text-sm">{t('version')}</p>
          <p className="text-muted-foreground font-mono text-xs">{version}</p>
        </div>
      </Section>
    </div>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card>
      <CardContent className="flex flex-col gap-4 p-5">
        <h2 className="text-foreground text-sm font-medium">{title}</h2>
        {children}
      </CardContent>
    </Card>
  )
}

function Row({
  icon: Icon,
  label,
  help,
  children,
}: {
  icon: LucideIcon
  label: string
  help: string
  children: ReactNode
}) {
  return (
    <div className="flex items-center gap-3">
      <Icon className="text-muted-foreground size-4 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">{label}</p>
        <p className="text-muted-foreground text-xs">{help}</p>
      </div>
      {children}
    </div>
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
