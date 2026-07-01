/**
 * Paxeer block-explorer read layer (Blockscout v2).
 *
 * Ported from Paxport-Mobile-Wallet/src/lib/data (client + addresses API +
 * wallet facades), trimmed to the wallet-panel surface: native + token
 * balances with USD, and transaction history. The aggregation logic
 * (formatWei, computeUsdValue, mapTransaction, getWalletPortfolio/Balance/
 * Transactions) is preserved verbatim from the proven implementation.
 *
 * Source of truth is the public Blockscout instance (paxscan.paxeer.app).
 * Reads are unauthenticated. If the upstream blocks browser CORS, point
 * NEXT_PUBLIC_PAXEER_EXPLORER_API at a same-origin proxy route instead.
 */
import { explorerApiBase } from '@/lib/paxeer/config'

/* ── HTTP client ─────────────────────────────────────────────────────────── */

const DEFAULT_TIMEOUT_MS = 30_000

export class BlockscoutApiError extends Error {
  readonly status: number
  readonly url: string
  constructor(status: number, statusText: string, body: string, url: string) {
    super(`Blockscout ${status} ${statusText}: ${body.slice(0, 200)}`)
    this.name = 'BlockscoutApiError'
    this.status = status
    this.url = url
  }
}

type QueryParams = Record<string, string | number | boolean | undefined>

async function explorerGet<T>(
  path: string,
  params?: QueryParams,
  signal?: AbortSignal,
): Promise<T> {
  let url = `${explorerApiBase()}/api${path}`
  if (params) {
    const qs = Object.entries(params)
      .filter(([, v]) => v !== undefined)
      .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
      .join('&')
    if (qs) url += `?${qs}`
  }
  const ctrl = new AbortController()
  const onAbort = () => ctrl.abort(signal?.reason)
  signal?.addEventListener('abort', onAbort, { once: true })
  const timer = setTimeout(
    () => ctrl.abort(new DOMException('timeout', 'TimeoutError')),
    DEFAULT_TIMEOUT_MS,
  )
  try {
    const res = await fetch(url, {
      method: 'GET',
      headers: { Accept: 'application/json' },
      signal: ctrl.signal,
    })
    if (!res.ok) {
      const body = await res.text().catch(() => '')
      throw new BlockscoutApiError(res.status, res.statusText, body, url)
    }
    return (await res.json()) as T
  } finally {
    clearTimeout(timer)
    signal?.removeEventListener('abort', onAbort)
  }
}

/* ── Wire types (trimmed to fields the facade reads) ─────────────────────── */

export interface PaginatedResponse<T> {
  items: T[]
  next_page_params: Record<string, unknown> | null
}

interface BlockscoutToken {
  address_hash?: string
  symbol?: string | null
  name?: string | null
  decimals?: string | null
  exchange_rate?: string | null
  icon_url?: string | null
  type?: string | null
}

interface AddressInfo {
  coin_balance?: string | null
  exchange_rate?: string | null
}

interface AddressCounters {
  transactions_count?: string
  token_transfers_count?: string
}

interface TokenBalance {
  value: string
  token?: BlockscoutToken | null
}

interface AddrRef {
  hash: string
}

interface TokenTransferTotal {
  value?: string | null
  decimals?: string | null
}

interface TokenTransfer {
  transaction_hash: string
  token?: BlockscoutToken | null
  from?: AddrRef | null
  to?: AddrRef | null
  total?: TokenTransferTotal | null
  block_number: number
  timestamp?: string | null
  log_index: number
}

interface Transaction {
  hash: string
  block_number?: number | null
  timestamp?: string | null
  from?: AddrRef | null
  to?: AddrRef | null
  value: string
  gas_used?: string | null
  gas_price?: string | null
  fee?: { value?: string | null } | null
  status?: string | null
  transaction_types?: string[] | null
  method?: string | null
  token_transfers?: TokenTransfer[] | null
}

/* ── Address API subset ──────────────────────────────────────────────────── */

export interface AddressTxsParams {
  filter?: 'to' | 'from'
  items_count?: number
}

function getAddress(hash: string, signal?: AbortSignal): Promise<AddressInfo> {
  return explorerGet<AddressInfo>(`/v2/addresses/${hash}`, undefined, signal)
}

function getAddressCounters(hash: string, signal?: AbortSignal): Promise<AddressCounters> {
  return explorerGet<AddressCounters>(`/v2/addresses/${hash}/counters`, undefined, signal)
}

function getAddressTokenBalances(hash: string, signal?: AbortSignal): Promise<TokenBalance[]> {
  return explorerGet<TokenBalance[]>(`/v2/addresses/${hash}/token-balances`, undefined, signal)
}

function getAddressTransactions(
  hash: string,
  params?: AddressTxsParams,
  signal?: AbortSignal,
): Promise<PaginatedResponse<Transaction>> {
  return explorerGet<PaginatedResponse<Transaction>>(
    `/v2/addresses/${hash}/transactions`,
    params as QueryParams,
    signal,
  )
}

