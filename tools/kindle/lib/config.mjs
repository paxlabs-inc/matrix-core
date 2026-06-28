// kindle bridge — canonical config for KindleLaunch (the Sidiora launchpad on
// Paxeer Network, EVM chain id 125).
//
// SOURCE OF TRUTH: the 2026-06-20 deployment manifest
//   contracts/deployments/paxeer-addresses.json  (mirrored in protocol/addresses.go).
// These are the CURRENT launchpad contracts. They are DELIBERATELY NOT the
// `paxeer-net` config.sidioraFun block (router 0xB2D6…, USDL 0x7c69…): that is a
// different / older Sidiora.fun deployment, and transacting against it would hit
// the wrong contracts and the wrong USDL (6-dec, different address). The kindle
// bridge therefore carries its own address book bound to this manifest.
//
// Every value is overridable by environment so the same bridge runs against
// mainnet, a fork, or a staging deploy without code edits.

const env = process.env

function pick(...names) {
  for (const n of names) {
    const v = env[n]
    if (v !== undefined && String(v).trim() !== '') return String(v).trim()
  }
  return undefined
}

// ── Chain ──────────────────────────────────────────────────────────────────
export const CHAIN = {
  id: Number(pick('KINDLE_CHAIN_ID', 'PAXEER_CHAIN_ID') ?? 125),
  name: 'Paxeer Network',
  coin: 'PAX',
  decimals: 18,
}

// ── Endpoints ────────────────────────────────────────────────────────────────
// `rpc` is the chain-125 EVM JSON-RPC for on-chain views. `explorer` builds the
// human-meaningful tx links the consumer surface shows. `bff`/`indexer` are the
// KindleLaunch data gateways (api.kindlelaunch.com) for token/market detail and
// pool discovery; both optional (on-chain reads cover the same ground).
export const ENDPOINTS = {
  rpc: pick('KINDLE_RPC_URL', 'PAXEER_RPC_URL', 'GIDEON_RPC_URL')
    ?? 'https://public-mainnet.rpcpaxeer.online/evm',
  explorer: (pick('KINDLE_EXPLORER_URL', 'PAXEER_PAXSCAN_URL') ?? 'https://paxscan.io').replace(/\/+$/, ''),
  // KindleLaunch public DATA gateway (core/api): /bff/token/{pool}, /stats/{pool}, /rankings/{cat}.
  bff: (pick('KINDLE_DATA_API', 'KINDLE_BFF_URL') ?? 'https://api.kindlelaunch.com').replace(/\/+$/, ''),
}

// ── Protocol contracts (Paxeer mainnet, chain 125; 2026-06-20 manifest) ──────
// Proxies are the call targets; impls are listed in the manifest but never
// called directly. Every address is env-overridable (KINDLE_<NAME>).
export const CONTRACTS = {
  router:          pick('KINDLE_ROUTER')           ?? '0xCC7298801112682e10ee14b8a520309caD80336d',
  feesRouter:      pick('KINDLE_FEES_ROUTER')      ?? '0x02Df12a44F2658080E76fbcF7D6B34Baa97843b6',
  feeAccumulator:  pick('KINDLE_FEE_ACCUMULATOR')  ?? '0x50C69dF6637b3DCE6a7407C5A4b4F99E68514A76',
  quoter:          pick('KINDLE_QUOTER')           ?? '0xB768e183b6EfDeDf8b2AA7af732039D1C3c452d0',
  poolRegistry:    pick('KINDLE_POOL_REGISTRY')    ?? '0x7684382c89f79104574D8EF9b31eFf2eD2C2BA0b',
  sidioraNft:      pick('KINDLE_SIDIORA_NFT')      ?? '0xDF73b354ed9dcB473cc9D01541c46f507591e190',
  sidioraFactory:  pick('KINDLE_SIDIORA_FACTORY')  ?? '0x8a1A09CEe72c1D39dF33B8284E38baeF8371f465',
  opticalRegistry: pick('KINDLE_OPTICAL_REGISTRY') ?? '0x4CdA6e48632d51Ee4Fa735D81BF09F7543f644a1',
  protocolConfig:  pick('KINDLE_PROTOCOL_CONFIG')  ?? '0xEeDF5409cFD30bd14D0399318c7d2150265575e5',
  treasury:        pick('KINDLE_TREASURY')         ?? '0x15405D535ce533BfFb98c83e42f4DD242AA5e079',
  // Self-service factory that deploys per-creator LaunchpadOptical instances.
  // NOT in the 2026-06-20 manifest (deferred — Open item O1). Defaults to the
  // zero address; set KINDLE_LAUNCHPAD_OPTICAL_FACTORY once it is deployed and
  // the preset-deploy path lights up automatically.
  launchpadOpticalFactory: pick('KINDLE_LAUNCHPAD_OPTICAL_FACTORY')
    ?? '0x0000000000000000000000000000000000000000',
}

