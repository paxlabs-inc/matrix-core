import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { createElement, type ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ConversationRecord, ConversationSummary } from '@/lib/api/conversations'
import type { SSEUpdate } from '@/lib/realtime/sse'

/* -------------------------------------------------------------------------- */
/*  F2 — explicit "still working / reconnecting" UI state on useChat          */
/* -------------------------------------------------------------------------- */
/*  These tests pin the hook half of NEO-UX-FIXES.kvx [item.F2]:              */
/*    (1) `resuming=true` while F1's live_run resume path is mid-replay,      */
/*        then false after the first real event lands.                        */
/*    (2) a non-terminal stream `error` does NOT immediately flip phase to    */
/*        'idle' — instead `connectionRetrying=true` is exposed (visible      */
/*        "Connection lost — retrying…") and a bounded re-subscribe runs.    */
/*                                                                            */
/*  Mocks ONLY the network boundary (the api modules). The hook's internal    */
/*  reducer, callbacks, and types are exercised for real with real types.     */

const conversationsApi = {
  listConversations: vi.fn(),
  getConversation: vi.fn(),
}
vi.mock('@/lib/api/conversations', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api/conversations')>()
  return {
    ...actual,
    listConversations: (signal?: AbortSignal) => conversationsApi.listConversations(signal),
    getConversation: (id: string, signal?: AbortSignal) =>
      conversationsApi.getConversation(id, signal),
    // F3 — durable-workspace fetch on settled reopen; empty here (hermetic).
    getConversationTrace: () => Promise.resolve({ intent_id: '', live_run: '', events: [] }),
  }
})

// Capture the onUpdate callback so each test can push synthetic SSE updates.
const eventsApi = {
  subscribeEvents: vi.fn(),
  lastOnUpdate: null as ((u: SSEUpdate) => void) | null,
  lastIntentId: '',
  lastReplay: false as boolean | undefined,
  closeMock: vi.fn(),
}
vi.mock('@/lib/api/events', () => ({
  subscribeEvents: (opts: {
    intentId: string
    replay?: boolean
    onUpdate: (u: SSEUpdate) => void
  }) => eventsApi.subscribeEvents(opts),
}))

const runsApi = {
  getAsyncJob: vi.fn(),
  answerGate: vi.fn(),
  answerAsk: vi.fn(),
  cancelRun: vi.fn(),
}
vi.mock('@/lib/api/runs', () => ({
  getAsyncJob: (intentId: string) => runsApi.getAsyncJob(intentId),
  answerGate: (...args: unknown[]) => runsApi.answerGate(...args),
  answerAsk: (...args: unknown[]) => runsApi.answerAsk(...args),
  cancelRun: (...args: unknown[]) => runsApi.cancelRun(...args),
}))

vi.mock('@/lib/api/chat', () => ({ sendChat: vi.fn() }))

vi.mock('next-intl', () => ({
  useTranslations: () => (key: string, vars?: Record<string, unknown>) => {
    if (vars && 'reason' in vars) return `${key}:${String(vars.reason)}`
    return key
  },
}))

vi.mock('sonner', () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}))

import { useChat } from '@/hooks/api/useChat'

function queryWrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return function QueryWrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client }, children)
  }
}

const LIVE_SUMMARY: ConversationSummary = {
  conversation_id: 'conv_live',
  title: 'live thread',
  preview: 'working',
  turn_count: 1,
  updated: '2026-06-24T00:01:00Z',
}

function liveRecord(): ConversationRecord {
  return {
    conversation_id: 'conv_live',
    title: 'live thread',
    updated: '2026-06-24T00:01:00Z',
    live_run: 'neo_inflight',
    turns: [
      {
        role: 'user',
        text: 'do something long',
        intent_id: 'neo_inflight',
        ts: '2026-06-24T00:01:00Z',
      },
    ],
  }
}

beforeEach(() => {
  conversationsApi.listConversations.mockReset()
  conversationsApi.getConversation.mockReset()
  eventsApi.subscribeEvents.mockReset()
  eventsApi.lastOnUpdate = null
  eventsApi.lastIntentId = ''
  eventsApi.lastReplay = undefined
  eventsApi.closeMock = vi.fn()
  eventsApi.subscribeEvents.mockImplementation(
    (opts: { intentId: string; replay?: boolean; onUpdate: (u: SSEUpdate) => void }) => {
      eventsApi.lastOnUpdate = opts.onUpdate
      eventsApi.lastIntentId = opts.intentId
      eventsApi.lastReplay = opts.replay
      return { close: eventsApi.closeMock }
    },
  )
  runsApi.getAsyncJob.mockReset()
  runsApi.answerGate.mockReset()
  runsApi.answerAsk.mockReset()
  runsApi.cancelRun.mockReset()
})

