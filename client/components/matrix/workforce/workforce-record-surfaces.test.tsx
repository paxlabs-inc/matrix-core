import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NextIntlClientProvider } from 'next-intl'
import { describe, expect, it } from 'vitest'
import messages from '@/messages/en.json'
import { WorkforceRecordSurfaces } from './workforce-record-surfaces'
import type { WorkforceResource, WorkforceResourceItem } from '@/lib/api/workforce'
import type { WorkforceCommandDraft } from '@/lib/workforce/control-signing'

const at = '2026-07-30T12:00:00Z'
const records = new Map<WorkforceResource, WorkforceResourceItem[]>([
  [
    'mail',
    [
      item('message:one', {
        thread_id: 'thread:one',
        kind: 'question',
        sender_seat_id: 'seat:research:lead',
        binding_state: 'ready',
        recipients: [{ seat_id: 'seat:developer:lead', state: 'opened' }],
      }),
    ],
  ],
  [
    'approvals',
    [
      item('batch:one', {
        intent_ids: ['intent:one', 'intent:two'],
        intent_set_hash: 'a'.repeat(64),
        aggregate_ceiling_microunits: 500,
        consumed_microunits: 0,
        expires_at: '2099-07-30T12:00:00Z',
      }),
    ],
  ],
  [
    'receipts',
    [
      item('receipt:progress', {
        wake_id: 'wake:one',
        intent_id: 'intent:one',
        disposition: 'progress',
        content_hash: 'b'.repeat(64),
      }),
      item('receipt:complete', {
        wake_id: 'wake:two',
        intent_id: 'intent:two',
        disposition: 'goal_completed',
        content_hash: 'c'.repeat(64),
      }),
    ],
  ],
  [
    'policies',
    [
      item(
        'policy:one:2',
        {
          authority_kind: 'policy',
          authority_id: 'policy:one',
          effective_at: at,
          canonical_hash: 'd'.repeat(64),
          material_change: true,
        },
        2,
      ),
    ],
  ],
  [
    'project-brain',
    [
      item('brain:one', {
        project_id: 'project:one',
        workspace_id: 'workspace:one',
        kind: 'test_evidence',
        source_root: 'e'.repeat(64),
        graph_generation: 9,
        fresh: true,
        canonical_hash: 'f'.repeat(64),
      }),
    ],
  ],
  [
    'corrections',
    [
      item('correction:one', {
        status: 'open',
        affected: [{ record_id: 'record:affected', state: 'pending', paused: true }],
      }),
    ],
  ],
  [
    'audit-disagreements',
    [
      item('audit:one', {
        disagreement: true,
        original_outcome: 'pass',
        reaudit_outcome: 'requires_human',
      }),
    ],
  ],
  [
    'replay-lineage',
    [
      item('evidence:one', {
        wake_id: 'wake:one',
        replay_retained: true,
        request_hash: '1'.repeat(64),
        response_hash: '2'.repeat(64),
      }),
    ],
  ],
  ['effect-status', [item('effect:one', { state: 'externally_ambiguous', operation: 'publish' })]],
])

describe('Workforce record surfaces', () => {
  it('navigates every record view and signs only the exact reported approval set', async () => {
    const user = userEvent.setup()
    const previews: WorkforceCommandDraft[] = []
    render(
      <NextIntlClientProvider locale="en" messages={messages}>
        <WorkforceRecordSurfaces
          items={(resource) => records.get(resource) ?? []}
          controlVersion={() => 3}
          onPreview={(draft) => previews.push(draft)}
        />
      </NextIntlClientProvider>,
    )

    expect(
      screen.getByText('Message content remains sealed and is not exposed in this projection.'),
    ).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Approvals' }))
    await user.click(screen.getByRole('button', { name: 'Review exact batch' }))
    expect(previews).toEqual([
      expect.objectContaining({
        action: 'approve_batch',
        expectedVersion: 3,
        change: expect.objectContaining({ intent_ids: ['intent:one', 'intent:two'] }),
      }),
    ])

    await user.click(screen.getByRole('tab', { name: 'Receipts' }))
    expect(screen.getAllByText('Verified completion')).toHaveLength(1)
    expect(screen.getByText('progress')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Policies' }))
    expect(screen.getByText('material change')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Project Brain' }))
    expect(screen.getByText('project:one / workspace:one')).toBeInTheDocument()
    expect(screen.getByText('current')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'Assurance' }))
    expect(screen.getByText('record:affected · pending · paused')).toBeInTheDocument()
    expect(screen.getByText('disagreement')).toBeInTheDocument()
    expect(screen.getByText('needs reconciliation')).toBeInTheDocument()
  })
})

function item(id: string, fields: Record<string, unknown>, version = 1): WorkforceResourceItem {
  return { id, version, updated_at: at, fields }
}
