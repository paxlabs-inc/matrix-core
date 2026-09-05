import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  ComputerStage,
  screenProjection,
  type ComputerScreenProjection,
} from '@/components/computer/ComputerStage'
import {
  TeachTaskPanel,
  teachingProjection,
  type TeachingProjection,
} from '@/components/computer/TeachTaskPanel'

afterEach(cleanup)

const computer: ComputerScreenProjection = {
  id: 'screen_qualification',
  computer_session_id: 'computer_qualification',
  profile_id: 'profile_qualification',
  lifecycle: 'running',
  connection: 'connected',
  quality: 'high',
  owner: 'user_control',
  lease_revision: 12,
  frame_sequence: 44,
  viewport: { width: 1280, height: 720 },
  stream_path: '/api/computers/screen_qualification/screen',
  active_action: 'Waiting for your manual change',
  intended_action: 'Resume after control returns',
  recording: true,
  safe_error: null,
}

const teaching: TeachingProjection = {
  recording: {
    id: 'demonstration_qualification',
    title: 'Submit the report',
    state: 'completed',
    elapsed_ms: 12_400,
    event_count: 6,
    control_owner: 'user_control',
    timeline: [{
      sequence: 1,
      elapsed_ms: 400,
      kind: 'credential_parameterized',
      summary: 'Sensitive field replaced with primary-login.',
      control_owner: 'user_control',
      redacted: true,
    }],
  },
  recipe: {
    id: 'recipe_qualification',
    title: 'Submit the report',
    description: 'Open, review, and submit the report.',
    revision: 3,
    inputs: [{ name: 'report-file', label: 'Report file', kind: 'file', required: true }],
    steps: [{
      id: 'submit-step',
      title: 'Submit the report',
      target_role: 'button',
      target_name: 'Send report',
      checkpoint: 'reviewed',
      approval_reason: 'Confirm the exact report and recipient',
      expected: ['Submission confirmation is visible'],
    }],
    completion: ['Submission confirmation is visible'],
    checks_passed: 1,
    checks_total: 1,
    accepted: true,
    published_skill_id: null,
    versions: [
      { revision: 2, parent_revision: 1, rollback_of: null, created_at: 2, active: false },
      { revision: 3, parent_revision: 2, rollback_of: null, created_at: 3, active: true },
    ],
    replay: {
      state: 'passed',
      mode: 'shadow',
      checks: [{ description: 'Changed target recovered', passed: true }],
      suggested_targets: [],
    },
  },
  safe_error: null,
}

describe('Keith Computer and teaching web journey', () => {
  it('renders the authenticated live screen and sends input only under the current user lease', async () => {
    const onCommand = vi.fn().mockResolvedValue({
      protocol: { major: 1, minor: 0 },
      command_id: 'command_qualification',
      completed_at: 1,
      result: { status: 'accepted', payload: { action_id: null } },
    })
    render(
      <ComputerStage
        screen={computer}
        csrf="csrf_qualification"
        fallback={<p>Computer unavailable</p>}
        onCommand={onCommand}
      />,
    )

    expect(screen.getByText('You have control')).toBeInTheDocument()
    expect(screen.getByText('Recording')).toBeInTheDocument()
    expect(screen.getByText('Waiting for your manual change')).toBeInTheDocument()
    expect(screen.getByRole('img', { name: "Live screen from Keith's isolated computer" }))
      .toHaveAttribute('src', '/api/computers/screen_qualification/screen')

    const viewport = screen.getByLabelText('Computer screen. Keyboard and pointer input are active.')
    fireEvent.keyDown(viewport, { key: 'Enter', code: 'Enter' })
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith({
      command: 'computer',
      parameters: {
        action: 'input',
        screen_id: computer.id,
        computer_session_id: computer.computer_session_id,
        expected_revision: 12,
        input: 'keyboard',
        key: 'Enter',
        code: 'Enter',
        alt: false,
        control: false,
        meta: false,
        shift: false,
        frame_sequence: 44,
      },
    }))

    expect(screenProjection({
      ...computer,
      control: { owner: 'user_control', revision: 12 },
      stream_path: '/api/computers/screen_qualification/screen?ticket=reusable',
    })?.stream_path).toBeNull()
  })

  it('keeps synchronized redacted teaching data revision-bound through checkpoint replay', async () => {
    expect(teachingProjection(teaching)).toEqual(teaching)
    expect(teachingProjection({
      ...teaching,
      recording: {
        ...teaching.recording!,
        timeline: [{
          ...teaching.recording!.timeline[0]!,
          summary: 'password=raw-secret',
        }],
      },
    })).toBeNull()

    const onAction = vi.fn().mockResolvedValue({ ok: true })
    render(<TeachTaskPanel teaching={teaching} onAction={onAction} />)
    fireEvent.change(screen.getByLabelText('Replay checkpoint'), { target: { value: 'reviewed' } })
    fireEvent.click(screen.getByRole('button', { name: 'Replay from checkpoint' }))
    await waitFor(() => expect(onAction).toHaveBeenCalledWith({
      action: 'replay_checkpoint',
      recipe_id: 'recipe_qualification',
      revision: 3,
      checkpoint: 'reviewed',
    }))
    expect(screen.getByText(/sensitive value replaced/)).toBeInTheDocument()
    expect(screen.getByText('Changed target recovered')).toBeInTheDocument()
  })
})
