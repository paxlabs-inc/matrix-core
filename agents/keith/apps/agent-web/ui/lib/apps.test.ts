import { describe, expect, it } from 'vitest'

import {
  assertBrowserSafeConnectedApps,
  connectedAppsCommand,
  connectedAppsData,
  type ConnectedAppsCommandData,
} from '@/lib/apps'
import type { CommandResult } from '@/lib/keith'

const safeProjection: ConnectedAppsCommandData = {
  projection: {
    profile_id: 'profile-1',
    session: {
      state: 'active',
      generation: 3,
      expires_at: 2_000,
      last_transition_at: 1_000,
      safe_error: null,
    },
    accounts: [
      {
        id: 'account-1',
        toolkit: 'gmail',
        account_identity: 'person@example.com',
        auth_config_id: 'gmail-oauth',
        granted_scopes: ['email.read'],
        state: 'active',
        selection_precedence: 0,
        last_health_at: 1_000,
      },
    ],
    allowed_tools: [{ toolkit: 'gmail', tool: 'GMAIL_FETCH_EMAILS', risk: 'read' }],
  },
  auth_link: {
    redirect_url: 'https://connect.example/authorize?state=opaque',
    expires_at: 2_000,
  },
  audit_correlation: 'audit-1',
}

describe('connected Apps browser contract', () => {
  it('builds profile-scoped commands without accepting credential fields', () => {
    expect(connectedAppsCommand('profile-1', { action: 'test', account_id: 'account-1' })).toEqual({
      command: 'connected_apps',
      parameters: { profile_id: 'profile-1', action: 'test', account_id: 'account-1' },
    })
    expect(JSON.stringify(connectedAppsCommand('profile-1', { action: 'browse' }))).not.toMatch(
      /secret|access_token|api_key/i,
    )
  })

  it('accepts the complete non-secret account, health, tool, auth-link, and audit projection', () => {
    expect(() => assertBrowserSafeConnectedApps(safeProjection)).not.toThrow()
    const result: CommandResult = {
      protocol: { major: 1, minor: 0 },
      command_id: 'command-1',
      completed_at: 1_000,
      result: {
        status: 'data',
        payload: { kind: 'connected_apps_projection', value: safeProjection },
      },
    }
    expect(connectedAppsData(result)).toEqual(safeProjection)
  })

  it('refuses provider credentials, hosted MCP endpoints, and unsafe authorization URLs', () => {
    expect(() =>
      assertBrowserSafeConnectedApps({
        ...safeProjection,
        projection: {
          ...safeProjection.projection,
          accounts: [{ ...safeProjection.projection.accounts[0]!, access_token: 'leak' }],
        },
      } as unknown as ConnectedAppsCommandData),
    ).toThrow(/credential-bearing/)
    expect(() =>
      assertBrowserSafeConnectedApps({
        ...safeProjection,
        projection: { ...safeProjection.projection, mcp_endpoint: 'https://mcp.example' },
      } as unknown as ConnectedAppsCommandData),
    ).toThrow(/credential-bearing/)
    expect(() =>
      assertBrowserSafeConnectedApps({
        ...safeProjection,
        auth_link: { redirect_url: 'http://connect.example', expires_at: 2_000 },
      }),
    ).toThrow(/unsafe/)
  })
})
