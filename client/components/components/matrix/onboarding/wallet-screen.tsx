'use client'

/**
 * WalletScreen — Screen 3 of onboarding. Surfaces the agent-wallet leash
 * from the existing Paxport owner control plane (/v1/agents/:did/*). Maps
 * controls to policy fields: mode, per-tx limit, daily cap, native transfer.
 * Obtains the agent DID from daemon GET /me. Least-privilege default
 * (read_only). Skippable — the user can proceed with safe defaults.
 *
 * This screen REUSES the existing AgentLeashSheet internals (the wallet
 * client + hooks) but presents a simplified onboarding-oriented surface.
 */
import { useEffect, useState } from 'react'
import {
  ArrowLeft,
  ArrowRight,
  Eye,
  Loader2,
  Repeat,
  ShieldCheck,
  SkipForward,
  Zap,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toast } from 'sonner'
import { getMe } from '@/lib/api/identity'
import { listAgents, updateAgentPolicy } from '@/lib/paxeer/wallet-client'
import { parseUnits } from '@/lib/paxeer/format'
import { type AgentMode, type AgentSummary } from '@/lib/paxeer/types'

interface Props {
  isActive: boolean
  onNext: () => void
  onBack: () => void
}

const MODE_META: Record<AgentMode, { label: string; desc: string; icon: typeof Eye }> = {
  read_only: { label: 'modeReadOnly', desc: 'modeReadOnlyDesc', icon: Eye },
  trade_only: { label: 'modeTradeOnly', desc: 'modeTradeOnlyDesc', icon: Repeat },
  full: { label: 'modeFull', desc: 'modeFullDesc', icon: Zap },
}

