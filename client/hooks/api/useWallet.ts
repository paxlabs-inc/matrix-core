'use client'

/**
 * Wallet hooks — the user's primary embedded wallet, the agent wallets they
 * own (owner control plane), and on-chain reads (balances/USD/tx history)
 * for any address. All authenticated calls reuse the Matrix Supabase session
 * via the paxeer wallet client; on-chain reads hit the public explorer.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  addAgentBudget,
  deactivateAgentBudget,
  freezeAgent,
  fundAgent,
  getAgent,
  getAgentActivity,
  getPrimaryWallet,
  listAgents,
  provisionPrimaryWallet,
  sweepAgent,
  unfreezeAgent,
  updateAgentPolicy,
  type AddAgentBudgetInput,
} from '@/lib/paxeer/wallet-client'
import {
  getWalletPortfolio,
  getWalletTransactions,
  type WalletPortfolio,
  type WalletTransactionsResponse,
} from '@/lib/paxeer/explorer'
import { optimisticPortfolioTransfer } from '@/lib/paxeer/portfolio-optimistic'
import type {
  AgentDetail,
  AgentPolicyPatch,
  AgentSummary,
  FundAgentInput,
  PrimaryWalletResponse,
  SweepAgentInput,
} from '@/lib/paxeer/types'

/* ── Queries ─────────────────────────────────────────────────────────────── */

/** The user's primary embedded wallet, or null when not yet provisioned. */
export function usePrimaryWallet() {
  return useQuery<PrimaryWalletResponse | null>({
    queryKey: qk.primaryWallet(),
    queryFn: ({ signal }) => getPrimaryWallet(signal),
    staleTime: 60_000,
  })
}

/** Every agent wallet the signed-in user owns. */
export function useAgentWallets() {
  return useQuery<AgentSummary[]>({
    queryKey: qk.agentWallets(),
    queryFn: ({ signal }) => listAgents(signal),
    staleTime: 30_000,
  })
}

/** Full detail + leash for one owned agent. */
export function useAgentWallet(did: string | null) {
  return useQuery<AgentDetail>({
    queryKey: qk.agentWallet(did ?? ''),
    queryFn: ({ signal }) => getAgent(did as string, signal),
    enabled: Boolean(did),
    staleTime: 20_000,
  })
}

/** Recent on-chain signatures for an owned agent. */
export function useAgentActivity(did: string | null) {
  return useQuery({
    queryKey: qk.agentActivity(did ?? ''),
    queryFn: ({ signal }) => getAgentActivity(did as string, signal),
    enabled: Boolean(did),
    staleTime: 20_000,
  })
}

/** Native + token balances with USD for any address (public explorer). */
export function useWalletPortfolio(address: string | null | undefined) {
  return useQuery<WalletPortfolio>({
    queryKey: qk.walletPortfolio(address ?? ''),
    queryFn: ({ signal }) => getWalletPortfolio(address as string, signal),
    enabled: Boolean(address),
    staleTime: 20_000,
  })
}

/** Native transactions + token transfers for any address (public explorer). */
export function useWalletTransactions(address: string | null | undefined, limit = 25) {
  return useQuery<WalletTransactionsResponse>({
    queryKey: qk.walletTxs(address ?? ''),
    queryFn: ({ signal }) =>
      getWalletTransactions(address as string, { items_count: limit }, signal),
    enabled: Boolean(address),
    staleTime: 20_000,
  })
}

/* ── Mutations ───────────────────────────────────────────────────────────── */

/** Provision the primary wallet (idempotent), then refresh it. */
export function useProvisionPrimaryWallet() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => provisionPrimaryWallet(),
    onSuccess: (data) => {
      qc.setQueryData(qk.primaryWallet(), data)
    },
  })
}

/** Freeze / unfreeze an agent (the kill switch), with optimistic flip. */
export function useSetAgentFrozen(did: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (frozen: boolean) => (frozen ? freezeAgent(did) : unfreezeAgent(did)),
    onMutate: async (frozen) => {
      await qc.cancelQueries({ queryKey: qk.agentWallet(did) })
      const prevDetail = qc.getQueryData<AgentDetail>(qk.agentWallet(did))
      const prevList = qc.getQueryData<AgentSummary[]>(qk.agentWallets())
      if (prevDetail)
        qc.setQueryData<AgentDetail>(qk.agentWallet(did), { ...prevDetail, is_frozen: frozen })
      if (prevList) {
        qc.setQueryData<AgentSummary[]>(
          qk.agentWallets(),
          prevList.map((a) => (a.did === did ? { ...a, is_frozen: frozen } : a)),
        )
      }
      return { prevDetail, prevList }
    },
    onError: (_e, _frozen, ctx) => {
      if (ctx?.prevDetail) qc.setQueryData(qk.agentWallet(did), ctx.prevDetail)
      if (ctx?.prevList) qc.setQueryData(qk.agentWallets(), ctx.prevList)
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.agentWallet(did) })
      qc.invalidateQueries({ queryKey: qk.agentWallets() })
    },
  })
}

