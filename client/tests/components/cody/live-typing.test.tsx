/**
 * NEO-WORKBENCH task 3.4 (req 6.1–6.4): the live-typing channel on the
 * client — contiguous tool.delta fragments accumulate byte-exactly (offsets
 * are UTF-8 bytes), a gap drops the buffer honestly (fallback to the final
 * content, never a corrupted splice), and a starting write force-follows
 * into the Code view unless the user pinned a view this run. Byte-identical
 * convergence with the disk file is proven end-to-end on the daemon side
 * (neo/internal/agent/livetype_test.go).
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, fireEvent, screen } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'

vi.mock('sonner', () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
  Toaster: () => null,
}))
vi.mock('@/lib/api/media', () => ({
  uploadMedia: vi.fn(),
  mediaKindForMime: () => 'file',
  loadMediaObjectURL: vi.fn(async () => 'blob:probe'),
}))

import { foldTypingDelta, EMPTY_TASK, type NeoTask, type UseChatResult } from '@/hooks/api/useChat'
import { CodyWorkspace } from '@/components/matrix/cody/cody-workspace'

class RO {
  observe() {}
  unobserve() {}
  disconnect() {}
}
beforeEach(() => {
  vi.stubGlobal('ResizeObserver', RO)
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            entries: [],
            truncated: false,
            git: false,
            diff: '',
            untracked: [],
            path: 'src/app.ts',
            content: '',
            size: 0,
            hash: 'h',
            items: [],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
    ),
  )
})

describe('foldTypingDelta — bounded, honest accumulation', () => {
  it('appends contiguous fragments byte-exactly (UTF-8 offsets)', () => {
    const path = '/workspace/app/src/app.ts'
    let typing = foldTypingDelta(undefined, path, 'héllo ', 0)
    // 'héllo ' is 7 UTF-8 bytes; the next fragment arrives at byte 7.
    typing = foldTypingDelta(typing, path, 'wörld', 7)
    expect(typing[path].content).toBe('héllo wörld')
    expect(typing[path].dropped).toBeUndefined()
  })

  it('a gap marks the buffer dropped instead of splicing at a guess', () => {
    const path = 'a.ts'
    let typing = foldTypingDelta(undefined, path, 'abc', 0)
    typing = foldTypingDelta(typing, path, 'zzz', 99)
    expect(typing[path].dropped).toBe(true)
    expect(typing[path].content).toBe('abc')
    // Once dropped, later fragments never resurrect a corrupted buffer.
    typing = foldTypingDelta(typing, path, 'more', 102)
    expect(typing[path].content).toBe('abc')
    expect(typing[path].dropped).toBe(true)
  })

  it('offset 0 restarts the buffer (a fresh write to the same file)', () => {
    const path = 'a.ts'
    let typing = foldTypingDelta(undefined, path, 'first', 0)
    typing = foldTypingDelta(typing, path, 'second', 0)
    expect(typing[path].content).toBe('second')
  })
})

function chatWith(task: NeoTask | null): UseChatResult {
  return {
    messages: [],
    phase: task && !task.done ? 'working' : 'idle',
    activeIntentId: task?.intentId ?? null,
    conversationId: 'conv_lt',
    error: null,
    pendingGate: null,
    resuming: false,
    connectionRetrying: false,
    task,
    dismissTask: () => {},
    conversations: [],
    loadingThread: false,
    send: () => {},
    reset: () => {},
    selectConversation: () => {},
    refreshConversations: () => {},
    renameConversation: () => {},
    archiveConversation: () => {},
    deleteConversation: () => {},
    forkConversation: () => {},
    answerGate: () => {},
    respondAsk: () => {},
  }
}

function writingTask(): NeoTask {
  return {
    ...EMPTY_TASK,
    intentId: 'run_lt',
    steps: [
      {
        id: 'w1',
        kind: 'editor',
        running: true,
        ok: true,
        title: 'Writing a file',
        action: 'write',
        path: '/workspace/app/src/index.ts',
      },
    ],
    typing: {
      '/workspace/app/src/index.ts': { content: 'const x = 1\n', nextOffset: 12 },
    },
  }
}

describe('force-follow (req 6.2)', () => {
  it('a starting write opens the workbench on Code view with the file selected', async () => {
    const { rerender } = renderWithIntl(
      <CodyWorkspace chat={chatWith(null)} project="app" projectName="App" />,
    )
    await act(async () => {
      rerender(<CodyWorkspace chat={chatWith(writingTask())} project="app" projectName="App" />)
    })
    const wb = screen.getByTestId('workbench')
    expect(wb.dataset.open).toBe('true')
    const codeTab = screen.getByRole('tab', { name: /code/i })
    expect(codeTab.getAttribute('aria-selected')).toBe('true')
    // The written file is selected (rel path shown in the editor header) and
    // locked read-only while Neo writes.
    expect(await screen.findByText('src/index.ts')).toBeInTheDocument()
    expect(screen.getByText(/Neo is writing/i)).toBeInTheDocument()
  })

  it('a user-pinned view is respected — the write selects the file but stays on Diff', async () => {
    const running: NeoTask = {
      ...EMPTY_TASK,
      intentId: 'run_lt',
      steps: [
        { id: 'n0', kind: 'terminal', running: true, ok: true, title: 'Terminal', command: 'ls' },
      ],
    }
    const { rerender } = renderWithIntl(
      <CodyWorkspace chat={chatWith(running)} project="app" projectName="App" />,
    )
    // The user pins the Diff view mid-run.
    fireEvent.click(screen.getByRole('tab', { name: /diff/i }))
    await act(async () => {
      rerender(<CodyWorkspace chat={chatWith(writingTask())} project="app" projectName="App" />)
    })
    const diffTab = screen.getByRole('tab', { name: /diff/i })
    expect(diffTab.getAttribute('aria-selected')).toBe('true')
  })
})
