import type { BootstrapData, Command, CommandResult, Timestamp } from '@/lib/keith'

export type ConnectedAccountState =
  | 'connecting'
  | 'active'
  | 'disabled'
  | 'expired'
  | 'revoked'
  | 'failed'

export interface ConnectedAppToolProjection {
  toolkit: string
  tool: string
  risk: string
}

export interface ConnectedAppAccountProjection {
  id: string
  toolkit: string
  account_identity: string
  auth_config_id: string
  granted_scopes: string[]
  state: ConnectedAccountState
  selection_precedence: number
  link_expires_at?: Timestamp | null
  last_health_at: Timestamp
  safe_error?: string | null
}

export interface ConnectedAppSessionProjection {
  state: string
  generation: number
  expires_at: Timestamp
  last_transition_at: Timestamp
  safe_error?: string | null
}

export interface ConnectedAppsProjection {
  profile_id: string
  session?: ConnectedAppSessionProjection | null
  accounts: ConnectedAppAccountProjection[]
  allowed_tools: ConnectedAppToolProjection[]
}

export interface ConnectedAppAuthLink {
  redirect_url: string
  expires_at: Timestamp
}

export interface ConnectedAppsCommandData {
  projection: ConnectedAppsProjection
  auth_link?: ConnectedAppAuthLink | null
  audit_correlation?: string | null
  safe_result?: unknown
}

export type ConnectedAppsIntent =
  | { action: 'browse' }
  | { action: 'connect'; toolkit: string; auth_config_id: string }
  | { action: 'complete_callback'; account_id: string }
  | { action: 'test'; account_id: string }
  | { action: 'select'; account_id: string }
  | { action: 'set_tools'; toolkit: string; tools: string[] }
  | { action: 'disable'; account_id: string }
  | { action: 'resume'; account_id: string }
  | { action: 'revoke'; account_id: string }
  | { action: 'delete'; account_id: string }

export function connectedAppsCommand(profileId: string, intent: ConnectedAppsIntent): Command {
  return {
    command: 'connected_apps',
    parameters: { profile_id: profileId, ...intent },
  }
}

export function connectedAppsData(result: CommandResult): ConnectedAppsCommandData | null {
  if (result.result.status !== 'data' || result.result.payload.kind !== 'connected_apps_projection') {
    return null
  }
  const value = result.result.payload.value as ConnectedAppsCommandData
  assertBrowserSafeConnectedApps(value)
  return value
}

export function assertBrowserSafeConnectedApps(data: ConnectedAppsCommandData): void {
  const forbiddenKeys = new Set([
    'access_token',
    'api_key',
    'authorization',
    'bearer',
    'mcp_endpoint',
    'mcp_server_id',
    'provider_account_id',
    'provider_session_id',
    'provider_user_id',
    'secret',
  ])
  const visit = (value: unknown): void => {
    if (!value || typeof value !== 'object') return
    if (Array.isArray(value)) {
      value.forEach(visit)
      return
    }
    for (const [key, child] of Object.entries(value)) {
      if (forbiddenKeys.has(key.toLowerCase())) {
        throw new Error('Keith returned a credential-bearing connected-app projection.')
      }
      visit(child)
    }
  }
  visit(data)
  if (!data.projection.profile_id || !Array.isArray(data.projection.accounts)) {
    throw new Error('Keith returned an invalid connected-app projection.')
  }
  if (data.auth_link) {
    const link = new URL(data.auth_link.redirect_url)
    if (link.protocol !== 'https:' || link.username || link.password) {
      throw new Error('Keith returned an unsafe connected-app authorization link.')
    }
  }
}

export async function executeConnectedApps(
  bootstrap: BootstrapData,
  profileId: string,
  intent: ConnectedAppsIntent,
): Promise<ConnectedAppsCommandData> {
  const response = await fetch('/api/connected-apps/commands', {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      accept: 'application/json',
      'content-type': 'application/json',
      'x-keith-csrf': bootstrap.csrf,
    },
    body: JSON.stringify({ profile_id: profileId, ...intent }),
  })
  if (!response.ok) throw new Error('Keith could not complete the connected-app request.')
  const data = (await response.json()) as ConnectedAppsCommandData
  assertBrowserSafeConnectedApps(data)
  return data
}
