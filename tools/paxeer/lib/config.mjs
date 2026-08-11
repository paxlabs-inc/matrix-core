// paxeer-net bridge — canonical config: endpoints, chain, and token registry.
//
// Every value below is overridable by environment so the same bridge runs
// against mainnet, a fork, or a staging deploy without code edits.

const env = process.env

function pick(...names) {
  for (const n of names) {
    const v = env[n]
    if (v !== undefined && String(v).trim() !== '') return String(v).trim()
  }
  return undefined
}

// ── Chain ────────────────────────────────────────────────────────────────
export const CHAIN = {
  id: Number(pick('PAXEER_CHAIN_ID') ?? 125),
  cosmosId: pick('PAXEER_COSMOS_CHAIN_ID') ?? 'hyperpax_125_1',
  name: 'Paxeer Network',
  coin: 'PAX',
  cosmosAlias: 'hpx',
  bech32Prefix: 'pax',
  decimals: 18,
}

// ── Read endpoints (the "massive data layer" agents get) ───────────────────
// All read-only. No key material or auth.
export const ENDPOINTS = {
  // EVM JSON-RPC (direct node access).
  rpc: pick('PAXEER_RPC_URL', 'GIDEON_RPC_URL') ?? 'https://stats.paxscan.io',
  // Alternate documented EVM RPC.
  rpcAlt: pick('PAXEER_RPC_ALT_URL') ?? 'https://stats.paxscan.io',
  // PaxScan = Paxeer's Blockscout v2 explorer. Client hits `${paxscan}/api/v2/...`.
  paxscan: (pick('PAXEER_PAXSCAN_URL') ?? 'https://api.paxscan.io').replace(/\/+$/, ''),
  // Price + OHLC data API (PAX + bridged majors).
  price: (pick('PAXEER_PRICE_URL') ?? 'https://data-api.crossverse.app/api').replace(/\/+$/, ''),
}

// ── Embedded wallet REST API (the network-side custody + enforcement surface) ─
// Agents sign/send here; keys never leave the server. Auth = Supabase JWT.
export const WALLET_API = {
  base: (pick('PAXEER_WALLET_API', 'PAXNET_WALLET_API') ?? 'https://connect.paxportwallet.com').replace(/\/+$/, ''),
  supabaseUrl: (pick('PAXEER_SUPABASE_URL') ?? 'https://supabase.paxeer.app').replace(/\/+$/, ''),
  supabaseAnonKey: pick('PAXEER_SUPABASE_ANON_KEY', 'PAXEER_SUPABASE_PUBLISHABLE_KEY'),
  // Headless agent auth (server-side). Provide ONE of:
  //   PAXEER_WALLET_TOKEN  — a ready Supabase access_token (Bearer JWT), OR
  //   PAXEER_WALLET_EMAIL + PAXEER_WALLET_PASSWORD — password grant the bridge
  //     exchanges for a token via Supabase /auth/v1/token?grant_type=password.
  token: pick('PAXEER_WALLET_TOKEN'),
  email: pick('PAXEER_WALLET_EMAIL'),
  password: pick('PAXEER_WALLET_PASSWORD'),
}

// ── Agent-native DID auth (preferred write lane; see lib/agentauth.mjs) ─────
// The daemon's ed25519 executor key proves the agent's did:matrix identity to
// /v1/agent/*. keyfile + label mirror executor/cmd/mcl-execute/identity.go and
// default to the daemon's own env so no extra wiring is needed in hosted mode.
export const AGENT_AUTH = {
  keyfile: pick('PAXEER_AGENT_KEYFILE', 'MATRIX_EXECUTOR_KEYFILE')
    ?? `${pick('MATRIX_DATA_DIR') ?? '/data'}/.matrix/executor.key`,
  label: pick('PAXEER_AGENT_LABEL', 'MATRIX_USER_ID', 'MATRIX_DID_LABEL'),
  disabled: pick('PAXEER_AGENT_AUTH_DISABLE') === '1',
}

// ── Token registry (mainnet addresses; decimals matter for amount math) ────
// Source: Paxport-Mobile-Wallet/src/lib/constants.ts + swap/sdk/addresses.ts.
export const TOKENS = {
  PAX:   { symbol: 'PAX',   name: 'Paxeer',        decimals: 18, native: true,  address: null },
  WPAX9: { symbol: 'WPAX9', name: 'Wrapped PAX',   decimals: 18, address: '0xD152891923C7D6fE84d3DCF58621aB2be0eFCbc2' },
  USDC:  { symbol: 'USDC',  name: 'USD Coin',      decimals: 6,  address: '0x4b29871681c95DFB2c7824BC4b0326B80217bCe8', stable: true },
  USDT:  { symbol: 'USDT',  name: 'Tether USD',    decimals: 6,  address: '0xe76f24bcF307290e4e09Ee45021CeC998c3749ce', stable: true },
  USDL:  { symbol: 'USDL',  name: 'Ledger USD',    decimals: 6,  address: '0x85FcD13735F4309833A503EE804ea32395851479', stable: true },
  SID:   { symbol: 'SID',   name: 'Sidiora',       decimals: 6,  address: '0x21f7b20a555199fa73A238B1a91FD0f549068fEe' },
  WETH:  { symbol: 'MTX',   name: 'Centra Token',  decimals: 6,  address: '0x471368EF4E11c6f8647e6743031Dfc346cB8A99c' },
  WBNB:  { symbol: 'WBNB',  name: 'Paxie ',        decimals: 6,  address: '0x21AEd826Df2e4dd3dE3B29b7347a7aCF61F19b21' },
}

// Resolve a token by symbol (case-insensitive) or 0x address. Returns the
// registry entry, or a synthetic entry for an unknown 0x address (decimals
// default 18 — callers that need exact decimals should pass them explicitly).
export function resolveToken(ref) {
  if (!ref) return null
  const s = String(ref).trim()
  if (/^0x[0-9a-fA-F]{40}$/.test(s)) {
    const hit = Object.values(TOKENS).find(
      (t) => t.address && t.address.toLowerCase() === s.toLowerCase(),
    )
    return hit ?? { symbol: s.slice(0, 8), name: 'Unknown token', decimals: 18, address: s }
  }
  return TOKENS[s.toUpperCase()] ?? null
}

// Limits / safety knobs (also referenced by the spend policy guard).
export const LIMITS = {
  httpTimeoutMs: Number(pick('PAXEER_HTTP_TIMEOUT_MS') ?? 20000),
  rpcTimeoutMs: Number(pick('PAXEER_RPC_TIMEOUT_MS') ?? 15000),
  maxResponseBytes: Number(pick('PAXEER_MAX_BYTES') ?? 1_000_000),
  // Per-call native PAX spend ceiling (wei). 0 = unlimited (rely on network policy).
  maxSpendWei: pick('PAXEER_MAX_SPEND_WEI') ?? '0',
}
