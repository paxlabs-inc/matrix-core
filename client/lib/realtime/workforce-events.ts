import { workforceEndpoint } from '@/lib/env'
import type { WorkforceLifecycleEvent } from '@/lib/api/workforce'

export type WorkforceStreamState = 'connecting' | 'live' | 'reconnecting' | 'closed'

export interface WorkforceStreamOptions {
  after?: number
  onEvent: (event: WorkforceLifecycleEvent) => void
  onState: (state: WorkforceStreamState) => void
  onResync: (after: number) => void
}

export function workforceStreamNeedsAccessToken(endpoint: string, currentOrigin: string): boolean {
  return new URL(endpoint, currentOrigin).origin !== new URL(currentOrigin).origin
}

export function startWorkforceEventStream(options: WorkforceStreamOptions): { close: () => void } {
  const controller = new AbortController()
  let cursor = options.after ?? 0
  let closed = false

  const run = async () => {
    let attempt = 0
    while (!closed) {
      options.onState(attempt === 0 ? 'connecting' : 'reconnecting')
      try {
        const endpoint = workforceEndpoint('/v1/workforce/events/stream')
        const url = new URL(endpoint, window.location.origin)
        let token: string | null = null
        if (workforceStreamNeedsAccessToken(endpoint, window.location.origin)) {
          const { getAccessToken } = await import('@/lib/auth/session')
          token = await getAccessToken()
        }
        if (cursor > 0) url.searchParams.set('after', String(cursor))
        const response = await fetch(url, {
          headers: {
            accept: 'text/event-stream',
            ...(token ? { authorization: `Bearer ${token}` } : {}),
          },
          cache: 'no-store',
          credentials: 'same-origin',
          signal: controller.signal,
        })
        if (!response.ok || !response.body) throw new Error(`Workforce stream: ${response.status}`)
        options.onState('live')
        attempt = 0
        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''
        while (!closed) {
          const { value, done } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })
          let end = buffer.search(/\r?\n\r?\n/)
          while (end >= 0) {
            const frame = buffer.slice(0, end)
            const terminator = buffer.slice(end).startsWith('\r\n\r\n') ? 4 : 2
            buffer = buffer.slice(end + terminator)
            const parsed = parseWorkforceFrame(frame)
            if (parsed.kind === 'event' && parsed.event.cursor > cursor) {
              cursor = parsed.event.cursor
              options.onEvent(parsed.event)
            } else if (parsed.kind === 'resync') {
              options.onResync(parsed.after)
            }
            end = buffer.search(/\r?\n\r?\n/)
          }
        }
      } catch {
        if (closed || controller.signal.aborted) break
      }
      attempt += 1
      if (attempt > 10) break
      await new Promise((resolve) =>
        setTimeout(resolve, Math.min(10_000, 300 * 2 ** attempt) * Math.random()),
      )
    }
    options.onState('closed')
  }
  void run()
  return {
    close: () => {
      if (closed) return
      closed = true
      controller.abort()
    },
  }
}

export function parseWorkforceFrame(
  frame: string,
):
  | { kind: 'event'; event: WorkforceLifecycleEvent }
  | { kind: 'resync'; after: number }
  | { kind: 'ignored' } {
  let eventType = ''
  let data = ''
  for (const line of frame.split(/\r?\n/)) {
    if (line.startsWith('event:')) eventType = line.slice(6).trim()
    if (line.startsWith('data:')) data += line.slice(5).trim()
  }
  if (!data) return { kind: 'ignored' }
  try {
    const parsed = JSON.parse(data) as Record<string, unknown>
    if (eventType === 'resync_required') {
      return { kind: 'resync', after: Number(parsed.after ?? 0) }
    }
    if (typeof parsed.cursor !== 'number' || typeof parsed.event_type !== 'string') {
      return { kind: 'ignored' }
    }
    return { kind: 'event', event: parsed as unknown as WorkforceLifecycleEvent }
  } catch {
    return { kind: 'ignored' }
  }
}
