/**
 * Paxeer token registry (mainnet). Ported from
 * Paxport-Mobile-Wallet/src/lib/constants.ts + the paxeer-net bridge
 * config. Decimals matter for amount math; `stable` marks USD-pegged
 * tokens so the UI can treat them as $1 when a live price is unavailable.
 *
 * Addresses are overridable via NEXT_PUBLIC_* so the same client runs
 * against mainnet, a fork, or staging without code edits.
 */

export interface TokenInfo {
  symbol: string
  name: string
  decimals: number
  native?: boolean
  address?: string
  stable?: boolean
}

function addr(envVar: string | undefined, fallback: string): string {
  return envVar && envVar.trim() !== '' ? envVar.trim() : fallback
}

export const TOKENS: Record<string, TokenInfo> = {
  PAX: { symbol: 'PAX', name: 'Paxeer', decimals: 18, native: true },
  WPAX9: {
    symbol: 'WPAX9',
    name: 'Wrapped PAX',
    decimals: 18,
    address: addr(process.env.NEXT_PUBLIC_WPAX9, '0xe5ccf339d1c89c7e6c6768b28507f78b861fc1de'),
  },
  USDC: {
    symbol: 'USDC',
    name: 'USD Coin',
    decimals: 6,
    address: addr(process.env.NEXT_PUBLIC_USDC, '0xf8850b62AE017c55be7f571BBad840b4f3DA7D49'),
    stable: true,
  },
  USDT: {
    symbol: 'USDT',
    name: 'Tether USD',
    decimals: 6,
    address: addr(process.env.NEXT_PUBLIC_USDT, '0x5dfE06Ae465a39c442c45ed273c523BaC2d1f6a8'),
    stable: true,
  },
  USDL: {
    symbol: 'USDL',
    name: 'Liquidity USD',
    decimals: 6,
    address: addr(process.env.NEXT_PUBLIC_USDL, '0x7c69c84daAEe90B21eeCABDb8f0387897E9B7B37'),
    stable: true,
  },
  USID: {
    symbol: 'USID',
    name: 'USD Sidiora',
    decimals: 18,
    address: addr(process.env.NEXT_PUBLIC_USID, '0x6C32c255EeBD6A72B56ee82454d7140020919652'),
    stable: true,
  },
  SID: {
    symbol: 'SID',
    name: 'Sidiora',
    decimals: 6,
    address: addr(process.env.NEXT_PUBLIC_SID, '0x86949e4CdB89496490890B67C9cfF63eD8efB4b1'),
  },
  WETH: {
    symbol: 'WETH',
    name: 'Wrapped ETH',
    decimals: 18,
    address: addr(process.env.NEXT_PUBLIC_WETH, '0x5ba2f89F60f5805512A265bdFbB8984C85b4c9B7'),
  },
  WBNB: {
    symbol: 'WBNB',
    name: 'Wrapped BNB',
    decimals: 18,
    address: addr(process.env.NEXT_PUBLIC_WBNB, '0x2cE6495AF2F6cF20ea1b4d637dC2E882a0276F36'),
  },
  WSOL: {
    symbol: 'WSOL',
    name: 'Wrapped SOL',
    decimals: 9,
    address: addr(process.env.NEXT_PUBLIC_WSOL, '0x38416f047c53C6D295AfF15e2fD296B6C896E2d8'),
  },
}

/** Native PAX decimals — used widely for gas + native-value math. */
export const NATIVE_DECIMALS = 18
export const NATIVE_SYMBOL = 'PAX'

/**
 * Resolve a token by symbol (case-insensitive) or 0x address. Returns the
 * registry entry, or a synthetic entry for an unknown 0x address (decimals
 * default 18 — callers needing exact decimals should pass them explicitly).
 */
export function resolveToken(ref: string | null | undefined): TokenInfo | null {
  if (!ref) return null
  const s = String(ref).trim()
  if (/^0x[0-9a-fA-F]{40}$/.test(s)) {
    const hit = Object.values(TOKENS).find(
      (t) => t.address && t.address.toLowerCase() === s.toLowerCase(),
    )
    return hit ?? { symbol: `${s.slice(0, 6)}…`, name: 'Unknown token', decimals: 18, address: s }
  }
  return TOKENS[s.toUpperCase()] ?? null
}

/** True when the symbol/address is a USD-pegged stable (treat as ~$1). */
export function isStable(ref: string | null | undefined): boolean {
  return resolveToken(ref)?.stable === true
}
