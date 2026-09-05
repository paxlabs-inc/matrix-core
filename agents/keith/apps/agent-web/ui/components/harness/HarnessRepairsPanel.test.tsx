import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { HarnessRepairsPanel, harnessRepairsProjection, type HarnessRepairsProjection } from './HarnessRepairsPanel'

const projection: HarnessRepairsProjection = {
  availability: { advisory: true, shadow: true, autonomous: true },
  selected_mode: 'shadow',
  repairs: [{
    id: 'repair_1', candidate_id: 'candidate_1', mode: 'shadow', phase: 'canary_passed',
    headline: 'Retry routing repair', summary: 'The canary passed. Live promotion is waiting for approval.',
    metrics: { cases: 30, task_success_basis_points: 9200, truthful_completion_basis_points: 10000, safety_basis_points: 10000, correction_adherence_basis_points: 10000, tokens: 42, external_cost_micros: 20, latency_ms: 450, retries: 0, cpu_ms: 300, peak_memory_bytes: 1048576, disk_bytes: 2048 },
    needs_approval: true, can_retry_current_task: false, can_promote: false, can_reverse: false,
    created_at: 1, updated_at: 2,
  }],
  safe_error: null,
}

describe('HarnessRepairsPanel', () => {
  it('accepts bounded safe projections and rejects hidden evaluator or secret material', () => {
    expect(harnessRepairsProjection(projection)).toEqual({ ...projection, availability: { ...projection.availability, shadow_unavailable_reason: null, autonomous_unavailable_reason: null } })
    expect(harnessRepairsProjection({ ...projection, held_out_cases: ['private'] })).toBeNull()
    expect(harnessRepairsProjection({ ...projection, repairs: [{ ...projection.repairs[0], summary: 'Authorization: Bearer reusable-secret-value' }] })).toBeNull()
    expect(harnessRepairsProjection({ ...projection, repairs: [{ ...projection.repairs[0], expected_output: 'private answer' }] })).toBeNull()
  })

  it('keeps retry blocked until authority is projected and sends exact approval and mode actions', async () => {
    const onAction = vi.fn().mockResolvedValue({ ok: true })
    const { rerender } = render(<HarnessRepairsPanel harness={projection} onAction={onAction} />)
    expect(screen.getByText('Current-task retry remains blocked')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Retry current task' })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Approve exact repair' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'approve', operation_id: 'repair_1' }))
    fireEvent.click(screen.getByRole('radio', { name: /Autonomous repair/ }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'set_mode', mode: 'autonomous' }))

    rerender(<HarnessRepairsPanel harness={{ ...projection, repairs: [{ ...projection.repairs[0], needs_approval: false, can_retry_current_task: true, can_promote: true }] }} onAction={onAction} />)
    fireEvent.click(screen.getByRole('button', { name: 'Retry current task' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'retry_current_task', operation_id: 'repair_1' }))
  })

  it('requires a second action before reversal', async () => {
    const onAction = vi.fn().mockResolvedValue({ ok: true })
    render(<HarnessRepairsPanel harness={{ ...projection, repairs: [{ ...projection.repairs[0], phase: 'reversal_required', needs_approval: false, can_reverse: true }] }} onAction={onAction} />)
    fireEvent.click(screen.getByRole('button', { name: 'Restore prior version…' }))
    expect(onAction).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm restore' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'reverse', operation_id: 'repair_1' }))
  })
})
