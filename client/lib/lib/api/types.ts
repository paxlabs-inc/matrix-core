/**
 * Backend wire types — the exact JSON shapes returned by the Matrix
 * Daemon (executor/cmd/mcl-execute/daemon_*.go), proxied through the
 * Matrix Router.
 *
 * These are intentionally separated from the UI types in
 * `lib/matrix-data.ts` so the mapper layer (`lib/api/mappers.ts`) can
 * evolve independently of the UI contract. NEVER let an UI component
 * import a wire type directly — always go through the mapper.
 */

/** GET /healthz response body (router + daemon share the same shape). */
export interface HealthzResponse {
  status: 'ok' | string
  version?: string
}

/** GET /me — actor identity surfaced by the daemon. */
export interface MeResponse {
  user_id?: string
  user_uri?: string
  did?: string
  agent?: string
  skill?: string
  cortex_root?: string
  build?: string
}

/** GET /settings — daemon runtime configuration snapshot. */
export interface SettingsResponse {
  skill?: string
  compiler_model?: string
  executor_model?: string
  embedder_model?: string
  gateway_url?: string
  free_tier?: boolean
  data_dir?: string
}

/** Wire shape mirroring `intentSummary` in
 *  executor/cmd/mcl-execute/daemon_intent_index.go. */
export interface IntentSummary {
  intent_id: string
  intent_hash?: string
  state:
    | 'drafting'
    | 'proposed'
    | 'clarifying'
    | 'accepted'
    | 'executing'
    | 'completed'
    | 'failed'
    | 'cancelled'
  verb?: string
  skill?: string
  prose?: string
  started_at?: string
  ended_at?: string
  duration_ms?: number
  node_count?: number
  walk_errors?: number
  lifecycle?: string
  envelope_count: number
  has_attest: boolean
  has_fail: boolean
  has_cancel: boolean
  post_replay_root?: string
  pre_replay_root?: string
  /** Set when the intent was started via /messages/async — the
   *  registry overlays the async lifecycle on top of the journal. */
  async_status?: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'
  pending_gates?: number
}

/** Paginated list envelope used by /intents and /memory/search. */
export interface PagedResponse<T> {
  items: T[]
  next_cursor?: string
  total?: number
}

/** GET /intents/:id full envelope chain. */
export interface IntentChainResponse {
  intent_id: string
  path: string
  envelopes: IntentEnvelopeFile[]
}

export interface IntentEnvelopeFile {
  seq: number
  filename: string
  kind: string
  envelope: unknown
}

/** GET /intents/:id/lifecycle item. */
export interface LifecycleTransition {
  seq: number
  at: string
  kind: string
  from: string
  to: string
  envelope_id?: string
}

/** GET /intents/:id/attestation — decoded intent.attest body. */
export interface IntentAttestation {
  intent_id?: string
  intent_hash?: string
  pre_replay_root?: string
  post_replay_root?: string
  signature?: string
  signed_at?: string
  signer_did?: string
  step_count?: number
  tool_call_count?: number
  cost_usd?: number
  artifacts?: Array<{
    id?: string
    name?: string
    kind?: string
    summary?: string
    uri?: string
  }>
  // Tolerate unknown fields rather than failing the type check; the
  // mapper picks out what it understands.
  [key: string]: unknown
}

/** GET /intents/:id/replay-roots. */
export interface ReplayRoots {
  pre_replay_root?: string
  post_replay_root?: string
  match: boolean
}

/** POST /messages/async response (202). */
export interface AsyncStartResponse {
  intent_id: string
  status: 'queued'
  events_url: string
  poll_url: string
  summary_url: string
  transcript: string
}

/** GET /messages/async/:id terminal poll. */
export interface AsyncJob {
  intent_id: string
  status: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'
  request: { prose: string; verb?: string; skill?: string; goal_id?: string }
  created_at: string
  started_at?: string
  ended_at?: string
  result?: {
    intent_id: string
    intent_hash: string
    status: 'completed' | 'failed'
    walk_errors: number
    node_count: number
    duration_ms: number
    error?: string
  }
  error?: string
}

/** GET /events SSE event row (mirrors transcript JSONL). */
export interface SSEEvent {
  seq: number
  ts: string
  phase: string
  type: string
  fields?: Record<string, unknown>
}

/**
 * Neo "show-the-work" SSE event field shapes.
 *
 * The Neo conversational server (neo/internal/server) surfaces tool activity
 * on the SAME /events stream as chat.assistant turns, via an agent
 * ToolObserver. These describe the `fields` payload of those event rows so the
 * client can render source/snippet cards and activity chips instead of
 * dropping them. NEVER imported by a UI component directly — the useChat hook
 * maps them into the UI-facing `NeoTask` shape.
 */

/** A single web-search result row inside a `tool.search` event. */
export interface ToolSearchCard {
  title?: string
  url?: string
  snippet?: string
  published?: string
}

/** `tool.search` — emitted for web_search / web_news with rich results. */
export interface ToolSearchFields {
  intent_id?: string
  conversation_id?: string
  tool?: string
  provider?: string
  query?: string
  answer?: string
  results?: ToolSearchCard[]
}

/** `tool.activity` — a compact "started using <tool>" chip. */
export interface ToolActivityFields {
  intent_id?: string
  conversation_id?: string
  tool?: string
  label?: string
}

/** `tool.result` — a compact "did X" line for any non-search tool. */
export interface ToolResultFields {
  intent_id?: string
  conversation_id?: string
  tool?: string
  label?: string
  ok?: boolean
  summary?: string
}

/** GET /agents/manifest — paxeer-net + skills + tools manifest. */
export interface AgentsManifest {
  default_skill?: string
  servers?: Array<{ id: string; command: string; args?: string[] }>
  tools?: Array<{ uri: string; description?: string; server?: string }>
  skills?: string[]
}

/** GET /tools — flattened list of registered MCP tools. */
export interface ToolEntry {
  uri: string
  name: string
  server?: string
  description?: string
  enabled?: boolean
}

/** GET /skills — installed skill catalog. */
export interface SkillEntry {
  uri: string
  slug: string
  version: string
  display?: string
  description?: string
}

/** GET /metrics — Prometheus text body; we only parse what we need. */
export interface ParsedMetricsLite {
  intentsCompleted: number
  intentsFailed: number
  ssEventsTotal: number
}

/** Standardised error envelope returned by every daemon route on
 *  4xx/5xx. The router maps its own errors into the same shape. The
 *  runtime form is the `ApiError` *class* exported from
 *  `lib/api/client.ts`; this interface is the structural contract. */
export interface WireError {
  status: number
  message: string
  /** Original wire body for debugging — kept opaque. */
  body?: unknown
  /** Set when the error is retryable (transient — 503, 504, network). */
  retryable: boolean
}
