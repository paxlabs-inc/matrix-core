// paxeer-net bridge — canonical config: endpoints, chain, token registry,
// precompile addresses, and protocol contracts.
//
// SOURCE-OF-TRUTH HIERARCHY (Andrew, 2026-05-30):
//   1. Everyday DEX/swap contracts -> what is WIRED INTO THE WALLET, i.e.
//      Paxport-Mobile-Wallet/src/lib/swap/sdk/addresses.ts (PECOR V3/V4 +
//      Sidiora). These win for swap routing/execution.
//   2. Sidiora.fun launchpad + HyperPax Perps + HyperPax DEX v5 -> the
//      docs.paxeer.app address tables. Docs index: https://docs.paxeer.app/llms.txt
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
// All read-only. No key material, no auth (except portfolio webhooks).
export const ENDPOINTS = {
  // EVM JSON-RPC (direct node access).
  rpc: pick('PAXEER_RPC_URL', 'GIDEON_RPC_URL') ?? 'https://public-mainnet.rpcpaxeer.online/evm',
  // Alternate documented EVM RPC.
  rpcAlt: pick('PAXEER_RPC_ALT_URL') ?? 'https://public-rpc.paxeer.app/rpc',
  // PaxScan = Paxeer's Blockscout v2 explorer. Client hits `${paxscan}/api/v2/...`.
  paxscan: (pick('PAXEER_PAXSCAN_URL') ?? 'https://paxscan.paxeer.app').replace(/\/+$/, ''),
  // Portfolio / Argus user-stats indexer (pnl, rank, performance, charts, rewards).
  portfolio: (pick('PAXEER_PORTFOLIO_URL') ?? 'https://us-east-1.user-stats.sidiora.exchange').replace(/\/+$/, ''),
  // PaxSpot DEX market-data API.
  spot: (pick('PAXEER_SPOT_URL') ?? 'https://us-east-1.spot-api.sidiora.exchange').replace(/\/+$/, ''),
  // Price + OHLC data API (PAX + bridged majors).
  price: (pick('PAXEER_PRICE_URL') ?? 'https://data-api.crossverse.app/api').replace(/\/+$/, ''),
  // Points / rewards indexer.
  points: (pick('PAXEER_POINTS_URL') ?? 'https://sidiora-points-indexer-production.up.railway.app').replace(/\/+$/, ''),
}

