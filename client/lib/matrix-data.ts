/**
 * Centra AI — backend-agnostic data layer.
 *
 * Everything here is plain TypeScript types + mock fixtures. No fetching, no
 * SDK clients, no env vars, no infra. The UI components consume these types
 * through props and callbacks, so a backend team can wire real data by:
 *   1. Implementing functions that return these shapes.
 *   2. Passing the results (and `on*` handlers) into the components.
 *
 * Nothing in the UI imports a network client — only these types + mocks.
 */

/* -------------------------------------------------------------------------- */
/*  Core domain types                                                         */
/* -------------------------------------------------------------------------- */

export type RunStatus = 'queued' | 'running' | 'needs_input' | 'completed' | 'failed' | 'paused'

export type StageStatus = 'done' | 'active' | 'pending' | 'failed'

/** A single phase of an agent run. Centra AI runs are form-shaped, multi-step. */
export interface RunStage {
  id: string
  label: string
  status: StageStatus
  detail?: string
}

/** Autonomy controls how far the agent goes before checking in. */
export type Autonomy = 'supervised' | 'checkpoints' | 'full'

export interface Run {
  id: string
  /** The user's goal, in their words. The form input — not a chat message. */
  title: string
  status: RunStatus
  agentId: string
  toolIds: string[]
  autonomy: Autonomy
  /** 0–100 completion. */
  progress: number
  stages: RunStage[]
  createdAt: string
  completedAt?: string
  /** Wall-clock duration in ms once finished. */
  durationMs?: number
  /** Set once the run completes and emits a receipt. */
  receiptId?: string
}

/** The product is the receipt: a signed, replay-tested record of completion. */
export interface Receipt {
  id: string
  runId: string
  title: string
  signedAt: string
  /** Content hash of the run transcript + artifacts. */
  hash: string
  /** Detached signature over the hash. */
  signature: string
  replayVerified: boolean
  stepCount: number
  toolCallCount: number
  /** Provider token cost, surfaced for transparency. */
  costUsd: number
  artifacts: ReceiptArtifact[]
}

export interface ReceiptArtifact {
  id: string
  name: string
  kind: 'file' | 'message' | 'commit' | 'record' | 'link'
  summary: string
}

export interface Agent {
  id: string
  name: string
  role: string
  description: string
  status: 'active' | 'idle'
  runsCompleted: number
  successRate: number
  /** Backing skill URI (matrix://skill/<slug>@<v>) when this persona is
   *  sourced from the daemon's live skill catalogue. Sent as the
   *  `skill` field when dispatching a task so the daemon knows which
   *  skill to compile against. Absent on the static fallback roster. */
  skillUri?: string
}

export interface Tool {
  id: string
  name: string
  category: string
  description: string
  enabled: boolean
}

export interface MetricPoint {
  /** ISO date (day granularity). */
  date: string
  completed: number
  failed: number
}

export interface Stats {
  tasksCompleted: number
  completionRate: number
  activeNow: number
  timeReclaimedHrs: number
}

/** Runtime tier — shapes copy only; no infra is wired here. */
export type RuntimeMode = 'free' | 'cloud_lite' | 'byo'

/* -------------------------------------------------------------------------- */
/*  Model selection                                                           */
/* -------------------------------------------------------------------------- */

/** An LLM the agent runtime can be pointed at. Backend wires the real id. */
export interface Model {
  id: string
  /** Display name, e.g. "DeepSeek V4 Pro". */
  name: string
  /** Provider label, e.g. "Fireworks · DeepSeek". Display only. */
  provider: string
  /** One-line positioning to help the user choose. */
  blurb: string
  /** Relative cost indicator, 1 (cheapest) – 3 (priciest). */
  costTier: 1 | 2 | 3
  /** Relative speed indicator, 1 (slowest) – 3 (fastest). */
  speedTier: 1 | 2 | 3
  /** Context window in thousands of tokens. */
  contextK: number
  /** Whether this model is reachable on the current runtime tier. */
  available: boolean
}

