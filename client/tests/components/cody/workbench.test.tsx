/**
 * NEO-WORKBENCH task 2.1 (req 2.2, 2.3, 1.4): the ONE workbench shell.
 *  - slider switching preserves panel state (panels stay mounted),
 *  - the workbench slides in when work steps arrive,
 *  - no Cody-branded strings render on the coding surface.
 * Real components throughout; only the network boundary (fetch) is served
 * with real-shaped JSON, the same posture as the Go httptest suites.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, fireEvent, act } from '@testing-library/react'
import { useState } from 'react'
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

import { NeoWorkbench, type WorkbenchView } from '@/components/matrix/cody/workbench'
import { CodyWorkspace } from '@/components/matrix/cody/cody-workspace'
import type { NeoTask, UseChatResult } from '@/hooks/api/useChat'
import { EMPTY_TASK } from '@/hooks/api/useChat'

// jsdom lacks ResizeObserver (react-use-measure needs it in the tree pane).
class RO {
  observe() {}
  unobserve() {}
  disconnect() {}
}

// Serve the workspace API endpoints with real-shaped bodies.
beforeEach(() => {
  vi.stubGlobal('ResizeObserver', RO)
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      const json = (body: unknown) =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        })
      if (url.includes('/workspace/tree')) return json({ entries: [], truncated: false })
      if (url.includes('/workspace/diff')) return json({ git: false, diff: '', untracked: [] })
      if (url.includes('/workspace/file')) {
        return json({ path: 'x', content: '', size: 0, hash: 'h', truncated: false })
      }
      if (url.includes('/conversations')) return json({ items: [] })
      return json({})
    }),
  )
})

function Host() {
  const [view, setView] = useState<WorkbenchView>('code')
  const [terminalOpen, setTerminalOpen] = useState(false)
  return (
    <NeoWorkbench
      open
      view={view}
      onViewChange={setView}
      onClose={() => {}}
      terminalOpen={terminalOpen}
      onToggleTerminal={() => setTerminalOpen((t) => !t)}
      code={<input aria-label="probe-input" defaultValue="" />}
      diff={<div data-testid="diff-panel-content">diff body</div>}
      preview={<div>preview body</div>}
      terminal={<div>terminal body</div>}
    />
  )
}

describe('NeoWorkbench shell', () => {
  it('slider switches views while panels stay mounted — typed editor state survives', () => {
    renderWithIntl(<Host />)
    const input = screen.getByLabelText('probe-input') as HTMLInputElement
    fireEvent.change(input, { target: { value: 'unsaved buffer text' } })

    const strip = screen.getByTestId('workbench-strip')
    expect(strip.style.transform).toContain('translateX(-0%)')

    fireEvent.click(screen.getByRole('tab', { name: /diff/i }))
    expect(strip.style.transform).not.toContain('translateX(-0%)')
    // The code panel (and the user's typed state) is still in the DOM.
    expect(screen.getByTestId('diff-panel-content')).toBeInTheDocument()
    expect((screen.getByLabelText('probe-input') as HTMLInputElement).value).toBe(
      'unsaved buffer text',
    )

    fireEvent.click(screen.getByRole('tab', { name: /code/i }))
    expect(strip.style.transform).toContain('translateX(-0%)')
    expect((screen.getByLabelText('probe-input') as HTMLInputElement).value).toBe(
      'unsaved buffer text',
    )
  })

  it('terminal drawer toggles without unmounting its content', () => {
    renderWithIntl(<Host />)
    const drawer = screen.getByTestId('workbench-terminal')
    expect(drawer.dataset.open).toBe('false')
    fireEvent.click(screen.getByRole('button', { name: /terminal/i }))
    expect(drawer.dataset.open).toBe('true')
    expect(screen.getByText('terminal body')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /terminal/i }))
    expect(drawer.dataset.open).toBe('false')
    // Still mounted (scrollback survives).
    expect(screen.getByText('terminal body')).toBeInTheDocument()
  })
})

function chatWith(task: NeoTask | null): UseChatResult {
  return {
    messages: [],
    phase: task && !task.done ? 'working' : 'idle',
    activeIntentId: task?.intentId ?? null,
    conversationId: 'conv_wb',
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

describe('CodyWorkspace — the one layout', () => {
  it('slides the workbench in when the first work step arrives', () => {
    const idle = chatWith(null)
    const { rerender } = renderWithIntl(
      <CodyWorkspace chat={idle} project="app" projectName="App" />,
    )
    const wb = () => screen.getByTestId('workbench')
    expect(wb().dataset.open).toBe('false')

    const working: NeoTask = {
      ...EMPTY_TASK,
      intentId: 'run1',
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
    }
    act(() => {
      rerender(<CodyWorkspace chat={chatWith(working)} project="app" projectName="App" />)
    })
    expect(wb().dataset.open).toBe('true')
  })

  it('renders no Cody-branded strings', () => {
    const working: NeoTask = {
      ...EMPTY_TASK,
      intentId: 'run1',
      steps: [
        {
          id: 't1',
          kind: 'terminal',
          running: false,
          ok: true,
          title: 'Terminal',
          command: 'npm test',
          output: 'ok',
        },
      ],
    }
    const { container } = renderWithIntl(
      <CodyWorkspace chat={chatWith(working)} project="app" projectName="App" />,
    )
    expect(container.textContent).not.toMatch(/cody/i)
  })
})
