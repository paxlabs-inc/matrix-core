/**
 * Paxeer embedded-wallet client configuration.
 *
 * The wallet API (connect.paxportwallet.com) is authenticated with the SAME
 * Supabase JWT the Centra AI client already holds: the user's Supabase identity
 * owns a primary embedded wallet on the server, which in turn owns the agent
 * wallet(s). So we reuse the existing session token (lib/auth/session
 * getAccessToken) instead of standing up a second Supabase client.
 *
 * IMPORTANT: NEXT_PUBLIC_SUPABASE_URL MUST point at the Paxport Supabase
 * project for the JWT to authenticate against the wallet API. The Centra AI
 * router accepts the same token, so one login serves both surfaces.
 */

/** Production wallet API origin. Override with NEXT_PUBLIC_PAXEER_WALLET_API. */
export const DEFAULT_WALLET_API_URL = 'https://connect.paxportwallet.com'

/** Paxeer Network mainnet. Override with NEXT_PUBLIC_PAXEER_CHAIN_ID. */
export const DEFAULT_CHAIN_ID = 125

/** Public read RPC. Override with NEXT_PUBLIC_PAXEER_RPC_URL. */
export const DEFAULT_RPC_URL = 'https://public-mainnet.rpcpaxeer.online/evm'

/** Blockscout v2 explorer API base (token balances, transfers, tx detail). */
export const DEFAULT_EXPLORER_API_URL = 'https://api.paxscan.io'

/** Portfolio / Argus user-stats indexer (USD values, PnL, charts). */
export const DEFAULT_PORTFOLIO_API_URL = 'https://us-east-1.user-stats.sidiora.exchange'

/** PAX + bridged-major price/OHLC API. */
export const DEFAULT_PRICE_API_URL = 'https://data-api.crossverse.app/api'

function trimTrailingSlash(s: string): string {
  return s.replace(/\/+$/, '')
}

function envOr(value: string | undefined, fallback: string): string {
  return value && value.trim() !== '' ? trimTrailingSlash(value.trim()) : fallback
}

/** Base URL of the embedded-wallet API (no trailing slash). */
export function walletApiBase(): string {
  return envOr(process.env.NEXT_PUBLIC_PAXEER_WALLET_API, DEFAULT_WALLET_API_URL)
}

/** Base URL of the Blockscout explorer API (no trailing slash). */
export function explorerApiBase(): string {
  return envOr(process.env.NEXT_PUBLIC_PAXEER_EXPLORER_API, DEFAULT_EXPLORER_API_URL)
}

/** Base URL of the portfolio / user-stats API (no trailing slash). */
export function portfolioApiBase(): string {
  return envOr(process.env.NEXT_PUBLIC_PAXEER_PORTFOLIO_API, DEFAULT_PORTFOLIO_API_URL)
}

/** Base URL of the price / OHLC API (no trailing slash). */
export function priceApiBase(): string {
  return envOr(process.env.NEXT_PUBLIC_PAXEER_PRICE_API, DEFAULT_PRICE_API_URL)
}

/** Configured chain id (defaults to Paxeer mainnet, 125). */
export function chainId(): number {
  const raw = process.env.NEXT_PUBLIC_PAXEER_CHAIN_ID
  const n = raw ? Number(raw) : NaN
  return Number.isFinite(n) && n > 0 ? n : DEFAULT_CHAIN_ID
}
