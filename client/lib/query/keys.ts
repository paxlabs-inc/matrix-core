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
  // Automatrix — proactive opt-in, opportunity queue, and completion inbox.
  automatrixSettings: () => ['automatrix', 'settings'] as const,
  automatrixQueue: () => ['automatrix', 'queue'] as const,
  automatrixInbox: () => ['automatrix', 'inbox'] as const,
} as const
