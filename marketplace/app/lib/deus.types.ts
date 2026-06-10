/**
 * Wire types for the Deus `/v1` REST API.
 *
 * Mirrors the real Go handlers (`deus/internal/server/*`, `deus/pkg/types/api.go`,
 * `deus/pkg/manifest`) — NOT the design docs. Pure type declarations so client
 * components can `import type` from here without pulling server-only code.
 */

// ─── Errors ───────────────────────────────────────────────────────────────
export interface ApiErrorBody {
  error: string;
  message: string;
  detail?: Record<string, unknown>;
}

// ─── Discover ───────────────────────────────────────────────────────────────
export interface DiscoverOperation {
  name: string;
  price_wei: string;
  unit: string;
}

export interface DiscoverResult {
  id: string;
  slug: string;
  display_name: string;
  summary: string;
  kind: string;
  quality_score?: string;
  uptime_bps?: number;
  score: number;
  operations: DiscoverOperation[];
}

export interface DiscoverResponse {
  results: DiscoverResult[];
  next_cursor: string | null;
}

export interface DiscoverFilters {
  kind?: "data" | "agent";
  max_price_wei?: string;
  min_uptime_bps?: string;
  confidential?: string;
}

export interface DiscoverRequest {
  query: string;
  filters?: Record<string, string>;
  limit?: number;
}

// ─── Manifest / Service detail ──────────────────────────────────────────────
export interface ManifestOperation {
  name: string;
  method?: string;
  input_schema?: Record<string, unknown>;
  output_schema?: Record<string, unknown>;
  timeout_ms?: number;
  max_response_bytes?: number;
}

export interface ManifestPricing {
  operation: string;
  model: string;
  unit: string;
  price_wei: string;
  min_charge_wei: string;
}

export interface ManifestEndpoint {
  proxy_url?: string;
}

export interface ManifestSLA {
  target_uptime_bps?: number;
  p99_latency_ms?: number;
}

export interface Manifest {
  schema_version?: string;
  slug: string;
  kind: string;
  display_name: string;
  summary: string;
  description?: string;
  tags?: string[];
  owner?: string;
  payout_address?: string;
  mode: string;
  confidential?: boolean;
  operations: ManifestOperation[];
  pricing: ManifestPricing[];
  endpoint?: ManifestEndpoint;
  sla?: ManifestSLA;
  attestation?: unknown;
}

export interface ServiceResponse {
  id: string;
  slug: string;
  status: string;
  kind: string;
  mode: string;
  display_name: string;
  summary: string;
  manifest_hash: string;
  chain_id?: number;
  manifest?: Manifest & Record<string, unknown>;
}

// ─── Quote ──────────────────────────────────────────────────────────────────
export interface EIP712Sig {
  domain: string;
  digest: string;
  signature: string;
}

export interface QuoteRequest {
  operation: string;
  estimated_units: string;
}

export interface QuoteResponse {
  quote_id: string;
  service_id: string;
  operation: string;
  unit_price_wei: string;
  max_units: string;
  max_total_wei: string;
  pricing_version: number;
  expires_at: string;
  eip712: EIP712Sig;
}

// ─── Invoke ──────────────────────────────────────────────────────────────────
export interface PaymentRail {
  rail: "direct" | "net" | "stream";
  stream_id?: string;
}

export interface InvokeRequest {
  operation: string;
  args: Record<string, unknown>;
  quote_id: string;
  payment: PaymentRail;
  idempotency_key?: string;
  caller_voucher_sig?: string;
}

export interface ReceiptSummary {
  digest: string;
  gateway_sig: string;
  runner_sig?: string | null;
  attestation?: unknown;
}

export interface VoucherSummary {
  channel_id: string;
  cumulative_wei: string;
  nonce: number;
  last_receipt_hash: string;
  digest: string;
  needs_signature: boolean;
  voucher_id?: string;
}

export interface InvokeResponse {
  invocation_id: string;
  outcome: string;
  result: Record<string, unknown>;
  charged_wei: string;
  latency_ms: number;
  receipt: ReceiptSummary;
  voucher?: VoucherSummary | null;
}