function getAddressTokenTransfers(
  hash: string,
  signal?: AbortSignal,
): Promise<PaginatedResponse<TokenTransfer>> {
  return explorerGet<PaginatedResponse<TokenTransfer>>(
    `/v2/addresses/${hash}/token-transfers`,
    undefined,
    signal,
  )
}

/* ── Output types (UI-facing) ────────────────────────────────────────────── */

export interface WalletNativeBalance {
  symbol: string
  balance_raw: string
  balance: string
  price_usd: string | null
  value_usd: string | null
}

export interface WalletTokenHolding {
  contract_address: string
  symbol: string | null
  name: string | null
  decimals: number
  balance_raw: string
  balance: string
  price_usd: string | null
  value_usd: string | null
  icon_url: string | null
  token_type: string | null
}

export interface WalletPortfolio {
  address: string
  native_balance: WalletNativeBalance
  token_holdings: WalletTokenHolding[]
  total_value_usd: string | null
  token_count: number
  transaction_count: number
  transfer_count: number
  computed_at: string
}

export interface WalletBalanceSummary {
  address: string
  native_balance: string
  native_balance_usd: string
  token_balance_usd: string
  total_balance_usd: string
  token_count: number
  computed_at: string
}

export type WalletTxType =
  | 'native_transfer'
  | 'token_transfer'
  | 'contract_call'
  | 'contract_deploy'
  | 'approval'
  | 'unknown'

export interface WalletTokenTransferItem {
  tx_hash: string
  token_address: string
  token_symbol: string | null
  token_name: string | null
  token_decimals: number | null
  from_address: string
  to_address: string
  amount_raw: string
  amount: string
  direction: 'in' | 'out'
  token_type: string
  block_number: number
  timestamp: string
  log_index: number
}

export interface WalletTransaction {
  tx_hash: string
  block_number: number
  timestamp: string
  from_address: string
  to_address: string | null
  value_raw: string
  value: string
  direction: 'in' | 'out'
  gas_fee: string
  status: boolean
  tx_type: WalletTxType
  method: string | null
  token_transfers: WalletTokenTransferItem[]
}

export interface WalletTransactionsResponse {
  address: string
  transactions: WalletTransaction[]
  token_transfers: WalletTokenTransferItem[]
  next_page_params: Record<string, unknown> | null
}

/* ── Helpers (ported verbatim) ───────────────────────────────────────────── */

function formatWei(raw: string, decimals: number): string {
  if (!raw || raw === '0') return '0'
  const str = raw.padStart(decimals + 1, '0')
  const intPart = str.slice(0, str.length - decimals) || '0'
  const fracPart = str.slice(str.length - decimals).replace(/0+$/, '')
  return fracPart ? `${intPart}.${fracPart}` : intPart
}

function computeUsdValue(
  balanceRaw: string,
  decimals: number,
  exchangeRate: string | null,
): string | null {
  if (!exchangeRate) return null
  const rate = parseFloat(exchangeRate)
  if (!rate || rate <= 0) return null
  const balance = parseInt(balanceRaw || '0', 10) / Math.pow(10, decimals)
  return (balance * rate).toFixed(2)
}

function inferTxType(tx: Transaction): WalletTxType {
  const types = tx.transaction_types || []
  if (types.includes('contract_creation')) return 'contract_deploy'
  if (types.includes('token_transfer')) return 'token_transfer'
  if (types.includes('coin_transfer')) return 'native_transfer'
  if (types.includes('contract_call')) return 'contract_call'
  if (tx.method === 'approve') return 'approval'
  return 'unknown'
}

function mapTokenTransfer(tt: TokenTransfer, walletAddress: string): WalletTokenTransferItem {
  const decimals = tt.token?.decimals ? parseInt(tt.token.decimals, 10) : 18
  const rawAmount = tt.total && 'value' in tt.total && tt.total.value ? tt.total.value || '0' : '0'
  return {
    tx_hash: tt.transaction_hash,
    token_address: tt.token?.address_hash || '',
    token_symbol: tt.token?.symbol || null,
    token_name: tt.token?.name || null,
    token_decimals: decimals,
    from_address: tt.from?.hash || '',
    to_address: tt.to?.hash || '',
    amount_raw: rawAmount,
    amount: formatWei(rawAmount, decimals),
    direction: tt.to?.hash?.toLowerCase() === walletAddress.toLowerCase() ? 'in' : 'out',
    token_type: tt.token?.type || 'ERC-20',
    block_number: tt.block_number,
    timestamp: tt.timestamp || '',
    log_index: tt.log_index,
  }
}

