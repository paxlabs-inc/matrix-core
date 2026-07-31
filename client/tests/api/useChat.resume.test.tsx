import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import type { ConversationRecord, ConversationSummary } from '@/lib/api/conversations'
import type { SSEUpdate } from '@/lib/realtime/sse'

/* -------------------------------------------------------------------------- */
/*  F1 — durable live-run resume                                              */
/* -------------------------------------------------------------------------- */
/*  These tests pin the client half of NEO-UX-FIXES.kvx [item.F1]:            */
/*  when GET /conversations/{id} carries `live_run` (the authoritative        */
/*  in-flight intent_id), useChat.selectConversation must subscribe to the    */
/*  SSE stream with replay:true IMMEDIATELY — no /messages/async/<id> poll.   */
/*  When `live_run` is empty/absent, the legacy trailing-user-turn + poll     */
/*  backstop runs (and, with no trailing user-with-intent turn, no subscribe  */
/*  fires).                                                                   */
/*                                                                            */
/*  Mocks ONLY the network boundary (the api modules). The hook's internal    */
/*  reducer, callbacks, and types are exercised for real.                     */

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
    // F3 — the durable-workspace fetch on the settled-thread reopen path.
    // Default to an empty timeline so these F1 resume tests stay hermetic
    // (a text-only thread leaves the surface idle, the assertion below).
    getConversationTrace: () => Promise.resolve({ intent_id: '', live_run: '', events: [] }),
  }
})

const eventsApi = {
  subscribeEvents: vi.fn(),
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

vi.mock('@/lib/api/chat', () => ({
  sendChat: vi.fn(),
}))

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
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return function QueryWrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>
  }
}

const SETTLED_SUMMARY: ConversationSummary = {
  conversation_id: 'conv_settled',
  title: 'old thread',
  preview: 'done',
  turn_count: 2,
  updated: '2026-06-24T00:00:00Z',
}

const LIVE_SUMMARY: ConversationSummary = {
  conversation_id: 'conv_live',
  title: 'live thread',
  preview: 'working',
  turn_count: 1,
  updated: '2026-06-24T00:01:00Z',
}

function settledRecord(): ConversationRecord {
  return {
    conversation_id: 'conv_settled',
    title: 'old thread',
    updated: '2026-06-24T00:00:00Z',
    live_run: '',
    turns: [
      { role: 'user', text: 'hello', ts: '2026-06-24T00:00:00Z' },
      { role: 'assistant', text: 'hi', intent_id: 'neo_done', ts: '2026-06-24T00:00:01Z' },
    ],
  }
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
  eventsApi.subscribeEvents.mockReturnValue({ close: vi.fn() })
  runsApi.getAsyncJob.mockReset()
  runsApi.answerGate.mockReset()
  runsApi.answerAsk.mockReset()
  runsApi.cancelRun.mockReset()
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('useChat — F1 durable live-run resume', () => {
  it('useChat.resumesLiveRunOnReload — subscribe(replay:true) with the live intent when GET /conversations/{id} carries live_run', async () => {
    // Opening a live conversation exercises the durable resume path.
    conversationsApi.listConversations.mockResolvedValue([LIVE_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(liveRecord())

    const { result } = renderHook(() => useChat(), { wrapper: queryWrapper() })
    act(() => result.current.selectConversation('conv_live'))

    await waitFor(() => {
      expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    })

    const call = eventsApi.subscribeEvents.mock.calls[0][0]
    expect(call.intentId).toBe('neo_inflight')
    expect(call.replay).toBe(true)
    // The authoritative live_run path bypasses the /messages/async poll.
    expect(runsApi.getAsyncJob).not.toHaveBeenCalled()

    // Direct selectConversation call must use the same path.
    eventsApi.subscribeEvents.mockClear()
    runsApi.getAsyncJob.mockClear()
    conversationsApi.getConversation.mockResolvedValueOnce(liveRecord())
    act(() => {
      result.current.selectConversation('conv_live')
    })
    await waitFor(() => {
      expect(eventsApi.subscribeEvents).toHaveBeenCalledTimes(1)
    })
    const second = eventsApi.subscribeEvents.mock.calls[0][0]
    expect(second.intentId).toBe('neo_inflight')
    expect(second.replay).toBe(true)
    expect(runsApi.getAsyncJob).not.toHaveBeenCalled()
  })

  it('useChat.noResumeWhenSettled — no subscribe when live_run is empty AND the last turn is assistant', async () => {
    conversationsApi.listConversations.mockResolvedValue([SETTLED_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(settledRecord())

    const { result } = renderHook(() => useChat(), { wrapper: queryWrapper() })
    act(() => result.current.selectConversation('conv_settled'))

    // Give the hook a tick to land the GET and decide.
    await waitFor(() => {
      expect(conversationsApi.getConversation).toHaveBeenCalled()
    })
    // Two beats: one for the GET, one for the resume decision microtask.
    await new Promise((r) => setTimeout(r, 0))

    expect(eventsApi.subscribeEvents).not.toHaveBeenCalled()
    // No fallback poll either — the trailing turn is assistant, not user.
    expect(runsApi.getAsyncJob).not.toHaveBeenCalled()
  })
})