export function WalletScreen({ isActive, onNext, onBack }: Props) {
  const t = useTranslations('onboarding')
  const tWallet = useTranslations('walletPanel')

  const [loading, setLoading] = useState(true)
  const [agentDID, setAgentDID] = useState<string | null>(null)
  const [mode, setMode] = useState<AgentMode>('read_only')
  const [perTxLimit, setPerTxLimit] = useState('0')
  const [dailyLimit, setDailyLimit] = useState('0')
  const [allowNativeTransfer, setAllowNativeTransfer] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!isActive) return
    let cancelled = false

    const load = async () => {
      try {
        const me = await getMe()
        if (cancelled) return
        const did = me.did
        if (!did) {
          setError(t('walletNoAgent'))
          setLoading(false)
          return
        }
        setAgentDID(did)

        const agents = await listAgents()
        if (cancelled) return
        const agent = agents.find((a: AgentSummary) => a.did === did)
        if (agent) {
          setMode(agent.mode)
        }
        setLoading(false)
      } catch {
        if (!cancelled) {
          setError(t('walletLoadError'))
          setLoading(false)
        }
      }
    }

    void load()
    return () => {
      cancelled = true
    }
  }, [isActive, t])

  const handleSave = async () => {
    if (!agentDID) {
      onNext()
      return
    }
    setSaving(true)
    try {
      // Inputs are in whole PAX (18 decimals), matching the Settings
      // leash sheet; convert to wei before writing the policy.
      await updateAgentPolicy(agentDID, {
        mode,
        max_tx_value_wei: parseUnits(perTxLimit || '0', 18),
        max_daily_value_wei: parseUnits(dailyLimit || '0', 18),
        allow_native_transfer: allowNativeTransfer,
      })
      toast.success(t('walletSaved'))
      onNext()
    } catch {
      toast.error(t('walletSaveError'))
      onNext()
    } finally {
      setSaving(false)
    }
  }

  const handleSkip = () => {
    onNext()
  }

  if (!isActive) {
    return (
      <div className="flex flex-1 flex-col">
        <div className="space-y-2">
          <h1 className="text-foreground text-2xl font-semibold tracking-tight">
            {t('walletTitle')}
          </h1>
          <p className="text-muted-foreground text-sm text-pretty">{t('walletWaiting')}</p>
        </div>
        <div className="text-muted-foreground mt-8 flex items-center gap-2 text-sm">
          <Loader2 className="size-4 animate-spin" />
          {t('walletWaitingDesc')}
        </div>
        <div className="mt-auto flex items-center gap-3 pt-8">
          <Button variant="ghost" size="lg" onClick={onBack}>
            <ArrowLeft data-icon="inline-start" />
            {t('back')}
          </Button>
          <Button variant="secondary" size="lg" className="flex-1" onClick={handleSkip}>
            <SkipForward data-icon="inline-end" />
            {t('skipForNow')}
          </Button>
        </div>
      </div>
    )
  }

  if (loading) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3">
        <Loader2 className="text-primary size-6 animate-spin" />
        <p className="text-muted-foreground text-sm">{t('walletLoading')}</p>
      </div>
    )
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="space-y-2">
        <h1 className="text-foreground text-2xl font-semibold tracking-tight">
          {t('walletTitle')}
        </h1>
        <p className="text-muted-foreground text-sm text-pretty">{t('walletDesc')}</p>
      </div>

      {error && (
        <div className="bg-destructive/10 mt-4 rounded-lg p-3">
          <p className="text-destructive text-xs">{error}</p>
        </div>
      )}

      <div className="mt-6 space-y-5">
        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('walletMode')}</label>
          <ToggleGroup
            type="single"
            value={mode}
            onValueChange={(v) => v && setMode(v as AgentMode)}
            className="grid w-full grid-cols-3 gap-2"
          >
            {(Object.keys(MODE_META) as AgentMode[]).map((m) => {
              const meta = MODE_META[m]
              const Icon = meta.icon
              return (
                <ToggleGroupItem
                  key={m}
                  value={m}
                  className="flex flex-col items-center gap-1.5 py-3"
                  aria-label={tWallet(meta.label)}
                >
                  <Icon className="size-4" />
                  <span className="text-xs font-medium">{tWallet(meta.label)}</span>
                </ToggleGroupItem>
              )
            })}
          </ToggleGroup>
          <p className="text-muted-foreground text-xs">{tWallet(MODE_META[mode].desc)}</p>
        </div>

        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('walletPerTxLimit')}</label>
          <Input
            value={perTxLimit}
            onChange={(e) => setPerTxLimit(e.target.value)}
            placeholder="0"
            type="number"
            min="0"
            className="h-11"
          />
          <p className="text-muted-foreground text-xs">{t('walletPerTxLimitHelp')}</p>
        </div>

        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('walletDailyLimit')}</label>
          <Input
            value={dailyLimit}
            onChange={(e) => setDailyLimit(e.target.value)}
            placeholder="0"
            type="number"
            min="0"
            className="h-11"
          />
          <p className="text-muted-foreground text-xs">{t('walletDailyLimitHelp')}</p>
        </div>

        <label className="bg-card flex cursor-pointer items-center justify-between gap-3 rounded-lg p-4">
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('walletAllowTransfer')}</p>
            <p className="text-muted-foreground text-xs">{t('walletAllowTransferHelp')}</p>
          </div>
          <Switch
            checked={allowNativeTransfer}
            onCheckedChange={setAllowNativeTransfer}
            aria-label={t('walletAllowTransfer')}
          />
        </label>
      </div>

      <div className="bg-card mt-4 rounded-lg p-3">
        <div className="flex items-center gap-2">
          <ShieldCheck className="text-primary size-4 shrink-0" />
          <p className="text-muted-foreground text-xs text-pretty">{t('walletDefaultNote')}</p>
        </div>
      </div>

      <div className="mt-auto flex items-center gap-3 pt-6">
        <Button variant="ghost" size="lg" onClick={onBack}>
          <ArrowLeft data-icon="inline-start" />
          {t('back')}
        </Button>
        <Button variant="secondary" size="lg" onClick={handleSkip}>
          <SkipForward data-icon="inline-end" />
          {t('skipForNow')}
        </Button>
        <Button size="lg" className="flex-1" disabled={saving} onClick={() => void handleSave()}>
          {saving ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <ArrowRight data-icon="inline-end" />
          )}
          {t('finish')}
        </Button>
      </div>
    </div>
  )
}
