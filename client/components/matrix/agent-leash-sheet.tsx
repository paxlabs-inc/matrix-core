'use client'

/**
 * AgentLeashSheet — the owner's control surface for one agent wallet. Slides in
 * from the agent row and wires the owner control plane (/v1/agents/*): the
 * freeze kill switch, the autonomy mode, spend limits, and moving funds in
 * (fund) or out (sweep). Reads the live leash from useAgentWallet and applies
 * every change through the owner mutations.
 */

import { useEffect, useRef, useState } from 'react'
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Eye,
  Loader2,
  OctagonAlert,
  Repeat,
  SlidersHorizontal,
  Zap,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { toast } from 'sonner'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/matrix/astryx-switch'
import { Skeleton } from '@/components/ui/skeleton'
import { Selector } from '@astryxdesign/core/Selector'
import { ToggleButton, ToggleButtonGroup } from '@astryxdesign/core/ToggleButton'
import { cn } from '@/lib/utils'
import {
  useAgentWallet,
  useFundAgent,
  useSetAgentFrozen,
  useSweepAgent,
  useUpdateAgentPolicy,
} from '@/hooks/api/useWallet'
import { formatBalance, parseUnits, shortenAddress } from '@/lib/paxeer/format'
import {
  WalletConfirmDialog,
  type WalletConfirmPayload,
} from '@/components/matrix/wallet-confirm-dialog'
import { TOKENS, resolveToken } from '@/lib/paxeer/tokens'
import type { AgentMode, AgentPolicy, AgentPolicyPatch, AgentSummary } from '@/lib/paxeer/types'

type T = ReturnType<typeof useTranslations>

const MODE_ORDER: AgentMode[] = ['read_only', 'trade_only', 'full']
const MODE_META: Record<AgentMode, { label: string; desc: string; icon: typeof Eye }> = {
  read_only: { label: 'modeReadOnly', desc: 'modeReadOnlyDesc', icon: Eye },
  trade_only: { label: 'modeTradeOnly', desc: 'modeTradeOnlyDesc', icon: Repeat },
  full: { label: 'modeFull', desc: 'modeFullDesc', icon: Zap },
}

const TOKEN_OPTIONS = Object.values(TOKENS)

/** wei → a clean editable decimal string (no trailing zeros). */
function weiToInput(wei: string): string {
  return formatBalance(wei, 18, 6).replace(/\.?0+$/, '') || '0'
}

export function AgentLeashSheet({
  agent,
  primaryAddress,
  open,
  onOpenChange,
}: {
  agent: AgentSummary
  primaryAddress: string | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const t = useTranslations('walletPanel')
  const detail = useAgentWallet(open ? agent.did : null)
  const agentAddress = detail.data?.wallet?.address ?? agent.address ?? null

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full gap-0 p-0 sm:max-w-md">
        <SheetHeader className="border-border border-b">
          <SheetTitle className="truncate">{agent.label || t('agentWallets')}</SheetTitle>
          <SheetDescription>
            {agentAddress ? shortenAddress(agentAddress, 4) : t('agentNoWallet')} · {t('leashDesc')}
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
          {detail.isLoading ? (
            <LeashSkeleton />
          ) : detail.isError ? (
            <div className="flex flex-col items-center gap-3 py-12 text-center">
              <p className="text-muted-foreground text-sm">{t('detailError')}</p>
              <Button variant="outline" size="sm" onClick={() => detail.refetch()}>
                {t('retry')}
              </Button>
            </div>
          ) : (
            <LeashBody
              t={t}
              did={agent.did}
              policy={detail.data?.policy ?? null}
              isFrozen={detail.data?.is_frozen ?? agent.is_frozen}
              agentAddress={agentAddress}
              primaryAddress={primaryAddress}
              open={open}
            />
          )}
        </div>
      </SheetContent>
    </Sheet>
  )
}