function mapTransaction(tx: Transaction, walletAddress: string): WalletTransaction {
  const gasFee = tx.fee?.value || '0'
  const tokenTransfers = (tx.token_transfers || []).map((tt) => mapTokenTransfer(tt, walletAddress))
  return {
    tx_hash: tx.hash,
    block_number: tx.block_number || 0,
    timestamp: tx.timestamp || '',
    from_address: tx.from?.hash || '',
    to_address: tx.to?.hash || null,
    value_raw: tx.value,
    value: formatWei(tx.value, 18),
    direction: tx.to?.hash?.toLowerCase() === walletAddress.toLowerCase() ? 'in' : 'out',
    gas_fee: gasFee,
    status: tx.status === 'ok',
    tx_type: inferTxType(tx),
    method: tx.method || null,
    token_transfers: tokenTransfers,
  }
}

/* ── Facades ─────────────────────────────────────────────────────────────── */

/** Full portfolio: native + token balances with USD, totals, counts. */
export async function getWalletPortfolio(
  addressHash: string,
  signal?: AbortSignal,
): Promise<WalletPortfolio> {
  const [addrInfo, tokenBalances, counters] = await Promise.all([
    getAddress(addressHash, signal),
    getAddressTokenBalances(addressHash, signal),
    getAddressCounters(addressHash, signal).catch(() => ({}) as AddressCounters),
  ])

  const nativeRaw = addrInfo.coin_balance || '0'
  const nativeRate = addrInfo.exchange_rate ?? null
  const nativeValueUsd = computeUsdValue(nativeRaw, 18, nativeRate)

  const holdings: WalletTokenHolding[] = tokenBalances.map((tb) => {
    const decimals = tb.token?.decimals ? parseInt(tb.token.decimals, 10) : 18
    return {
      contract_address: tb.token?.address_hash || '',
      symbol: tb.token?.symbol || null,
      name: tb.token?.name || null,
      decimals,
      balance_raw: tb.value,
      balance: formatWei(tb.value, decimals),
      price_usd: tb.token?.exchange_rate || null,
      value_usd: computeUsdValue(tb.value, decimals, tb.token?.exchange_rate || null),
      icon_url: tb.token?.icon_url || null,
      token_type: tb.token?.type || null,
    }
  })

  const tokenTotalUsd = holdings.reduce((sum, h) => sum + parseFloat(h.value_usd || '0'), 0)
  const totalUsd = parseFloat(nativeValueUsd || '0') + tokenTotalUsd

  return {
    address: addressHash,
    native_balance: {
      symbol: 'PAX',
      balance_raw: nativeRaw,
      balance: formatWei(nativeRaw, 18),
      price_usd: nativeRate,
      value_usd: nativeValueUsd,
    },
    token_holdings: holdings,
    total_value_usd: totalUsd > 0 ? totalUsd.toFixed(2) : null,
    token_count: tokenBalances.length,
    transaction_count: parseInt(counters.transactions_count || '0', 10),
    transfer_count: parseInt(counters.token_transfers_count || '0', 10),
    computed_at: new Date().toISOString(),
  }
}

/** Compact balance summary (native + token USD totals). */
export async function getWalletBalance(
  addressHash: string,
  signal?: AbortSignal,
): Promise<WalletBalanceSummary> {
  const [addrInfo, tokenBalances] = await Promise.all([
    getAddress(addressHash, signal),
    getAddressTokenBalances(addressHash, signal),
  ])
  const nativeRaw = addrInfo.coin_balance || '0'
  const nativeUsd = parseFloat(
    computeUsdValue(nativeRaw, 18, addrInfo.exchange_rate ?? null) || '0',
  )
  const tokenUsd = tokenBalances.reduce((sum, tb) => {
    const decimals = tb.token?.decimals ? parseInt(tb.token.decimals, 10) : 18
    return (
      sum + parseFloat(computeUsdValue(tb.value, decimals, tb.token?.exchange_rate || null) || '0')
    )
  }, 0)
  return {
    address: addressHash,
    native_balance: formatWei(nativeRaw, 18),
    native_balance_usd: nativeUsd.toFixed(2),
    token_balance_usd: tokenUsd.toFixed(2),
    total_balance_usd: (nativeUsd + tokenUsd).toFixed(2),
    token_count: tokenBalances.length,
    computed_at: new Date().toISOString(),
  }
}

/** Native transactions + token transfers for an address. */
export async function getWalletTransactions(
  addressHash: string,
  params?: AddressTxsParams,
  signal?: AbortSignal,
): Promise<WalletTransactionsResponse> {
  const [txResult, transferResult] = await Promise.all([
    getAddressTransactions(addressHash, params, signal),
    getAddressTokenTransfers(addressHash, signal).catch(
      () => ({ items: [], next_page_params: null }) as PaginatedResponse<TokenTransfer>,
    ),
  ])
  return {
    address: addressHash,
    transactions: txResult.items.map((tx) => mapTransaction(tx, addressHash)),
    token_transfers: transferResult.items.map((tt) => mapTokenTransfer(tt, addressHash)),
    next_page_params: txResult.next_page_params,
  }
}