/* -------------------------------------------------------------------------- */
/*  On-chain agent wallet                                                     */
/* -------------------------------------------------------------------------- */

export interface WalletNetwork {
  id: string
  name: string
  /** Short ticker, e.g. "ETH", "SOL". */
  symbol: string
  /** Single emoji-free glyph/initials shown in the avatar. */
  glyph: string
  /** Tailwind text color token-ish hex used only for the network dot. */
  accent: string
  testnet?: boolean
}

export interface WalletBalance {
  networkId: string
  /** Human token amount (already formatted to a number). */
  amount: number
  symbol: string
  /** USD value of the holding. */
  usd: number
}

export type TxDirection = 'in' | 'out'
export type TxStatus = 'confirmed' | 'pending' | 'failed'

export interface WalletTx {
  id: string
  networkId: string
  direction: TxDirection
  status: TxStatus
  /** What the agent did, in plain language. */
  label: string
  amount: number
  symbol: string
  usd: number
  /** Truncated counterparty address. */
  counterparty: string
  at: string
}

export interface WalletAccount {
  /** Full address; UI truncates for display. */
  address: string
  /** Networks this address is funded on. */
  networkIds: string[]
}

/* -------------------------------------------------------------------------- */
/*  Policies & limits                                                         */
/* -------------------------------------------------------------------------- */

export interface SpendLimits {
  /** Hard cap per single task, USD. */
  perTaskUsd: number
  /** Rolling daily cap, USD. */
  dailyUsd: number
  /** On-chain value the agent may move without approval, USD. */
  onchainPerTxUsd: number
}

export interface PolicyToggles {
  /** Require a human tap before any on-chain transaction. */
  approveOnchain: boolean
  /** Require approval before sending external messages/email. */
  approveExternalSend: boolean
  /** Allow the agent to install/use new tools mid-run. */
  allowNewTools: boolean
  /** Pause everything immediately (kill switch). */
  freezeAll: boolean
  /** Keep a signed receipt for every run. */
  alwaysReceipt: boolean
}

/* -------------------------------------------------------------------------- */
/*  Activity timeline (derived, friendly view of recent work)                 */
/* -------------------------------------------------------------------------- */

export type ActivityKind = 'completed' | 'dispatched' | 'needs_input' | 'onchain' | 'failed'

export interface ActivityItem {
  id: string
  kind: ActivityKind
  /** Plain-language description of what happened. */
  text: string
  /** Agent display name responsible. */
  agent: string
  /** "9:24 AM" style label. */
  time: string
}

export interface ActivityDay {
  /** "Today / Tuesday" style heading. */
  date: string
  items: ActivityItem[]
}

/* -------------------------------------------------------------------------- */
/*  Mock fixtures (replace with real data by passing props)                   */
/* -------------------------------------------------------------------------- */

export const agents: Agent[] = [
  {
    id: 'agt_research',
    name: 'Scout',
    role: 'Research & synthesis',
    description: 'Gathers sources, cross-checks facts, and returns a cited brief.',
    status: 'active',
    runsCompleted: 412,
    successRate: 0.97,
  },
  {
    id: 'agt_inbox',
    name: 'Courier',
    role: 'Inbox & scheduling',
    description: 'Triages messages, drafts replies, and books time across calendars.',
    status: 'active',
    runsCompleted: 1289,
    successRate: 0.94,
  },
  {
    id: 'agt_ops',
    name: 'Ledger',
    role: 'Spreadsheets & ops',
    description: 'Reconciles records, fills sheets, and produces a verifiable diff.',
    status: 'idle',
    runsCompleted: 631,
    successRate: 0.96,
  },
  {
    id: 'agt_builder',
    name: 'Forge',
    role: 'Docs & deliverables',
    description: 'Assembles documents, decks, and exports from your raw inputs.',
    status: 'idle',
    runsCompleted: 208,
    successRate: 0.93,
  },
]

