/**
 * Wire types for the Paxeer embedded-wallet API (connect.paxportwallet.com).
 *
 * Ported from Paxport-Mobile-Wallet/src/lib/wallet/embedded/sdk/types.ts and
 * extended with the owner control-plane shapes returned by the server's
 * routes/owner.ts (/v1/agents*). Wallet kinds: 'standard' (the user's primary
 * wallet), 'funded' (prop-firm tiers — a separate product), and 'agent'
 * (per-agent EOA owned by the primary).
 */

export type Hex = `0x${string}`

/** Public wallet record (no key material). Returned by /v1/wallet/me. */
export interface PublicWallet {
  id: string
  address: Hex
  chain_id: number
  created_at: string
  last_used_at: string | null
}

/** Chain pointer returned alongside the wallet. */
export interface ChainInfo {
  id: number
  rpc_url: string
  explorer_url: string | null
}

/** GET /v1/wallet/me success body. */
export interface PrimaryWalletResponse {
  wallet: PublicWallet
  chain: ChainInfo
}

/** A transaction request the server will sign/broadcast. Decimal strings. */
export interface TxRequest {
  to?: Hex
  value?: string
  data?: Hex
  gas?: string
  maxFeePerGas?: string
  maxPriorityFeePerGas?: string
  nonce?: number
  chainId?: number
}

export interface SignTxResponse {
  signed_tx: Hex
  address: Hex
  chain_id: number
}

export interface SendTxResponse {
  tx_hash: Hex
  address: Hex
  chain_id: number
}

export interface SignMessageResponse {
  signature: Hex
  address: Hex
}

/**
 * Typed error for every wallet-API call. `code` is the server's stable
 * machine code (e.g. 'no_wallet', 'WITHDRAWAL_BLOCKED', 'forbidden') or
 * 'HTTP_<status>' when the body carried none. `detail` holds the raw body
 * so callers can branch on structured deny payloads.
 */
export class PaxeerWalletError extends Error {
  readonly code: string
  readonly status?: number
  readonly detail?: unknown

  constructor(message: string, code: string, status?: number, detail?: unknown) {
    super(message)
    this.name = 'PaxeerWalletError'
    this.code = code
    this.status = status
    this.detail = detail
  }
}

/* ============================================================================
 * Owner control plane — /v1/agents* (auth: owner Supabase JWT)
 * ========================================================================== */

/** Agent autonomy mode (the owner-adjustable leash). */
export type AgentMode = 'read_only' | 'trade_only' | 'full'

/** Row from GET /v1/agents — one per agent the caller owns. */
export interface AgentSummary {
  did: string
  label: string
  is_frozen: boolean
  mode: AgentMode
  address: Hex | null
  created_at: string
  last_seen_at: string | null
}

/** Effective policy (wei fields are decimal strings). */
export interface AgentPolicy {
  mode: AgentMode
  max_tx_value_wei: string
  max_daily_value_wei: string
  rate_limit_per_min: number | null
  max_approve_wei: string
  allow_native_transfer: boolean
  withdrawal_allowlist_only: boolean
  daily_reset_utc_hour: number
}

export type AgentRuleEffect = 'allow' | 'deny'
export type AgentRuleSubject = 'contract' | 'selector' | 'token' | 'address' | 'withdrawal'

export interface AgentRule {
  id: string
  effect: AgentRuleEffect
  subject: AgentRuleSubject
  value: string
  max_value_wei: string | null
  note: string | null
}

export interface AgentBudget {
  id: string
  target_contract: string | null
  token: string | null
  cap_wei: string
  spent_wei: string
  remaining_wei: string
  expires_at: string
}

/** Wallet sub-object inside the agent detail (with live native balance). */
export interface AgentWallet {
  id: string
  address: Hex
  native_wei: string | null
}

/** GET /v1/agents/:did — full agent record + leash. */
export interface AgentDetail {
  did: string
  label: string
  owner_user_id: string | null
  is_frozen: boolean
  created_at: string
  last_seen_at: string | null
  wallet: AgentWallet | null
  policy: AgentPolicy
  rules: AgentRule[]
  budgets: AgentBudget[]
}

/** One row from GET /v1/agents/:did/activity. */
export interface AgentActivityEvent {
  kind: string
  to_address: string | null
  value_wei: string
  tx_hash: string | null
  created_at: string
}

/** Patch body for PUT /v1/agents/:did/policy. Omitted fields are untouched;
 *  null clears a numeric cap back to the env default. */
export interface AgentPolicyPatch {
  mode?: AgentMode
  max_tx_value_wei?: string | null
  max_daily_value_wei?: string | null
  rate_limit_per_min?: number | null
  max_approve_wei?: string | null
  allow_native_transfer?: boolean
  withdrawal_allowlist_only?: boolean
  daily_reset_utc_hour?: number
}

/** POST /v1/agents/:did/fund — move funds from the owner's primary wallet. */
export interface FundAgentInput {
  /** Base-unit amount (wei for native, token units for ERC-20). */
  amount: string
  /** ERC-20 token address; omit for native PAX. */
  token?: string
}

/** POST /v1/agents/:did/sweep — pull funds back to the owner (or `to`). */
export interface SweepAgentInput {
  amount: string
  token?: string
  /** Destination; defaults server-side to the owner's primary wallet. */
  to?: string
}

export interface TransferResult {
  tx_hash: Hex
  from: Hex
  to: Hex
}