export function LeashBody({
  t,
  did,
  policy,
  isFrozen,
  agentAddress,
  primaryAddress,
  open,
}: {
  t: T
  did: string
  policy: AgentPolicy | null
  isFrozen: boolean
  agentAddress: string | null
  primaryAddress: string | null
  open: boolean
}) {
  const frozenM = useSetAgentFrozen(did)
  const policyM = useUpdateAgentPolicy(did)

  const [mode, setMode] = useState<AgentMode>(policy?.mode ?? 'read_only')
  const [allowNative, setAllowNative] = useState(policy?.allow_native_transfer ?? false)
  const [allowlistOnly, setAllowlistOnly] = useState(policy?.withdrawal_allowlist_only ?? false)
  const [perTx, setPerTx] = useState('0')
  const [daily, setDaily] = useState('0')
  const [approve, setApprove] = useState('0')
  const [savingLimits, setSavingLimits] = useState(false)
  const baseline = useRef({ perTx: '0', daily: '0', approve: '0' })
  const seededFor = useRef<string | null>(null)

  function applyPolicy(p: AgentPolicy) {
    setMode(p.mode)
    setAllowNative(p.allow_native_transfer)
    setAllowlistOnly(p.withdrawal_allowlist_only)
    const tx = weiToInput(p.max_tx_value_wei)
    const dy = weiToInput(p.max_daily_value_wei)
    const ap = weiToInput(p.max_approve_wei)
    setPerTx(tx)
    setDaily(dy)
    setApprove(ap)
    baseline.current = { perTx: tx, daily: dy, approve: ap }
  }

  // Seed the form once per open, from the authoritative policy.
  useEffect(() => {
    if (!open) {
      seededFor.current = null
      return
    }
    if (policy && seededFor.current !== did) {
      applyPolicy(policy)
      seededFor.current = did
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, policy, did])

  function changeMode(next: AgentMode) {
    const prev = mode
    setMode(next)
    policyM.mutate(
      { mode: next },
      {
        onError: () => {
          setMode(prev)
          toast.error(t('updateFailed'))
        },
      },
    )
  }

  function changeSwitch(
    key: 'allow_native_transfer' | 'withdrawal_allowlist_only',
    value: boolean,
  ) {
    const isNative = key === 'allow_native_transfer'
    const set = isNative ? setAllowNative : setAllowlistOnly
    const prev = isNative ? allowNative : allowlistOnly
    set(value)
    const patch: AgentPolicyPatch = isNative
      ? { allow_native_transfer: value }
      : { withdrawal_allowlist_only: value }
    policyM.mutate(patch, {
      onError: () => {
        set(prev)
        toast.error(t('updateFailed'))
      },
    })
  }

  const limitsDirty =
    perTx !== baseline.current.perTx ||
    daily !== baseline.current.daily ||
    approve !== baseline.current.approve

  function saveLimits() {
    const patch: AgentPolicyPatch = {}
    if (perTx !== baseline.current.perTx) patch.max_tx_value_wei = parseUnits(perTx, 18)
    if (daily !== baseline.current.daily) patch.max_daily_value_wei = parseUnits(daily, 18)
    if (approve !== baseline.current.approve) patch.max_approve_wei = parseUnits(approve, 18)
    if (Object.keys(patch).length === 0) return
    setSavingLimits(true)
    policyM.mutate(patch, {
      onSuccess: (p) => {
        applyPolicy(p)
        toast.success(t('limitsSaved'))
      },
      onError: () => toast.error(t('updateFailed')),
      onSettled: () => setSavingLimits(false),
    })
  }

  const activeMode = MODE_META[mode]

  return (
    <>
      {/* Kill switch */}
      <section
        className={cn(
          'border-border flex items-center gap-3 rounded-xl border p-4',
          isFrozen && 'border-destructive/50 bg-destructive/5',
        )}
      >
        <span
          className={cn(
            'flex size-9 shrink-0 items-center justify-center rounded-lg',
            isFrozen ? 'bg-destructive/15 text-destructive' : 'bg-muted text-muted-foreground',
          )}
        >
          <OctagonAlert className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground text-sm font-medium">{t('killSwitch')}</p>
          <p className="text-muted-foreground text-xs">
            {isFrozen ? t('killSwitchFrozenDesc') : t('killSwitchActiveDesc')}
          </p>
        </div>
        <Switch
          checked={isFrozen}
          onCheckedChange={(v) =>
            frozenM.mutate(v, {
              onSuccess: () => toast.success(v ? t('agentFrozen') : t('agentResumed')),
              onError: () => toast.error(t('updateFailed')),
            })
          }
          disabled={frozenM.isPending}
          aria-label={t('killSwitch')}
        />
      </section>

      {/* Autonomy mode */}
      <section className="flex flex-col gap-3">
        <SectionLabel icon={SlidersHorizontal} title={t('autonomy')} />
        <ToggleButtonGroup
          value={mode}
          onChange={(value) => value && changeMode(value as AgentMode)}
          label={t('autonomy')}
          size="sm"
        >
          {MODE_ORDER.map((m) => {
            const Icon = MODE_META[m].icon
            return (
              <ToggleButton
                key={m}
                value={m}
                label={t(MODE_META[m].label)}
                icon={<Icon className="size-3.5 shrink-0" />}
              />
            )
          })}
        </ToggleButtonGroup>
        <p className="text-muted-foreground text-xs">{t(activeMode.desc)}</p>
      </section>

      {/* Spend limits */}
      <section className="flex flex-col gap-4">
        <SectionLabel icon={SlidersHorizontal} title={t('limits')} />
        <LimitField
          label={t('perTxLimit')}
          help={t('perTxLimitHelp')}
          value={perTx}
          onChange={setPerTx}
        />
        <LimitField
          label={t('dailyLimit')}
          help={t('dailyLimitHelp')}
          value={daily}
          onChange={setDaily}
        />
        <LimitField
          label={t('approveLimit')}
          help={t('approveLimitHelp')}
          value={approve}
          onChange={setApprove}
        />
        <ToggleRow
          label={t('allowNativeTransfer')}
          help={t('allowNativeTransferHelp')}
          checked={allowNative}
          disabled={policyM.isPending}
          onChange={(v) => changeSwitch('allow_native_transfer', v)}
        />
        <ToggleRow
          label={t('allowlistOnly')}
          help={t('allowlistOnlyHelp')}
          checked={allowlistOnly}
          disabled={policyM.isPending}
          onChange={(v) => changeSwitch('withdrawal_allowlist_only', v)}
        />
        <Button
          onClick={saveLimits}
          disabled={!limitsDirty || policyM.isPending}
          className="w-full"
        >
          {savingLimits ? (
            <>
              <Loader2 className="size-4 animate-spin" />
              {t('saving')}
            </>
          ) : (
            t('saveLimits')
          )}
        </Button>
      </section>

      {/* Move funds */}
      <section className="flex flex-col gap-4">
        <SectionLabel icon={ArrowDownToLine} title={t('moveFunds')} />
        {agentAddress ? (
          <>
            <FundForm t={t} did={did} agentAddress={agentAddress} primaryAddress={primaryAddress} />
            <SweepForm
              t={t}
              did={did}
              agentAddress={agentAddress}
              primaryAddress={primaryAddress}
            />
          </>
        ) : (
          <p className="text-muted-foreground text-xs">{t('noAgentWallet')}</p>
        )}
      </section>
    </>
  )
}

function FundForm({
  t,
  did,
  agentAddress,
  primaryAddress,
}: {
  t: T
  did: string
  agentAddress: string | null
  primaryAddress: string | null
}) {
  const fundM = useFundAgent(did, agentAddress, primaryAddress)
  const [amount, setAmount] = useState('')
  const [symbol, setSymbol] = useState('PAX')
  const [confirm, setConfirm] = useState<WalletConfirmPayload | null>(null)

  function submit() {
    const tok = resolveToken(symbol)
    const decimals = tok?.decimals ?? 18
    const base = parseUnits(amount, decimals)
    if (base === '0') return
    setConfirm({
      action: 'fund',
      amount,
      symbol: tok?.symbol ?? symbol,
      destination: agentAddress ?? did,
    })
  }

  function executeFund() {
    if (!confirm) return
    const tok = resolveToken(symbol)
    const decimals = tok?.decimals ?? 18
    const base = parseUnits(amount, decimals)
    const input =
      tok && !tok.native && tok.address ? { amount: base, token: tok.address } : { amount: base }
    fundM.mutate(input, {
      onSuccess: (r) => {
        setAmount('')
        setConfirm(null)
        toast.success(t('funded'), { description: shortenAddress(r.tx_hash, 6) })
      },
      onError: () => toast.error(t('txFailed')),
    })
  }

  return (
    <>
      <MoveForm
        t={t}
        direction="fund"
        amount={amount}
        onAmount={setAmount}
        symbol={symbol}
        onSymbol={setSymbol}
        pending={fundM.isPending}
        onSubmit={submit}
      />
      <WalletConfirmDialog
        open={Boolean(confirm)}
        payload={confirm}
        pending={fundM.isPending}
        onConfirm={executeFund}
        onCancel={() => setConfirm(null)}
      />
    </>
  )
}

function SweepForm({
  t,
  did,
  agentAddress,
  primaryAddress,
}: {
  t: T
  did: string
  agentAddress: string | null
  primaryAddress: string | null
}) {
  const sweepM = useSweepAgent(did, agentAddress, primaryAddress)
  const [amount, setAmount] = useState('')
  const [symbol, setSymbol] = useState('PAX')
  const [confirm, setConfirm] = useState<WalletConfirmPayload | null>(null)

  function submit() {
    const tok = resolveToken(symbol)
    const decimals = tok?.decimals ?? 18
    const base = parseUnits(amount, decimals)
    if (base === '0') return
    setConfirm({
      action: 'sweep',
      amount,
      symbol: tok?.symbol ?? symbol,
      destination: primaryAddress ?? t('yourWallet'),
    })
  }

  function executeSweep() {
    if (!confirm) return
    const tok = resolveToken(symbol)
    const decimals = tok?.decimals ?? 18
    const base = parseUnits(amount, decimals)
    const input =
      tok && !tok.native && tok.address ? { amount: base, token: tok.address } : { amount: base }
    sweepM.mutate(input, {
      onSuccess: (r) => {
        setAmount('')
        setConfirm(null)
        toast.success(t('swept'), { description: shortenAddress(r.tx_hash, 6) })
      },
      onError: () => toast.error(t('txFailed')),
    })
  }

  return (
    <>
      <MoveForm
        t={t}
        direction="sweep"
        amount={amount}
        onAmount={setAmount}
        symbol={symbol}
        onSymbol={setSymbol}
        pending={sweepM.isPending}
        onSubmit={submit}
      />
      <WalletConfirmDialog
        open={Boolean(confirm)}
        payload={confirm}
        pending={sweepM.isPending}
        onConfirm={executeSweep}
        onCancel={() => setConfirm(null)}
      />
    </>
  )
}

function MoveForm({
  t,
  direction,
  amount,
  onAmount,
  symbol,
  onSymbol,
  pending,
  onSubmit,
}: {
  t: T
  direction: 'fund' | 'sweep'
  amount: string
  onAmount: (v: string) => void
  symbol: string
  onSymbol: (v: string) => void
  pending: boolean
  onSubmit: () => void
}) {
  const tok = resolveToken(symbol)
  const decimals = tok?.decimals ?? 18
  const valid = amount.trim() !== '' && parseUnits(amount, decimals) !== '0'
  const Icon = direction === 'fund' ? ArrowDownToLine : ArrowUpFromLine

  return (
    <div className="border-border flex flex-col gap-2 rounded-xl border p-3">
      <p className="text-muted-foreground text-xs">
        {direction === 'fund' ? t('fundDesc') : t('sweepDesc')}
      </p>
      <div className="flex items-center gap-2">
        <Input
          inputMode="decimal"
          placeholder="0.0"
          value={amount}
          onChange={(e) => onAmount(e.target.value)}
          className="flex-1"
        />
        <Selector
          label="Token"
          isLabelHidden
          value={symbol}
          onChange={onSymbol}
          options={TOKEN_OPTIONS.map((token) => token.symbol)}
          width={112}
        />
      </div>
      <Button
        variant={direction === 'fund' ? 'default' : 'outline'}
        onClick={onSubmit}
        disabled={!valid || pending}
        className="w-full"
      >
        {pending ? <Loader2 className="size-4 animate-spin" /> : <Icon className="size-4" />}
        {direction === 'fund' ? t('fund') : t('sweep')}
      </Button>
    </div>
  )
}

function SectionLabel({ icon: Icon, title }: { icon: typeof Eye; title: string }) {
  return (
    <div className="flex items-center gap-2">
      <Icon className="text-muted-foreground size-4" />
      <h3 className="text-foreground text-sm font-medium">{title}</h3>
    </div>
  )
}

function LimitField({
  label,
  help,
  value,
  onChange,
}: {
  label: string
  help: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-baseline justify-between gap-3">
        <p className="text-foreground text-sm font-medium">{label}</p>
        <span className="text-muted-foreground text-xs">PAX</span>
      </div>
      <Input
        inputMode="decimal"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="0.0"
      />
      <p className="text-muted-foreground text-xs">{help}</p>
    </div>
  )
}

function ToggleRow({
  label,
  help,
  checked,
  disabled,
  onChange,
}: {
  label: string
  help: string
  checked: boolean
  disabled?: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <div className="flex items-center gap-3">
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">{label}</p>
        <p className="text-muted-foreground text-xs">{help}</p>
      </div>
      <Switch checked={checked} onCheckedChange={onChange} disabled={disabled} aria-label={label} />
    </div>
  )
}

export function LeashSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <Skeleton className="h-16 w-full" />
      <Skeleton className="h-20 w-full" />
      <Skeleton className="h-40 w-full" />
      <Skeleton className="h-32 w-full" />
    </div>
  )
}