export const tools: Tool[] = [
  {
    id: 'tool_web',
    name: 'Web search',
    category: 'Knowledge',
    description: 'Query the open web and read pages.',
    enabled: true,
  },
  {
    id: 'tool_files',
    name: 'Files',
    category: 'Workspace',
    description: 'Read and write files in your workspace.',
    enabled: true,
  },
  {
    id: 'tool_mail',
    name: 'Mail',
    category: 'Communication',
    description: 'Draft and send email on your behalf.',
    enabled: true,
  },
  {
    id: 'tool_calendar',
    name: 'Calendar',
    category: 'Communication',
    description: 'Read availability and create events.',
    enabled: true,
  },
  {
    id: 'tool_sheets',
    name: 'Sheets',
    category: 'Workspace',
    description: 'Read and edit structured spreadsheets.',
    enabled: false,
  },
  {
    id: 'tool_browser',
    name: 'Browser',
    category: 'Knowledge',
    description: 'Operate a headless browser for live tasks.',
    enabled: false,
  },
]

function stages(active: number, failedAt?: number): RunStage[] {
  const defs = [
    { id: 'plan', label: 'Plan', detail: 'Decompose the goal into steps' },
    { id: 'gather', label: 'Gather', detail: 'Collect context and inputs' },
    { id: 'execute', label: 'Execute', detail: 'Run the tools to do the work' },
    { id: 'verify', label: 'Verify', detail: 'Replay-test the result' },
    { id: 'sign', label: 'Sign', detail: 'Emit the signed receipt' },
  ]
  return defs.map((d, i) => {
    let status: StageStatus = 'pending'
    if (failedAt !== undefined && i === failedAt) status = 'failed'
    else if (i < active) status = 'done'
    else if (i === active) status = 'active'
    return { ...d, status }
  })
}

export const runs: Run[] = [
  {
    id: 'run_8fa21',
    title: 'Compile a competitive teardown of the top 5 note-taking apps',
    status: 'running',
    agentId: 'agt_research',
    toolIds: ['tool_web', 'tool_files'],
    autonomy: 'checkpoints',
    progress: 64,
    stages: stages(2),
    createdAt: new Date(Date.now() - 1000 * 60 * 6).toISOString(),
  },
  {
    id: 'run_7cd90',
    title: 'Triage my inbox and draft replies to anything from a customer',
    status: 'running',
    agentId: 'agt_inbox',
    toolIds: ['tool_mail', 'tool_calendar'],
    autonomy: 'supervised',
    progress: 38,
    stages: stages(1),
    createdAt: new Date(Date.now() - 1000 * 60 * 2).toISOString(),
  },
  {
    id: 'run_6bb14',
    title: 'Reconcile the Q2 vendor spreadsheet against the receipts folder',
    status: 'needs_input',
    agentId: 'agt_ops',
    toolIds: ['tool_sheets', 'tool_files'],
    autonomy: 'checkpoints',
    progress: 50,
    stages: stages(2),
    createdAt: new Date(Date.now() - 1000 * 60 * 18).toISOString(),
  },
  {
    id: 'run_5aa02',
    title: 'Draft a launch announcement and a 6-tweet thread from the spec',
    status: 'completed',
    agentId: 'agt_builder',
    toolIds: ['tool_files'],
    autonomy: 'full',
    progress: 100,
    stages: stages(5),
    createdAt: new Date(Date.now() - 1000 * 60 * 60 * 3).toISOString(),
    completedAt: new Date(Date.now() - 1000 * 60 * 60 * 2.6).toISOString(),
    durationMs: 1000 * 60 * 22,
    receiptId: 'rcpt_5aa02',
  },
  {
    id: 'run_4f9d7',
    title: 'Find every broken link on the docs site and list the fixes',
    status: 'completed',
    agentId: 'agt_research',
    toolIds: ['tool_web', 'tool_browser'],
    autonomy: 'full',
    progress: 100,
    stages: stages(5),
    createdAt: new Date(Date.now() - 1000 * 60 * 60 * 9).toISOString(),
    completedAt: new Date(Date.now() - 1000 * 60 * 60 * 8.7).toISOString(),
    durationMs: 1000 * 60 * 14,
    receiptId: 'rcpt_4f9d7',
  },
  {
    id: 'run_3e8c5',
    title: 'Book three intro calls with the leads tagged this week',
    status: 'completed',
    agentId: 'agt_inbox',
    toolIds: ['tool_calendar', 'tool_mail'],
    autonomy: 'checkpoints',
    progress: 100,
    stages: stages(5),
    createdAt: new Date(Date.now() - 1000 * 60 * 60 * 26).toISOString(),
    completedAt: new Date(Date.now() - 1000 * 60 * 60 * 25.5).toISOString(),
    durationMs: 1000 * 60 * 31,
    receiptId: 'rcpt_3e8c5',
  },
  {
    id: 'run_2d7b3',
    title: 'Export the design tokens to JSON and open a pull request',
    status: 'failed',
    agentId: 'agt_builder',
    toolIds: ['tool_files'],
    autonomy: 'full',
    progress: 72,
    stages: stages(2, 2),
    createdAt: new Date(Date.now() - 1000 * 60 * 60 * 30).toISOString(),
  },
]

