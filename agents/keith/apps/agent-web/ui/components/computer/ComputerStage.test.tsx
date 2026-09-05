import { fireEvent, render, screen as page, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ComputerStage, screenProjection, type ComputerScreenProjection } from './ComputerStage'

const projection: ComputerScreenProjection = {
  id: 'screen_1',
  computer_session_id: 'computer_1',
  profile_id: 'profile_1',
  lifecycle: 'running',
  connection: 'connected',
  quality: 'high',
  owner: 'keith_control',
  lease_revision: 7,
  frame_sequence: 9,
  viewport: { width: 1440, height: 900 },
  stream_path: '/api/computers/screen_1/screen',
  active_action: 'Reviewing the form',
  intended_action: 'Wait for approval',
  recording: true,
  safe_error: null,
}

describe('ComputerStage', () => {
  it('accepts only bounded typed projections and same-origin screen paths', () => {
    expect(screenProjection({
      ...projection,
      control: { owner: 'keith_control', revision: 7 },
    })).toEqual(projection)
    expect(screenProjection({
      ...projection,
      stream_path: '/api/computers/screen_1/screen?ticket=reusable-secret',
      control: { owner: 'keith_control', revision: 7 },
    })?.stream_path).toBeNull()
    expect(screenProjection({
      ...projection,
      control: { owner: 'both', revision: 7 },
    })).toBeNull()
    expect(screenProjection({
      ...projection,
      active_action: 'Authorization: Bearer reusable-secret',
      control: { owner: 'keith_control', revision: 7 },
    })?.active_action).toBeNull()
  })

  it('renders truthful control and sends an exact lease-scoped takeover command', async () => {
    const onCommand = vi.fn().mockResolvedValue({
      protocol: { major: 1, minor: 0 },
      command_id: 'command_1',
      completed_at: 1,
      result: { status: 'accepted', payload: { action_id: 'action_1' } },
    })
    render(
      <ComputerStage
        screen={projection}
        csrf="csrf_1"
        fallback={<p>Fallback</p>}
        onCommand={onCommand}
      />,
    )
    expect(page.getByText('Keith has control')).toBeInTheDocument()
    expect(page.getByText('Recording')).toBeInTheDocument()
    fireEvent.click(page.getByRole('button', { name: 'Take control' }))
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith({
      command: 'computer',
      parameters: {
        action: 'take_user_control',
        screen_id: 'screen_1',
        computer_session_id: 'computer_1',
        expected_revision: 7,
      },
    }))
  })
})
