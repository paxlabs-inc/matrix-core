/**
 * Query key factory — a single source of truth for every TanStack Query
 * cache key the app uses. Keying through these helpers eliminates a
 * whole class of cache-invalidation bugs (mismatched keys between a
 * mutation and the queries it should invalidate) and makes
 * `revalidateTag`-equivalent sweeps trivial.
 */
export const qk = {
  health: () => ['health'] as const,
  me: () => ['me'] as const,
  settings: () => ['settings'] as const,
  agentsManifest: () => ['agents', 'manifest'] as const,
  agentRoster: () => ['agents', 'roster'] as const,
  toolsCatalog: () => ['tools'] as const,
  skills: () => ['skills'] as const,
  snapshots: () => ['snapshots'] as const,
  runsAll: () => ['runs', 'all'] as const,
  runsPage: (cursor?: string, state?: string) => ['runs', 'page', { cursor, state }] as const,
  run: (id: string) => ['runs', 'one', id] as const,
  runAttest: (id: string) => ['runs', 'attest', id] as const,
  runReplay: (id: string) => ['runs', 'replay', id] as const,
  dashboardSnapshot: () => ['runs', 'dashboard'] as const,
  asyncJob: (id: string) => ['async', 'job', id] as const,
  // Wallet — primary embedded wallet + owned agent wallets + on-chain reads.
  primaryWallet: () => ['wallet', 'primary'] as const,
  agentWallets: () => ['wallet', 'agents'] as const,
  agentWallet: (did: string) => ['wallet', 'agent', did] as const,
  agentActivity: (did: string) => ['wallet', 'agent', did, 'activity'] as const,
  walletPortfolio: (address: string) => ['wallet', 'portfolio', address.toLowerCase()] as const,
  walletTxs: (address: string) => ['wallet', 'txs', address.toLowerCase()] as const,
  // LayerX — read-only public account view for the agent's DID.
  layerxAccount: (did: string) => ['layerx', 'account', did] as const,
  // LayerX explorer — the full public read surface behind /explorer.
  layerxInfo: () => ['layerx', 'info'] as const,
  layerxSupply: () => ['layerx', 'supply'] as const,
  layerxTransfers: (did?: string) => ['layerx', 'transfers', { did: did ?? '' }] as const,
  layerxReceipt: (seq: number) => ['layerx', 'receipt', seq] as const,
  layerxBatches: () => ['layerx', 'batches'] as const,
  layerxBatch: (id: string) => ['layerx', 'batch', id] as const,
  // Automatrix — proactive opt-in, opportunity queue, and completion inbox.
  automatrixSettings: () => ['automatrix', 'settings'] as const,
  automatrixQueue: () => ['automatrix', 'queue'] as const,
  automatrixInbox: () => ['automatrix', 'inbox'] as const,
  // Morning brief — schedule/opt-in and the saved personalization profile.
  briefSettings: () => ['brief', 'settings'] as const,
  personalization: () => ['personalization'] as const,
  telegram: () => ['integrations', 'telegram'] as const,
  machineMail: () => ['integrations', 'machinemail'] as const,
  memoryConsent: () => ['memory', 'consent'] as const,
  // Markets — every key is scoped by instrument so two panels watching the
  // same symbol share one entry, mirroring the router-side cache.
  financeHome: (symbols: string[]) => ['finance', 'home', symbols.join(',')] as const,
  financeSymbol: (symbol: string, range: string) =>
    ['finance', 'symbol', symbol.toUpperCase(), range] as const,
  financeQuote: (symbol: string) => ['finance', 'quote', symbol.toUpperCase()] as const,
  financeQuotes: (symbols: string[]) =>
    ['finance', 'quotes', symbols.map((s) => s.toUpperCase()).join(',')] as const,
  financeSeries: (symbol: string, range: string) =>
    ['finance', 'series', symbol.toUpperCase(), range] as const,
  financeBoard: (assetClass: string) => ['finance', 'board', assetClass] as const,
  financeNews: (scope: string, symbols?: string[]) =>
    ['finance', 'news', scope, symbols?.join(',') ?? ''] as const,
  financeSearch: (query: string) => ['finance', 'search', query.toLowerCase()] as const,
  financeEarnings: (symbol: string) => ['finance', 'earnings', symbol.toUpperCase()] as const,
  financeDividends: (symbol: string) => ['finance', 'dividends', symbol.toUpperCase()] as const,
  financeStatus: () => ['finance', 'status'] as const,
  financeResearch: (runId: string) => ['finance', 'research', runId] as const,
  workforceSession: () => ['workforce', 'session'] as const,
  workforceResource: (resource: string) => ['workforce', 'resource', resource] as const,
} as const
