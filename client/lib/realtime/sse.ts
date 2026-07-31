/**
 * Resilient Server-Sent-Events client.
 *
 * Why not the platform's `EventSource`? Two reasons:
 *
 *   1. EventSource cannot attach an `Authorization` header. Our SSE
 *      endpoint is JWT-protected through the matrix-router so we MUST
 *      authenticate every connection.
 *   2. EventSource has no backpressure / visibility-pause / sequence-
 *      gap semantics; we need all three for a real-time-trading-grade
 *      UI as called out in the production-readiness checklist.
 *
 * This client uses `fetch()` with a streaming response body, parses
 * the SSE wire format manually, and layers on:
 *
 *   - Auto-reconnect with full-jitter exponential backoff and a hard
 *     attempt cap.
 *   - Heartbeat watchdog (no event for 30s → reconnect).
 *   - Page-Visibility-aware pause: when the tab is hidden we keep the
 *     connection but stop dispatching events (frame-rate aware).
 *   - Sequence-gap detection — every event the daemon emits carries a
 *     monotonic `seq`; on a gap we surface `{ kind: 'gap', from, to }`
 *     so the caller can trigger a snapshot resync.
 *   - Backpressure-friendly delivery: events are coalesced through
 *     `requestAnimationFrame` so a flood doesn't pin the main thread.
 */
import { routerOrigin } from '@/lib/env'
import { getAccessToken } from '@/lib/auth/session'
import type { SSEEvent } from '@/lib/api/types'

export type SSEUpdate =
  | { kind: 'event'; event: SSEEvent }
  | { kind: 'open' }
  | { kind: 'reconnecting'; attempt: number }
  | { kind: 'gap'; from: number; to: number }
  | { kind: 'error'; error: Error }
  | { kind: 'closed' }

export interface SSEClientOptions {
  /** Path under the router origin, e.g. "/events?intent_id=xyz". */
  path: string
  /** Optional finite replay path drained BEFORE the live tail, e.g.
   *  "/events/replay/<intentId>". Re-emits the persisted historical
   *  transcript (so completed runs aren't empty) and seeds `lastSeq`,
   *  then the live stream tails from there with ?since_seq=. A missing
   *  transcript (404) is ignored — we just go straight to live. */
  replayPath?: string
  /** Optional starting seq for backfill — passed as ?since_seq=. */
  sinceSeq?: number
  /** Listener invoked for every update (events + lifecycle). The
   *  caller owns coalescing/throttling beyond what we already do. */
  onUpdate: (u: SSEUpdate) => void
  /** Heartbeat timeout in ms. Default 30s. The daemon emits a comment
   *  ping every 15s so a 30s window is safe. */
  heartbeatMs?: number
  /** Max reconnection attempts before surfacing `closed`. Default 10. */
  maxReconnects?: number
}

interface SSEClientHandle {
  /** Tear the connection down. Idempotent. */
  close: () => void
}

/** Drop coalesced events beyond this cap so a flood cannot grow unbounded. */
const MAX_EVENT_QUEUE = 200

