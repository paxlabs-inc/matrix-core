'use client'

/**
 * WalletPanel — the user's primary embedded wallet plus the agent wallets
 * they own. Reads live from the Paxeer wallet API (connect.paxportwallet.com,
 * authed with the Matrix Supabase session) and the public block explorer.
 */

import { useState } from 'react'
import {
  AlertCircle,
  ArrowDownLeft,
  ArrowUpRight,
  Bot,
  Check,
  ChevronRight,
  Copy,
  ExternalLink,
  Loader2,
  ShieldCheck,
  Wallet as WalletIcon,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Pill } from '@/components/matrix/primitives'
import { AgentLeashSheet } from '@/components/matrix/agent-leash-sheet'
import { cn } from '@/lib/utils'
import { useAuth } from '@/lib/auth/AuthProvider'
import {
  useAgentWallets,
  usePrimaryWallet,
  useProvisionPrimaryWallet,
  useWalletPortfolio,
  useWalletTransactions,
} from '@/hooks/api/useWallet'
import { formatUsd, shortenAddress } from '@/lib/paxeer/format'
import { explorerApiBase } from '@/lib/paxeer/config'
import { PaxeerWalletError, type AgentMode, type AgentSummary } from '@/lib/paxeer/types'
import type { WalletTransaction } from '@/lib/paxeer/explorer'

type T = ReturnType<typeof useTranslations>

const MODE_LABEL: Record<AgentMode, string> = {
  read_only: 'modeReadOnly',
  trade_only: 'modeTradeOnly',
  full: 'modeFull',
}

function usd(value: string | null | undefined): string {
  return formatUsd(value ? parseFloat(value) : 0)
}

