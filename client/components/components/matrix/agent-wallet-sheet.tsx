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
import {
  Activity,
  Check,
  Coins,
  Copy,
  Loader2,
  Plus,
  Trash2Icon,
  Wallet,
  X,
} from '@/lib/matrix-icons'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
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
  formatBalance,
  formatUsdFromDecimal,
  parseUnits,
  shortenAddress,
} from '@/lib/paxeer/format'
import type { AgentBudget, AgentRule, AgentRuleEffect, AgentRuleSubject } from '@/lib/paxeer/types'
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
          aria-label={t('title')}
        >
          {/* header */}
          <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
            <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
              <Wallet className="size-5" />
            </span>
            <div className="min-w-0 flex-1">
              <h1 className="text-foreground text-lg font-bold tracking-tight">{t('title')}</h1>
              <p className="text-muted-foreground text-xs">{t('desc')}</p>
            </div>
            <button
              type="button"
              onClick={onClose}
              aria-label={t('close')}
              className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition"
            >
              <X className="size-5" />
            </button>
          </div>

          {/* body */}
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="mx-auto w-full max-w-2xl space-y-8 px-4 pb-10 sm:px-6">
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
      ) : null}
    </AnimatePresence>
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

  return (
    <>
      {/* Smart wallet — identity + holdings */}
      <section className="space-y-3">
        <SectionHeading icon={Wallet} title={t('walletSection')} help={t('walletSectionHelp')} />
        <div className="bg-card space-y-3 rounded-lg p-4">
          <KeyValueRow label={t('agentDid')} value={did} mono copyable copiedLabel={t('copied')} />
          <KeyValueRow
            label={t('walletAddress')}
            value={agentAddress ?? t('noWalletYet')}
            mono={Boolean(agentAddress)}
            copyable={Boolean(agentAddress)}
            copiedLabel={t('copied')}
          />
          {portfolio.data ? (
            <>
              <KeyValueRow
                label={t('nativeBalance')}
                value={`${portfolio.data.native_balance.balance} ${portfolio.data.native_balance.symbol}`}
              />
              {portfolio.data.total_value_usd !== null && (
                <KeyValueRow
                  label={t('totalValue')}
                  value={formatUsdFromDecimal(portfolio.data.total_value_usd)}
                />
              )}
            </>
          ) : agentAddress && portfolio.isLoading ? (
            <Skeleton className="h-5 w-40" />
          ) : null}
        </div>
      </section>

      {/* Leash — the existing owner controls, reused verbatim */}
      <section className="space-y-3">
        <SectionHeading icon={Activity} title={t('leashSection')} help={t('leashSectionHelp')} />
        {detail.isLoading || claimM.isPending ? (
          <LeashSkeleton />
        ) : notRegistered ? (
          <div className="bg-card flex flex-col items-start gap-3 rounded-lg p-4">
            <p className="text-muted-foreground text-xs">{t('agentNotRegistered')}</p>
            {onAskNeo && (
              <Button size="sm" onClick={() => onAskNeo(t('setupWalletPrompt'))}>
                <Wallet className="size-4" />
                {t('setupWallet')}
              </Button>
            )}
          </div>
        ) : detail.isError ? (
          <ErrorCard
            message={tw('detailError')}
            onRetry={() => detail.refetch()}
            retryLabel={tw('retry')}
          />
        ) : (
          <div className="flex flex-col gap-6">
            <LeashBody
              t={tw}
              did={did}
              policy={detail.data?.policy ?? null}
              isFrozen={detail.data?.is_frozen ?? false}
              agentAddress={agentAddress}
              primaryAddress={primaryAddress}
              open={open}
            />
          </div>
        )}
      </section>

      {/* Rules / budgets / activity live on the same owner-plane record: only
          render them when the record actually loaded, so a failed fetch never
          shows up as a false "no rules yet". */}
      {detail.data ? (
        <>
          <RulesSection did={did} rules={detail.data.rules} />
          <BudgetsSection did={did} budgets={detail.data.budgets} />
          <ActivitySection did={did} />
        </>
      ) : null}

      {/* LayerX — read-only */}
      <LayerXSection did={did} />
    </>
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
          <Select value={effect} onValueChange={(v) => setEffect(v as AgentRuleEffect)}>
            <SelectTrigger className="w-28 shrink-0">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="allow">{t('ruleAllow')}</SelectItem>
              <SelectItem value="deny">{t('ruleDeny')}</SelectItem>
            </SelectContent>
          </Select>
          <Select value={subject} onValueChange={(v) => setSubject(v as AgentRuleSubject)}>
            <SelectTrigger className="w-36 shrink-0">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {RULE_SUBJECTS.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`subject_${s}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
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
                  {formatBalance(b.remaining_wei, 18, 4)} / {formatBalance(b.cap_wei, 18, 4)} PAX
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
                  {formatBalance(e.value_wei, 18, 4)} PAX
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
            <KeyValueRow label={t('layerxBalance')} value={`${account.data.balance_usdx} USDX`} />
            <KeyValueRow label={t('layerxEscrow')} value={`${account.data.escrow_usdx} USDX`} />
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
        {transfer.amount_usdx} USDX
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
    <div className="flex items-center gap-3">
      <p className="text-muted-foreground w-28 shrink-0 text-xs">{label}</p>
      <p
        className={`text-foreground min-w-0 flex-1 truncate text-sm ${mono ? 'font-mono text-xs' : ''}`}
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
