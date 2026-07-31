'use client'

/**
 * useRunEvents — subscribe to the daemon's SSE event stream filtered
 * to one intent_id. Surfaces the resilient connection's lifecycle so
 * the UI can show a "reconnecting" badge when the stream blips.
 */
import { useEffect, useState } from 'react'
import { subscribeEvents } from '@/lib/api/events'
import type { SSEUpdate } from '@/lib/realtime/sse'
import type { SSEEvent } from '@/lib/api/types'

export interface RunStreamState {
  events: SSEEvent[]
  connection: 'connecting' | 'open' | 'reconnecting' | 'closed' | 'error'
  reconnectAttempt: number
  lastError: string | null
}

const HISTORY_CAP = 500

export function useRunEvents(intentId: string | null): RunStreamState {
  const [state, setState] = useState<RunStreamState>({
    events: [],
    connection: 'connecting',
    reconnectAttempt: 0,
    lastError: null,
  })

  useEffect(() => {
    if (!intentId) {
      setState((s) => ({ ...s, connection: 'closed' }))
      return
    }
    setState({ events: [], connection: 'connecting', reconnectAttempt: 0, lastError: null })
    const handle = subscribeEvents({
      intentId,
      // Backfill the persisted transcript first, then tail live. Without
      // this a transcript opened on an already-completed run streams
      // nothing (the live firehose only carries post-subscription events)
      // and sits forever on "waiting for activity".
      replay: true,
      onUpdate: (u: SSEUpdate) => {
        switch (u.kind) {
          case 'open':
            setState((s) => ({ ...s, connection: 'open', lastError: null }))
            break
          case 'event':
            setState((s) => {
              const next = s.events.length >= HISTORY_CAP ? s.events.slice(1) : s.events
              return { ...s, events: [...next, u.event] }
            })
            break
          case 'reconnecting':
            setState((s) => ({ ...s, connection: 'reconnecting', reconnectAttempt: u.attempt }))
            break
          case 'gap':
            // Surface as a synthetic event so the consumer can render
            // a "snapshot reconciled" notice without re-implementing
            // gap detection.
            setState((s) => ({
              ...s,
              events: [
                ...s.events,
                {
                  seq: u.to,
                  ts: new Date().toISOString(),
                  phase: 'sse',
                  type: 'sse.gap',
                  fields: { from: u.from, to: u.to },
                },
              ],
            }))
            break
          case 'error':
            setState((s) => ({ ...s, connection: 'error', lastError: u.error.message }))
            break
          case 'closed':
            setState((s) => ({ ...s, connection: 'closed' }))
            break
        }
      },
    })
    return () => handle.close()
  }, [intentId])

  return state
}