// ── Deployed preset opticals (singletons; attach directly via the optical param) ─
// Each is a deployed, immutable instance. `flags` is the getFlags() bitmap
// (bit0 beforeSwap, bit1 afterSwap, bit2 beforeFeeDistribution, bit3 afterFee).
export const PRESET_OPTICALS = {
  antisnipe: {
    key: 'antisnipe', name: 'AntiSnipeOptical', flags: 0b0001,
    address: pick('KINDLE_OPTICAL_ANTISNIPE') ?? '0x5ed0084Aa348eC45673af22e01CaF2f3500b77b5',
    description: 'Blocks buys larger than a set percent of supply for the first N blocks after launch (anti-snipe). Sells unaffected; protection auto-expires.',
  },
  maxwallet: {
    key: 'maxwallet', name: 'MaxWalletOptical', flags: 0b0001,
    address: pick('KINDLE_OPTICAL_MAXWALLET') ?? '0x0086B61fAd8fc50b2f81F92337518Ca8b4A7cc01',
    description: 'Caps how much of the token any single wallet can hold by rejecting buys that would exceed the max-wallet limit.',
  },
  tax: {
    key: 'tax', name: 'TaxOptical', flags: 0b0100,
    address: pick('KINDLE_OPTICAL_TAX') ?? '0x285411005079AaBB12bb2516bF6578fbfB11Be90',
    description: 'Applies an extra fee on trades, adjusting the fee taken during fee distribution.',
  },
  cooldown: {
    key: 'cooldown', name: 'CooldownOptical', flags: 0b0001,
    address: pick('KINDLE_OPTICAL_COOLDOWN') ?? '0xe7d450534Bc401494075e753Bb142685CF868238',
    description: 'Enforces a minimum time between trades from the same wallet (cooldown), throttling rapid-fire bots.',
  },
  buybackburn: {
    key: 'buybackburn', name: 'BuybackBurnOptical', flags: 0b0100,
    address: pick('KINDLE_OPTICAL_BUYBACKBURN') ?? '0x14ebb4F1e32070085a138296970aB90a4B5E3940',
    description: 'Diverts part of the fees toward buying back and burning the token, reducing supply over time.',
  },
}

export function resolvePresetOptical(ref) {
  if (!ref) return null
  const s = String(ref).trim().toLowerCase()
  if (PRESET_OPTICALS[s]) return PRESET_OPTICALS[s]
  return Object.values(PRESET_OPTICALS).find(
    (p) => p.name.toLowerCase() === s || p.address.toLowerCase() === s,
  ) ?? null
}

// ── Token registry — decimals MATTER for amount math (never assume 18) ─────────
// USDL is the quote token at 6 decimals; SID at 6; native PAX at 18. Launched
// tokens default to 18 but their real decimals are resolved on-chain when in
// doubt (see decimalsFor in lib/chain.mjs).
export const TOKENS = {
  PAX:  { symbol: 'PAX',  name: 'Paxeer',        decimals: 18, native: true, address: null },
  USDL: { symbol: 'USDL', name: 'Liquidity USD', decimals: 6,  stable: true,
    address: pick('KINDLE_USDL') ?? '0x85FcD13735F4309833A503EE804ea32395851479' },
  SID:  { symbol: 'SID',  name: 'Sidiora',       decimals: 6,
    address: pick('KINDLE_SID') ?? '0x21f7b20a555199fa73A238B1a91FD0f549068fEe' },
}

// USDL address + decimals are referenced widely (quote token, creation fee).
export const USDL = TOKENS.USDL

// resolveToken — by symbol (case-insensitive) or 0x address. Unknown 0x
// addresses get a synthetic entry with decimals=null so callers know to resolve
// the exact decimals on-chain rather than silently defaulting to 18.
export function resolveToken(ref) {
  if (!ref) return null
  const s = String(ref).trim()
  if (/^0x[0-9a-fA-F]{40}$/.test(s)) {
    const hit = Object.values(TOKENS).find((t) => t.address && t.address.toLowerCase() === s.toLowerCase())
    return hit ?? { symbol: s.slice(0, 10), name: 'Unknown token', decimals: null, address: s }
  }
  return TOKENS[s.toUpperCase()] ?? null
}

// ── Fee strategy enum (matches IRouter/IFeesRouter docs) ─────────────────────
export const FEE_STRATEGY = { CLAIM: 0, BURN: 1, AIRDROP: 2, LP_REWARDS: 3 }
export const FEE_STRATEGY_NAME = ['CLAIM', 'BURN', 'AIRDROP', 'LP_REWARDS']

// strategyToEnum — map a human strategy name to its enum, or null if unknown
// (callers reject unknown rather than guessing). Accepts an already-numeric 0..3.
export function strategyToEnum(ref) {
  if (ref === undefined || ref === null || ref === '') return null
  if (typeof ref === 'number' && Number.isInteger(ref) && ref >= 0 && ref <= 3) return ref
  const s = String(ref).trim().toUpperCase().replace(/[\s-]+/g, '_')
  if (/^[0-3]$/.test(s)) return Number(s)
  const i = FEE_STRATEGY[s]
  return i === undefined ? null : i
}

// ── Limits / safety knobs ────────────────────────────────────────────────────
export const LIMITS = {
  httpTimeoutMs: Number(pick('KINDLE_HTTP_TIMEOUT_MS', 'PAXEER_HTTP_TIMEOUT_MS') ?? 20000),
  rpcTimeoutMs: Number(pick('KINDLE_RPC_TIMEOUT_MS', 'PAXEER_RPC_TIMEOUT_MS') ?? 15000),
  // Default trade slippage tolerance in basis points (100 = 1%) when a caller
  // does not specify one. Used to derive quote-bounded minimum outputs.
  defaultSlippageBps: Number(pick('KINDLE_DEFAULT_SLIPPAGE_BPS') ?? 100),
  // Default trade/launch deadline horizon in seconds from now.
  deadlineSeconds: Number(pick('KINDLE_DEADLINE_SECONDS') ?? 600),
}