export const receipts: Receipt[] = [
  {
    id: 'rcpt_5aa02',
    runId: 'run_5aa02',
    title: 'Draft a launch announcement and a 6-tweet thread from the spec',
    signedAt: new Date(Date.now() - 1000 * 60 * 60 * 2.6).toISOString(),
    hash: 'b1946ac92492d2347c6235b4d2611184',
    signature: 'ed25519:9f8c…a3e1',
    replayVerified: true,
    stepCount: 14,
    toolCallCount: 6,
    costUsd: 0.42,
    artifacts: [
      {
        id: 'a1',
        name: 'announcement.md',
        kind: 'file',
        summary: '480-word launch post, ready to publish',
      },
      { id: 'a2', name: 'thread.md', kind: 'file', summary: '6 tweets, under 280 chars each' },
    ],
  },
  {
    id: 'rcpt_4f9d7',
    runId: 'run_4f9d7',
    title: 'Find every broken link on the docs site and list the fixes',
    signedAt: new Date(Date.now() - 1000 * 60 * 60 * 8.7).toISOString(),
    hash: '98f13708210194c475687be6106a3b84',
    signature: 'ed25519:2b7d…f04c',
    replayVerified: true,
    stepCount: 22,
    toolCallCount: 41,
    costUsd: 0.88,
    artifacts: [
      {
        id: 'a3',
        name: 'broken-links.csv',
        kind: 'file',
        summary: '17 broken links with suggested targets',
      },
      {
        id: 'a4',
        name: 'summary',
        kind: 'message',
        summary: '9 internal, 8 external — 3 are quick wins',
      },
    ],
  },
  {
    id: 'rcpt_3e8c5',
    runId: 'run_3e8c5',
    title: 'Book three intro calls with the leads tagged this week',
    signedAt: new Date(Date.now() - 1000 * 60 * 60 * 25.5).toISOString(),
    hash: '7d793037a0760186574b0282f2f435e7',
    signature: 'ed25519:c19a…7be2',
    replayVerified: true,
    stepCount: 11,
    toolCallCount: 9,
    costUsd: 0.31,
    artifacts: [
      {
        id: 'a5',
        name: '3 calendar holds',
        kind: 'record',
        summary: 'Tue 10:00, Wed 14:30, Thu 09:00',
      },
      {
        id: 'a6',
        name: '3 confirmations sent',
        kind: 'message',
        summary: 'Personalized email to each lead',
      },
    ],
  },
]

