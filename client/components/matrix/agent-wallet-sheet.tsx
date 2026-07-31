'use client'

/**
 * AgentWalletSheet — the owner's management page for their agent's on-chain
 * presence. Rendered as a full-page overlay (mirroring SettingsSheet), it
 * covers three surfaces for the signed-in user's ONE agent (DID from /me):
 *
 *   - Smart wallet   address, holdings, and the full leash (mode, limits,
 *                    kill switch, fund/sweep) via the Paxport owner plane.
 *   - Rules & budgets  allow/deny rules and time-boxed spend budgets.
 *   - LayerX account  READ-ONLY view of the agent's LayerX balance, escrow,
 *                    bound payout address, and transfer history. All LayerX
 *                    writes are DID-signed by the agent itself — the UI
 *                    only observes.
 *
 * Separation is by background TONE only (bg-background stage, bg-card
 * panels) — no border strokes for depth, no emojis / gradients / glow.
 */
import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useTranslations } from 'next-intl'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { toast } from 'sonner'
import { Dialog as AstryxDialog } from '@astryxdesign/core/Dialog'
import { Button as AstryxButton } from '@astryxdesign/core/Button'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Card } from '@astryxdesign/core/Card'
import { Tab, TabList } from '@astryxdesign/core/TabList'
import {
  Activity,
  Check,
  Coins,
  Copy,
  Loader2,
  Plus,
  ShieldCheck,
  Trash2Icon,
  Wallet,
  X,
} from '@/lib/matrix-icons'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Selector } from '@astryxdesign/core/Selector'
import { qk } from '@/lib/query/keys'
import { getMe } from '@/lib/api/identity'
import {
  useAgentActivity,
  useAgentBudgets,
  useAgentRules,
  useAgentWallet,
  usePrimaryWallet,
  useWalletPortfolio,
} from '@/hooks/api/useWallet'
import { layerxEnabled, useLayerXAccount } from '@/hooks/api/useLayerX'
import { claimAgent } from '@/lib/paxeer/wallet-client'
import { PaxeerWalletError } from '@/lib/paxeer/types'
import { LeashBody, LeashSkeleton } from '@/components/matrix/agent-leash-sheet'
import {
  formatDecimalAmount,
  formatTokenAmount,
  formatUsdFromDecimal,
  parseUnits,
  shortenAddress,
} from '@/lib/paxeer/format'
import type { AgentBudget, AgentRule, AgentRuleEffect, AgentRuleSubject } from '@/lib/paxeer/types'
import type { WalletPortfolio } from '@/lib/paxeer/explorer'
import type { LayerXTransfer } from '@/lib/layerx/client'

const RULE_SUBJECTS: AgentRuleSubject[] = ['contract', 'selector', 'token', 'address', 'withdrawal']

