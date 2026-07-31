'use client'

/**
 * PoliciesPanel — guardrails for what agents may do on their own: spend limits,
 * approval gates, and a freeze switch. Controlled by the parent so the same
 * state is reflected in the floating ControlDock.
 */

import {
  Coins,
  CalendarClock,
  Link2,
  ShieldCheck,
  Mail,
  Puzzle,
  ReceiptText,
  OctagonAlert,
  type LucideIcon,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/matrix/astryx-card'
import { Switch } from '@/components/matrix/astryx-switch'
import { Slider } from '@/components/ui/slider'
import { cn } from '@/lib/utils'
import { formatUsd, type PolicyToggles, type SpendLimits } from '@/lib/matrix-data'

interface LimitDef {
  key: keyof SpendLimits
  label: string
  help: string
  icon: LucideIcon
  min: number
  max: number
  step: number
}

interface ToggleDef {
  key: keyof PolicyToggles
  label: string
  help: string
  icon: LucideIcon
}

export function PoliciesPanel({
  limits,
  onLimitChange,
  toggles,
  onToggle,
}: {
  limits: SpendLimits
  onLimitChange: (key: keyof SpendLimits, value: number) => void
  toggles: PolicyToggles
  onToggle: (key: keyof PolicyToggles, value: boolean) => void
}) {
  const t = useTranslations('policiesPanel')

  const LIMIT_DEFS: LimitDef[] = [
    {
      key: 'perTaskUsd',
      label: t('perTaskBudget'),
      help: t('perTaskBudgetHelp'),
      icon: Coins,
      min: 1,
      max: 50,
      step: 1,
    },
    {
      key: 'dailyUsd',
      label: t('dailyBudget'),
      help: t('dailyBudgetHelp'),
      icon: CalendarClock,
      min: 10,
      max: 500,
      step: 10,
    },
    {
      key: 'onchainPerTxUsd',
      label: t('onchainPerTx'),
      help: t('onchainPerTxHelp'),
      icon: Link2,
      min: 0,
      max: 1000,
      step: 25,
    },
  ]

  const APPROVAL_DEFS: ToggleDef[] = [
    {
      key: 'approveOnchain',
      label: t('approveSpends'),
      help: t('approveSpendsHelp'),
      icon: ShieldCheck,
    },
    {
      key: 'approveExternalSend',
      label: t('approveMessages'),
      help: t('approveMessagesHelp'),
      icon: Mail,
    },
    { key: 'allowNewTools', label: t('allowNewTools'), help: t('allowNewToolsHelp'), icon: Puzzle },
    {
      key: 'alwaysReceipt',
      label: t('alwaysReceipt'),
      help: t('alwaysReceiptHelp'),
      icon: ReceiptText,
    },
  ]

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      {/* Kill switch */}
      <Card className={cn(toggles.freezeAll && 'border-destructive/50 ring-destructive/30 ring-1')}>
        <CardContent className="flex items-center gap-4 p-5">
          <span
            className={cn(
              'flex size-10 shrink-0 items-center justify-center rounded-xl',
              toggles.freezeAll
                ? 'bg-destructive/15 text-destructive'
                : 'bg-muted text-muted-foreground',
            )}
          >
            <OctagonAlert className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('freezeAll')}</p>
            <p className="text-muted-foreground text-sm">
              {toggles.freezeAll ? t('frozenDesc') : t('unfrozenDesc')}
            </p>
          </div>
          <Switch
            checked={toggles.freezeAll}
            onCheckedChange={(v) => onToggle('freezeAll', v)}
            aria-label={t('freezeAll')}
          />
        </CardContent>
      </Card>

      {/* Spend limits */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">{t('spendLimits')}</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-6">
          {LIMIT_DEFS.map((def) => {
            const value = limits[def.key]
            return (
              <div key={def.key} className="flex flex-col gap-2">
                <div className="flex items-center gap-3">
                  <def.icon className="text-muted-foreground size-4" />
                  <div className="min-w-0 flex-1">
                    <p className="text-foreground text-sm font-medium">{def.label}</p>
                    <p className="text-muted-foreground text-xs">{def.help}</p>
                  </div>
                  <span className="text-foreground font-mono text-sm font-medium">
                    {value === 0 ? t('off') : formatUsd(value)}
                  </span>
                </div>
                <Slider
                  value={[value]}
                  onValueChange={(v) => onLimitChange(def.key, v[0])}
                  min={def.min}
                  max={def.max}
                  step={def.step}
                />
              </div>
            )
          })}
        </CardContent>
      </Card>

      {/* Approval gates */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">{t('approvalGates')}</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-1">
          {APPROVAL_DEFS.map((def) => (
            <label
              key={def.key}
              className="hover:bg-accent flex cursor-pointer items-center gap-3 rounded-xl px-2 py-3 transition-colors"
            >
              <def.icon className="text-muted-foreground size-4" />
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">{def.label}</p>
                <p className="text-muted-foreground text-xs">{def.help}</p>
              </div>
              <Switch checked={toggles[def.key]} onCheckedChange={(v) => onToggle(def.key, v)} />
            </label>
          ))}
        </CardContent>
      </Card>
    </div>
  )
}
