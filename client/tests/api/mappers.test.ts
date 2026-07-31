import { describe, it, expect } from 'vitest'
import {
  mapRunStatus,
  stageIndex,
  deriveProgress,
  intentToRun,
  attestationToReceipt,
  intentsToStats,
  intentsToActivity,
  deriveLiveStages,
} from '@/lib/api/mappers'
import { buildStages } from '@/lib/matrix-data'
import type { IntentSummary, IntentAttestation, SSEEvent } from '@/lib/api/types'

const fixture: IntentSummary = {
  intent_id: 'intent_abc',
  intent_hash: 'h_x',
  state: 'executing',
  prose: 'Compile a teardown of 5 note-taking apps',
  started_at: new Date(Date.now() - 60_000).toISOString(),
  envelope_count: 4,
  has_attest: false,
  has_fail: false,
  has_cancel: false,
}

describe('mapRunStatus', () => {
  it('async status overrides legacy state', () => {
    expect(mapRunStatus('executing', 'completed')).toBe('completed')
    expect(mapRunStatus('executing', 'failed')).toBe('failed')
    expect(mapRunStatus('completed', 'cancelled')).toBe('failed')
  })
  it('drafting/proposed/clarifying are queued', () => {
    expect(mapRunStatus('drafting')).toBe('queued')
    expect(mapRunStatus('proposed')).toBe('queued')
    expect(mapRunStatus('clarifying')).toBe('queued')
  })
  it('accepted/executing are running', () => {
    expect(mapRunStatus('accepted')).toBe('running')
    expect(mapRunStatus('executing')).toBe('running')
  })
})

describe('stageIndex', () => {
  it('returns failedAt for failed/cancelled', () => {
    expect(stageIndex('failed').failedAt).toBe(2)
    expect(stageIndex('cancelled').failedAt).toBe(2)
  })
  it('completed lights all 5 stages', () => {
    expect(stageIndex('completed').active).toBe(5)
  })
})

describe('deriveProgress', () => {
  it('100% when attest present', () => {
    expect(deriveProgress('executing', true)).toBe(100)
    expect(deriveProgress('completed', false)).toBe(100)
  })
  it('grows with state', () => {
    expect(deriveProgress('drafting', false)).toBeLessThan(deriveProgress('executing', false))
  })
})

describe('intentToRun', () => {
  it('lights every stage when attest is present', () => {
    const r = intentToRun({ ...fixture, has_attest: true, state: 'completed' })
    for (const s of r.stages) expect(s.status).toBe('done')
    expect(r.receiptId).toBe('rcpt_intent_abc')
  })
  it('preserves prose as title', () => {
    const r = intentToRun(fixture)
    expect(r.title).toContain('teardown')
  })
})

describe('attestationToReceipt', () => {
  const att: IntentAttestation = {
    intent_id: 'intent_abc',
    intent_hash: 'h_x',
    pre_replay_root: 'r1',
    post_replay_root: 'r1',
    signature: 'ed25519:...',
    signed_at: new Date().toISOString(),
    step_count: 9,
    tool_call_count: 4,
    cost_usd: 0.42,
    artifacts: [{ id: 'a1', name: 'brief.md', kind: 'file', summary: '500 words' }],
  }
  it('marks replayVerified when roots match', () => {
    const r = attestationToReceipt('intent_abc', 'goal', att)
    expect(r.replayVerified).toBe(true)
    expect(r.stepCount).toBe(9)
    expect(r.toolCallCount).toBe(4)
    expect(r.costUsd).toBeCloseTo(0.42)
  })
  it('does not mark replayVerified when roots differ', () => {
    const r = attestationToReceipt('intent_abc', 'goal', { ...att, post_replay_root: 'r2' })
    expect(r.replayVerified).toBe(false)
  })
})

