import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ConnectedAppsPanel } from './ConnectedAppsPanel'

const response = {
  protocol: { major: 1, minor: 0 },
  command_id: 'command_1',
  completed_at: 1,
  result: {
    status: 'data' as const,
    payload: {
      kind: 'connected_apps_projection',
      value: {
        projection: {
          profile_id: 'profile_1',
          session: {
            state: 'active',
            generation: 2,
            expires_at: 2_000,
            last_transition_at: 1_000,
            safe_error: null,
          },
          accounts: [{
            id: 'account_1',
            toolkit: 'gmail',
            account_identity: 'person@example.com',
            auth_config_id: 'gmail-oauth',
            granted_scopes: ['email.read'],
            state: 'active',
            selection_precedence: 0,
            last_health_at: 1_000,
          }],
          allowed_tools: [{ toolkit: 'gmail', tool: 'GMAIL_FETCH_EMAILS', risk: 'read' }],
        },
        audit_correlation: 'audit_1',
      },
    },
  },
}

describe('ConnectedAppsPanel', () => {
  it('loads the safe projection and sends profile-scoped lifecycle and allowlist commands', async () => {
    const onCommand = vi.fn().mockResolvedValue(response)
    render(<ConnectedAppsPanel profileId="profile_1" onCommand={onCommand} />)
    expect(await screen.findByText('person@example.com')).toBeInTheDocument()
    expect(onCommand).toHaveBeenNthCalledWith(1, {
      command: 'connected_apps',
      parameters: { profile_id: 'profile_1', action: 'browse' },
    })

    fireEvent.click(screen.getByRole('button', { name: 'Test' }))
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith({
      command: 'connected_apps',
      parameters: { profile_id: 'profile_1', action: 'test', account_id: 'account_1' },
    }))

    fireEvent.change(screen.getByLabelText('Allowed tools'), {
      target: { value: 'GMAIL_FETCH_EMAILS, GMAIL_SEND_EMAIL' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save allowlist' }))
    await waitFor(() => expect(onCommand).toHaveBeenCalledWith({
      command: 'connected_apps',
      parameters: {
        profile_id: 'profile_1',
        action: 'set_tools',
        toolkit: 'gmail',
        tools: ['GMAIL_FETCH_EMAILS', 'GMAIL_SEND_EMAIL'],
      },
    }))
  })

  it('keeps the panel explicitly unavailable without a profile', () => {
    render(<ConnectedAppsPanel profileId={null} onCommand={vi.fn()} />)
    expect(screen.getByText('Choose a Keith profile before connecting an app.')).toBeInTheDocument()
  })
})
