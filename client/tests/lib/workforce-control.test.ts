import { describe, expect, it } from 'vitest'
import { canonicalCommand } from '@/lib/workforce/control-signing'
import {
  parseWorkforceFrame,
  workforceStreamNeedsAccessToken,
} from '@/lib/realtime/workforce-events'
import type { WorkforceSignedCommand } from '@/lib/api/workforce'
import {
  approvalState,
  groupWorkforceMail,
  isVerifiedCompletionReceipt,
} from '@/lib/workforce/projections'

describe('Workforce owner control protocol', () => {
  it('canonicalizes nested change keys while preserving the signed wire schema', () => {
    const command: WorkforceSignedCommand = {
      schema_version: 'workforce.control.v1',
      command_id: 'command:web:one',
      organization_id: 'organization:one',
      owner_id: 'owner:one',
      action: 'set_autonomy',
      resource_kind: 'policy',
      resource_id: 'policy:global-autonomy',
      expected_version: 2,
      change: { zeta: true, alpha: { later: 2, first: 1 } },
      effective_at: '2026-07-30T12:00:00.000Z',
      signature: { algorithm: 'ed25519', key_id: 'key:web:one', value: 'replaced' },
    }
    const parsed = JSON.parse(canonicalCommand(command)) as Record<string, unknown>
    expect(Object.keys(parsed)).toEqual([
      'schema_version',
      'command_id',
      'organization_id',
      'owner_id',
      'action',
      'resource_kind',
      'resource_id',
      'expected_version',
      'change',
      'effective_at',
      'signature',
    ])
    expect(Object.keys(parsed.change as Record<string, unknown>)).toEqual(['alpha', 'zeta'])
    expect(Object.keys((parsed.change as { alpha: Record<string, unknown> }).alpha)).toEqual([
      'first',
      'later',
    ])
    expect(parsed.effective_at).toBe('2026-07-30T12:00:00Z')
    expect((parsed.signature as { value: string }).value).not.toBe('replaced')
  })

  it('parses lifecycle replay and explicit backpressure resync frames', () => {
    const event = parseWorkforceFrame(
      [
        'id: 8',
        'event: intent.waiting',
        'data: {"schema_version":"workforce.control.v1","cursor":8,"event_id":"event:8","organization_id":"organization:one","event_type":"intent.waiting","resource_kind":"intent","resource_id":"intent:one","resource_version":2,"verified_completion":false,"fields":{"status":"waiting_dependency"},"created_at":"2026-07-30T12:00:00Z"}',
      ].join('\n'),
    )
    expect(event.kind).toBe('event')
    if (event.kind === 'event') expect(event.event.cursor).toBe(8)

    expect(parseWorkforceFrame('event: resync_required\ndata: {"after":8}')).toEqual({
      kind: 'resync',
      after: 8,
    })
  })

  it('keeps same-origin Workforce streams on the server-authenticated proxy boundary', () => {
    expect(workforceStreamNeedsAccessToken('/api/workforce/events', 'https://matrix.example')).toBe(
      false,
    )
    expect(
      workforceStreamNeedsAccessToken(
        'https://matrix.example/api/workforce/events',
        'https://matrix.example',
      ),
    ).toBe(false)
    expect(
      workforceStreamNeedsAccessToken(
        'https://router.example/v1/workforce/events',
        'https://matrix.example',
      ),
    ).toBe(true)
  })

  it('projects threaded mail and never infers completion from activity', () => {
    const base = {
      version: 1,
      fields: {},
    }
    const threads = groupWorkforceMail([
      {
        ...base,
        id: 'message:reply',
        updated_at: '2026-07-30T12:01:00Z',
        fields: { thread_id: 'thread:one', kind: 'answer' },
      },
      {
        ...base,
        id: 'message:first',
        updated_at: '2026-07-30T12:00:00Z',
        fields: { thread_id: 'thread:one', kind: 'question' },
      },
    ])
    expect(threads).toHaveLength(1)
    expect(threads[0].messages.map((item) => item.id)).toEqual(['message:first', 'message:reply'])
    expect(
      isVerifiedCompletionReceipt({
        ...base,
        id: 'receipt:activity',
        updated_at: '2026-07-30T12:02:00Z',
        fields: { disposition: 'progress' },
      }),
    ).toBe(false)
    expect(
      isVerifiedCompletionReceipt({
        ...base,
        id: 'receipt:complete',
        updated_at: '2026-07-30T12:03:00Z',
        fields: { disposition: 'goal_completed' },
      }),
    ).toBe(true)
  })

  it('fails approval projection closed for revoked, expired, and consumed batches', () => {
    const item = {
      id: 'approval:one',
      version: 1,
      updated_at: '2026-07-30T12:00:00Z',
      fields: {
        expires_at: '2026-07-30T13:00:00Z',
        aggregate_ceiling_microunits: 100,
        consumed_microunits: 0,
      },
    }
    const now = new Date('2026-07-30T12:30:00Z')
    expect(approvalState(item, now)).toBe('available')
    expect(
      approvalState({ ...item, fields: { ...item.fields, revoked_at: now.toISOString() } }, now),
    ).toBe('revoked')
    expect(
      approvalState({ ...item, fields: { ...item.fields, expires_at: now.toISOString() } }, now),
    ).toBe('expired')
    expect(
      approvalState({ ...item, fields: { ...item.fields, consumed_microunits: 100 } }, now),
    ).toBe('consumed')
  })
})