// ─── Create / publish ─────────────────────────────────────────────────────────
export interface CreateServiceRequest {
  manifest: Record<string, unknown>;
}

export interface ValidationResult {
  ok: boolean;
  warnings: string[];
}

export interface CreateServiceResponse {
  id: string;
  slug: string;
  status: string;
  manifest_hash: string;
  validation: ValidationResult;
}

export interface PublishServiceResponse {
  id: string;
  chain_id: number;
  status: string;
  manifest_hash: string;
  tx_hash: string;
}

// ─── Hosting ──────────────────────────────────────────────────────────────────
export interface UploadArtifactResponse {
  artifact_key: string;
  url?: string;
}

export interface DeployServiceRequest {
  artifact_key: string;
  runtime?: string;
  always_warm?: boolean;
  region?: string;
}

export interface DeployServiceResponse {
  deployment_id: string;
  status: string;
  exec_endpoint?: string;
  runtime: string;
}

export interface DeploymentResponse {
  id: string;
  service_id: string;
  status: string;
  runtime: string;
  exec_endpoint?: string;
  always_warm: boolean;
}

// ─── Catalog (real Go shape: handlers_discovery.go + types.CatalogResponse) ──
export interface CatalogItem {
  id: string;
  slug: string;
  kind: string;
  mode?: string;
  display_name: string;
  summary: string;
  status: string;
  manifest_hash?: string;
  quality_score?: string;
  uptime_bps?: number;
  // Emitted by the Go handler from the stored manifest (headline pricing +
  // tags); also synthesized client-side on the discover-adapted path.
  price_wei?: string;
  unit?: string;
  tags?: string[];
}

/** GET /v1/catalog — `{services,total,limit,offset}` per deus/pkg/types/api.go. */
export interface CatalogResponse {
  services: CatalogItem[];
  total: number;
  limit: number;
  offset: number;
}

// ─── Dashboard endpoints ──────────────────────────────────────────────────────
// Backed by the real Go handlers (deus/internal/server/handlers_dashboard.go):
// /v1/me, /v1/me/spend, /v1/me/services, /v1/me/earnings, and the per-service
// logs/analytics/payout/pause/delist routes. The local mock mirrors them.

export interface MeResponse {
  did: string;
  wallet?: string;
  /** Not emitted by the Go backend; populated from the Supabase session. */
  email?: string;
  display_name?: string;
}

export interface MyService {
  id: string;
  slug: string;
  display_name: string;
  status: string;
  kind: string;
  mode: string;
  invocations: number;
  revenue_wei: string;
  uptime_bps?: number;
  quality_score?: string;
}

export interface AnalyticsPoint {
  date: string;
  invocations: number;
  revenue_wei: string;
  avg_latency_ms: number;
  success_rate: number;
}

export interface ServiceAnalytics {
  service_id: string;
  total_invocations: number;
  total_revenue_wei: string;
  avg_latency_ms: number;
  success_rate: number;
  uptime_bps: number;
  series: AnalyticsPoint[];
  top_operations: { operation: string; invocations: number; revenue_wei: string }[];
}

export interface DeploymentLogLine {
  ts: string;
  level: "info" | "warn" | "error" | "debug";
  message: string;
}

export interface Settlement {
  id: string;
  window_start: string;
  window_end: string;
  amount_wei: string;
  status: string;
  tx_hash?: string;
}

export interface EarningsResponse {
  total_earned_wei: string;
  pending_wei: string;
  available_wei: string;
  payout_address?: string;
  settlements: Settlement[];
}

export interface SpendEntry {
  service_id: string;
  display_name: string;
  invocations: number;
  total_wei: string;
}

export interface SpendResponse {
  total_spent_wei: string;
  entries: SpendEntry[];
}

export type ServiceStatus = "draft" | "active" | "paused" | "delisted";

// ─── Developer auth (SIWE, EIP-4361) ────────────────────────────────────────

export interface DeveloperNonceResponse {
  nonce: string;
  expires_at: string;
}

export interface DeveloperAuthResponse {
  wallet: string;
  token: string;
  expires_at: string;
}