export function AgentWalletSheet({
  open,
  onClose,
  onAskNeo,
}: {
  open: boolean
  onClose: () => void
  /** Send a prompt to Neo in the chat (the sheet closes first so the
   *  conversation is visible). Used for actions only the agent can do,
   *  like provisioning its own wallet. */
  onAskNeo?: (prompt: string) => void
}) {
  const t = useTranslations('agentWallet')
  const reduce = useReducedMotion()

  const me = useQuery({
    queryKey: qk.me(),
    queryFn: ({ signal }) => getMe(signal),
    enabled: open,
    staleTime: 5 * 60_000,
  })
  const did = me.data?.did ?? null

  return (
    <AstryxDialog
      isOpen={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      variant="fullscreen"
      purpose="info"
      padding={0}
      aria-label={t('title')}
    >
      <motion.div
        initial={reduce ? false : { opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        transition={{ duration: 0.2 }}
        className="bg-background flex h-dvh flex-col"
      >
        {/* header */}
        <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
          <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
            <Wallet className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <Heading level={1}>{t('title')}</Heading>
            <Text type="supporting" display="block">
              {t('desc')}
            </Text>
          </div>
          <AstryxButton
            label={t('close')}
            variant="ghost"
            size="sm"
            icon={<X className="size-5" />}
            isIconOnly
            onClick={onClose}
          />
        </div>

        {/* body */}
        <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-6xl px-4 pb-10 sm:px-6">
            {me.isLoading ? (
              <LeashSkeleton />
            ) : me.isError || !did ? (
              <ErrorCard
                message={t('agentUnavailable')}
                onRetry={() => me.refetch()}
                retryLabel={t('retry')}
              />
            ) : (
              <SheetBody did={did} open={open} onAskNeo={onAskNeo} />
            )}
          </div>
        </div>
      </motion.div>
    </AstryxDialog>
  )
}

function SheetBody({
  did,
  open,
  onAskNeo,
}: {
  did: string
  open: boolean
  onAskNeo?: (prompt: string) => void
}) {
  const t = useTranslations('agentWallet')
  const tw = useTranslations('walletPanel')
  const reduce = useReducedMotion()
  const [tab, setTab] = useState<'overview' | 'controls' | 'rules' | 'layerx'>('overview')

  const detail = useAgentWallet(did)
  const primary = usePrimaryWallet()
  const agentAddress = detail.data?.wallet?.address ?? null
  const primaryAddress = primary.data?.wallet.address ?? null
  const portfolio = useWalletPortfolio(agentAddress)

  // The owner plane's failure modes for GET /v1/agents/:did:
  //   404 — the agent principal hasn't registered with the wallet service
  //         yet (nothing to manage; not an error worth a retry card).
  //   403 — the principal exists but this user hasn't claimed it; claim it
  //         once automatically (idempotent for the rightful owner), then
  //         refetch. Anything else is a genuine error.
  const detailStatus = detail.error instanceof PaxeerWalletError ? detail.error.status : undefined
  const notRegistered = detailStatus === 404
  const claimTried = useRef(false)
  const claimM = useMutation({
    mutationFn: () => claimAgent(did),
    onSettled: () => void detail.refetch(),
  })
  useEffect(() => {
    if (detailStatus === 403 && !claimTried.current) {
      claimTried.current = true
      claimM.mutate()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detailStatus])

  const walletReady = Boolean(detail.data)
  const isFrozen = detail.data?.is_frozen ?? false
  const nativeSymbol = portfolio.data?.native_balance.symbol ?? 'PAX'
  const nativeBalance = portfolio.data
    ? formatDecimalAmount(portfolio.data.native_balance.balance)
    : detail.data?.wallet?.native_wei
      ? formatTokenAmount(detail.data.wallet.native_wei)
      : '0'

  const tabs = [
    { id: 'overview' as const, label: t('tabOverview') },
    { id: 'controls' as const, label: t('tabControls') },
    { id: 'rules' as const, label: t('tabRulesBudgets') },
    { id: 'layerx' as const, label: t('tabLayerx') },
  ]

  return (
    <div className="space-y-6">
      <Card variant="muted" padding={5} className="sm:p-7">
        <div className="flex flex-col gap-6 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <p className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
              {t('portfolioValue')}
            </p>
            <p className="text-foreground mt-2 text-3xl font-semibold tracking-tight sm:text-4xl">
              {portfolio.data?.total_value_usd !== null &&
              portfolio.data?.total_value_usd !== undefined
                ? formatUsdFromDecimal(portfolio.data.total_value_usd)
                : '—'}
            </p>
            <p className="text-muted-foreground mt-2 text-sm">
              {nativeBalance} {nativeSymbol}
            </p>
          </div>
          <div
            className={`flex w-fit items-center gap-2 rounded-full px-3 py-1.5 text-xs font-medium ${
              isFrozen
                ? 'bg-destructive/15 text-destructive'
                : walletReady
                  ? 'bg-primary/15 text-primary'
                  : 'bg-muted text-muted-foreground'
            }`}
          >
            <ShieldCheck className="size-3.5" />
            {isFrozen ? t('statusFrozen') : walletReady ? t('statusActive') : t('statusPending')}
          </div>
        </div>

        <div className="mt-7 grid grid-cols-2 gap-3 sm:grid-cols-3">
          <PortfolioMetric
            label={t('assets')}
            value={portfolio.data ? String(portfolio.data.token_count + 1) : '—'}
          />
          <PortfolioMetric
            label={t('transactions')}
            value={portfolio.data ? String(portfolio.data.transaction_count) : '—'}
          />
          <PortfolioMetric
            label={t('transfers')}
            value={portfolio.data ? String(portfolio.data.transfer_count) : '—'}
          />
        </div>
      </Card>

      <TabList
        value={tab}
        onChange={(value) => setTab(value as typeof tab)}
        aria-label={t('walletNavigation')}
        layout="fill"
        size="lg"
      >
        {tabs.map((item) => (
          <Tab key={item.id} value={item.id} label={item.label} />
        ))}
      </TabList>

      <AnimatePresence mode="wait" initial={false}>
        <motion.div
          key={tab}
          initial={reduce ? false : { opacity: 0, y: 6 }}
          animate={{ opacity: 1, y: 0 }}
          exit={{ opacity: 0, y: -4 }}
          transition={{ duration: 0.16, ease: [0.22, 1, 0.36, 1] }}
        >
          {tab === 'overview' ? (
            <div className="grid gap-6 lg:grid-cols-[minmax(0,1.55fr)_minmax(18rem,0.8fr)]">
              <div className="min-w-0 space-y-6">
                <HoldingsSection
                  portfolio={portfolio.data}
                  loading={Boolean(agentAddress) && portfolio.isLoading}
                  error={Boolean(agentAddress) && portfolio.isError}
                  onRetry={() => portfolio.refetch()}
                />
                {walletReady ? <ActivitySection did={did} /> : null}
              </div>
              <section className="min-w-0 space-y-3">
                <SectionHeading
                  icon={Wallet}
                  title={t('walletDetails')}
                  help={t('walletSectionHelp')}
                />
                <div className="bg-card min-w-0 space-y-4 overflow-hidden rounded-xl p-4">
                  <KeyValueRow
                    label={t('walletAddress')}
                    value={agentAddress ?? t('noWalletYet')}
                    mono={Boolean(agentAddress)}
                    copyable={Boolean(agentAddress)}
                    copiedLabel={t('copied')}
                  />
                  <KeyValueRow
                    label={t('agentDid')}
                    value={did}
                    mono
                    copyable
                    copiedLabel={t('copied')}
                  />
                  <KeyValueRow
                    label={t('nativeBalance')}
                    value={`${nativeBalance} ${nativeSymbol}`}
                  />
                </div>
                {notRegistered ? (
                  <div className="bg-card flex flex-col items-start gap-3 rounded-xl p-4">
                    <p className="text-muted-foreground text-xs">{t('agentNotRegistered')}</p>
                    {onAskNeo ? (
                      <Button size="sm" onClick={() => onAskNeo(t('setupWalletPrompt'))}>
                        <Wallet className="size-4" />
                        {t('setupWallet')}
                      </Button>
                    ) : null}
                  </div>
                ) : null}
              </section>
            </div>
          ) : null}

          {tab === 'controls' ? (
            <section className="space-y-3">
              <SectionHeading
                icon={Activity}
                title={t('leashSection')}
                help={t('leashSectionHelp')}
              />
              {detail.isLoading || claimM.isPending ? (
                <LeashSkeleton />
              ) : notRegistered ? (
                <div className="bg-card flex flex-col items-start gap-3 rounded-xl p-4">
                  <p className="text-muted-foreground text-xs">{t('agentNotRegistered')}</p>
                  {onAskNeo ? (
                    <Button size="sm" onClick={() => onAskNeo(t('setupWalletPrompt'))}>
                      <Wallet className="size-4" />
                      {t('setupWallet')}
                    </Button>
                  ) : null}
                </div>
              ) : detail.isError ? (
                <ErrorCard
                  message={tw('detailError')}
                  onRetry={() => detail.refetch()}
                  retryLabel={tw('retry')}
                />
              ) : (
                <LeashBody
                  t={tw}
                  did={did}
                  policy={detail.data?.policy ?? null}
                  isFrozen={isFrozen}
                  agentAddress={agentAddress}
                  primaryAddress={primaryAddress}
                  open={open}
                />
              )}
            </section>
          ) : null}

          {tab === 'rules' ? (
            detail.data ? (
              <div className="grid gap-8 lg:grid-cols-2">
                <RulesSection did={did} rules={detail.data.rules} />
                <BudgetsSection did={did} budgets={detail.data.budgets} />
              </div>
            ) : (
              <div className="bg-card rounded-xl p-5">
                <p className="text-muted-foreground text-sm">{t('agentNotRegistered')}</p>
              </div>
            )
          ) : null}

          {tab === 'layerx' ? <LayerXSection did={did} /> : null}
        </motion.div>
      </AnimatePresence>
    </div>
  )
}

/* ── Portfolio overview ──────────────────────────────────────────────────── */

function PortfolioMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-background/55 rounded-xl px-4 py-3">
      <p className="text-muted-foreground text-xs">{label}</p>
      <p className="text-foreground mt-1 text-lg font-semibold tabular-nums">{value}</p>
    </div>
  )
}

function HoldingsSection({
  portfolio,
  loading,
  error,
  onRetry,
}: {
  portfolio: WalletPortfolio | undefined
  loading: boolean
  error: boolean
  onRetry: () => void
}) {
  const t = useTranslations('agentWallet')

  return (
    <section className="space-y-3">
      <SectionHeading icon={Coins} title={t('holdings')} help={t('holdingsHelp')} />
      {loading ? (
        <Skeleton className="h-40 w-full rounded-xl" />
      ) : error ? (
        <ErrorCard message={t('portfolioUnavailable')} onRetry={onRetry} retryLabel={t('retry')} />
      ) : portfolio ? (
        <div className="bg-card overflow-hidden rounded-xl">
          <div className="text-muted-foreground bg-muted/35 grid grid-cols-[minmax(0,1fr)_auto] gap-4 px-4 py-2.5 text-xs font-medium sm:grid-cols-[minmax(0,1fr)_minmax(8rem,auto)_minmax(7rem,auto)]">
            <span>{t('asset')}</span>
            <span className="text-right">{t('balance')}</span>
            <span className="hidden text-right sm:block">{t('value')}</span>
          </div>
          <HoldingRow
            name={portfolio.native_balance.symbol}
            detail={t('nativeAsset')}
            balance={`${formatDecimalAmount(portfolio.native_balance.balance)} ${portfolio.native_balance.symbol}`}
            value={
              portfolio.native_balance.value_usd
                ? formatUsdFromDecimal(portfolio.native_balance.value_usd)
                : '—'
            }
          />
          {portfolio.token_holdings.map((holding) => (
            <HoldingRow
              key={holding.contract_address}
              name={holding.symbol || holding.name || t('unknownToken')}
              detail={holding.name || shortenAddress(holding.contract_address, 5)}
              balance={`${formatDecimalAmount(holding.balance)} ${holding.symbol || ''}`.trim()}
              value={holding.value_usd ? formatUsdFromDecimal(holding.value_usd) : '—'}
            />
          ))}
        </div>
      ) : (
        <div className="bg-card rounded-xl p-5">
          <p className="text-muted-foreground text-sm">{t('noHoldings')}</p>
        </div>
      )}
    </section>
  )
}

function HoldingRow({
  name,
  detail,
  balance,
  value,
}: {
  name: string
  detail: string
  balance: string
  value: string
}) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-4 px-4 py-3.5 sm:grid-cols-[minmax(0,1fr)_minmax(8rem,auto)_minmax(7rem,auto)]">
      <div className="flex min-w-0 items-center gap-3">
        <span className="bg-primary/12 text-primary grid size-9 shrink-0 place-items-center rounded-full">
          <Coins className="size-4" />
        </span>
        <div className="min-w-0">
          <p className="text-foreground truncate text-sm font-medium">{name}</p>
          <p className="text-muted-foreground truncate text-xs">{detail}</p>
        </div>
      </div>
      <p className="text-foreground text-right text-sm font-medium tabular-nums">{balance}</p>
      <p className="text-muted-foreground hidden text-right text-sm tabular-nums sm:block">
        {value}
      </p>
    </div>
  )
}

