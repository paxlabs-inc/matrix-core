/**
 * SSE connection hub — multiplexes subscribers onto a single physical
 * connection per unique stream key (path + replayPath + sinceSeq).
 *
 * Without this, opening a run transcript while the chat hook tails the
 * same intent_id would spin up two identical SSE connections.
 */
import { startSSE, type SSEClientOptions, type SSEUpdate } from '@/lib/realtime/sse'

type Subscriber = (u: SSEUpdate) => void

interface Entry {
  close: () => void
  subscribers: Set<Subscriber>
}

const entries = new Map<string, Entry>()

function streamKey(opts: Pick<SSEClientOptions, 'path' | 'replayPath' | 'sinceSeq'>): string {
  return `${opts.path}|${opts.replayPath ?? ''}|${opts.sinceSeq ?? 0}`
}

function fanOut(entry: Entry, u: SSEUpdate) {
  for (const sub of entry.subscribers) {
    try {
      sub(u)
    } catch {
      /* subscriber owns error handling */
    }
  }
}

/** Subscribe to an SSE stream. Returns an unsubscribe/close handle. */
export function subscribeSSEHub(
  opts: Omit<SSEClientOptions, 'onUpdate'>,
  onUpdate: Subscriber,
): { close: () => void } {
  const key = streamKey(opts)
  let entry = entries.get(key)
  if (!entry) {
    const subscribers = new Set<Subscriber>()
    const handle = startSSE({
      ...opts,
      onUpdate: (u) => {
        const e = entries.get(key)
        if (e) fanOut(e, u)
      },
    })
    entry = {
      subscribers,
      close: () => {
        handle.close()
        entries.delete(key)
      },
    }
    entries.set(key, entry)
  }
  entry.subscribers.add(onUpdate)
  return {
    close: () => {
      const e = entries.get(key)
      if (!e) return
      e.subscribers.delete(onUpdate)
      if (e.subscribers.size === 0) e.close()
    },
  }
}

/** Test helper — tear down every hub connection. */
export function _resetSSEHubForTests() {
  for (const e of entries.values()) e.close()
  entries.clear()
}
