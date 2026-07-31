/**
 * NEO-WORKBENCH task 2.2 (req 1.1, 1.3, 1.5): the coding chat rail is a REAL
 * Neo conversation — the same reducer state the dashboard folds — and history
 * renders from the real server-backed conversation list.
 *
 * The trace here is the exact durable shape the daemon persists (tool.step
 * start/end pairs, narration, preview.*); it is folded through the REAL
 * buildTaskFromTrace (the same function the dashboard's reopen path uses) and
 * rendered through the REAL ChatRail + artifact cards.
 */
import { describe, it, expect, vi } from 'vitest'
import { screen } from '@testing-library/react'
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

import { buildTaskFromTrace, type UseChatResult, type NeoTask } from '@/hooks/api/useChat'
import type { TraceEvent, ConversationSummary } from '@/lib/api/conversations'
import { ChatRail } from '@/components/matrix/cody/chat-rail'
import { CodyHistory } from '@/components/matrix/cody/cody-history'

const INTENT = 'neo_workbench_run'

/** The durable trace a real coding run persists: narration, an editor write
 *  (running→done pair on one id), a command (running→done), preview.ready. */
function codingTrace(): TraceEvent[] {
  return [
    {
      seq: 1,
      type: 'chat.assistant',
      fields: { text: 'Setting up the app skeleton.', intent_id: INTENT },
    },
    {
      seq: 2,
      type: 'tool.step',
      fields: {
        id: 'w1',
        surface: 'editor',
        title: 'Writing a file',
        action: 'write',
        path: '/workspace/app/src/index.ts',
        running: true,
      },
    },
    {
      seq: 3,
      type: 'tool.step',
      fields: {
        id: 'c1',
        surface: 'terminal',
        title: 'Terminal',
        command: 'npm install',
        running: true,
      },
    },
    {
      seq: 4,
      type: 'tool.step',
      fields: {
        id: 'w1',
        surface: 'editor',
        title: 'Writing a file',
        action: 'write',
        path: '/workspace/app/src/index.ts',
        running: false,
        ok: true,
      },
    },
    {
      seq: 5,
      type: 'tool.step',
      fields: {
        id: 'c1',
        surface: 'terminal',
        title: 'Terminal',
        command: 'npm install',
        running: false,
        ok: false,
        output: 'ERESOLVE',
      },
    },
    { seq: 6, type: 'preview.ready', fields: { url: 'https://sbx.example/app' } },
  ]
}

function chatWith(task: NeoTask): UseChatResult {
  return {
    messages: [
      { id: 'u1', role: 'user', text: 'build the app', ts: 1 },
      { id: 'a1', role: 'assistant', text: 'Done.', ts: 2 },
    ],
    phase: 'idle',
    activeIntentId: null,
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

describe('chat rail — real trace fold parity (reopen)', () => {
  it('folds the durable trace into the dashboard state and renders cards with FINAL statuses', () => {
    const task = buildTaskFromTrace(codingTrace(), INTENT, 'build the app')

    // The same fold the dashboard uses: start/end pairs collapse by id.
    const editor = task.steps.filter((s) => s.kind === 'editor')
    expect(editor).toHaveLength(1)
    expect(editor[0].running).toBe(false)
    expect(editor[0].ok).toBe(true)
    const terminal = task.steps.filter((s) => s.kind === 'terminal')
    expect(terminal).toHaveLength(1)
    expect(terminal[0].ok).toBe(false)
    // The durable preview state was rebuilt (req 7.2).
    expect(task.preview).toEqual({ state: 'ready', url: 'https://sbx.example/app' })

    renderWithIntl(
      <ChatRail
        chat={chatWith(task)}
        projectName="App"
        onOpenFile={() => {}}
        onRevealTerminal={() => {}}
        onStop={() => {}}
      />,
    )

    // One artifact card, headed by the narration, rows with final statuses.
    const card = screen.getByTestId('artifact-card')
    expect(card.textContent).toContain('Setting up the app skeleton.')
    const fileRow = screen.getByText('/workspace/app/src/index.ts').closest('button')!
    expect(fileRow.dataset.rowStatus).toBe('complete')
    const cmdRow = screen.getByText('$ npm install').closest('button')!
    expect(cmdRow.dataset.rowStatus).toBe('failed')
  })
})

describe('history — real server-backed conversation list', () => {
  it('renders the project-scoped conversations and opens one', () => {
    const items: ConversationSummary[] = [
      {
        conversation_id: 'conv_1',
        title: 'build the app',
        preview: 'Done.',
        turn_count: 4,
        updated: '2026-07-11T10:00:00Z',
        project: 'app',
      },
      {
        conversation_id: 'conv_2',
        title: 'fix the tests',
        preview: 'On it.',
        turn_count: 2,
        updated: '2026-07-11T12:00:00Z',
        project: 'app',
      },
    ]
    const opened: string[] = []
    renderWithIntl(<CodyHistory conversations={items} onOpen={(id) => opened.push(id)} />)
    const rows = screen.getAllByRole('button')
    // Newest first.
    expect(rows[0].textContent).toContain('fix the tests')
    rows[1].click()
    expect(opened).toEqual(['conv_1'])
  })
})
