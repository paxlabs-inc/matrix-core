/**
 * LayerX read-only client — the public explorer lane of layerxd.
 *
 * The UI only READS the agent's LayerX account (GET /v1/account/{did} is a
 * public route: zero balances + empty history for unknown DIDs, uniform
 * envelope). All value-moving LayerX lanes are DID-signed by the agent
 * daemon and are deliberately NOT reachable from the browser.
 */

/**
 * Base path of the LayerX API. layerxd sets no CORS headers, so the browser
 * never calls its origin directly: NEXT_PUBLIC_LAYERX_API only signals that
 * the surface is configured, and the actual requests go to the same-origin
 * /layerx-api/* path that next.config.mjs rewrites (server-side proxy) to
 * the layerxd origin. Unset means the surface is not configured.
 */
export function layerxApiBase(): string | null {
  const raw = process.env.NEXT_PUBLIC_LAYERX_API
  if (!raw || raw.trim() === '') return null
  return '/layerx-api'
}

/** One row of the account's transfer history (public explorer view). */
export interface LayerXTransfer {
  seq: number
  batch_id?: string
  from_did: string
  to_did: string
  amount_usdx: string
  tier: string
  leaf_hash: string
  batch_root?: string
  anchor_tx?: string
  settled: boolean
  ts: string
}

/** GET /v1/account/{did} — the public account view. USDX decimal strings. */
export interface LayerXAccount {
  did: string
  evm_address?: string
  balance_usdx: string
  escrow_usdx: string
  history: LayerXTransfer[]
}

interface LayerXEnvelope<T> {
  ok: boolean
  data?: T
  error?: { code: string; message: string }
}

export class LayerXError extends Error {
  readonly code: string
  readonly status?: number

  constructor(message: string, code: string, status?: number) {
    super(message)
    this.name = 'LayerXError'
    this.code = code
    this.status = status
  }
}

/** Read the agent's LayerX account. Throws LayerXError when unconfigured
 *  or on any transport / envelope failure. */
export async function getLayerXAccount(did: string, signal?: AbortSignal): Promise<LayerXAccount> {
  const base = layerxApiBase()
  if (!base) {
    throw new LayerXError('LayerX API is not configured', 'NOT_CONFIGURED')
  }
  let res: Response
  try {
    res = await fetch(`${base}/v1/account/${encodeURIComponent(did)}`, {
      headers: { Accept: 'application/json' },
      signal,
    })
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    throw new LayerXError(msg, 'NETWORK')
  }
  let payload: LayerXEnvelope<LayerXAccount> | null = null
  try {
    payload = (await res.json()) as LayerXEnvelope<LayerXAccount>
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