/* ── Rules ───────────────────────────────────────────────────────────────── */

function RulesSection({ did, rules }: { did: string; rules: AgentRule[] }) {
  const t = useTranslations('agentWallet')
  const rulesM = useAgentRules(did)
  const [effect, setEffect] = useState<AgentRuleEffect>('deny')
  const [subject, setSubject] = useState<AgentRuleSubject>('contract')
  const [value, setValue] = useState('')

  function submit() {
    const v = value.trim()
    if (!v) return
    rulesM.add.mutate(
      { effect, subject, value: v },
      {
        onSuccess: () => {
          setValue('')
          toast.success(t('ruleAdded'))
        },
        onError: () => toast.error(t('updateFailed')),
      },
    )
  }

  return (
    <section className="space-y-3">
      <SectionHeading icon={Check} title={t('rulesSection')} help={t('rulesSectionHelp')} />
      {rules.length === 0 ? (
        <p className="text-muted-foreground text-xs">{t('noRules')}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {rules.map((r) => (
            <li key={r.id} className="bg-card flex items-center gap-3 rounded-lg px-3 py-2.5">
              <span
                className={
                  r.effect === 'allow'
                    ? 'bg-primary/15 text-primary rounded-md px-2 py-0.5 text-xs font-medium'
                    : 'bg-destructive/15 text-destructive rounded-md px-2 py-0.5 text-xs font-medium'
                }
              >
                {r.effect === 'allow' ? t('ruleAllow') : t('ruleDeny')}
              </span>
              <div className="min-w-0 flex-1">
                <p className="text-foreground truncate font-mono text-xs">{r.value}</p>
                <p className="text-muted-foreground text-xs">
                  {t(`subject_${r.subject}`)}
                  {r.note ? ` · ${r.note}` : ''}
                </p>
              </div>
              <button
                type="button"
                aria-label={t('deleteRule')}
                disabled={rulesM.remove.isPending}
                onClick={() =>
                  rulesM.remove.mutate(r.id, {
                    onSuccess: () => toast.success(t('ruleDeleted')),
                    onError: () => toast.error(t('updateFailed')),
                  })
                }
                className="text-muted-foreground hover:bg-muted hover:text-destructive grid size-8 shrink-0 place-items-center rounded-lg transition"
              >
                <Trash2Icon className="size-4" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="bg-card flex flex-col gap-2 rounded-lg p-3">
        <div className="flex items-center gap-2">
          <Selector
            label="Rule effect"
            isLabelHidden
            value={effect}
            onChange={(next) => setEffect(next as AgentRuleEffect)}
            options={[
              { value: 'allow', label: t('ruleAllow') },
              { value: 'deny', label: t('ruleDeny') },
            ]}
            width={112}
          />
          <Selector
            label="Rule subject"
            isLabelHidden
            value={subject}
            onChange={(next) => setSubject(next as AgentRuleSubject)}
            options={RULE_SUBJECTS.map((item) => ({
              value: item,
              label: t(`subject_${item}`),
            }))}
            width={144}
          />
        </div>
        <div className="flex items-center gap-2">
          <Input
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={t('ruleValuePlaceholder')}
            className="flex-1 font-mono"
          />
          <Button
            size="sm"
            onClick={submit}
            disabled={!value.trim() || rulesM.add.isPending}
            className="shrink-0"
          >
            {rulesM.add.isPending ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Plus className="size-4" />
            )}
            {t('addRule')}
          </Button>
        </div>
      </div>
    </section>
  )
}

/* ── Budgets ─────────────────────────────────────────────────────────────── */

function BudgetsSection({ did, budgets }: { did: string; budgets: AgentBudget[] }) {
  const t = useTranslations('agentWallet')
  const budgetsM = useAgentBudgets(did)
  const [cap, setCap] = useState('')
  const [hours, setHours] = useState('24')

  const capWei = parseUnits(cap, 18)
  const hoursN = Number(hours)
  const valid = capWei !== '0' && Number.isFinite(hoursN) && hoursN > 0

  function submit() {
    if (!valid) return
    budgetsM.add.mutate(
      { cap_wei: capWei, expires_in_seconds: Math.round(hoursN * 3600) },
      {
        onSuccess: () => {
          setCap('')
          toast.success(t('budgetAdded'))
        },
        onError: () => toast.error(t('updateFailed')),
      },
    )
  }

  return (
    <section className="space-y-3">
      <SectionHeading icon={Coins} title={t('budgetsSection')} help={t('budgetsSectionHelp')} />
      {budgets.length === 0 ? (
        <p className="text-muted-foreground text-xs">{t('noBudgets')}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {budgets.map((b) => (
            <li key={b.id} className="bg-card flex items-center gap-3 rounded-lg px-3 py-2.5">
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">
                  {formatTokenAmount(b.remaining_wei)} / {formatTokenAmount(b.cap_wei)} PAX
                </p>
                <p className="text-muted-foreground text-xs">
                  {t('budgetExpires', { when: new Date(b.expires_at).toLocaleString() })}
                </p>
              </div>
              <button
                type="button"
                aria-label={t('revokeBudget')}
                disabled={budgetsM.remove.isPending}
                onClick={() =>
                  budgetsM.remove.mutate(b.id, {
                    onSuccess: () => toast.success(t('budgetRevoked')),
                    onError: () => toast.error(t('updateFailed')),
                  })
                }
                className="text-muted-foreground hover:bg-muted hover:text-destructive grid size-8 shrink-0 place-items-center rounded-lg transition"
              >
                <Trash2Icon className="size-4" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="bg-card flex items-center gap-2 rounded-lg p-3">
        <Input
          inputMode="decimal"
          value={cap}
          onChange={(e) => setCap(e.target.value)}
          placeholder={t('budgetCapPlaceholder')}
          className="flex-1"
        />
        <Input
          inputMode="numeric"
          value={hours}
          onChange={(e) => setHours(e.target.value)}
          placeholder={t('budgetHoursPlaceholder')}
          className="w-24 shrink-0"
        />
        <Button
          size="sm"
          onClick={submit}
          disabled={!valid || budgetsM.add.isPending}
          className="shrink-0"
        >
          {budgetsM.add.isPending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Plus className="size-4" />
          )}
          {t('addBudget')}
        </Button>
      </div>
    </section>
  )
}

/* ── Activity ────────────────────────────────────────────────────────────── */

function ActivitySection({ did }: { did: string }) {
  const t = useTranslations('agentWallet')
  const activity = useAgentActivity(did)
  const events = activity.data ?? []

  return (
    <section className="space-y-3">
      <SectionHeading
        icon={Activity}
        title={t('activitySection')}
        help={t('activitySectionHelp')}
      />
      {activity.isLoading ? (
        <Skeleton className="h-16 w-full" />
      ) : events.length === 0 ? (
        <p className="text-muted-foreground text-xs">{t('noActivity')}</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {events.slice(0, 10).map((e, i) => (
            <li
              key={`${e.tx_hash ?? e.created_at}-${i}`}
              className="bg-card flex items-center gap-3 rounded-lg px-3 py-2.5"
            >
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">{e.kind}</p>
                <p className="text-muted-foreground truncate text-xs">
                  {e.to_address ? shortenAddress(e.to_address, 6) : '—'} ·{' '}
                  {new Date(e.created_at).toLocaleString()}
                </p>
              </div>
              {e.value_wei !== '0' && (
                <span className="text-foreground shrink-0 text-xs font-medium">
                  {formatTokenAmount(e.value_wei)} PAX
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/* ── LayerX (read-only) ──────────────────────────────────────────────────── */

function LayerXSection({ did }: { did: string }) {
  const t = useTranslations('agentWallet')
  const account = useLayerXAccount(did)

  return (
    <section className="space-y-3">
      <SectionHeading icon={Coins} title={t('layerxSection')} help={t('layerxSectionHelp')} />
      {!layerxEnabled() ? (
        <p className="text-muted-foreground text-xs">{t('layerxNotConfigured')}</p>
      ) : account.isLoading ? (
        <Skeleton className="h-24 w-full" />
      ) : account.isError ? (
        <ErrorCard
          message={t('layerxUnavailable')}
          onRetry={() => account.refetch()}
          retryLabel={t('retry')}
        />
      ) : account.data ? (
        <>
          <div className="bg-card space-y-3 rounded-lg p-4">
            <KeyValueRow
              label={t('layerxBalance')}
              value={`${formatDecimalAmount(account.data.balance_usdx)} USDX`}
            />
            <KeyValueRow
              label={t('layerxEscrow')}
              value={`${formatDecimalAmount(account.data.escrow_usdx)} USDX`}
            />
            <KeyValueRow
              label={t('layerxPayout')}
              value={account.data.evm_address || t('layerxNoPayout')}
              mono={Boolean(account.data.evm_address)}
              copyable={Boolean(account.data.evm_address)}
              copiedLabel={t('copied')}
            />
          </div>
          {account.data.history.length === 0 ? (
            <p className="text-muted-foreground text-xs">{t('layerxNoHistory')}</p>
          ) : (
            <ul className="flex flex-col gap-2">
              {account.data.history.slice(0, 10).map((tr) => (
                <LayerXTransferRow key={tr.seq} did={did} transfer={tr} />
              ))}
            </ul>
          )}
          <p className="text-muted-foreground text-xs">{t('layerxReadOnly')}</p>
        </>
      ) : null}
    </section>
  )
}

function LayerXTransferRow({ did, transfer }: { did: string; transfer: LayerXTransfer }) {
  const t = useTranslations('agentWallet')
  const outgoing = transfer.from_did === did
  const counterparty = outgoing ? transfer.to_did : transfer.from_did

  return (
    <li className="bg-card flex items-center gap-3 rounded-lg px-3 py-2.5">
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">
          {outgoing ? t('layerxSent') : t('layerxReceived')}
        </p>
        <p className="text-muted-foreground truncate text-xs">
          {counterparty} · {new Date(transfer.ts).toLocaleString()}
          {transfer.settled ? ` · ${t('layerxSettled')}` : ''}
        </p>
      </div>
      <span className="text-foreground shrink-0 text-xs font-medium">
        {outgoing ? '-' : '+'}
        {formatDecimalAmount(transfer.amount_usdx)} USDX
      </span>
    </li>
  )
}

/* ── Shared bits ─────────────────────────────────────────────────────────── */

function SectionHeading({
  icon: Icon,
  title,
  help,
}: {
  icon: typeof Wallet
  title: string
  help: string
}) {
  return (
    <div>
      <h3 className="text-muted-foreground flex items-center gap-2 text-xs font-medium tracking-wide uppercase">
        <Icon className="size-3.5" />
        {title}
      </h3>
      <p className="text-muted-foreground/70 mt-1 text-xs">{help}</p>
    </div>
  )
}

function KeyValueRow({
  label,
  value,
  mono,
  copyable,
  copiedLabel,
}: {
  label: string
  value: string
  mono?: boolean
  copyable?: boolean
  copiedLabel?: string
}) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const id = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(id)
  }, [copied])

  return (
    <div className="grid min-w-0 grid-cols-[minmax(5.5rem,7rem)_minmax(0,1fr)_auto] items-center gap-3">
      <p className="text-muted-foreground min-w-0 text-xs">{label}</p>
      <p
        title={value}
        className={`text-foreground min-w-0 truncate text-sm ${mono ? 'font-mono text-xs' : ''}`}
      >
        {value}
      </p>
      {copyable && (
        <button
          type="button"
          aria-label={`${label}: copy`}
          onClick={() => {
            void navigator.clipboard.writeText(value).then(() => {
              setCopied(true)
              if (copiedLabel) toast.success(copiedLabel)
            })
          }}
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-7 shrink-0 place-items-center rounded-md transition"
        >
          {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
        </button>
      )}
    </div>
  )
}

function ErrorCard({
  message,
  onRetry,
  retryLabel,
}: {
  message: string
  onRetry: () => void
  retryLabel: string
}) {
  return (
    <div className="bg-card flex flex-col items-center gap-3 rounded-lg p-6 text-center">
      <p className="text-muted-foreground text-sm">{message}</p>
      <Button variant="secondary" size="sm" onClick={onRetry}>
        {retryLabel}
      </Button>
    </div>
  )
}
