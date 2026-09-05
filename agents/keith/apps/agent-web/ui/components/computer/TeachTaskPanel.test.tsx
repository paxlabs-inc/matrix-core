import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { TeachTaskPanel, teachingProjection, type TeachingProjection } from './TeachTaskPanel'

const projection: TeachingProjection = {
  recording: {
    id: 'demonstration_1',
    title: 'Submit the weekly report',
    state: 'completed',
    elapsed_ms: 72_000,
    event_count: 4,
    control_owner: 'user_control',
    timeline: [{ sequence: 0, elapsed_ms: 0, kind: 'frame_captured', summary: 'The report page was visible.', control_owner: 'user_control', redacted: false }],
  },
  recipe: {
    id: 'recipe_1', title: 'Submit the weekly report', description: 'Open the report and submit it.', revision: 2,
    inputs: [{ name: 'report-file', label: 'Report file', kind: 'file', required: true }],
    steps: [{ id: 'step-1', title: 'Activate Continue', target_role: 'button', target_name: 'Continue', checkpoint: 'report-open', approval_reason: 'Confirm the current report', expected: ['The submitted message appears'] }],
    completion: ['The report is marked submitted'], checks_passed: 2, checks_total: 2, accepted: true, published_skill_id: null,
    versions: [{ revision: 1, parent_revision: null, rollback_of: null, created_at: 1, active: false }, { revision: 2, parent_revision: 1, rollback_of: null, created_at: 2, active: true }],
    replay: { state: 'passed', mode: 'shadow', checks: [{ description: 'Changed layout recovered', passed: true }], suggested_targets: [] },
  },
  safe_error: null,
}

describe('TeachTaskPanel', () => {
  it('accepts bounded teaching projections and refuses secret-bearing browser data', () => {
    expect(teachingProjection(projection)).toEqual(projection)
    expect(teachingProjection({ ...projection, credential_value: 'must-not-reach-browser' })).toBeNull()
    expect(teachingProjection({
      ...projection,
      recording: {
        ...projection.recording!,
        timeline: [{ ...projection.recording!.timeline[0], summary: 'Authorization: Bearer reusable-value' }],
      },
    })).toBeNull()
    expect(teachingProjection({ ...projection, recipe: { ...projection.recipe, published_skill_id: '../unsafe' } })).toBeNull()
  })

  it('sends revision-bound replay, correction, publication, rollback, and deletion actions', async () => {
    const onAction = vi.fn().mockResolvedValue({ ok: true })
    render(<TeachTaskPanel teaching={projection} onAction={onAction} />)

    fireEvent.click(screen.getByRole('button', { name: 'Run shadow replay' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'replay_shadow', recipe_id: 'recipe_1', revision: 2 }))

    fireEvent.click(screen.getByRole('button', { name: 'Correct target' }))
    fireEvent.change(screen.getByLabelText('Visible name'), { target: { value: 'Send report' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save correction' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'correct_target', recipe_id: 'recipe_1', revision: 2, step_id: 'step-1', role: 'button', accessible_name: 'Send report' }))

    fireEvent.click(screen.getByRole('checkbox', { name: /I reviewed this procedure/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Publish as skill' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'publish_recipe', recipe_id: 'recipe_1', revision: 2, skill_id: 'submit-the-weekly-report' }))

    fireEvent.click(screen.getByRole('button', { name: 'Restore' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'rollback_recipe', recipe_id: 'recipe_1', revision: 2, target_revision: 1 }))

    fireEvent.click(screen.getByRole('button', { name: 'Delete recording…' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete permanently' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({ action: 'delete_demonstration', demonstration_id: 'demonstration_1' }))
  })
})
