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
  Repeat,
  ShieldCheck,
  SkipForward,
  Zap,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@astryxdesign/core/Button'
import { NumberInput } from '@astryxdesign/core/NumberInput'
import { ToggleButton, ToggleButtonGroup } from '@astryxdesign/core/ToggleButton'
import { Card } from '@astryxdesign/core/Card'
import { Spinner } from '@astryxdesign/core/Spinner'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Switch } from '@/components/matrix/astryx-switch'
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
          <Heading level={1}>{t('walletTitle')}</Heading>
          <Text type="supporting" color="secondary" display="block">
            {t('walletWaiting')}
          </Text>
        </div>
        <Spinner className="mt-8" label={t('walletWaitingDesc')} />
        <div className="mt-auto flex items-center gap-3 pt-8">
          <Button
            label={t('back')}
            variant="ghost"
            size="lg"
            icon={<ArrowLeft />}
            onClick={onBack}
          />
          <Button
            label={t('skipForNow')}
            variant="secondary"
            size="lg"
            width="100%"
            endContent={<SkipForward />}
            onClick={handleSkip}
          />
        </div>
      </div>
    )
  }

  if (loading) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3">
        <Spinner size="xl" label={t('walletLoading')} />
      </div>
    )
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="space-y-2">
        <Heading level={1}>{t('walletTitle')}</Heading>
        <Text type="supporting" color="secondary" display="block">
          {t('walletDesc')}
        </Text>
      </div>

      {error && (
        <div className="bg-destructive/10 mt-4 rounded-lg p-3">
          <p className="text-destructive text-xs">{error}</p>
        </div>
      )}

      <div className="mt-6 space-y-5">
        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('walletMode')}</label>
          <ToggleButtonGroup
            value={mode}
            onChange={(value) => value && setMode(value as AgentMode)}
            label={t('walletMode')}
          >
            {(Object.keys(MODE_META) as AgentMode[]).map((m) => {
              const meta = MODE_META[m]
              const Icon = meta.icon
              return (
                <ToggleButton
                  key={m}
                  value={m}
                  label={tWallet(meta.label)}
                  icon={<Icon className="size-4" />}
                />
              )
            })}
          </ToggleButtonGroup>
          <p className="text-muted-foreground text-xs">{tWallet(MODE_META[mode].desc)}</p>
        </div>

        <NumberInput
          label={t('walletPerTxLimit')}
          description={t('walletPerTxLimitHelp')}
          value={Number(perTxLimit) || 0}
          onChange={(value) => setPerTxLimit(String(value))}
          min={0}
          width="100%"
        />

        <NumberInput
          label={t('walletDailyLimit')}
          description={t('walletDailyLimitHelp')}
          value={Number(dailyLimit) || 0}
          onChange={(value) => setDailyLimit(String(value))}
          min={0}
          width="100%"
        />

        <Card variant="transparent" padding={4}>
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0 flex-1">
              <p className="text-foreground text-sm font-medium">{t('walletAllowTransfer')}</p>
              <p className="text-muted-foreground text-xs">{t('walletAllowTransferHelp')}</p>
            </div>
            <Switch
              checked={allowNativeTransfer}
              onCheckedChange={setAllowNativeTransfer}
              aria-label={t('walletAllowTransfer')}
            />
          </div>
        </Card>
      </div>

      <Card variant="muted" padding={3} className="mt-4">
        <div className="flex items-center gap-2">
          <ShieldCheck className="text-primary size-4 shrink-0" />
          <p className="text-muted-foreground text-xs text-pretty">{t('walletDefaultNote')}</p>
        </div>
      </Card>

      <div className="mt-auto flex items-center gap-3 pt-6">
        <Button label={t('back')} variant="ghost" size="lg" icon={<ArrowLeft />} onClick={onBack} />
        <Button
          label={t('skipForNow')}
          variant="secondary"
          size="lg"
          endContent={<SkipForward />}
          onClick={handleSkip}
        />
        <Button
          label={t('finish')}
          size="lg"
          width="100%"
          isDisabled={saving}
          isLoading={saving}
          endContent={<ArrowRight />}
          onClick={() => void handleSave()}
        />
      </div>
    </div>
  )
}