/** Adjust the agent leash (mode + caps + switches). */
export function useUpdateAgentPolicy(did: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (patch: AgentPolicyPatch) => updateAgentPolicy(did, patch),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.agentWallet(did) })
      qc.invalidateQueries({ queryKey: qk.agentWallets() })
    },
  })
}

/** Move funds from the owner's primary wallet into the agent wallet. */
export function useFundAgent(
  did: string,
  agentAddress?: string | null,
  primaryAddress?: string | null,
) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: FundAgentInput) => fundAgent(did, input),
    onMutate: async (input) => {
      const token = input.token
      const amount = input.amount
      const keys: Array<[string, 'debit' | 'credit']> = []
      if (primaryAddress) keys.push([primaryAddress, 'debit'])
      if (agentAddress) keys.push([agentAddress, 'credit'])

      const prev = new Map<string, WalletPortfolio>()
      for (const [addr, dir] of keys) {
        await qc.cancelQueries({ queryKey: qk.walletPortfolio(addr) })
        const snap = qc.getQueryData<WalletPortfolio>(qk.walletPortfolio(addr))
        if (snap) {
          prev.set(addr, snap)
          qc.setQueryData(
            qk.walletPortfolio(addr),
            optimisticPortfolioTransfer(snap, amount, token, dir),
          )
        }
      }
      return { prev }
    },
    onError: (_e, _input, ctx) => {
      ctx?.prev.forEach((snap, addr) => qc.setQueryData(qk.walletPortfolio(addr), snap))
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.agentWallet(did) })
      if (agentAddress) qc.invalidateQueries({ queryKey: qk.walletPortfolio(agentAddress) })
      if (primaryAddress) qc.invalidateQueries({ queryKey: qk.walletPortfolio(primaryAddress) })
    },
  })
}

/** Pull funds from the agent wallet back to the owner. */
export function useSweepAgent(
  did: string,
  agentAddress?: string | null,
  primaryAddress?: string | null,
) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: SweepAgentInput) => sweepAgent(did, input),
    onMutate: async (input) => {
      const token = input.token
      const amount = input.amount
      const keys: Array<[string, 'debit' | 'credit']> = []
      if (agentAddress) keys.push([agentAddress, 'debit'])
      if (primaryAddress) keys.push([primaryAddress, 'credit'])

      const prev = new Map<string, WalletPortfolio>()
      for (const [addr, dir] of keys) {
        await qc.cancelQueries({ queryKey: qk.walletPortfolio(addr) })
        const snap = qc.getQueryData<WalletPortfolio>(qk.walletPortfolio(addr))
        if (snap) {
          prev.set(addr, snap)
          qc.setQueryData(
            qk.walletPortfolio(addr),
            optimisticPortfolioTransfer(snap, amount, token, dir),
          )
        }
      }
      return { prev }
    },
    onError: (_e, _input, ctx) => {
      ctx?.prev.forEach((snap, addr) => qc.setQueryData(qk.walletPortfolio(addr), snap))
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.agentWallet(did) })
      if (agentAddress) qc.invalidateQueries({ queryKey: qk.walletPortfolio(agentAddress) })
      if (primaryAddress) qc.invalidateQueries({ queryKey: qk.walletPortfolio(primaryAddress) })
    },
  })
}

/** Grant or revoke a time-boxed agent spend budget. */
export function useAgentBudgets(did: string) {
  const qc = useQueryClient()
  const add = useMutation({
    mutationFn: (input: AddAgentBudgetInput) => addAgentBudget(did, input),
    onSettled: () => qc.invalidateQueries({ queryKey: qk.agentWallet(did) }),
  })
  const remove = useMutation({
    mutationFn: (budgetId: string) => deactivateAgentBudget(did, budgetId),
    onSettled: () => qc.invalidateQueries({ queryKey: qk.agentWallet(did) }),
  })
  return { add, remove }
}