export const throughput: MetricPoint[] = (() => {
  const base = [9, 12, 7, 14, 11, 16, 13, 18, 15, 21, 19, 24, 22, 27]
  const fails = [1, 0, 2, 1, 1, 0, 2, 1, 0, 2, 1, 1, 0, 1]
  return base.map((c, i) => {
    const d = new Date()
    d.setDate(d.getDate() - (base.length - 1 - i))
    return { date: d.toISOString().slice(0, 10), completed: c, failed: fails[i] }
  })
})()

export const stats: Stats = {
  tasksCompleted: 1284,
  completionRate: 96,
  activeNow: 2,
  timeReclaimedHrs: 318,
}

/**
 * The live Centra AI model catalogue, sourced from `models/models.kvx` (the
 * Fireworks AI serverless tier the runtime actually dispatches against).
 * Tiers are derived from each model's listed pricing + context window.
 */
export const models: Model[] = [
  {
    id: 'mdl_deepseek_pro',
    name: 'MTX•DeepSeek V4 Pro',
    provider: 'Fireworks · DeepSeek',
    blurb: 'Deepest reasoning with a 1M-token window for high-stakes work.',
    costTier: 3,
    speedTier: 1,
    contextK: 1000,
    available: true,
  },
  {
    id: 'mdl_deepseek_flash',
    name: 'MTX•DeepSeek V4 Flash',
    provider: 'Fireworks · DeepSeek',
    blurb: 'Ultra-cheap 1M-context workhorse for long, routine jobs.',
    costTier: 1,
    speedTier: 3,
    contextK: 1000,
    available: true,
  },
  {
    id: 'mdl_kimi_k26',
    name: 'MTX•Kimi K2.6',
    provider: 'Fireworks · Moonshot',
    blurb: 'Vision + tool-calling generalist with a 262K window.',
    costTier: 2,
    speedTier: 2,
    contextK: 262,
    available: true,
  },
  {
    id: 'mdl_glm_51',
    name: 'MTX•GLM 5.1',
    provider: 'Fireworks · Z.ai',
    blurb: 'Balanced, tunable model for everyday agent work.',
    costTier: 2,
    speedTier: 2,
    contextK: 202,
    available: true,
  },
  {
    id: 'mdl_minimax_m27',
    name: 'MTX•MiniMax M2.7',
    provider: 'Fireworks · MiniMax',
    blurb: 'Fast and inexpensive for well-scoped tasks.',
    costTier: 1,
    speedTier: 3,
    contextK: 196,
    available: true,
  },
  {
    id: 'mdl_qwen36_plus',
    name: 'MTX•Qwen3.6 Plus',
    provider: 'Fireworks · Qwen',
    blurb: 'Multimodal generalist with strong tool fluency.',
    costTier: 2,
    speedTier: 2,
    contextK: 256,
    available: true,
  },
  {
    id: 'mdl_gpt_oss_120b',
    name: 'MTX•GPT-OSS 120B',
    provider: 'Fireworks · OpenAI (open weights)',
    blurb: 'Open-weights OpenAI model — cheap, with low-cost cached input.',
    costTier: 1,
    speedTier: 2,
    contextK: 131,
    available: true,
  },
]

// Matches the executor default (Q13: DeepSeek-V4-Pro on Fireworks).
export const DEFAULT_MODEL_ID = 'mdl_deepseek_pro'

export const networks: WalletNetwork[] = [
  { id: 'net_eth', name: 'Ethereum', symbol: 'ETH', glyph: 'Ξ', accent: '#7c8cf8' },
  { id: 'net_base', name: 'Base', symbol: 'ETH', glyph: 'B', accent: '#3aa0ff' },
  { id: 'net_arb', name: 'Arbitrum', symbol: 'ETH', glyph: 'A', accent: '#4fb0e0' },
  { id: 'net_op', name: 'Optimism', symbol: 'ETH', glyph: 'O', accent: '#f06a6a' },
  { id: 'net_sol', name: 'Solana', symbol: 'SOL', glyph: 'S', accent: '#52d6a8' },
  { id: 'net_poly', name: 'Polygon', symbol: 'MATIC', glyph: 'P', accent: '#8b9bf4' },
  {
    id: 'net_base_sep',
    name: 'Base Sepolia',
    symbol: 'ETH',
    glyph: 'b',
    accent: '#7a8699',
    testnet: true,
  },
]