// ── Embedded wallet REST API (the network-side custody + enforcement surface) ─
// Agents sign/send here; keys never leave the server. Auth = Supabase JWT.
export const WALLET_API = {
  base: (pick('PAXEER_WALLET_API', 'PAXNET_WALLET_API') ?? 'https://connect.paxportwallet.com').replace(/\/+$/, ''),
  supabaseUrl: (pick('PAXEER_SUPABASE_URL') ?? 'https://zezsqawedbikldiedlse.supabase.co').replace(/\/+$/, ''),
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
  WPAX9: { symbol: 'WPAX9', name: 'Wrapped PAX',   decimals: 18, address: '0xe5ccf339d1c89c7e6c6768b28507f78b861fc1de' },
  USDC:  { symbol: 'USDC',  name: 'USD Coin',      decimals: 6,  address: '0xf8850b62AE017c55be7f571BBad840b4f3DA7D49', stable: true },
  USDT:  { symbol: 'USDT',  name: 'Tether USD',    decimals: 6,  address: '0x5dfE06Ae465a39c442c45ed273c523BaC2d1f6a8', stable: true },
  USDL:  { symbol: 'USDL',  name: 'Liquidity USD', decimals: 6,  address: '0x7c69c84daAEe90B21eeCABDb8f0387897E9B7B37', stable: true },
  USID:  { symbol: 'USID',  name: 'USD Sidiora',   decimals: 18, address: '0x6C32c255EeBD6A72B56ee82454d7140020919652', stable: true },
  SID:   { symbol: 'SID',   name: 'Sidiora',       decimals: 6,  address: '0x86949e4CdB89496490890B67C9cfF63eD8efB4b1' },
  WETH:  { symbol: 'WETH',  name: 'Wrapped ETH',   decimals: 18, address: '0x5ba2f89F60f5805512A265bdFbB8984C85b4c9B7' },
  WBNB:  { symbol: 'WBNB',  name: 'Wrapped BNB',   decimals: 18, address: '0x2cE6495AF2F6cF20ea1b4d637dC2E882a0276F36' },
  WUNI:  { symbol: 'WUNI',  name: 'Wrapped UNI',   decimals: 18, address: '0x2235fB5dFe619d67FcA1F9a70BD2B6725b13aE50' },
  WSOL:  { symbol: 'WSOL',  name: 'Wrapped SOL',   decimals: 9,  address: '0x38416f047c53C6D295AfF15e2fD296B6C896E2d8' },
  WDOGE: { symbol: 'WDOGE', name: 'Wrapped DOGE',  decimals: 8,  address: '0x7Bf5FFa044DC7789Bb3d71EA91d84a918a53a1F5' },
  WBCH:  { symbol: 'WBCH',  name: 'Wrapped BCH',   decimals: 8,  address: '0x9CcD709EB3460Dfb3ba62Ed5f1987A707a7Fbd59' },
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

// ── Precompiles (EVM-native agent primitives). Verified in HyperPax-OS. ────
// 0x09xx = the agent-economy suite (v19_paxspot + v21agent upgrades).
export const PRECOMPILES = {
  orob:        '0x0000000000000000000000000000000000000901', // OROB resolver (oracle-relative pricing)
  clearing:    '0x0000000000000000000000000000000000000902', // BatchClearing (settlement)
  oracle:      '0x0000000000000000000000000000000000000903', // OracleAggregator (price feeds)
  pofq:        '0x0000000000000000000000000000000000000904', // Proof of Fill Quality (reputation)
  scheduler:   '0x0000000000000000000000000000000000000905', // Scheduler (deferred/recurring txs)
  streams:     '0x0000000000000000000000000000000000000906', // PaymentStreams (continuous pay)
  teeAttestor: '0x0000000000000000000000000000000000000907', // TEEAttestor (verifiable compute)
  eip712:      '0x0000000000000000000000000000000000000908', // EIP712Helper (typed-data hashing)
  staking:     '0x0000000000000000000000000000000000000800', // Cosmos staking
  bech32:      '0x0000000000000000000000000000000000000400', // bech32 <-> hex
  p256:        '0x0000000000000000000000000000000000000100', // secp256r1 (EIP-7212)
}

// ── Protocol contracts ─────────────────────────────────────────────────────
// `swap` = WIRED INTO THE WALLET (source of truth for swap execution).
// `hyperpaxDex` / `perps` / `sidioraFun` / `sidioraAg` = docs.paxeer.app.
export const CONTRACTS = {
  swap: {
    // PECOR V4 router layer (current). Primary swap entry the wallet uses.
    pecorRouter:    '0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa',
    oracleHub:      '0x18DA624C9C5Ff17612EC5fC0A5070611053A180f',
    priceAdapter:   '0x22C3D06F512ca59D9B523DB17ADB563fff68d065',
    sidioraAdapter: '0x64967B75a8295d50D024415510B11a15049713d1',
    vaultAdapter:   '0x66934c81c0E63b615264492aEB2988BC0f34b571',
    // PECOR V3 stack.
    pecorV3:        '0x1AB090064857063bBB935cAe2b0FD2fE62F0d63B',
    pecorQuoterV3:  '0x63b53724EC271799a7e2c9702072F12080F09e13',
    pecorOrders:    '0xE89a3E5dffEfbB7f8C9E9f597bbfd4F4ADE77404',
    pecorStopOrders:'0x49e2Fff129f9a351D94E3A25b2642Bfe37aCA912',
    pecorVault:     '0xDe5A8fc4396aE392957b547154B29b000D906a87',
    priceOracleV3:  '0xe7B20ef0bE7322D4B8d7E054e810b2209032750B',
    // Sidiora launchpad routing (wired; matches docs).
    sidioraRouter:   '0xB2D63300FE8b3508A83728e8f36B98e845eBD980',
    sidioraQuoter:   '0xeDb3B45E320A8ab2306Fa1C303742f2478fd3E0a',
    sidioraRegistry: '0x1F22f11325197fae71937598F6935cc4e9231970',
    // HLPMM v2 launchpad AMM.
    hlpmmRouter:  '0xaedb6bB0451F9CA908f884345dEf5c538ca63022',
    hlpmmQuoter:  '0x131928667BAB3081A3A47e429052617aF5530D87',
    hlpmmFactory: '0x41897edE845Ec558E73dAb28Db55e0b16C85df89',
  },
  // HyperPax DEX (network-operated v5 Adaptive Sigmoid AMM, EIP-2535 Diamond).
  hyperpaxDex: {
    diamond:         '0x9595a92d63884d2D9924e0002D45C34d717DB291',
    router:          '0x635aC031f7d26035FCc8b138b0835fec0cf6b8AA',
    quoter:          '0x2092D242Cc5d3673D1644128DBd4D199dE51266e',
    positionManager: '0x8f60EcD67Ef9aF953Dfc1a94F03C1D7e4363e092',
    orderManager:    '0xB6430A1A4373C14Fa359b242713fBeB4BF2559A4',
    eventEmitter:    '0x3FCa66c12B99e395619EE4d0aeabC2339F97E1FF',
  },
  // HyperPax Perps (synthetic perpetuals, 19-facet Diamond; events from Diamond).
  perps: {
    diamond:            '0xeA65FE02665852c615774A3041DFE6f00fb77537',
    userVaultImpl:      '0x4195155D92451a47bF76987315DaEE499f1D7352',
  },
  // Sidiora.fun launchpad (docs; same router/quoter as wired sidiora*).
  sidioraFun: {
    router:        '0xB2D63300FE8b3508A83728e8f36B98e845eBD980',
    quoter:        '0xeDb3B45E320A8ab2306Fa1C303742f2478fd3E0a',
    factory:       '0x322170E27d0c5Bd252337791fadED31dc4E85cA6',
    poolRegistry:  '0x1F22f11325197fae71937598F6935cc4e9231970',
    protocolConfig:'0x325e6Fb9c3505A35785365674089aEf8497C697B',
    eventEmitter:  '0x6679aF411d534de222C32ed0AF94C3BD67090672',
  },
  // Sidiora.ag / PECOR meta-aggregator (docs). Distinct from wired V4 router.
  sidioraAg: {
    pecorRouter: '0x5925FA311707C406D83FC76317a69bb1Ba263F32',
    oracleHub:   '0xED7620DC28759d55D89fF802E307Dd246d61D409',
    pecorQuoter: '0x4e643931fbb2df1B5965739B46CF70BCe622BD0a',
  },
}

// Limits / safety knobs (also referenced by the spend policy guard).
export const LIMITS = {
  httpTimeoutMs: Number(pick('PAXEER_HTTP_TIMEOUT_MS') ?? 20000),
  rpcTimeoutMs: Number(pick('PAXEER_RPC_TIMEOUT_MS') ?? 15000),
  maxResponseBytes: Number(pick('PAXEER_MAX_BYTES') ?? 1_000_000),
  // Per-call native PAX spend ceiling (wei). 0 = unlimited (rely on network policy).
  maxSpendWei: pick('PAXEER_MAX_SPEND_WEI') ?? '0',
}
