import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import type {
  ConversationRecord,
  ConversationSummary,
  ConversationTrace,
  TraceEvent,
} from '@/lib/api/conversations'
import type { SSEUpdate } from '@/lib/realtime/sse'

/* -------------------------------------------------------------------------- */
/*  F3 — durable workspace trace ("Neo's Computer" survives reopen)           */
/* -------------------------------------------------------------------------- */
/*  Pins the client half of NEO-UX-FIXES.kvx [item.F3]: the persisted         */
/*  per-run workspace timeline (tool steps / source cards / media / Construct  */
/*  surfaces / Agent-Swarm windows) is folded back into the NeoTask on        */
/*  reopen, so a settled thread shows the prior workspace instead of an empty  */
/*  computer. The reducer (buildTaskFromTrace) and the hook wiring are         */
/*  exercised for real; ONLY the network boundary is mocked.                  */

const conversationsApi = {
  listConversations: vi.fn(),
  getConversation: vi.fn(),
  getConversationTrace: vi.fn(),
}
vi.mock('@/lib/api/conversations', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/api/conversations')>()
  return {
    ...actual,
    listConversations: (signal?: AbortSignal) => conversationsApi.listConversations(signal),
    getConversation: (id: string, signal?: AbortSignal) =>
      conversationsApi.getConversation(id, signal),
    getConversationTrace: (id: string, signal?: AbortSignal) =>
      conversationsApi.getConversationTrace(id, signal),
  }
})

const eventsApi = { subscribeEvents: vi.fn() }
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
  useTranslations: () => (key: string) => key,
}))

vi.mock('sonner', () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}))

import { useChat, buildTaskFromTrace } from '@/hooks/api/useChat'

const INTENT = 'neo_trace_run'

// A representative workspace trace: a terminal step (running→done collapses to
// one viewport), a web-search with source cards, generated media, a non-final
// narration turn, and a final answer that MUST be ignored (the turn log owns
// it). seq order is deliberately the publish order.
function workspaceTrace(): TraceEvent[] {
  return [
    {
      seq: 1,
      type: 'chat.assistant',
      fields: { text: 'Let me check the build.', intent_id: INTENT },
    },
    {
      seq: 2,
      type: 'tool.step',
      fields: {
        id: 's1',
        surface: 'terminal',
        title: 'Terminal',
        running: true,
        command: 'go build ./...',
      },
    },
    {
      seq: 3,
      type: 'tool.step',
      fields: {
        id: 's1',
        surface: 'terminal',
        title: 'Terminal',
        running: false,
        command: 'go build ./...',
        output: 'ok',
      },
    },
    {
      seq: 4,
      type: 'tool.search',
      fields: {
        tool: 'web_search',
        query: 'go embed fs',
        answer: 'use //go:embed',
        results: [{ title: 'docs', url: 'https://go.dev', snippet: 'embed' }],
      },
    },
    {
      seq: 5,
      type: 'tool.media',
      fields: { kind: 'image', url: '/media/diagram.png', mime: 'image/png' },
    },
    {
      seq: 6,
      type: 'chat.assistant',
      fields: { text: 'Done — here is your tool.', final: true, intent_id: INTENT },
    },
  ]
}

const SETTLED_SUMMARY: ConversationSummary = {
  conversation_id: 'conv_trace',
  title: 'build a tool',
  preview: 'done',
  turn_count: 2,
  updated: '2026-06-24T00:00:00Z',
}

function settledRecord(): ConversationRecord {
  return {
    conversation_id: 'conv_trace',
    title: 'build a tool',
    updated: '2026-06-24T00:00:00Z',
    live_run: '',
    turns: [
      { role: 'user', text: 'build me a tool', intent_id: INTENT, ts: '2026-06-24T00:00:00Z' },
      {
        role: 'assistant',
        text: 'Done — here is your tool.',
        intent_id: INTENT,
        ts: '2026-06-24T00:00:01Z',
      },
    ],
  }
}

function trace(events: TraceEvent[]): ConversationTrace {
  return { intent_id: INTENT, live_run: '', events }
}