export const DEFAULT_NETWORK_ID = 'net_base'

export const walletAccount: WalletAccount = {
  address: '0x7a3F9c2E1b4D8a06C5e7F21b9aD3c84B2eF10D6a',
  networkIds: ['net_eth', 'net_base', 'net_arb', 'net_sol'],
}

export const balances: WalletBalance[] = [
  { networkId: 'net_base', amount: 1.842, symbol: 'ETH', usd: 6234.12 },
  { networkId: 'net_eth', amount: 0.514, symbol: 'ETH', usd: 1738.4 },
  { networkId: 'net_arb', amount: 0.96, symbol: 'ETH', usd: 3247.0 },
  { networkId: 'net_sol', amount: 41.3, symbol: 'SOL', usd: 6402.5 },
]

export const transactions: WalletTx[] = [
  {
    id: 'tx_01',
    networkId: 'net_base',
    direction: 'out',
    status: 'confirmed',
    label: 'Paid API credits invoice',
    amount: 0.08,
    symbol: 'ETH',
    usd: 271.2,
    counterparty: '0x91c…3aF2',
    at: new Date(Date.now() - 1000 * 60 * 22).toISOString(),
  },
  {
    id: 'tx_02',
    networkId: 'net_base',
    direction: 'in',
    status: 'confirmed',
    label: 'Treasury top-up',
    amount: 1.0,
    symbol: 'ETH',
    usd: 3390.0,
    counterparty: '0x4De…91bC',
    at: new Date(Date.now() - 1000 * 60 * 60 * 5).toISOString(),
  },
  {
    id: 'tx_03',
    networkId: 'net_sol',
    direction: 'out',
    status: 'pending',
    label: 'Settled data provider',
    amount: 6.5,
    symbol: 'SOL',
    usd: 1007.5,
    counterparty: '8xKq…Lm2P',
    at: new Date(Date.now() - 1000 * 60 * 4).toISOString(),
  },
  {
    id: 'tx_04',
    networkId: 'net_arb',
    direction: 'out',
    status: 'confirmed',
    label: 'Bridged to Base',
    amount: 0.25,
    symbol: 'ETH',
    usd: 847.5,
    counterparty: '0xBr1…dGe0',
    at: new Date(Date.now() - 1000 * 60 * 60 * 26).toISOString(),
  },
  {
    id: 'tx_05',
    networkId: 'net_eth',
    direction: 'out',
    status: 'failed',
    label: 'Gas spike — reverted',
    amount: 0.0,
    symbol: 'ETH',
    usd: 0,
    counterparty: '0x00a…d3e9',
    at: new Date(Date.now() - 1000 * 60 * 60 * 30).toISOString(),
  },
]

export const defaultSpendLimits: SpendLimits = {
  perTaskUsd: 5,
  dailyUsd: 50,
  onchainPerTxUsd: 250,
}

export const defaultPolicyToggles: PolicyToggles = {
  approveOnchain: true,
  approveExternalSend: true,
  allowNewTools: false,
  freezeAll: false,
  alwaysReceipt: true,
}