export function startSSE(opts: SSEClientOptions): SSEClientHandle {
  const ctrl = new AbortController()
  const heartbeatMs = opts.heartbeatMs ?? 30_000
  const maxReconnects = opts.maxReconnects ?? 10
  let lastSeq = opts.sinceSeq ?? 0
  let attempts = 0
  let closed = false
  let raf = 0
  const queue: SSEEvent[] = []
  let lastBeat = Date.now()
  let hbTimer: ReturnType<typeof setTimeout> | null = null

  function dispatch(u: SSEUpdate) {
    if (!closed) opts.onUpdate(u)
  }

  function scheduleFlush() {
    if (raf || closed) return
    raf = requestAnimationFrame(() => {
      raf = 0
      // Drop coalesced events older than what we've already
      // dispatched (reconnect with new sinceSeq could otherwise
      // re-emit the entire backlog).
      const tail = queue.splice(0, queue.length)
      for (const ev of tail) {
        if (ev.seq && ev.seq <= lastSeq) continue
        if (ev.seq && lastSeq && ev.seq > lastSeq + 1) {
          dispatch({ kind: 'gap', from: lastSeq, to: ev.seq })
        }
        if (ev.seq) lastSeq = ev.seq
        dispatch({ kind: 'event', event: ev })
      }
    })
  }

  function pushEvent(ev: SSEEvent) {
    queue.push(ev)
    // Backpressure: when hidden or flooded, keep only the newest events.
    if (queue.length > MAX_EVENT_QUEUE) {
      queue.splice(0, queue.length - MAX_EVENT_QUEUE)
    }
    lastBeat = Date.now()
    if (typeof document === 'undefined' || document.visibilityState !== 'hidden') {
      scheduleFlush()
    }
    // When hidden we keep accumulating but never schedule rAF — the
    // visibility listener flushes on resume.
  }

  function startHeartbeat() {
    stopHeartbeat()
    hbTimer = setInterval(
      () => {
        if (Date.now() - lastBeat > heartbeatMs) {
          // Force the in-flight reader to abort + trigger reconnect.
          ctrl.abort(new DOMException('heartbeat-timeout', 'TimeoutError'))
        }
      },
      Math.max(2_000, Math.floor(heartbeatMs / 3)),
    )
  }

  function stopHeartbeat() {
    if (hbTimer) clearInterval(hbTimer)
    hbTimer = null
  }

  function onVisibility() {
    if (typeof document === 'undefined') return
    if (document.visibilityState === 'visible') {
      scheduleFlush()
    }
  }
  if (typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', onVisibility)
  }

  // Drain the finite replay stream first (if any) so historical events
  // are rendered before we tail live. Best-effort: a 404 (no transcript
  // yet) or any replay error is swallowed and we proceed to the live tail.
  async function drainReplay(): Promise<void> {
    if (closed || !opts.replayPath) return
    const token = await getAccessToken()
    const headers: Record<string, string> = {
      accept: 'text/event-stream',
      'cache-control': 'no-cache',
    }
    if (token) headers.authorization = `Bearer ${token}`
    const resp = await fetch(routerOrigin() + opts.replayPath, {
      method: 'GET',
      headers,
      signal: ctrl.signal,
      credentials: 'omit',
    })
    if (!resp.ok || !resp.body) return
    dispatch({ kind: 'open' })
    const reader = resp.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    while (true) {
      const { value, done } = await reader.read()
      if (done) return
      buf += decoder.decode(value, { stream: true })
      let idx: number
      while ((idx = indexOfFrameEnd(buf)) !== -1) {
        const frame = buf.slice(0, idx)
        buf = buf.slice(idx + frameTerminatorLen(buf, idx))
        const event = parseFrame(frame)
        if (event) pushEvent(event)
      }
    }
  }

  async function connect() {
    if (opts.replayPath) {
      try {
        await drainReplay()
      } catch {
        // Replay is optional; fall through to the live tail regardless.
      }
    }
    while (!closed) {
      try {
        await once()
        attempts = 0
        // Server closed cleanly; treat as a transient and reconnect
        // unless we've been told to stop.
        if (closed) return
      } catch (e) {
        if (closed) return
        attempts++
        if (attempts > maxReconnects) {
          dispatch({ kind: 'error', error: e instanceof Error ? e : new Error(String(e)) })
          dispatch({ kind: 'closed' })
          stopHeartbeat()
          return
        }
        dispatch({ kind: 'reconnecting', attempt: attempts })
      }
      // Backoff with full jitter, cap at 10s.
      const cap = 10_000
      const base = 500
      const delay = Math.floor(Math.random() * Math.min(cap, base * 2 ** attempts))
      await sleep(delay)
    }
  }

  async function once(): Promise<void> {
    const token = await getAccessToken()
    const url = new URL(routerOrigin() + opts.path)
    if (lastSeq) url.searchParams.set('since_seq', String(lastSeq + 1))
    const headers: Record<string, string> = {
      accept: 'text/event-stream',
      'cache-control': 'no-cache',
    }
    if (token) headers.authorization = `Bearer ${token}`

    const resp = await fetch(url.toString(), {
      method: 'GET',
      headers,
      signal: ctrl.signal,
      credentials: 'omit',
    })
    if (!resp.ok || !resp.body) {
      throw new Error(`sse: ${resp.status} ${resp.statusText}`)
    }
    dispatch({ kind: 'open' })
    lastBeat = Date.now()
    startHeartbeat()

    const reader = resp.body.getReader()
    const decoder = new TextDecoder('utf-8')
    let buf = ''
    while (true) {
      const { value, done } = await reader.read()
      if (done) return
      buf += decoder.decode(value, { stream: true })
      // SSE frames are separated by a blank line. Process one frame
      // at a time so partial events stay buffered.
      let idx: number
      while ((idx = indexOfFrameEnd(buf)) !== -1) {
        const frame = buf.slice(0, idx)
        buf = buf.slice(idx + frameTerminatorLen(buf, idx))
        const event = parseFrame(frame)
        if (event) pushEvent(event)
        else lastBeat = Date.now() // comment / ping
      }
    }
  }

  connect()

  return {
    close() {
      if (closed) return
      closed = true
      ctrl.abort(new DOMException('client-close', 'AbortError'))
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', onVisibility)
      }
      stopHeartbeat()
      if (raf) cancelAnimationFrame(raf)
      dispatch({ kind: 'closed' })
    },
  }
}

function sleep(ms: number) {
  return new Promise<void>((r) => setTimeout(r, ms))
}

/** Locate the end of the current SSE frame (LF LF or CRLF CRLF). */
function indexOfFrameEnd(buf: string): number {
  const lf = buf.indexOf('\n\n')
  const crlf = buf.indexOf('\r\n\r\n')
  if (lf === -1) return crlf
  if (crlf === -1) return lf
  return Math.min(lf, crlf)
}

function frameTerminatorLen(buf: string, idx: number): number {
  return buf.startsWith('\r\n\r\n', idx) ? 4 : 2
}

function parseFrame(frame: string): SSEEvent | null {
  const lines = frame.split(/\r?\n/)
  const dataParts: string[] = []
  for (const line of lines) {
    if (line.startsWith(':')) continue // comment / ping
    if (line.startsWith('data:')) dataParts.push(line.slice(5).trimStart())
  }
  if (dataParts.length === 0) return null
  const json = dataParts.join('\n')
  try {
    const parsed = JSON.parse(json) as SSEEvent
    if (typeof parsed !== 'object' || !parsed) return null
    return parsed
  } catch {
    return null
  }
}
