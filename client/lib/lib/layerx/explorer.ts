/**
 * LayerX explorer client — the full public read surface of layerxd, used by
 * the /explorer pages. Same transport posture as lib/layerx/client.ts: the
 * browser only ever fetches the same-origin /layerx-api/* path (a Next
 * rewrite proxies to the layerxd origin; layerxd sets no CORS headers).
 */

import { LayerXError, layerxApiBase, type LayerXTransfer } from './client'

export type { LayerXTransfer }

/** GET /v1/info — sequencer identity + on-chain wiring. */
export interface LayerXInfo {
  service: string
  version: string
  chain_id: number
  vault_address?: string
  anchor_address?: string
  usdl_address?: string
  dex_router?: string
  reserve_asset: string
  sequencer_pubkey: string
  window_seconds: number
  micro_threshold_usdx: string
  micro_per_usdx: number
  chain_configured: boolean
  transport_auth_required: boolean
}

/** GET /v1/supply — the reserve proof (circulating vs on-chain USDL). */
export interface LayerXSupply {
  circulating_usdx: string
  reserve_usdl?: string
  drift_usdx?: string
  fully_reserved: boolean
  reserve_known: boolean
  accounts: number
  transfers: number
}

/** Public view of a settlement batch. */
export interface LayerXBatch {
  id: string
  root: string
  status: string
  anchor_tx?: string
  transfer_count: number
  window_start: string
  window_end: string
  created_at: string
}

/** GET /v1/batches (offset-paginated). */
export interface LayerXBatchesResponse {
  batches: LayerXBatch[]
  limit: number
  offset: number
  count: number
}

/** GET /v1/transfers (keyset-paginated; next_before = cursor for older). */
export interface LayerXTransfersResponse {
  transfers: LayerXTransfer[]
  did?: string
  limit: number
  next_before?: number
  count: number
}

/** GET /v1/receipt/{seq} — the signed, Merkle-anchored transfer proof. */
export interface LayerXReceipt {
  seq: number
  batch_id?: string
  from_did: string
  to_did: string
  amount_usdx: string
  tier: string
  ts: string
  leaf_hash: string
  sequencer_sig: string
  sequencer_pubkey: string
  batch_root?: string
  inclusion_path?: string[]
  anchor_tx?: string
  settled: boolean
}

/** GET /v1/anchor/{root} — root -> settlement batch / anchor tx lookup. */
export interface LayerXAnchor {
  root: string
  batch_id: string
  status: string
  anchor_tx?: string
  anchored: boolean
}

interface LayerXEnvelope<T> {
  ok: boolean
  data?: T
  error?: { code: string; message: string }
}

/** Uniform envelope-aware GET against the proxied layerxd surface. */
async function layerxGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  const base = layerxApiBase()
  if (!base) {
    throw new LayerXError('LayerX API is not configured', 'NOT_CONFIGURED')
  }
  let res: Response
  try {
    res = await fetch(`${base}${path}`, {
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    throw new LayerXError(msg, 'NETWORK')
  }
  let payload: LayerXEnvelope<T> | null = null
  try {
    payload = (await res.json()) as LayerXEnvelope<T>
  } catch {
    payload = null
  }
  if (!res.ok || !payload?.ok || !payload.data) {
    throw new LayerXError(
      payload?.error?.message ?? `request failed: ${res.status}`,
      payload?.error?.code ?? `HTTP_${res.status}`,
      res.status,
    )
  }
  return payload.data
}

export function getLayerXInfo(signal?: AbortSignal): Promise<LayerXInfo> {
  return layerxGet<LayerXInfo>('/v1/info', signal)
}

export function getLayerXSupply(signal?: AbortSignal): Promise<LayerXSupply> {
  return layerxGet<LayerXSupply>('/v1/supply', signal)
}

export function getLayerXBatches(
  limit = 25,
  offset = 0,
  signal?: AbortSignal,
): Promise<LayerXBatchesResponse> {
  return layerxGet<LayerXBatchesResponse>(`/v1/batches?limit=${limit}&offset=${offset}`, signal)
}

export function getLayerXBatch(id: string, signal?: AbortSignal): Promise<LayerXBatch> {
  return layerxGet<LayerXBatch>(`/v1/batch/${encodeURIComponent(id)}`, signal)
}

export function getLayerXAnchor(root: string, signal?: AbortSignal): Promise<LayerXAnchor> {
  const clean = root.trim().replace(/^0x/i, '').toLowerCase()
  return layerxGet<LayerXAnchor>(`/v1/anchor/${encodeURIComponent(clean)}`, signal)
}

export function getLayerXReceipt(seq: number, signal?: AbortSignal): Promise<LayerXReceipt> {
  return layerxGet<LayerXReceipt>(`/v1/receipt/${seq}`, signal)
}

export function getLayerXTransfers(
  opts: { did?: string; limit?: number; before?: number } = {},
  signal?: AbortSignal,
): Promise<LayerXTransfersResponse> {
  const params = new URLSearchParams()
  params.set('limit', String(opts.limit ?? 25))
  if (opts.did) params.set('did', opts.did)
  if (opts.before && opts.before > 0) params.set('before', String(opts.before))
  return layerxGet<LayerXTransfersResponse>(`/v1/transfers?${params.toString()}`, signal)
}

/**
 * URL of the live SSE stream (transfer + anchor events). Null when the
 * LayerX surface is not configured.
 */
export function layerxStreamUrl(since?: number): string | null {
  const base = layerxApiBase()
  if (!base) return null
  return since && since > 0 ? `${base}/v1/stream?since=${since}` : `${base}/v1/stream`
}
