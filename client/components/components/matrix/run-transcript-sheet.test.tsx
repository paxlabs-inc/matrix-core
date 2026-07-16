import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen } from '@testing-library/react'
import { fireEvent } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'
import { buildStages, type Run } from '@/lib/matrix-data'
import type { RunStreamState } from '@/hooks/api/useRunEvents'

/* The sheet renders whatever the SSE consumer hook reports; drive that
 * hook directly so the test asserts the rendering contract for each
 * connection state without standing up a real stream. */
let streamState: RunStreamState
vi.mock('@/hooks/api/useRunEvents', () => ({
  useRunEvents: () => streamState,
}))

import { RunTranscriptSheet } from '@/components/matrix/run-transcript-sheet'

const run: Run = {
  id: 'intent_abc',
  title: 'Compile a competitive teardown',
  status: 'running',
  agentId: 'paxeer-assistant',
  toolIds: [],
  autonomy: 'checkpoints',
  progress: 60,
  stages: buildStages(2),
  createdAt: new Date().toISOString(),
}

beforeEach(() => {
  streamState = { events: [], connection: 'connecting', reconnectAttempt: 0, lastError: null }
})

describe('RunTranscriptSheet', () => {
  it('shows the run title + id when open', () => {
    streamState = { ...streamState, connection: 'open' }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    expect(screen.getByText('Compile a competitive teardown')).toBeInTheDocument()
    expect(screen.getByText('intent_abc')).toBeInTheDocument()
  })

  it('renders the consumer Activity view with curated, plain-English rows', () => {
    streamState = {
      connection: 'open',
      reconnectAttempt: 0,
      lastError: null,
      events: [
        {
          seq: 1,
          ts: '2026-01-01T10:00:00Z',
          phase: 'boot',
          type: 'message.start',
          fields: { intent_id: 'intent_abc', prose: 'what is paxeer' },
        },
        {
          seq: 2,
          ts: '2026-01-01T10:00:01Z',
          phase: 'lifecycle',
          type: 'lifecycle.transition',
          fields: { intent_id: 'intent_abc', from: 'drafting', to: 'proposed' },
        },
        {
          seq: 3,
          ts: '2026-01-01T10:00:02Z',
          phase: 'lifecycle',
          type: 'lifecycle.transition',
          fields: { intent_id: 'intent_abc', from: 'proposed', to: 'accepted' },
        },
        {
          seq: 4,
          ts: '2026-01-01T10:00:03Z',
          phase: 'lifecycle',
          type: 'lifecycle.transition',
          fields: { intent_id: 'intent_abc', from: 'accepted', to: 'executing' },
        },
        {
          seq: 5,
          ts: '2026-01-01T10:00:04Z',
          phase: 'walk',
          type: 'step.text',
          fields: { intent_id: 'intent_abc', node_id: 'n1', text: 'Paxeer is a Layer-2 …' },
        },
      ],
    }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    expect(screen.getByText('Live')).toBeInTheDocument()
    // Header section default = Activity, not Details
    expect(screen.getByText('Activity')).toBeInTheDocument()
    expect(screen.getByText('Got your request')).toBeInTheDocument()
    expect(screen.getByText('“what is paxeer”')).toBeInTheDocument()
    expect(screen.getByText('Plan drafted')).toBeInTheDocument()
    expect(screen.getByText('Plan accepted')).toBeInTheDocument()
    expect(screen.getByText('Started running')).toBeInTheDocument()
    expect(screen.getByText('Agent answered')).toBeInTheDocument()
    expect(screen.getByText('Paxeer is a Layer-2 …')).toBeInTheDocument()
    // Protocol jargon is filtered out of the consumer view.
    expect(screen.queryByText('compile.intent.hashed')).not.toBeInTheDocument()
    expect(screen.queryByText('lifecycle.transition')).not.toBeInTheDocument()
  })

  it('reveals raw event rows when "Show details" is toggled on', () => {
    streamState = {
      connection: 'open',
      reconnectAttempt: 0,
      lastError: null,
      events: [
        {
          seq: 1,
          ts: '2026-01-01T10:00:00Z',
          phase: 'compile',
          type: 'compile.intent.hashed',
          fields: { intent_id: 'intent_abc' },
        },
        {
          seq: 2,
          ts: '2026-01-01T10:00:01Z',
          phase: 'walk',
          type: 'plan.tool.dispatch',
          fields: { intent_id: 'intent_abc', tool: 'web.search' },
        },
      ],
    }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    fireEvent.click(screen.getByRole('button', { name: /show details/i }))
    expect(screen.getByText('Technical details')).toBeInTheDocument()
    expect(screen.getByText('Planning')).toBeInTheDocument()
    expect(screen.getByText('Running a step')).toBeInTheDocument()
    expect(screen.getByText('compile.intent.hashed')).toBeInTheDocument()
    expect(screen.getByText('plan.tool.dispatch')).toBeInTheDocument()
    expect(screen.getByText('web.search')).toBeInTheDocument()
  })

  it('shows a waiting state when connected with no events yet', () => {
    streamState = { ...streamState, connection: 'open' }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    expect(screen.getByText('Connected — waiting for activity')).toBeInTheDocument()
  })

  it('surfaces a reconnecting badge with the attempt count', () => {
    streamState = { ...streamState, connection: 'reconnecting', reconnectAttempt: 2 }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    expect(screen.getByText('Reconnecting (2)')).toBeInTheDocument()
  })

  it('shows an error state with the last error message', () => {
    streamState = {
      events: [],
      connection: 'error',
      reconnectAttempt: 5,
      lastError: 'sse: 502 Bad Gateway',
    }
    renderWithIntl(<RunTranscriptSheet run={run} open onOpenChange={() => {}} />)
    expect(screen.getByText('Stream unavailable')).toBeInTheDocument()
    expect(screen.getByText('sse: 502 Bad Gateway')).toBeInTheDocument()
  })
})