describe('intentsToStats', () => {
  it('counts completed + active correctly', () => {
    const items: IntentSummary[] = [
      { ...fixture, state: 'completed', duration_ms: 60_000 },
      { ...fixture, intent_id: 'b', state: 'executing' },
      { ...fixture, intent_id: 'c', state: 'failed' },
    ]
    const s = intentsToStats(items)
    expect(s.tasksCompleted).toBe(1)
    expect(s.activeNow).toBe(1)
    expect(s.completionRate).toBe(33)
  })
})

describe('intentsToActivity', () => {
  it('groups by day with friendly labels', () => {
    const items: IntentSummary[] = [
      { ...fixture, intent_id: 'a', state: 'completed', ended_at: new Date().toISOString() },
    ]
    const days = intentsToActivity(items)
    expect(days.length).toBeGreaterThan(0)
    expect(days[0].items[0].kind).toBe('completed')
  })
})

/* deriveLiveStages — Plan→Gather→Execute→Verify→Sign step refinement
 * driven by SSE events. The seed is the snapshot mapped from the
 * polled intent state; events refine it forward only. */
describe('deriveLiveStages', () => {
  const ev = (type: string, fields: Record<string, unknown> = {}, seq = 1): SSEEvent => ({
    seq,
    ts: '2026-01-01T10:00:00Z',
    phase: 'lifecycle',
    type,
    fields,
  })

  it('returns the seed unchanged when no events are present', () => {
    const seed = buildStages(0)
    const r = deriveLiveStages(seed, [])
    expect(r.stages).toBe(seed)
    expect(r.allDone).toBe(false)
  })

  it('advances Plan -> Gather on lifecycle.transition to=proposed', () => {
    const seed = buildStages(0)
    const r = deriveLiveStages(seed, [ev('lifecycle.transition', { to: 'proposed' })])
    expect(r.stages[0].status).toBe('done')
    expect(r.stages[1].status).toBe('active')
  })

  it('advances to Execute active on to=accepted', () => {
    const seed = buildStages(0)
    const r = deriveLiveStages(seed, [ev('lifecycle.transition', { to: 'accepted' })])
    expect(r.stages[2].status).toBe('active')
    expect(r.stages[1].status).toBe('done')
  })

  it('advances to Verify active on walk.cortex.post', () => {
    const seed = buildStages(2)
    const r = deriveLiveStages(seed, [
      ev('lifecycle.transition', { to: 'executing' }, 1),
      ev('walk.cortex.post', {}, 2),
    ])
    expect(r.stages[2].status).toBe('done')
    expect(r.stages[3].status).toBe('active')
  })

  it('advances to Sign active on envelope.signed kind=intent.attest', () => {
    const seed = buildStages(2)
    const r = deriveLiveStages(seed, [
      ev('walk.cortex.post', {}, 1),
      ev('envelope.signed', { kind: 'intent.attest' }, 2),
    ])
    expect(r.stages[3].status).toBe('done')
    expect(r.stages[4].status).toBe('active')
  })

  it('marks every stage done when lifecycle reaches completed', () => {
    const seed = buildStages(0)
    const r = deriveLiveStages(seed, [ev('lifecycle.transition', { to: 'completed' })])
    expect(r.allDone).toBe(true)
    expect(r.stages.every((s) => s.status === 'done')).toBe(true)
  })

  it('marks the run failed when lifecycle reaches failed', () => {
    const seed = buildStages(2)
    const r = deriveLiveStages(seed, [ev('lifecycle.transition', { to: 'failed' })])
    expect(r.failedAt).toBeDefined()
    expect(r.stages.some((s) => s.status === 'failed')).toBe(true)
  })

  it('never regresses the stage index from the seed', () => {
    // Seed says Verify is already active; an out-of-order proposed
    // transition must not pull the stepper backwards.
    const seed = buildStages(3)
    const r = deriveLiveStages(seed, [ev('lifecycle.transition', { to: 'proposed' })])
    // Stage 3 should still be active or done; stages 0-2 done.
    expect(r.stages[3].status === 'active' || r.stages[3].status === 'done').toBe(true)
    expect(r.stages[0].status).toBe('done')
    expect(r.stages[1].status).toBe('done')
    expect(r.stages[2].status).toBe('done')
  })
})
