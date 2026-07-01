/**
 * Events resource — wraps the resilient SSE client into a focused
 * helper used by per-intent transcript views and the global firehose
 * (when enabled).
 */
import { subscribeSSEHub } from '@/lib/realtime/sse-hub'
import type { SSEUpdate } from '@/lib/realtime/sse'

export interface SubscribeOptions {
  intentId?: string
  phase?: string
  sinceSeq?: number
  /** Backfill the persisted transcript via /events/replay/<intentId>
   *  before tailing live. Requires intentId. Without it, a stream opened
   *  on an already-finished (or mid-flight) run shows nothing because the
   *  live firehose only carries events emitted after the subscription. */
  replay?: boolean
  onUpdate: (u: SSEUpdate) => void
}

export function subscribeEvents(opts: SubscribeOptions) {
  const qs = new URLSearchParams()
  if (opts.intentId) qs.set('intent_id', opts.intentId)
  if (opts.phase) qs.set('phase', opts.phase)
  const replayPath =
    opts.replay && opts.intentId ? `/events/replay/${encodeURIComponent(opts.intentId)}` : undefined
  return subscribeSSEHub(
    {
      path: `/events${qs.toString() ? `?${qs.toString()}` : ''}`,
      replayPath,
      sinceSeq: opts.sinceSeq,
    },
    opts.onUpdate,
  )
}
