import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { IntegrationPanel } from '@/components/KeithApp'
import type {
  Command,
  CommandResult,
  IntegrationService,
  ProfileIntegrationsProjection,
} from '@/lib/keith'

const profileId = '01J00000000000000000000301'
const sessionId = '01J00000000000000000000302'

afterEach(cleanup)

function projection(service: IntegrationService): ProfileIntegrationsProjection {
  const plugin = service === 'plugin'
  return {
    profile_id: profileId,
    through_sequence: 11,
    services: [{ service, availability: { state: 'available' } }],
    resources: [{
      id: plugin ? '01J00000000000000000000303' : '01J00000000000000000000304',
      profile_id: profileId,
      owning_session_id: sessionId,
      service,
      native_resource_key: plugin ? 'calendar-tools/2.1.0' : 'gmail/work',
      display_label: plugin ? 'Calendar Tools · Acme · 2.1.0' : 'Gmail work account',
      lifecycle: 'active',
      cancellation_id: '01J00000000000000000000305',
      audit_correlation: '01J00000000000000000000306',
      controls: ['cancel', 'export', 'delete'],
      safe_error: null,
      revision: 4,
      created_at: 1_000,
      updated_at: 2_000,
    }],
  }
}

function result(value: ProfileIntegrationsProjection): CommandResult {
  return {
    protocol: { major: 1, minor: 0 },
    command_id: '01J00000000000000000000307',
    completed_at: 2_001,
    result: {
      status: 'data',
      payload: { kind: 'profile_integrations', value },
    },
  }
}

describe('Apps and plugin authority surfaces', () => {
  it('keeps app connection and deletion behind trusted approval while permitting read-only health', async () => {
    const apps = projection('connected_app')
    const onCommand = vi.fn(async (command: Command) => {
      const parameters = command.parameters as { action?: string }
      return parameters.action === 'list'
        ? result(apps)
        : {
            protocol: { major: 1, minor: 0 },
            command_id: '01J00000000000000000000308',
            completed_at: 2_002,
            result: { status: 'accepted', payload: { action_id: null } },
          } satisfies CommandResult
    })
    render(
      <IntegrationPanel
        profileId={profileId}
        sessionId={sessionId}
        service="connected_app"
        projection={apps}
        onProjection={vi.fn()}
        onCommand={onCommand}
      />,
    )

    expect(screen.getByText('Gmail work account')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect app · verified approval required' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Remove · approval required' })).toBeDisabled()
    expect(screen.queryByLabelText(/api.?key|access.?token|password|secret/i)).not.toBeInTheDocument()

    const testConnection = screen.getByRole('button', { name: 'Test connection' })
    await waitFor(() => expect(testConnection).toBeEnabled())
    fireEvent.click(testConnection)
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith(expect.objectContaining({
      command: 'integration',
      parameters: expect.objectContaining({
        action: 'mutate',
        parameters: expect.objectContaining({
          profile_id: profileId,
          service: 'connected_app',
          native_resource_key: 'gmail/work',
          operation: 'test',
          authority: expect.objectContaining({
            risk: 'read_only',
            approval: { risk: 'read_only', state: { state: 'not_required' } },
          }),
        }),
      }),
    })))
  })

  it('shows signed plugin provenance while refusing browser-side install or uninstall approval', () => {
    const plugins = projection('plugin')
    render(
      <IntegrationPanel
        profileId={profileId}
        sessionId={sessionId}
        service="plugin"
        projection={plugins}
        onProjection={vi.fn()}
        onCommand={vi.fn().mockResolvedValue(result(plugins))}
      />,
    )

    expect(screen.getByText('Calendar Tools · Acme · 2.1.0')).toBeInTheDocument()
    expect(screen.getByText('calendar-tools/2.1.0')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Install signed plugin · approval required' })).toBeDisabled()
    for (const button of screen.getAllByRole('button', { name: 'Remove · approval required' })) {
      expect(button).toBeDisabled()
    }
    expect(screen.queryByRole('button', { name: /approve|grant/i })).not.toBeInTheDocument()
  })
})