afterEach(() => {
  vi.clearAllMocks()
  vi.useRealTimers()
})

describe('useChat — F2 explicit resuming / connection-retrying state', () => {
  it('useChat.exposesResumingState — `resuming` is true while F1 reattach is mid-replay, then false after the first event lands', async () => {
    conversationsApi.listConversations.mockResolvedValue([LIVE_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(liveRecord())

    const { result } = renderHook(() => useChat(), { wrapper: queryWrapper() })
    await vi.waitFor(() => {
      expect(conversationsApi.listConversations).toHaveBeenCalled()
    })
    act(() => {
      result.current.selectConversation('conv_live')
    })

    // Selecting the live conversation subscribes with replay:true and asserts
    // the visible resuming state.
    await waitFor(() => {
      expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    })
    expect(eventsApi.lastIntentId).toBe('neo_inflight')
    expect(eventsApi.lastReplay).toBe(true)

    await waitFor(() => {
      expect(result.current.resuming).toBe(true)
    })
    // The hook is NOT idle — phase is already 'working' (the seeded task).
    expect(result.current.phase).toBe('working')

    // Push the first real event over the reconnected stream; resuming clears.
    act(() => {
      eventsApi.lastOnUpdate?.({
        kind: 'event',
        event: {
          seq: 7,
          ts: '2026-06-24T00:01:02Z',
          phase: 'working',
          type: 'tool.step',
          fields: {
            id: 'step_42',
            surface: 'terminal',
            running: true,
            ok: true,
            title: 'Running shell',
          },
        },
      })
    })

    await waitFor(() => {
      expect(result.current.resuming).toBe(false)
    })
    // Still NOT idle — the run is still being narrated.
    expect(result.current.phase).toBe('working')
    expect(result.current.connectionRetrying).toBe(false)
  })

  it('useChat.streamDropShowsRetryingNotIdle — a non-terminal stream `error` does NOT immediately flip phase to idle and DOES expose a retrying signal', async () => {
    vi.useFakeTimers()
    conversationsApi.listConversations.mockResolvedValue([LIVE_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(liveRecord())

    const { result } = renderHook(() => useChat(), { wrapper: queryWrapper() })
    await vi.waitFor(() => {
      expect(conversationsApi.listConversations).toHaveBeenCalled()
    })
    act(() => {
      result.current.selectConversation('conv_live')
    })

    await vi.waitFor(() => {
      expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    })
    const firstClose = eventsApi.closeMock
    expect(result.current.phase).toBe('working')

    // Push a non-terminal SSE error (no message.complete / lifecycle.failed
    // arrived yet — the run is still going on the daemon).
    act(() => {
      eventsApi.lastOnUpdate?.({ kind: 'error', error: new Error('connection lost') })
    })

    // The retrying signal is exposed AND phase is still 'working'.
    expect(result.current.connectionRetrying).toBe(true)
    expect(result.current.phase).toBe('working')
    expect(result.current.activeIntentId).toBe('neo_inflight')
    expect(result.current.task).not.toBeNull()
    expect(result.current.task?.done).toBe(false)
    // The dead subscription was torn down (close was called).
    expect(firstClose).toHaveBeenCalled()

    // Advance past the first retry backoff (1s) — re-subscribe must fire.
    eventsApi.subscribeEvents.mockClear()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1100)
    })

    expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    expect(eventsApi.lastIntentId).toBe('neo_inflight')
    expect(eventsApi.lastReplay).toBe(true)
    // Still NOT idle while retrying.
    expect(result.current.phase).toBe('working')
    expect(result.current.connectionRetrying).toBe(true)
  })

  it('useChat — transient `reconnecting` SSE updates fold into connectionRetrying without flipping phase to idle', async () => {
    conversationsApi.listConversations.mockResolvedValue([LIVE_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(liveRecord())

    const { result } = renderHook(() => useChat(), { wrapper: queryWrapper() })
    await waitFor(() => {
      expect(conversationsApi.listConversations).toHaveBeenCalled()
    })
    act(() => {
      result.current.selectConversation('conv_live')
    })
    await waitFor(() => {
      expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    })

    act(() => {
      eventsApi.lastOnUpdate?.({ kind: 'reconnecting', attempt: 1 })
    })

    expect(result.current.connectionRetrying).toBe(true)
    expect(result.current.phase).toBe('working')

    // A subsequent real event clears the visible retrying flag.
    act(() => {
      eventsApi.lastOnUpdate?.({
        kind: 'event',
        event: {
          seq: 9,
          ts: '2026-06-24T00:01:03Z',
          phase: 'working',
          type: 'chat.thinking',
          fields: { text: 'thinking out loud' },
        },
      })
    })
    expect(result.current.connectionRetrying).toBe(false)
    expect(result.current.phase).toBe('working')
  })
})