beforeEach(() => {
  conversationsApi.listConversations.mockReset()
  conversationsApi.getConversation.mockReset()
  conversationsApi.getConversationTrace.mockReset()
  eventsApi.subscribeEvents.mockReset()
  eventsApi.subscribeEvents.mockReturnValue({ close: vi.fn() })
  runsApi.getAsyncJob.mockReset()
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('buildTaskFromTrace — pure workspace reducer', () => {
  it('folds tool steps, source cards, media and narration; ignores the final answer', () => {
    const task = buildTaskFromTrace(workspaceTrace(), INTENT, 'build a tool')

    expect(task.intentId).toBe(INTENT)
    expect(task.title).toBe('build a tool')
    expect(task.done).toBe(true)

    // The terminal step's running→done pair collapses to ONE settled viewport.
    const terminal = task.steps.filter((s) => s.kind === 'terminal')
    expect(terminal).toHaveLength(1)
    expect(terminal[0].running).toBe(false)
    expect(terminal[0].command).toBe('go build ./...')
    expect(terminal[0].output).toBe('ok')

    // Non-final narration becomes a narration step keyed like the live reducer.
    const narration = task.steps.filter((s) => s.kind === 'narration')
    expect(narration).toHaveLength(1)
    expect(narration[0].id).toBe(`${INTENT}:1`)
    expect(narration[0].text).toBe('Let me check the build.')

    // Search cards + media rebuilt.
    expect(task.searches).toHaveLength(1)
    expect(task.searches[0].results[0].url).toBe('https://go.dev')
    expect(task.media).toHaveLength(1)
    expect(task.media[0].url).toBe('/media/diagram.png')

    // The final answer is NEVER a step/answer here — the durable turn log owns
    // it, so it must not be double-rendered as workspace content.
    expect(task.answer).toBe('')
    expect(task.steps.some((s) => s.text === 'Done — here is your tool.')).toBe(false)
  })

  it('rebuilds an Agent-Swarm window from swarm/subagent events', () => {
    const events: TraceEvent[] = [
      {
        seq: 1,
        type: 'swarm.started',
        fields: {
          swarm_id: 'sw1',
          count: 2,
          agents: [
            { index: 1, name: 'Scout' },
            { index: 2, name: 'Builder' },
          ],
        },
      },
      {
        seq: 2,
        type: 'subagent.step',
        fields: {
          agent_index: 1,
          id: 'a1s1',
          surface: 'browser',
          title: 'Browser',
          running: false,
          url: 'https://x',
        },
      },
      {
        seq: 3,
        type: 'subagent.status',
        fields: { agent_index: 1, status: 'done', summary: 'found it' },
      },
      { seq: 4, type: 'swarm.completed', fields: { swarm_id: 'sw1' } },
    ]
    const task = buildTaskFromTrace(events, INTENT)
    expect(task.swarm?.done).toBe(true)
    expect(task.swarm?.agents).toHaveLength(2)
    const scout = task.swarm?.agents.find((a) => a.index === 1)
    expect(scout?.status).toBe('done')
    expect(scout?.summary).toBe('found it')
    expect(scout?.steps).toHaveLength(1)
    expect(scout?.steps[0].kind).toBe('browser')
  })

  it('returns an idle-but-done task for an empty trace', () => {
    const task = buildTaskFromTrace([], INTENT)
    expect(task.steps).toHaveLength(0)
    expect(task.searches).toHaveLength(0)
    expect(task.done).toBe(true)
  })
})

describe('useChat — F3 reopen hydration', () => {
  it('useChat.hydratesWorkspaceFromTraceOnReopen — settled thread rebuilds the workspace from the trace', async () => {
    conversationsApi.listConversations.mockResolvedValue([SETTLED_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(settledRecord())
    conversationsApi.getConversationTrace.mockResolvedValue(trace(workspaceTrace()))

    const { result } = renderHook(() => useChat())

    // The settled thread fetches the durable trace (no live subscribe).
    await waitFor(() => {
      expect(conversationsApi.getConversationTrace).toHaveBeenCalledWith('conv_trace', undefined)
    })
    await waitFor(() => {
      expect(result.current.task).not.toBeNull()
    })

    const task = result.current.task!
    // The reopened "Neo's Computer" shows the prior workspace, not an empty one.
    expect(task.steps.filter((s) => s.kind === 'terminal')).toHaveLength(1)
    expect(task.searches).toHaveLength(1)
    expect(task.media).toHaveLength(1)
    expect(task.done).toBe(true)
    // Text turns still load as bubbles from the durable turn log.
    expect(result.current.messages.map((m) => m.text)).toContain('Done — here is your tool.')
    // A settled reopen never opens a live stream.
    expect(eventsApi.subscribeEvents).not.toHaveBeenCalled()
  })

  it('useChat.reopenWithEmptyTraceStaysTextOnly — no workspace when the trace is empty', async () => {
    conversationsApi.listConversations.mockResolvedValue([SETTLED_SUMMARY])
    conversationsApi.getConversation.mockResolvedValue(settledRecord())
    conversationsApi.getConversationTrace.mockResolvedValue(trace([]))

    const { result } = renderHook(() => useChat())

    await waitFor(() => {
      expect(conversationsApi.getConversationTrace).toHaveBeenCalled()
    })
    await new Promise((r) => setTimeout(r, 0))

    // Empty trace → the surface stays idle (text-only thread), never errors.
    expect(result.current.task).toBeNull()
    expect(eventsApi.subscribeEvents).not.toHaveBeenCalled()
  })
})
