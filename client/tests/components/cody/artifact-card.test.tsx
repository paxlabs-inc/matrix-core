/**
 * NEO-WORKBENCH task 3.3 (req 3.1–3.5): artifact/action cards fold a REAL
 * event sequence — rows transition status live off the daemon's start/end
 * pairs (never synthesized timing), click-through routes file rows to the
 * Code view and command rows to the terminal, and the same fold rebuilds
 * identical cards from the durable trace (proven in chat-rail.trace.test).
 */
import { describe, it, expect, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'

import { ArtifactCards, groupArtifactCards } from '@/components/matrix/cody/artifact-card'
import { buildTaskFromTrace } from '@/hooks/api/useChat'
import type { NeoStep } from '@/hooks/api/useChat'
import type { TraceEvent } from '@/lib/api/conversations'

function step(partial: Partial<NeoStep> & Pick<NeoStep, 'id' | 'kind'>): NeoStep {
  return { running: false, ok: true, title: '', ...partial }
}

const liveSequence: NeoStep[] = [
  step({ id: 'n1', kind: 'narration', text: 'Scaffolding the app.' }),
  step({ id: 'w1', kind: 'editor', action: 'write', path: 'src/app.ts', running: true }),
  step({ id: 'c1', kind: 'terminal', command: 'npm install', running: true }),
]

describe('artifact cards — live status transitions', () => {
  it('rows tick running → complete/failed as the SAME step ids settle', () => {
    const onOpenFile = vi.fn()
    const onRevealTerminal = vi.fn()
    const r = render(
      <ArtifactCards
        steps={liveSequence}
        onOpenFile={onOpenFile}
        onRevealTerminal={onRevealTerminal}
      />,
    )
    const fileRow = () => screen.getByText('src/app.ts').closest('button')!
    const cmdRow = () => screen.getByText('$ npm install').closest('button')!
    expect(fileRow().dataset.rowStatus).toBe('running')
    expect(cmdRow().dataset.rowStatus).toBe('running')

    // The daemon's end events land (same ids, running:false).
    const settled = [
      liveSequence[0],
      step({ id: 'w1', kind: 'editor', action: 'write', path: 'src/app.ts', running: false }),
      step({
        id: 'c1',
        kind: 'terminal',
        command: 'npm install',
        running: false,
        ok: false,
      }),
    ]
    r.rerender(
      <ArtifactCards steps={settled} onOpenFile={onOpenFile} onRevealTerminal={onRevealTerminal} />,
    )
    expect(fileRow().dataset.rowStatus).toBe('complete')
    expect(cmdRow().dataset.rowStatus).toBe('failed')

    // Click-through (req 3.3).
    fireEvent.click(fileRow())
    expect(onOpenFile).toHaveBeenCalledWith('src/app.ts')
    fireEvent.click(cmdRow())
    expect(onRevealTerminal).toHaveBeenCalled()
  })

  it('groups per narration turn; inspection steps stay off the card', () => {
    const steps: NeoStep[] = [
      step({ id: 'n1', kind: 'narration', text: 'First batch.' }),
      step({ id: 'r1', kind: 'editor', action: 'read', path: 'README.md' }),
      step({ id: 'w1', kind: 'editor', action: 'write', path: 'a.ts' }),
      step({ id: 'n2', kind: 'narration', text: 'Second batch.' }),
      step({ id: 'w2', kind: 'editor', action: 'edit', path: 'b.ts' }),
    ]
    const groups = groupArtifactCards(steps)
    expect(groups).toHaveLength(2)
    expect(groups[0].header).toBe('First batch.')
    expect(groups[0].rows.map((r) => r.label)).toEqual(['a.ts'])
    expect(groups[1].header).toBe('Second batch.')
    expect(groups[1].rows.map((r) => r.label)).toEqual(['b.ts'])
  })

  it('rebuilds identically from the durable trace (live fold ≡ trace fold)', () => {
    const traceEvents: TraceEvent[] = [
      { seq: 1, type: 'chat.assistant', fields: { text: 'Scaffolding the app.', intent_id: 'i1' } },
      {
        seq: 2,
        type: 'tool.step',
        fields: {
          id: 'w1',
          surface: 'editor',
          action: 'write',
          path: 'src/app.ts',
          running: true,
        },
      },
      {
        seq: 3,
        type: 'tool.step',
        fields: {
          id: 'w1',
          surface: 'editor',
          action: 'write',
          path: 'src/app.ts',
          running: false,
          ok: true,
        },
      },
    ]
    const reopened = buildTaskFromTrace(traceEvents, 'i1')
    const groups = groupArtifactCards(reopened.steps)
    expect(groups).toHaveLength(1)
    expect(groups[0].header).toBe('Scaffolding the app.')
    expect(groups[0].rows).toEqual([
      { id: 'w1', kind: 'file', label: 'src/app.ts', action: 'write', status: 'complete' },
    ])
  })
})