function timeAgo(iso: string): string {
  const then = new Date(iso).getTime()
  if (!iso || Number.isNaN(then)) return ''
  const secs = Math.max(0, Math.floor((Date.now() - then) / 1000))
  if (secs < 60) return `${secs}s ago`
  const mins = Math.floor(secs / 60)
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ago`
  return `${Math.floor(hrs / 24)}d ago`
}

function explorerUrlFor(kind: 'address' | 'tx', value: string, base?: string | null): string {
  const root = (base || explorerApiBase()).replace(/\/+$/, '')
  return `${root}/${kind}/${value}`
}

function isNoSession(err: unknown): boolean {
  return err instanceof PaxeerWalletError && err.code === 'NO_SESSION'
}

export function WalletPanel() {
  const t = useTranslations('walletPanel')
  const { enabled: authEnabled, user, loading: authLoading } = useAuth()
  const primary = usePrimaryWallet()

  if (authEnabled && authLoading) return <WalletSkeleton />
  if (authEnabled && !user) {
    return (
      <CenteredState
        icon={<WalletIcon className="size-6" />}
        title={t('signInTitle')}
        desc={t('signInDesc')}
      />
    )
  }
  if (primary.isLoading) return <WalletSkeleton />
  if (primary.isError) {
    if (isNoSession(primary.error)) {
      return (
        <CenteredState
          icon={<WalletIcon className="size-6" />}
          title={t('signInTitle')}
          desc={t('signInDesc')}
        />
      )
    }
    return (
      <CenteredState
        icon={<AlertCircle className="text-destructive size-6" />}
        title={t('loadError')}
        action={
          <Button variant="outline" size="sm" onClick={() => primary.refetch()}>
            {t('retry')}
          </Button>
        }
      />
    )
  }
  if (!primary.data) return <ProvisionCard t={t} />

  const address = primary.data.wallet.address
  const explorerUrl = primary.data.chain.explorer_url

  return (
    <div className="flex flex-col gap-6">
      <PrimaryCard t={t} address={address} explorerUrl={explorerUrl} />
      <AgentWalletsSection t={t} primaryAddress={address} />
      <ActivitySection t={t} address={address} explorerUrl={explorerUrl} />
    </div>
  )
}

function PrimaryCard({
  t,
  address,
  explorerUrl,
}: {
  t: T
  address: string
  explorerUrl: string | null
}) {
  const portfolio = useWalletPortfolio(address)
  const holdings = portfolio.data?.token_holdings ?? []
  const native = portfolio.data?.native_balance

  return (
    <Card className="overflow-hidden">
      <CardContent className="flex flex-col gap-5 p-5 sm:p-6">
        <div className="flex items-center gap-2">
          <ShieldCheck className="text-primary size-4" />
          <span className="text-muted-foreground text-sm">{t('yourWallet')}</span>
        </div>

        <div>
          <p className="text-muted-foreground text-sm">{t('totalValue')}</p>
          {portfolio.isLoading ? (
            <Skeleton className="mt-2 h-10 w-40" />
          ) : (
            <p className="text-foreground mt-1 text-4xl font-semibold tracking-tight">
              {usd(portfolio.data?.total_value_usd)}
            </p>
          )}
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <AddressChip address={address} t={t} />
          <a
            href={explorerUrlFor('address', address, explorerUrl)}
            target="_blank"
            rel="noreferrer"
            className="border-border bg-muted/40 text-muted-foreground hover:bg-accent hover:text-foreground flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-xs transition-colors"
          >
            {t('viewOnExplorer')}
            <ExternalLink className="size-3.5" />
          </a>
        </div>

        <div>
          <h3 className="text-muted-foreground mb-2 text-xs font-medium tracking-wide uppercase">
            {t('holdings')}
          </h3>
          {portfolio.isLoading ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-12 w-full" />
            </div>
          ) : !native && holdings.length === 0 ? (
            <p className="text-muted-foreground py-6 text-center text-sm">{t('noHoldings')}</p>
          ) : (
            <ul className="divide-border divide-y">
              {native && (
                <HoldingRow
                  symbol={native.symbol}
                  name="Paxeer"
                  balance={native.balance}
                  valueUsd={native.value_usd}
                />
              )}
              {holdings.map((h) => (
                <HoldingRow
                  key={h.contract_address || h.symbol || h.name}
                  symbol={h.symbol ?? '—'}
                  name={h.name ?? ''}
                  balance={h.balance}
                  valueUsd={h.value_usd}
                />
              ))}
            </ul>
          )}
        </div>
      </CardContent>
    </Card>
  )
}

function HoldingRow({
  symbol,
  name,
  balance,
  valueUsd,
}: {
  symbol: string
  name: string
  balance: string
  valueUsd: string | null
}) {
  return (
    <li className="flex items-center gap-3 py-3">
      <span className="bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold">
        {symbol.slice(0, 3).toUpperCase()}
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground truncate text-sm font-medium">{symbol}</p>
        {name && <p className="text-muted-foreground truncate text-xs">{name}</p>}
      </div>
      <div className="text-right">
        <p className="text-foreground font-mono text-sm">{balance}</p>
        {valueUsd && <p className="text-muted-foreground text-xs">{usd(valueUsd)}</p>}
      </div>
    </li>
  )
}

function AgentWalletsSection({ t, primaryAddress }: { t: T; primaryAddress: string | null }) {
  const agents = useAgentWallets()

  return (
    <div>
      <div className="mb-1 flex items-center gap-2">
        <Bot className="text-muted-foreground size-4" />
        <h2 className="text-foreground text-sm font-medium">{t('agentWallets')}</h2>
      </div>
      <p className="text-muted-foreground mb-3 text-xs">{t('agentWalletsDesc')}</p>

      {agents.isLoading ? (
        <div className="flex flex-col gap-3">
          <Skeleton className="h-[68px] w-full" />
          <Skeleton className="h-[68px] w-full" />
        </div>
      ) : agents.data && agents.data.length > 0 ? (
        <div className="grid grid-cols-1 gap-3">
          {agents.data.map((a) => (
            <AgentRow key={a.did} agent={a} t={t} primaryAddress={primaryAddress} />
          ))}
        </div>
      ) : (
        <Card>
          <CardContent className="flex flex-col items-center gap-1 py-8 text-center">
            <p className="text-foreground text-sm font-medium">{t('noAgents')}</p>
            <p className="text-muted-foreground text-xs">{t('noAgentsDesc')}</p>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

function AgentRow({
  agent,
  t,
  primaryAddress,
}: {
  agent: AgentSummary
  t: T
  primaryAddress: string | null
}) {
  const [open, setOpen] = useState(false)
  const portfolio = useWalletPortfolio(agent.address)
  const shortName = agent.label.length > 18 ? `${agent.label.slice(0, 18)}…` : agent.label

  return (
    <>
      <Card className="overflow-hidden">
        <button
          type="button"
          onClick={() => setOpen(true)}
          className="hover:bg-accent/40 flex w-full items-center gap-3 p-4 text-left transition-colors"
        >
          <span
            className={cn(
              'flex size-10 shrink-0 items-center justify-center rounded-full',
              agent.is_frozen ? 'bg-muted text-muted-foreground' : 'bg-primary/10 text-primary',
            )}
          >
            <Bot className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <p className="text-foreground truncate text-sm font-medium">{shortName || 'Agent'}</p>
              <ModeBadge mode={agent.mode} t={t} />
            </div>
            {agent.address ? (
              <p className="text-muted-foreground font-mono text-xs">
                {shortenAddress(agent.address, 4)}
              </p>
            ) : (
              <p className="text-muted-foreground text-xs">{t('agentNoWallet')}</p>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-2 text-right">
            {agent.is_frozen ? (
              <Pill className="px-2 py-0.5">{t('frozen')}</Pill>
            ) : (
              <span className="text-foreground text-sm font-medium">
                {agent.address ? usd(portfolio.data?.total_value_usd) : '—'}
              </span>
            )}
            <ChevronRight className="text-muted-foreground size-4" />
          </div>
        </button>
      </Card>
      <AgentLeashSheet
        agent={agent}
        primaryAddress={primaryAddress}
        open={open}
        onOpenChange={setOpen}
      />
    </>
  )
}

function ModeBadge({ mode, t }: { mode: AgentMode; t: T }) {
  return (
    <Pill tone={mode === 'full' ? 'signal' : 'neutral'} className="px-2 py-0 text-[10px]">
      {t(MODE_LABEL[mode])}
    </Pill>
  )
}

function ActivitySection({
  t,
  address,
  explorerUrl,
}: {
  t: T
  address: string
  explorerUrl: string | null
}) {
  const txs = useWalletTransactions(address)
  const items = txs.data?.transactions ?? []

  return (
    <div>
      <h2 className="text-foreground mb-3 text-sm font-medium">{t('recentActivity')}</h2>
      <Card>
        <CardContent className="p-0">
          {txs.isLoading ? (
            <div className="flex flex-col gap-2 p-4">
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-12 w-full" />
            </div>
          ) : items.length === 0 ? (
            <p className="text-muted-foreground p-8 text-center text-sm">{t('noActivity')}</p>
          ) : (
            <ul className="divide-border divide-y">
              {items.slice(0, 12).map((tx) => (
                <TxRow key={tx.tx_hash} tx={tx} explorerUrl={explorerUrl} t={t} />
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function TxRow({
  tx,
  explorerUrl,
  t,
}: {
  tx: WalletTransaction
  explorerUrl: string | null
  t: T
}) {
  const incoming = tx.direction === 'in'
  const transfer = tx.token_transfers[0]
  const amount = transfer
    ? `${transfer.amount} ${transfer.token_symbol ?? ''}`.trim()
    : `${tx.value} PAX`
  const counterparty = incoming ? tx.from_address : tx.to_address
  const label = tx.method || (incoming ? t('received') : t('sent'))

  return (
    <li>
      <a
        href={explorerUrlFor('tx', tx.tx_hash, explorerUrl)}
        target="_blank"
        rel="noreferrer"
        className="hover:bg-accent/50 flex items-center gap-3 px-4 py-3 transition-colors"
      >
        <span
          className={cn(
            'flex size-8 shrink-0 items-center justify-center rounded-full',
            incoming ? 'bg-primary/10 text-primary' : 'bg-muted text-muted-foreground',
          )}
        >
          {incoming ? <ArrowDownLeft className="size-4" /> : <ArrowUpRight className="size-4" />}
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground truncate text-sm">{label}</p>
          <p className="text-muted-foreground truncate font-mono text-xs">
            {counterparty ? shortenAddress(counterparty, 4) : '—'}
            {tx.timestamp ? ` · ${timeAgo(tx.timestamp)}` : ''}
            {!tx.status ? ` · ${t('failed')}` : ''}
          </p>
        </div>
        <p
          className={cn(
            'shrink-0 text-sm font-medium',
            incoming ? 'text-primary' : 'text-foreground',
          )}
        >
          {incoming ? '+' : '-'}
          {amount}
        </p>
      </a>
    </li>
  )
}

function AddressChip({ address, t }: { address: string; t: T }) {
  const [copied, setCopied] = useState(false)
  async function copy() {
    try {
      await navigator.clipboard.writeText(address)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      /* clipboard unavailable */
    }
  }
  return (
    <button
      type="button"
      onClick={copy}
      title={t('copyAddress')}
      className="border-border bg-muted/40 text-muted-foreground hover:bg-accent hover:text-foreground flex items-center gap-2 rounded-lg border px-3 py-1.5 font-mono text-xs transition-colors"
    >
      {shortenAddress(address, 4)}
      {copied ? <Check className="text-primary size-3.5" /> : <Copy className="size-3.5" />}
    </button>
  )
}

function ProvisionCard({ t }: { t: T }) {
  const provision = useProvisionPrimaryWallet()
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 px-6 py-12 text-center">
        <span className="bg-primary/10 text-primary flex size-12 items-center justify-center rounded-full">
          <WalletIcon className="size-6" />
        </span>
        <div className="max-w-sm">
          <p className="text-foreground text-base font-semibold">{t('noPrimaryTitle')}</p>
          <p className="text-muted-foreground mt-1 text-sm">{t('noPrimaryDesc')}</p>
        </div>
        <Button onClick={() => provision.mutate()} disabled={provision.isPending}>
          {provision.isPending ? (
            <>
              <Loader2 className="size-4 animate-spin" data-icon="inline-start" />
              {t('creating')}
            </>
          ) : (
            t('createWallet')
          )}
        </Button>
        {provision.isError && (
          <p className="text-destructive text-xs">
            {provision.error instanceof Error ? provision.error.message : t('loadError')}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

function CenteredState({
  icon,
  title,
  desc,
  action,
}: {
  icon: React.ReactNode
  title: string
  desc?: string
  action?: React.ReactNode
}) {
  return (
    <Card>
      <CardContent className="flex flex-col items-center gap-3 px-6 py-12 text-center">
        <span className="bg-muted text-muted-foreground flex size-12 items-center justify-center rounded-full">
          {icon}
        </span>
        <div className="max-w-sm">
          <p className="text-foreground text-base font-semibold">{title}</p>
          {desc && <p className="text-muted-foreground mt-1 text-sm">{desc}</p>}
        </div>
        {action}
      </CardContent>
    </Card>
  )
}

function WalletSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardContent className="flex flex-col gap-5 p-5 sm:p-6">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-10 w-40" />
          <Skeleton className="h-8 w-56" />
          <div className="flex flex-col gap-2">
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-12 w-full" />
          </div>
        </CardContent>
      </Card>
      <Skeleton className="h-20 w-full" />
      <Skeleton className="h-40 w-full" />
    </div>
  )
}
