import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { IntegrationPanel } from '@/components/KeithApp'
import type {
  Command,
  CommandResult,
  ProfileIntegrationsProjection,
} from '@/lib/keith'

const profileId = '01J00000000000000000000101'
const sessionId = '01J00000000000000000000102'

const projection: ProfileIntegrationsProjection = {
  profile_id: profileId,
  through_sequence: 9,
  services: [{ service: 'connected_app', availability: { state: 'available' } }],
  resources: [{
    id: '01J00000000000000000000103',
    profile_id: profileId,
    owning_session_id: sessionId,
    service: 'connected_app',
    native_resource_key: 'github/primary',
    display_label: 'GitHub work account',
    lifecycle: 'active',
    cancellation_id: '01J00000000000000000000104',
    audit_correlation: '01J00000000000000000000105',
    controls: ['cancel', 'export', 'delete'],
    safe_error: null,
    revision: 3,
    created_at: 1_000,
    updated_at: 2_000,
  }],
}

const listResult: CommandResult = {
  protocol: { major: 1, minor: 0 },
  command_id: '01J00000000000000000000106',
  completed_at: 2_000,
  result: {
    status: 'data',
    payload: { kind: 'profile_integrations', value: projection },
  },
}

describe('unified platform surfaces', () => {
  it('renders truthful lifecycle and sends exact cancellation without fabricating deletion approval', async () => {
    const accepted: CommandResult = {
      protocol: { major: 1, minor: 0 },
      command_id: '01J00000000000000000000107',
      completed_at: 2_001,
      result: { status: 'accepted', payload: { action_id: null } },
    }
    const onCommand = vi.fn(async (command: Command) => {
      const parameters = command.parameters as { action?: string }
      return parameters.action === 'list' ? listResult : accepted
    })
    const onProjection = vi.fn()
    render(
      <IntegrationPanel
        profileId={profileId}
        sessionId={sessionId}
        service="connected_app"
        projection={projection}
        onProjection={onProjection}
        onCommand={onCommand}
      />,
    )

    expect(screen.getByRole('region', { name: 'Connected Apps' })).toBeInTheDocument()
    expect(screen.getByText('Service enabled')).toBeInTheDocument()
    expect(screen.getByText('GitHub work account')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Remove · approval required' })).toBeDisabled()

    await waitFor(() => expect(onProjection).toHaveBeenCalledWith(projection))
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() => {
      const mutation = onCommand.mock.calls
        .map(([command]) => command)
        .find((command) => (command.parameters as { action?: string }).action === 'mutate')
      expect(mutation).toMatchObject({
        command: 'integration',
        parameters: {
          action: 'mutate',
          parameters: {
            profile_id: profileId,
            resource_id: projection.resources[0]!.id,
            expected_revision: 3,
            operation: 'cancel',
            authority: {
              cancellation_id: projection.resources[0]!.cancellation_id,
              approval: { state: { state: 'not_required' } },
            },
          },
        },
      })
    })
  })
})
