import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { IntegrationPanel } from '@/components/KeithApp'
import type { Command, CommandResult, ProfileIntegrationsProjection } from '@/lib/keith'

const profileId = '01J00000000000000000000201'
const sessionId = '01J00000000000000000000202'
const projection: ProfileIntegrationsProjection = {
  profile_id: profileId,
  through_sequence: 5,
  services: [{ service: 'channel_account', availability: { state: 'available' } }],
  resources: [{
    id: '01J00000000000000000000203',
    profile_id: profileId,
    owning_session_id: sessionId,
    service: 'channel_account',
    native_resource_key: 'slack/work',
    display_label: 'Slack work account',
    lifecycle: 'active',
    cancellation_id: '01J00000000000000000000204',
    audit_correlation: '01J00000000000000000000205',
    controls: ['cancel', 'export', 'delete'],
    safe_error: null,
    revision: 2,
    created_at: 1_000,
    updated_at: 2_000,
  }],
}
const projectionResult: CommandResult = {
  protocol: { major: 1, minor: 0 },
  command_id: '01J00000000000000000000206',
  completed_at: 2_001,
  result: {
    status: 'data',
    payload: { kind: 'profile_integrations', value: projection },
  },
}

describe('channel account surface', () => {
  it('inspects and controls admitted accounts without collecting raw credentials or fabricating approval', async () => {
    const accepted: CommandResult = {
      protocol: { major: 1, minor: 0 },
      command_id: '01J00000000000000000000207',
      completed_at: 2_002,
      result: { status: 'accepted', payload: { action_id: null } },
    }
    const onCommand = vi.fn(async (command: Command) => {
      const parameters = command.parameters as { action?: string }
      return parameters.action === 'list' ? projectionResult : accepted
    })
    const onProjection = vi.fn()
    render(
      <IntegrationPanel
        profileId={profileId}
        sessionId={sessionId}
        service="channel_account"
        projection={projection}
        onProjection={onProjection}
        onCommand={onCommand}
      />,
    )

    expect(screen.getByText('Slack work account')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect account · approval required' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Configure · approval required' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Remove · approval required' })).toBeDisabled()
    expect(screen.queryByLabelText(/secret|token|password/i)).not.toBeInTheDocument()

    await waitFor(() => expect(onProjection).toHaveBeenCalledWith(projection))
    fireEvent.click(screen.getByRole('button', { name: 'Pause' }))
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({
      command: 'integration',
      parameters: expect.objectContaining({
        action: 'mutate',
        parameters: expect.objectContaining({
          profile_id: profileId,
          service: 'channel_account',
          resource_id: projection.resources[0]!.id,
          expected_revision: 2,
          operation: 'pause',
          authority: expect.objectContaining({
            risk: 'reversible_local_write',
            approval: { risk: 'reversible_local_write', state: { state: 'not_required' } },
          }),
        }),
      }),
    })))
  })
})