export const activity: ActivityDay[] = [
  {
    date: 'Today / Tuesday',
    items: [
      {
        id: 'ac_1',
        kind: 'completed',
        text: 'Drafted the launch announcement and a 6-tweet thread',
        agent: 'Forge',
        time: '9:24 AM',
      },
      {
        id: 'ac_2',
        kind: 'onchain',
        text: 'Paid the API credits invoice — 0.08 ETH on Base',
        agent: 'Ledger',
        time: '9:02 AM',
      },
      {
        id: 'ac_3',
        kind: 'needs_input',
        text: 'Paused the vendor reconciliation — needs your call on 2 rows',
        agent: 'Ledger',
        time: '8:41 AM',
      },
      {
        id: 'ac_4',
        kind: 'dispatched',
        text: 'Started triaging your inbox and drafting customer replies',
        agent: 'Courier',
        time: '8:30 AM',
      },
    ],
  },
  {
    date: 'Yesterday / Monday',
    items: [
      {
        id: 'ac_5',
        kind: 'completed',
        text: 'Listed every broken link on the docs site with fixes',
        agent: 'Scout',
        time: '6:12 PM',
      },
      {
        id: 'ac_6',
        kind: 'completed',
        text: "Booked three intro calls with this week's leads",
        agent: 'Courier',
        time: '2:48 PM',
      },
      {
        id: 'ac_7',
        kind: 'onchain',
        text: 'Topped up the agent treasury — received 1.0 ETH',
        agent: 'Ledger',
        time: '11:05 AM',
      },
      {
        id: 'ac_8',
        kind: 'failed',
        text: 'Token export reverted on a gas spike — will retry',
        agent: 'Forge',
        time: '9:30 AM',
      },
    ],
  },
  {
    date: 'Sunday',
    items: [
      {
        id: 'ac_9',
        kind: 'completed',
        text: 'Compiled a competitor teardown of 5 note-taking apps',
        agent: 'Scout',
        time: '4:20 PM',
      },
      {
        id: 'ac_10',
        kind: 'completed',
        text: "Reconciled last week's expenses into the sheet",
        agent: 'Ledger',
        time: '1:15 PM',
      },
      {
        id: 'ac_11',
        kind: 'onchain',
        text: 'Bridged 0.25 ETH from Arbitrum to Base',
        agent: 'Ledger',
        time: '10:02 AM',
      },
    ],
  },
]

/* -------------------------------------------------------------------------- */
/*  Helpers                                                                   */
/* -------------------------------------------------------------------------- */

export function getAgent(id: string): Agent | undefined {
  return agents.find((a) => a.id === id)
}

export function getTool(id: string): Tool | undefined {
  return tools.find((t) => t.id === id)
}

export function getReceiptForRun(runId: string): Receipt | undefined {
  return receipts.find((r) => r.runId === runId)
}

export function getModel(id: string): Model | undefined {
  return models.find((m) => m.id === id)
}

export function getNetwork(id: string): WalletNetwork | undefined {
  return networks.find((n) => n.id === id)
}

export function shortAddress(addr: string, lead = 6, tail = 4): string {
  if (addr.length <= lead + tail) return addr
  return `${addr.slice(0, lead)}…${addr.slice(-tail)}`
}

export function formatUsd(n: number): string {
  return n.toLocaleString('en-US', {
    style: 'currency',
    currency: 'USD',
    maximumFractionDigits: n < 100 ? 2 : 0,
  })
}

export const totalUsd = (bs: WalletBalance[] = balances): number =>
  bs.reduce((sum, b) => sum + b.usd, 0)

export const STATUS_LABEL: Record<RunStatus, string> = {
  queued: 'Queued',
  running: 'Running',
  needs_input: 'Needs input',
  completed: 'Completed',
  failed: 'Failed',
  paused: 'Paused',
}

export const AUTONOMY_LABEL: Record<Autonomy, string> = {
  supervised: 'Supervised',
  checkpoints: 'Checkpoints',
  full: 'Full autonomy',
}

/** Build the canonical 5-stage pipeline at a given active index. */
export function buildStages(active: number, failedAt?: number): RunStage[] {
  return stages(active, failedAt)
}

export function formatDuration(ms?: number): string {
  if (!ms) return '—'
  const m = Math.round(ms / 60000)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

export function relativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const m = Math.round(diff / 60000)
  if (m < 1) return 'just now'
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  return `${d}d ago`
}
